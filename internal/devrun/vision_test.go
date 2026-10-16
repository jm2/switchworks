// SPDX-License-Identifier: GPL-3.0-or-later

package devrun

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/acp/acptest"
	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/attach"
	"github.com/jm2/switchyard/internal/codex/codextest"
	"github.com/jm2/switchyard/internal/harness"
)

func TestMain(m *testing.M) {
	if codextest.MaybeRun() || acptest.MaybeRun() {
		return
	}
	os.Exit(m.Run())
}

func TestProbePNG(t *testing.T) {
	code := NewProbeCode(6)
	if len(code) != 6 || strings.Trim(code, probeAlphabet) != "" {
		t.Fatalf("code %q", code)
	}
	b, err := ProbePNG(code)
	if err != nil {
		t.Fatal(err)
	}
	if attach.Sniff(b) != "image/png" || attach.PNGText(b, attach.ProbeKey) != code {
		t.Fatal("not a PNG carrying its code")
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	// Black text on white: both appear, and the corner is white.
	black, white := 0, 0
	r := img.Bounds()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if v, _, _, _ := img.At(x, y).RGBA(); v == 0 {
				black++
			} else {
				white++
			}
		}
	}
	if v, _, _, _ := img.At(0, 0).RGBA(); v == 0 || black == 0 || white < black {
		t.Fatalf("black %d white %d", black, white)
	}
	if r.Dx() < 300 || r.Dy() < 100 {
		t.Fatalf("image too small to read: %v", r)
	}
	if len(b) > 64<<10 {
		t.Fatalf("probe image is %d bytes", len(b))
	}
	if _, err := ProbePNG("O0"); err == nil {
		t.Fatal("look-alike characters accepted")
	}
	for _, c := range probeAlphabet {
		if _, ok := glyphs[c]; !ok {
			t.Fatalf("no glyph for %q", c)
		}
	}
}

func probeSpec(t *testing.T) adapter.StartSpec {
	return adapter.StartSpec{Name: "dev/vision-probe", Cwd: t.TempDir(), ApprovalPolicy: "on-request", Sandbox: "read-only"}
}

// The probe end to end through the Codex adapter (localImage files) and
// the fake app-server, which reads the image file it is given.
func TestProbeCodexFake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opts := harness.Options{CodexBin: os.Args[0], CodexArgs: []string{"app-server"}, CodexEnv: codextest.Env(t.TempDir()), Logf: t.Logf}
	res, err := Probe(ctx, opts, "codex", probeSpec(t), t.Logf)
	if err != nil || !res.Pass || res.Answer != res.Code {
		t.Fatalf("probe: %+v %v", res, err)
	}
	var out bytes.Buffer
	if printProbe(&out, io.Discard, res, nil) != 0 || !strings.HasPrefix(out.String(), "PASS") {
		t.Fatalf("report %q", out.String())
	}
}

// The probe through the ACP adapter (image content blocks) and the fake
// ACP agent; an agent that does not advertise images is refused.
func TestProbeACPFake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opts := harness.Options{ACP: map[string]harness.ACPAgent{"fakeacp": {Bin: os.Args[0], Env: acptest.Env(t.TempDir())}}, Logf: t.Logf}
	res, err := Probe(ctx, opts, "fakeacp", probeSpec(t), t.Logf)
	if err != nil || !res.Pass {
		t.Fatalf("probe: %+v %v", res, err)
	}

	env := acptest.Env(t.TempDir())
	env[acptest.EnvNoImages] = "1"
	opts = harness.Options{ACP: map[string]harness.ACPAgent{"fakeacp": {Bin: os.Args[0], Env: env}}, Logf: t.Logf}
	res, err = Probe(ctx, opts, "fakeacp", probeSpec(t), t.Logf)
	if !errors.Is(err, adapter.ErrNoImages) || res.Pass {
		t.Fatalf("probe without image support: %+v %v", res, err)
	}
	var out bytes.Buffer
	if printProbe(&out, io.Discard, res, err) != 1 || !strings.HasPrefix(out.String(), "FAIL") {
		t.Fatalf("report %q", out.String())
	}
}

func TestNormalizeAnswer(t *testing.T) {
	for _, c := range []struct {
		answer string
		pass   bool
	}{
		{"K7XW3P", true}, {"k7xw3p", true}, {"`K7XW3P`", true}, {"The code is K7XW3P.", true},
		{"K7XW3", false}, {"", false}, {"I cannot see images.", false},
	} {
		a := normalizeAnswer(c.answer)
		got := a != "" && strings.Contains(a, "K7XW3P") && len(a) <= len("K7XW3P")+40
		if got != c.pass {
			t.Errorf("%q: pass %v", c.answer, got)
		}
	}
}

// The mock provider reads images in both its emulations: the probe's code
// from the PNG's text chunk, and the digest of any other image.
func TestMockProviderSeesImages(t *testing.T) {
	m, err := codextest.StartMockProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	code := NewProbeCode(6)
	img, _ := ProbePNG(code)
	b64 := base64.StdEncoding.EncodeToString(img)
	post := func(path string, body any) string {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(m.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return string(out)
	}
	// OpenAI Responses: Codex sends a local image as an input_image data URL.
	resp := post("/responses", map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": ProbePrompt},
		map[string]any{"type": "input_text", "text": "<image name=[Image #1]>"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + b64},
		map[string]any{"type": "input_text", "text": "</image>"},
	}}}})
	if !strings.Contains(resp, `"text":"`+code+`"`) {
		t.Fatalf("responses: no code %s in %s", code, resp)
	}
	resp = post("/responses", map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": "what is this"},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + b64},
	}}}})
	if !strings.Contains(resp, "sha256="+attach.SHA256(img)) {
		t.Fatalf("responses: no digest in %s", resp)
	}
	// Anthropic Messages: base64 image blocks.
	resp = post("/messages", map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": ProbePrompt},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": b64}},
	}}}})
	if !strings.Contains(resp, `"text":"`+code+`"`) {
		t.Fatalf("messages: no code %s in %s", code, resp)
	}
	// No image: the plain script.
	resp = post("/messages", map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": ProbePrompt}}})
	if strings.Contains(resp, code) || !strings.Contains(resp, "mock: ") {
		t.Fatalf("messages without image: %s", resp)
	}
}

// With a real goose (SWYD_GOOSE_BIN or goose on PATH), the probe goes
// through goose's ACP image blocks to the mock's Anthropic emulation.
func TestProbeRealGoose(t *testing.T) {
	bin := os.Getenv("SWYD_GOOSE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose not installed (set SWYD_GOOSE_BIN to run this test)")
		}
	}
	m, err := codextest.StartMockProvider()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	home := t.TempDir()
	noProxy := "127.0.0.1,localhost"
	opts := harness.Options{Logf: t.Logf, ACP: map[string]harness.ACPAgent{"goose": {Bin: bin, Env: map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME": filepath.Join(home, ".local", "state"),
		"GOOSE_PROVIDER": "anthropic", "GOOSE_MODEL": "claude-sonnet-4-5",
		"ANTHROPIC_HOST": m.AnthropicHost(), "ANTHROPIC_API_KEY": "mock",
		"GOOSE_DISABLE_KEYRING": "1", "GOOSE_TELEMETRY_ENABLED": "false",
		"NO_PROXY": noProxy, "no_proxy": noProxy,
	}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := Probe(ctx, opts, "goose", probeSpec(t), t.Logf)
	if err != nil || !res.Pass {
		t.Fatalf("goose probe: %+v %v", res, err)
	}
}

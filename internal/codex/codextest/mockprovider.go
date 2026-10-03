package codextest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MockProvider is a minimal OpenAI Responses-API endpoint for running the
// real Codex binary without an account or network access. Point Codex at it
// with a model_providers entry (see WriteCodexHome).
//
// Behavior, keyed on the latest user message:
//   - "RUN: <cmd>": the model calls exec_command with <cmd>; once Codex sends
//     the function_call_output back, the model answers "Command finished."
//   - anything else: the model answers "mock: <message>" in three deltas
//
// Every response reports usage of 120 input (40 cached) + 7 output tokens.
type MockProvider struct {
	URL  string // base URL including /v1
	srv  *http.Server
	ln   net.Listener
	mu   sync.Mutex
	reqs int
}

// StartMockProvider listens on 127.0.0.1 on a random port.
func StartMockProvider() (*MockProvider, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &MockProvider{ln: ln, URL: "http://" + ln.Addr().String() + "/v1"}
	m.srv = &http.Server{Handler: http.HandlerFunc(m.serve), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = m.srv.Serve(ln) }()
	return m, nil
}

// Close stops the server.
func (m *MockProvider) Close() error { return m.srv.Close() }

// Requests returns how many model requests were served.
func (m *MockProvider) Requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reqs
}

// WriteCodexHome creates a CODEX_HOME directory whose config.toml routes
// Codex to this provider, and returns environment overrides for app-server.
func (m *MockProvider) WriteCodexHome(dir string) (map[string]string, error) {
	cfg := fmt.Sprintf(`model = "mock-model"
model_provider = "mock"

[model_providers.mock]
name = "Switchyard mock"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
`, m.URL)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o644); err != nil {
		return nil, err
	}
	noProxy := "127.0.0.1,localhost"
	if v := os.Getenv("NO_PROXY"); v != "" {
		noProxy = v + "," + noProxy
	}
	return map[string]string{"CODEX_HOME": dir, "NO_PROXY": noProxy, "no_proxy": noProxy}, nil
}

type respInput struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (m *MockProvider) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"models":[]}`))
		return
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.reqs++
	n := m.reqs
	m.mu.Unlock()

	lastUser, sawOutput := "", false
	for _, raw := range body.Input {
		var it respInput
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		switch it.Type {
		case "function_call_output":
			sawOutput = true
		case "message":
			if it.Role == "user" {
				sawOutput = false
				for _, c := range it.Content {
					if c.Type == "input_text" {
						lastUser = c.Text
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	ev := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", v["type"], b)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(10 * time.Millisecond)
	}
	respID := fmt.Sprintf("resp_%d", n)
	ev(map[string]any{"type": "response.created", "response": map[string]any{"id": respID}})

	switch {
	case !sawOutput && strings.HasPrefix(lastUser, "RUN: "):
		args, _ := json.Marshal(map[string]any{"cmd": strings.TrimPrefix(lastUser, "RUN: ")})
		item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", n), "call_id": fmt.Sprintf("call_%d", n),
			"name": "exec_command", "arguments": string(args)}
		ev(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		ev(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	default:
		text := "mock: " + lastUser
		if sawOutput {
			text = "Command finished."
		}
		msgID := fmt.Sprintf("msg_%d", n)
		ev(map[string]any{"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"type": "message", "role": "assistant", "id": msgID, "content": []any{}}})
		third := len(text) / 3
		for _, d := range []string{text[:third], text[third : 2*third], text[2*third:]} {
			ev(map[string]any{"type": "response.output_text.delta", "item_id": msgID, "output_index": 0, "content_index": 0, "delta": d})
		}
		ev(map[string]any{"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{"type": "message", "role": "assistant", "id": msgID,
				"content": []any{map[string]any{"type": "output_text", "text": text}}}})
	}
	ev(map[string]any{"type": "response.completed", "response": map[string]any{"id": respID, "usage": map[string]any{
		"input_tokens": 120, "input_tokens_details": map[string]any{"cached_tokens": 40},
		"output_tokens": 7, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 127}}})
}

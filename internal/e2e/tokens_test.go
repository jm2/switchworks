// SPDX-License-Identifier: GPL-3.0-or-later

package e2e

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/cli"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/node"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func wantForbidden(t *testing.T, what string, err error) {
	t.Helper()
	var ae *apiclient.APIError
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("%s: got %v, want 403", what, err)
	}
}

// A workstation that runs agents gets an operate-scoped cli.json made by
// `swyd token create --cli-config`, never the admin token. With it the CLI
// drives sessions but cannot change the fleet.
func TestScopedCLI(t *testing.T) {
	ctx := context.Background()
	coordDir := t.TempDir()
	nodeDir := t.TempDir()
	work := t.TempDir()
	co := startCoordinator(t, coordDir, "127.0.0.1:0", "127.0.0.1:0")
	defer func() { co.stop() }()
	admin := apiClient(t, coordDir)

	tok, err := admin.EnrollToken(ctx, "devbox", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Enroll(ctx, nodeDir, tok, false); err != nil {
		t.Fatal(err)
	}
	nd := startNode(t, nodeDir, t.TempDir())
	defer nd.stop(t)
	waitOnline(t, admin, "devbox")

	// On the admin machine: make a cli.json for the workstation.
	t.Setenv("SWYD_CLI_CONFIG", filepath.Join(coordDir, "cli.json"))
	code, out, errOut := runCLI(t, "token", "create", "devbox-cli", "--scope", "operate", "--cli-config")
	if code != 0 {
		t.Fatalf("token create: %d %s", code, errOut)
	}
	scoped := filepath.Join(t.TempDir(), "cli.json")
	if err := os.WriteFile(scoped, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := apiclient.LoadConfig(scoped)
	if err != nil {
		t.Fatalf("generated cli.json: %v\n%s", err, out)
	}
	adminCfg, _ := apiclient.LoadConfig(filepath.Join(coordDir, "cli.json"))
	if cfg.API != adminCfg.API || cfg.CA != adminCfg.CA || cfg.Token == adminCfg.Token {
		t.Fatal("generated cli.json must keep the API and CA and carry the new token, not the admin's")
	}
	if code, out, _ := runCLI(t, "token", "ls"); code != 0 || !strings.Contains(out, "devbox-cli") || !strings.Contains(out, "operate") {
		t.Fatalf("token ls: %d %s", code, out)
	}

	// On the workstation, through the CLI.
	t.Setenv("SWYD_CLI_CONFIG", scoped)
	if code, out, errOut := runCLI(t, "hosts"); code != 0 || !strings.Contains(out, "devbox") {
		t.Fatalf("hosts: %d %s %s", code, out, errOut)
	}
	if code, _, errOut := runCLI(t, "spawn", "devbox", "--cwd", work, "--name", "proj", "hello"); code != 0 {
		t.Fatalf("spawn with an operate token: %s", errOut)
	}
	if code, out, errOut := runCLI(t, "ui-link"); code != 0 || !strings.Contains(out, "/login#code=") || !strings.Contains(errOut, "operate scope") {
		t.Fatalf("ui-link: %d %s %s", code, out, errOut)
	}
	if code, _, errOut := runCLI(t, "token", "ls"); code == 0 || !strings.Contains(errOut, "needs a token with admin scope") {
		t.Fatalf("token ls with an operate token: %d %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "hosts", "rm", "devbox"); code == 0 || !strings.Contains(errOut, "admin scope") {
		t.Fatalf("hosts rm with an operate token: %d %s", code, errOut)
	}

	// The same through the API client: a whole session round trip.
	op, err := apiclient.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := op.Resolve(ctx, "devbox/proj")
	if err != nil {
		t.Fatal(err)
	}
	w := watch(op, sess.ID)
	defer w.cancel()
	i := w.waitFor(t, "first reply", 0, agentSaid("echo: hello"))
	i = w.waitFor(t, "first turn done", i, turnEnded("completed"))
	waitStatus(t, op, sess.ID, model.StatusIdle)
	if _, err := op.Send(ctx, sess.ID, "again", "queue", ""); err != nil {
		t.Fatal(err)
	}
	w.waitFor(t, "second reply", i, agentSaid("echo: again"))
	_, err = op.EnrollToken(ctx, "intruder", time.Hour)
	wantForbidden(t, "enroll token with an operate token", err)
	_, err = op.CreateToken(ctx, "escalate", "admin", 0)
	wantForbidden(t, "token create with an operate token", err)
	_, err = op.UILink(ctx, "admin")
	wantForbidden(t, "admin link from an operate token", err)

	// A read token watches but cannot act.
	ro, err := admin.CreateToken(ctx, "wallboard", "read", 0)
	if err != nil {
		t.Fatal(err)
	}
	rcfg := cfg
	rcfg.Token = ro.Secret
	rc, err := apiclient.New(rcfg)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := rc.Sessions(ctx); err != nil || len(list) != 1 {
		t.Fatalf("sessions with a read token: %v %v", list, err)
	}
	rw := watch(rc, sess.ID)
	defer rw.cancel()
	rw.waitFor(t, "stream with a read token", 0, agentSaid("echo: again"))
	_, err = rc.Send(ctx, sess.ID, "nope", "queue", "")
	wantForbidden(t, "send with a read token", err)
	wantForbidden(t, "stop with a read token", rc.Stop(ctx, sess.ID))
	if l, err := rc.UILink(ctx, ""); err != nil || l.Scope != "read" {
		t.Fatalf("ui-link with a read token: %+v %v", l, err)
	}

	// Revoking the workstation's token locks its CLI out at once.
	t.Setenv("SWYD_CLI_CONFIG", filepath.Join(coordDir, "cli.json"))
	if code, _, errOut := runCLI(t, "token", "rm", "devbox-cli"); code != 0 {
		t.Fatalf("token rm: %s", errOut)
	}
	if _, err := op.Hosts(ctx); err == nil {
		t.Fatal("revoked token still works")
	}
	if _, err := admin.Hosts(ctx); err != nil {
		t.Fatalf("admin token after revoking another: %v", err)
	}
}

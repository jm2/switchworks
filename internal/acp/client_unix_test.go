// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package acp

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

type nopHandler struct{}

func (nopHandler) OnUpdate(SessionNotification)                           {}
func (nopHandler) OnPermission(json.RawMessage, RequestPermissionRequest) {}

// An agent that sends a request the moment it starts, before Start has
// its connection, gets an answer instead of crashing the node.
func TestRequestBeforeInitialize(t *testing.T) {
	script := `printf '%s\n' '{"jsonrpc":"2.0","id":"early","method":"fs/read_text_file","params":{}}'
read a; read b
case "$a$b" in *'"id":"early"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":` + strconv.Itoa(ProtocolVersion) + `}}';; esac
cat >/dev/null`
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := Start(ctx, Options{Bin: "sh", Args: []string{"-c", script}}, nopHandler{})
		cancel()
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		_ = c.Kill()
	}
}

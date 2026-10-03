// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jm2/switchyard/internal/adapter"
	"github.com/jm2/switchyard/internal/ulid"
	"github.com/jm2/switchyard/internal/wire"
)

// The local API is how `swyd mcp` (started by the harness inside a session)
// reaches its node. It listens on loopback only. Each session gets its own
// credential, HMAC(node secret, session id), passed in the MCP server's
// environment, so one session's agent cannot act as another.

// Environment variables read by `swyd mcp`.
const (
	EnvNodeURL     = "SWYD_NODE_URL"
	EnvSession     = "SWYD_SESSION"
	EnvSessionPath = "SWYD_SESSION_PATH"
	EnvToken       = "SWYD_TOKEN"
)

// MCPToolTimeout is the per-call timeout given to the harness for fleet
// tools: longer than the longest fleet_ask wait.
const MCPToolTimeout = 31 * 60

type localAPI struct {
	ln     net.Listener
	srv    *http.Server
	url    string
	secret []byte
}

func (l *localAPI) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.srv.Shutdown(ctx)
}

func (l *localAPI) token(sessionID string) string {
	m := hmac.New(sha256.New, l.secret)
	m.Write([]byte(sessionID))
	return hex.EncodeToString(m.Sum(nil))
}

// ops the local API forwards, keyed by URL path segment.
var localOps = map[string]string{
	"list": wire.ReqList, "send": wire.ReqSend, "ask": wire.ReqAsk, "reply": wire.ReqReply,
	"inbox": wire.ReqInbox, "spawn": wire.ReqSpawn, "status": wire.ReqStatus, "stop": wire.ReqStop,
}

// LocalResponse is the local API's reply.
type LocalResponse struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	// Transient is set when the coordinator could not be reached; the
	// caller may retry.
	Transient bool `json:"transient,omitempty"`
}

func (n *Node) startLocal() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	l := &localAPI{ln: ln, secret: secret, url: "http://" + ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/fleet/{op}", n.handleLocal)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }()
	n.local = l

	n.mcpCmd = n.cfg.MCP.Command
	if n.mcpCmd == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("find swyd binary for the MCP server: %w", err)
		}
		n.mcpCmd = exe
	}
	n.mcpArgs = n.cfg.MCP.Args
	if len(n.mcpArgs) == 0 {
		n.mcpArgs = []string{"mcp"}
	}
	return nil
}

// mcpFor describes the fleet-tools MCP server for one session.
func (n *Node) mcpFor(sessionID, path string) *adapter.MCPServer {
	if n.local == nil {
		return nil
	}
	return &adapter.MCPServer{Name: "swyd", Command: n.mcpCmd, Args: n.mcpArgs, ToolTimeout: MCPToolTimeout,
		Env: map[string]string{
			EnvNodeURL: n.local.url, EnvSession: sessionID, EnvSessionPath: path, EnvToken: n.local.token(sessionID),
		}}
}

func writeLocal(w http.ResponseWriter, status int, r LocalResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

func (n *Node) handleLocal(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Swyd-Session")
	tok := r.Header.Get("Authorization")
	if len(tok) > 7 && tok[:7] == "Bearer " {
		tok = tok[7:]
	}
	if id == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(n.local.token(id))) != 1 {
		writeLocal(w, 401, LocalResponse{Error: "invalid session credential"})
		return
	}
	s, err := n.session(id)
	if err != nil {
		writeLocal(w, 404, LocalResponse{Error: err.Error()})
		return
	}
	s.mu.Lock()
	stopped := s.rec.Stopped
	s.mu.Unlock()
	if stopped {
		writeLocal(w, 409, LocalResponse{Error: "session is stopped"})
		return
	}
	op, ok := localOps[r.PathValue("op")]
	if !ok {
		writeLocal(w, 404, LocalResponse{Error: "unknown operation"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeLocal(w, 400, LocalResponse{Error: err.Error()})
		return
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	res, err := n.request(r.Context(), op, id, body)
	switch {
	case errors.Is(err, errNoCoordinator):
		writeLocal(w, 503, LocalResponse{Error: err.Error(), Transient: true})
	case err != nil:
		writeLocal(w, 502, LocalResponse{Error: err.Error()})
	case !res.OK:
		writeLocal(w, 200, LocalResponse{Error: res.Error})
	default:
		writeLocal(w, 200, LocalResponse{OK: true, Data: res.Data})
	}
}

// --- node -> coordinator requests ---

type reqReply struct {
	r   wire.Result
	err error
}

var errNoCoordinator = errors.New("coordinator unreachable (the node is reconnecting); try again shortly")

// request sends a fleet operation to the coordinator and waits for its
// answer.
func (n *Node) request(ctx context.Context, op, sessionID string, args json.RawMessage) (wire.Result, error) {
	n.connMu.Lock()
	c := n.conn
	n.connMu.Unlock()
	if c == nil {
		return wire.Result{}, errNoCoordinator
	}
	id := ulid.New()
	ch := make(chan reqReply, 1)
	n.reqMu.Lock()
	n.reqs[id] = ch
	n.reqMu.Unlock()
	drop := func() {
		n.reqMu.Lock()
		delete(n.reqs, id)
		n.reqMu.Unlock()
	}
	if err := c.Send(wire.TCmd, id, "", wire.NodeReq{Op: op, Session: sessionID, Args: args}); err != nil {
		drop()
		return wire.Result{}, errNoCoordinator
	}
	select {
	case rep := <-ch:
		return rep.r, rep.err
	case <-ctx.Done():
		drop()
		return wire.Result{}, ctx.Err()
	case <-n.closing:
		drop()
		return wire.Result{}, errNoCoordinator
	}
}

func (n *Node) answerRequest(re string, r wire.Result) {
	n.reqMu.Lock()
	ch := n.reqs[re]
	delete(n.reqs, re)
	n.reqMu.Unlock()
	if ch != nil {
		ch <- reqReply{r: r}
	}
}

// failRequests ends every in-flight request when the link drops.
func (n *Node) failRequests() {
	n.reqMu.Lock()
	defer n.reqMu.Unlock()
	for id, ch := range n.reqs {
		ch <- reqReply{err: errNoCoordinator}
		delete(n.reqs, id)
	}
}

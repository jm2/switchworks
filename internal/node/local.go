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
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/adapter"
	"github.com/jm2/switchworks/internal/ulid"
	"github.com/jm2/switchworks/internal/wire"
)

// The local API is how `swk mcp` (started by the harness inside a session)
// reaches its node. It listens on loopback only. Each session gets its own
// credential, HMAC(node secret, session id), passed in the MCP server's
// environment, so one session's agent cannot act as another.

// Environment variables read by `swk mcp`.
const (
	EnvNodeURL     = "SWK_NODE_URL"
	EnvSession     = "SWK_SESSION"
	EnvSessionPath = "SWK_SESSION_PATH"
	EnvToken       = "SWK_TOKEN"
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
	"remind": wire.ReqRemind,
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
	ln = capListener(ln, maxLocalConns)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/fleet/{op}", n.handleLocal)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 64 << 10}
	go func() { _ = l.srv.Serve(ln) }()
	n.local = l

	n.mcpCmd = n.cfg.MCP.Command
	if n.mcpCmd == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("find swk binary for the MCP server: %w", err)
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
	return &adapter.MCPServer{Name: "swk", Command: n.mcpCmd, Args: n.mcpArgs, ToolTimeout: MCPToolTimeout,
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
	id := r.Header.Get("X-Swk-Session")
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
	stopped, busy := s.rec.Stopped, s.localReqs >= maxSessionReqs
	if !stopped && !busy {
		s.localReqs++
	}
	s.mu.Unlock()
	if stopped {
		writeLocal(w, 409, LocalResponse{Error: "session is stopped"})
		return
	}
	if busy {
		// One session's waiting asks cannot use up the host's share.
		writeLocal(w, 429, LocalResponse{Error: fmt.Sprintf("this session already has %d fleet operations in progress", maxSessionReqs), Transient: true})
		return
	}
	defer func() {
		s.mu.Lock()
		s.localReqs--
		s.mu.Unlock()
	}()
	op, ok := localOps[r.PathValue("op")]
	if !ok {
		writeLocal(w, 404, LocalResponse{Error: "unknown operation"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeLocal(w, 413, LocalResponse{Error: "request body over 1 MiB"})
			return
		}
		writeLocal(w, 400, LocalResponse{Error: err.Error()})
		return
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	switch op {
	case wire.ReqSend, wire.ReqAsk, wire.ReqSpawn:
		// Image and file paths are read here, on the sender's host, under
		// the session's working directory only. Files go with messages,
		// not spawn prompts.
		var extra []pathArg
		if op != wire.ReqSpawn {
			extra = append(extra, filesArg)
		}
		if body, err = n.resolveImages(s, body, extra...); err != nil {
			writeLocal(w, 200, LocalResponse{Error: err.Error()})
			return
		}
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
	// The session's events go first: the coordinator reads them in order,
	// so it knows which message started the turn this request comes from
	// (its hop count) before it routes what the turn sends.
	n.sendMu.Lock()
	sent := true
	if s, err := n.session(sessionID); err == nil {
		sent = n.sendEvents(c, s)
	}
	if sent {
		sent = c.Send(wire.TCmd, id, "", wire.NodeReq{Op: op, Session: sessionID, Args: args}) == nil
	}
	n.sendMu.Unlock()
	if !sent {
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

// maxLocalConns caps open connections to the local API. Anything on the host
// can connect to it, token or not.
const maxLocalConns = 512

// maxSessionReqs bounds one session's fleet operations in progress.
const maxSessionReqs = 8

// capListener closes new connections at once while limit are open.
func capListener(ln net.Listener, limit int) net.Listener {
	return &cappedListener{Listener: ln, sem: make(chan struct{}, limit)}
}

type cappedListener struct {
	net.Listener
	sem chan struct{}
}

func (l *cappedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &cappedConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
			_ = c.Close()
		}
	}
}

type cappedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *cappedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

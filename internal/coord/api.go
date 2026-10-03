package coord

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jm2/switchyard/internal/apiclient"
	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/pki"
	"github.com/jm2/switchyard/internal/store"
	"github.com/jm2/switchyard/internal/ulid"
	"github.com/jm2/switchyard/internal/version"
	"github.com/jm2/switchyard/internal/wire"
)

// Handler returns the HTTPS API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"name": version.Name, "version": version.Version})
	})
	mux.HandleFunc("POST /v1/enroll", s.handleEnroll)

	authed := http.NewServeMux()
	authed.HandleFunc("GET /v1/hosts", s.handleHosts)
	authed.HandleFunc("GET /v1/sessions", s.handleSessions)
	authed.HandleFunc("POST /v1/sessions", s.handleSpawn)
	authed.HandleFunc("GET /v1/sessions/{id}", s.handleSession)
	authed.HandleFunc("GET /v1/sessions/{id}/events", s.handleEvents)
	authed.HandleFunc("GET /v1/sessions/{id}/stream", s.handleStream)
	authed.HandleFunc("POST /v1/sessions/{id}/messages", s.handleMessage)
	authed.HandleFunc("POST /v1/sessions/{id}/interrupt", s.handleSimple(wire.OpInterrupt))
	authed.HandleFunc("POST /v1/sessions/{id}/stop", s.handleSimple(wire.OpStop))
	authed.HandleFunc("POST /v1/sessions/{id}/approvals/{aid}", s.handleApprove)
	authed.HandleFunc("POST /v1/enroll-tokens", s.handleEnrollToken)
	mux.Handle("/v1/", s.requireToken(authed))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	want := []byte(s.cfg.AdminTokenHash)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(pki.HashSecret(tok)), want) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) view(sess model.Session) apiclient.SessionView {
	v := apiclient.SessionView{Session: sess, Online: s.Online(sess.Host)}
	if !v.Online && sess.Status != model.StatusStopped && sess.Status != model.StatusGone {
		v.Status = model.StatusOffline
	}
	return v
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	hosts := s.store.Hosts()
	for i := range hosts {
		hosts[i].Connected = s.Online(hosts[i].ID)
	}
	writeJSON(w, 200, hosts)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	out := []apiclient.SessionView{}
	for _, sess := range s.store.Sessions() {
		if path != "" && sess.Path != path {
			continue
		}
		out = append(out, s.view(sess))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.store.Session(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	writeJSON(w, 200, s.view(sess))
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var spec model.SpawnSpec
	if err := decodeBody(r, &spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	sess, err := s.Spawn(r.Context(), spec)
	switch {
	case errors.Is(err, store.ErrPathInUse):
		writeErr(w, 409, fmt.Errorf("%s/%s: %w", spec.Host, spec.Name, err))
	case errors.Is(err, ErrOffline):
		writeErr(w, 409, err)
	case err != nil:
		writeErr(w, 400, err)
	default:
		writeJSON(w, 201, s.view(sess))
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Session(id); !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	evs, err := s.store.Events(id, after, limit)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if evs == nil {
		evs = []model.Event{}
	}
	writeJSON(w, 200, evs)
}

// handleStream serves a session's events as Server-Sent Events: the stored
// backlog after ?after=N, then live events. Each event's SSE id is its seq,
// so clients resume with ?after=<last id>.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Session(id); !ok {
		writeErr(w, 404, store.ErrNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	// Subscribe before reading the backlog so nothing falls in between.
	ch := s.subscribe(id)
	defer s.unsubscribe(id, ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	last := after
	send := func(ev model.Event) error {
		if ev.Seq <= last {
			return nil
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, b); err != nil {
			return err
		}
		last = ev.Seq
		return nil
	}
	for {
		backlog, err := s.store.Events(id, last, 1000)
		if err != nil {
			return
		}
		for _, ev := range backlog {
			if send(ev) != nil {
				return
			}
		}
		flusher.Flush()
		if len(backlog) < 1000 {
			break
		}
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return // dropped as a slow subscriber; client reconnects
			}
			if ev.Seq > last+1 {
				// Gap (events published before this subscriber caught up):
				// fill from the store.
				backlog, err := s.store.Events(id, last, 0)
				if err != nil {
					return
				}
				for _, b := range backlog {
					if send(b) != nil {
						return
					}
				}
			}
			if send(ev) != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
		Mode string `json:"mode"`
		From string `json:"from"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, 400, errors.New("text is required"))
		return
	}
	switch body.Mode {
	case "", "queue", "steer":
	default:
		writeErr(w, 400, fmt.Errorf("unknown mode %q (queue, steer)", body.Mode))
		return
	}
	if body.From == "" {
		body.From = "user:cli"
	}
	msgID := ulid.New()
	_, err := s.SessionCommand(r.Context(), r.PathValue("id"), wire.Cmd{Op: wire.OpDeliver,
		Message: &wire.MessageCmd{ID: msgID, From: body.From, Text: body.Text, Mode: body.Mode}})
	if s.commandError(w, err) {
		return
	}
	writeJSON(w, 202, apiclient.SendResult{MessageID: msgID})
}

func (s *Server) commandError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, err)
	case errors.Is(err, ErrOffline):
		writeErr(w, 409, err)
	default:
		writeErr(w, 400, err)
	}
	return true
}

func (s *Server) handleSimple(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := s.SessionCommand(r.Context(), r.PathValue("id"), wire.Cmd{Op: op})
		if s.commandError(w, err) {
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string `json:"decision"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	_, err := s.SessionCommand(r.Context(), r.PathValue("id"), wire.Cmd{Op: wire.OpResolve,
		Resolve: &wire.ResolveCmd{ApprovalID: r.PathValue("aid"), Decision: body.Decision}})
	if s.commandError(w, err) {
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleEnrollToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Host       string `json:"host"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := model.ValidateName("host", body.Host); err != nil {
		writeErr(w, 400, err)
		return
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > 7*24*time.Hour {
		ttl = time.Hour
	}
	secret := pki.RandomSecret(24)
	if err := s.store.AddToken(pki.HashSecret(secret), body.Host, time.Now().Add(ttl)); err != nil {
		writeErr(w, 500, err)
		return
	}
	tok := pki.Token{API: s.cfg.PublicAPI, Secret: secret, Fingerprint: pki.Fingerprint(s.ca.Cert), Host: body.Host}
	writeJSON(w, 201, map[string]string{"token": tok.Encode(), "expires": time.Now().Add(ttl).Format(time.RFC3339)})
}

// EnrollRequest is posted by `swyd node enroll`.
type EnrollRequest struct {
	Secret string `json:"secret"`
	CSR    string `json:"csr"`
}

// EnrollResponse returns the node's identity.
type EnrollResponse struct {
	Host     string `json:"host"`
	Cert     string `json:"cert"`
	CA       string `json:"ca"`
	NodeAddr string `json:"node_addr"`
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	host, err := s.store.ConsumeToken(pki.HashSecret(req.Secret))
	if err != nil {
		writeErr(w, 403, err)
		return
	}
	cert, err := s.ca.SignNodeCSR([]byte(req.CSR), host)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, known := s.store.Host(host); !known {
		_ = s.store.UpsertHost(model.Host{ID: host, LastSeen: time.Now()})
	}
	s.logf("enrolled host %s from %s", host, remoteHost(r))
	writeJSON(w, 200, EnrollResponse{Host: host, Cert: string(cert), CA: string(s.ca.PEM), NodeAddr: s.cfg.PublicNode})
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// ServeAPI serves the HTTPS API on ln until ctx ends.
func (s *Server) ServeAPI(ctx context.Context, ln net.Listener) error {
	tlsCfg, err := pki.ServerTLS(s.dir, nil)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err = srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// SPDX-License-Identifier: GPL-3.0-or-later

// Package apiclient talks to the coordinator's HTTPS API. It is used by the
// CLI and by tests.
package apiclient

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/wire"
)

// Config is the CLI's connection file (cli.json).
type Config struct {
	API   string `json:"api"`   // https://host:port
	CA    string `json:"ca"`    // PEM
	Token string `json:"token"` // bearer token: the admin token or a scoped API token

	// Dial, when set, is the address to connect to instead of API's host
	// and port; the certificate is still checked against API's host name.
	// $SWK_DIAL sets it too: inside the coordinator's container, where
	// API's name may resolve to an address that does not lead back in.
	Dial string `json:"-"`
}

// LoadConfig reads a cli.json.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.API == "" || c.CA == "" || c.Token == "" {
		return c, fmt.Errorf("%s: missing api, ca or token", path)
	}
	return c, nil
}

// Client is an API client.
type Client struct {
	cfg  Config
	http *http.Client
	base string
}

// New builds a client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.API)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := pki.ClientTLS([]byte(cfg.CA), nil, nil, u.Hostname())
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dial := dialer.DialContext
	if to := cmp.Or(cfg.Dial, os.Getenv("SWK_DIAL")); to != "" {
		dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, to)
		}
	}
	tr := &http.Transport{
		TLSClientConfig:     tlsCfg,
		Proxy:               nil, // the coordinator is on the LAN; never route it through a proxy
		DialContext:         dial,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{cfg: cfg, http: &http.Client{Transport: tr}, base: strings.TrimRight(cfg.API, "/")}, nil
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("coordinator: %s (HTTP %d)", e.Message, e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// SessionView is a session as the API reports it.
type SessionView struct {
	model.Session
	Online bool `json:"online"`
	// Preview is the session's last agent message (clipped).
	Preview string `json:"preview,omitempty"`
	// Approvals are the prompts waiting for a decision, oldest first.
	Approvals []model.ApprovalRequested `json:"approvals,omitempty"`
}

func (c *Client) Hosts(ctx context.Context) ([]model.Host, error) {
	var out []model.Host
	return out, c.do(ctx, "GET", "/v1/hosts", nil, &out)
}

// RemoveHost revokes a host's certificate and forgets it.
func (c *Client) RemoveHost(ctx context.Context, host string) error {
	return c.do(ctx, "DELETE", "/v1/hosts/"+url.PathEscape(host), nil, nil)
}

func (c *Client) Sessions(ctx context.Context) ([]SessionView, error) {
	var out []SessionView
	return out, c.do(ctx, "GET", "/v1/sessions", nil, &out)
}

func (c *Client) Session(ctx context.Context, id string) (SessionView, error) {
	var out SessionView
	return out, c.do(ctx, "GET", "/v1/sessions/"+url.PathEscape(id), nil, &out)
}

// Resolve turns "host/name", a session id, or a fleet-unique name into a
// session. For a path it returns the newest session with that path.
func (c *Client) Resolve(ctx context.Context, ref string) (SessionView, error) {
	all, err := c.Sessions(ctx)
	if err != nil {
		return SessionView{}, err
	}
	var byName []SessionView
	var byPath *SessionView
	for i := range all {
		s := all[i]
		if s.ID == ref {
			return s, nil
		}
		if s.Path == ref && (byPath == nil || s.ID > byPath.ID) {
			byPath = &all[i]
		}
		if s.Name == ref && !strings.Contains(ref, "/") {
			byName = append(byName, s)
		}
	}
	if byPath != nil {
		return *byPath, nil
	}
	live := byName[:0]
	for _, s := range byName {
		if s.Status != model.StatusStopped {
			live = append(live, s)
		}
	}
	switch {
	case len(live) == 1:
		return live[0], nil
	case len(live) > 1:
		return SessionView{}, fmt.Errorf("%q matches %d sessions; use host/name", ref, len(live))
	}
	return SessionView{}, fmt.Errorf("no session %q", ref)
}

func (c *Client) Spawn(ctx context.Context, spec model.SpawnSpec) (SessionView, error) {
	var out SessionView
	return out, c.do(ctx, "POST", "/v1/sessions", spec, &out)
}

func (c *Client) Events(ctx context.Context, id string, after int64, limit int) ([]model.Event, error) {
	var out []model.Event
	return out, c.do(ctx, "GET", fmt.Sprintf("/v1/sessions/%s/events?after=%d&limit=%d", url.PathEscape(id), after, limit), nil, &out)
}

// SendResult is the response to a message.
// Queue is a session's queue (GET /v1/sessions/{id}/queue), in the order
// its messages will be delivered.
type Queue struct {
	Session  string          `json:"session"`
	Messages []QueuedMessage `json:"messages"`
	// Offline: the session's host is not connected, so Messages are only
	// the ones the coordinator holds for it.
	Offline bool `json:"offline,omitempty"`
	// NodeTooOld: the host's node (before 0.1.5) cannot list its queue, so
	// Messages are only the ones the coordinator holds.
	NodeTooOld bool `json:"node_too_old,omitempty"`
}

// QueuedMessage is one queued message: the node's entry, and what the
// coordinator's record adds.
type QueuedMessage struct {
	wire.QueueEntry
	Text      string    `json:"text,omitempty"`
	Via       string    `json:"via,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	// Held: the coordinator still holds it, so it is not in the node's
	// queue yet (its host is offline). Cancel works; it is delivered in its
	// mode when the host returns, unless it expires first.
	Held bool `json:"held,omitempty"`
}

type SendResult struct {
	MessageID string `json:"message_id"`
	State     string `json:"state,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// Send delivers a message into a session, with optional images.
func (c *Client) Send(ctx context.Context, id, text, mode, from string, images ...model.Attachment) (SendResult, error) {
	return c.SendMessage(ctx, id, SessionMessage{Text: text, Mode: mode, From: from, Attachments: images})
}

// SessionMessage is the body of POST /v1/sessions/{id}/messages.
type SessionMessage struct {
	Text        string             `json:"text"`
	Mode        string             `json:"mode"`
	From        string             `json:"from"`
	Supersedes  string             `json:"supersedes,omitempty"`  // see fleet.SendArgs
	Attachments []model.Attachment `json:"attachments,omitempty"` // images
	Files       []model.Attachment `json:"files,omitempty"`       // text files
}

// SendMessage delivers a message into a session.
func (c *Client) SendMessage(ctx context.Context, id string, m SessionMessage) (SendResult, error) {
	var out SendResult
	return out, c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/messages", m, &out)
}

// Attachment fetches a stored image by its SHA-256.
func (c *Client) Attachment(ctx context.Context, sha256 string) (model.Attachment, error) {
	var out model.Attachment
	return out, c.do(ctx, "GET", "/v1/attachments/"+url.PathEscape(sha256), nil, &out)
}

// PostMessage is the body of POST /v1/messages: a message from a person
// to any address.
type PostMessage struct {
	From string `json:"from,omitempty"` // "user:<name>" (default user:cli)
	// To is the address. In JSON "to" may also be a list of addresses
	// (notify and broadcast only), which fills ToList instead.
	To          string   `json:"to"`
	ToList      []string `json:"-"`
	Kind        string   `json:"kind,omitempty"` // notify (default) | ask | broadcast
	Text        string   `json:"text"`
	Mode        string   `json:"mode,omitempty"`
	TTLSeconds  int      `json:"ttl_seconds,omitempty"`
	Supersedes  string   `json:"supersedes,omitempty"`   // notify and broadcast only; see fleet.SendArgs
	WaitSeconds *int     `json:"wait_seconds,omitempty"` // ask only
	// Attachments are images (data, base64 in JSON); see docs/messaging.md.
	Attachments []model.Attachment `json:"attachments,omitempty"`
	// Files are text files (data, base64 in JSON), for agent sessions.
	Files []model.Attachment `json:"files,omitempty"`
}

type postMessageJSON PostMessage

// UnmarshalJSON takes "to" as one address or a list (fleet.DecodeTo),
// refusing unknown fields.
func (m *PostMessage) UnmarshalJSON(b []byte) error {
	var aux struct {
		postMessageJSON
		To json.RawMessage `json:"to"`
	}
	if err := fleet.StrictUnmarshal(b, &aux); err != nil {
		return err
	}
	*m = PostMessage(aux.postMessageJSON)
	var err error
	m.To, m.ToList, err = fleet.DecodeTo(aux.To)
	return err
}

// MarshalJSON writes "to" as a string unless ToList is set.
func (m PostMessage) MarshalJSON() ([]byte, error) {
	aux := struct {
		postMessageJSON
		To any `json:"to"`
	}{postMessageJSON(m), m.To}
	if len(m.ToList) > 0 {
		aux.To = m.ToList
	}
	return json.Marshal(aux)
}

// PostMessage sends a notify or broadcast; the result is a fleet.SendResult.
func (c *Client) PostMessage(ctx context.Context, m PostMessage) (fleet.SendResult, error) {
	var out fleet.SendResult
	return out, c.do(ctx, "POST", "/v1/messages", m, &out)
}

// Ask sends an ask from a person, waiting up to m.WaitSeconds for the answer.
func (c *Client) Ask(ctx context.Context, m PostMessage) (fleet.AskResult, error) {
	m.Kind = model.MsgAsk
	var out fleet.AskResult
	return out, c.do(ctx, "POST", "/v1/messages", m, &out)
}

// MessageQuery filters Messages.
type MessageQuery struct {
	Session string // messages to or from this session id
	Inbox   bool   // messages to people
	Limit   int
}

func (c *Client) Messages(ctx context.Context, q MessageQuery) ([]model.Message, error) {
	v := url.Values{}
	if q.Session != "" {
		v.Set("session", q.Session)
	}
	if q.Inbox {
		v.Set("inbox", "1")
	}
	if q.Limit > 0 {
		v.Set("limit", fmt.Sprint(q.Limit))
	}
	var out []model.Message
	return out, c.do(ctx, "GET", "/v1/messages?"+v.Encode(), nil, &out)
}

func (c *Client) Message(ctx context.Context, id string) (model.Message, error) {
	var out model.Message
	return out, c.do(ctx, "GET", "/v1/messages/"+url.PathEscape(id), nil, &out)
}

// Reply answers an ask addressed to a person.
func (c *Client) Reply(ctx context.Context, askID, text, from string) (fleet.ReplyResult, error) {
	var out fleet.ReplyResult
	return out, c.do(ctx, "POST", "/v1/messages/"+url.PathEscape(askID)+"/reply", map[string]string{"text": text, "from": from}, &out)
}

// Promote delivers a queued message now: steered into the running turn,
// or with mode interrupt, cancelling it.
func (c *Client) Promote(ctx context.Context, id, mode string) (SendResult, error) {
	var out SendResult
	return out, c.do(ctx, "POST", "/v1/messages/"+url.PathEscape(id)+"/promote", map[string]string{"mode": mode}, &out)
}

// CancelMessage takes a queued message out of its target's queue.
func (c *Client) CancelMessage(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/messages/"+url.PathEscape(id)+"/cancel", nil, nil)
}

// Reminders is a session's pending reminders (GET
// /v1/sessions/{id}/reminders), soonest first.
type Reminders struct {
	Session   string           `json:"session"`
	Reminders []fleet.Reminder `json:"reminders"`
}

// Reminders lists the reminders a session has set itself with
// fleet_remind that are still to come.
func (c *Client) Reminders(ctx context.Context, sessionID string) (Reminders, error) {
	var out Reminders
	return out, c.do(ctx, "GET", "/v1/sessions/"+url.PathEscape(sessionID)+"/reminders", nil, &out)
}

// CancelReminder cancels one of a session's pending reminders, by its id
// or its message id (owners only).
func (c *Client) CancelReminder(ctx context.Context, sessionID, id string) (fleet.RemindResult, error) {
	var out fleet.RemindResult
	return out, c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(sessionID)+"/reminders/"+url.PathEscape(id)+"/cancel", nil, &out)
}

// Dismiss ends an ask to a person without an answer; its asker is told
// none will come.
func (c *Client) Dismiss(ctx context.Context, askID, from string) (model.Message, error) {
	var out model.Message
	return out, c.do(ctx, "POST", "/v1/messages/"+url.PathEscape(askID)+"/dismiss", map[string]string{"from": from}, &out)
}

// MarkRead marks a message to a person read, for every client.
func (c *Client) MarkRead(ctx context.Context, id, from string) (model.Message, error) {
	var out model.Message
	return out, c.do(ctx, "POST", "/v1/messages/"+url.PathEscape(id)+"/read", map[string]string{"from": from}, &out)
}

// UsageReport aggregates token use.
type UsageReport struct {
	Today          string                 `json:"today"`
	TodayTokens    int64                  `json:"today_tokens"`
	MaxDailyTokens int64                  `json:"max_daily_tokens,omitempty"`
	Daily          map[string]int64       `json:"daily"`
	ByHost         map[string]model.Usage `json:"by_host"`
	ByModel        map[string]model.Usage `json:"by_model"`
}

// UILink is a one-time web sign-in link.
type UILink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	// Scope is what the browser it signs in may do.
	Scope string `json:"scope,omitempty"`
}

// UILink mints a one-time link that signs a browser in to the web UI, with
// this client's scope, or a lower one if scope is set.
func (c *Client) UILink(ctx context.Context, scope string) (UILink, error) {
	var out UILink
	body := map[string]any{}
	if scope != "" {
		body["scope"] = scope
	}
	return out, c.do(ctx, "POST", "/v1/ui-links", body, &out)
}

func (c *Client) Usage(ctx context.Context) (UsageReport, error) {
	var out UsageReport
	return out, c.do(ctx, "GET", "/v1/usage", nil, &out)
}

func (c *Client) Interrupt(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/interrupt", nil, nil)
}

// SetPolicy changes a running session's approval policy.
func (c *Client) SetPolicy(ctx context.Context, id, policy string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/policy",
		map[string]string{"approval_policy": policy}, nil)
}

func (c *Client) Approve(ctx context.Context, id, approvalID, decision string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/approvals/"+url.PathEscape(approvalID),
		map[string]string{"decision": decision}, nil)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/stop", nil, nil)
}

// RemoveSession forgets an ended session: its record and event log.
func (c *Client) RemoveSession(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/sessions/"+url.PathEscape(id), nil, nil)
}

// EnrollToken asks the coordinator for a one-time token for host.
func (c *Client) EnrollToken(ctx context.Context, host string, ttl time.Duration) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, "POST", "/v1/enroll-tokens", map[string]any{"host": host, "ttl_seconds": int(ttl.Seconds())}, &out)
	return out.Token, err
}

// Stream follows a session's events with SSE, calling fn for each one,
// reconnecting from the last seen seq until ctx ends or fn returns false.
func (c *Client) Stream(ctx context.Context, id string, after int64, fn func(model.Event) bool) error {
	backoff := 500 * time.Millisecond
	for {
		cont, err := c.streamOnce(ctx, id, &after, fn)
		if !cont || ctx.Err() != nil {
			return ctx.Err()
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status != http.StatusServiceUnavailable {
			return err
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, id string, after *int64, fn func(model.Event) bool) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/sessions/%s/stream?after=%d", c.base, url.PathEscape(id), *after), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return true, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	br := bufio.NewReaderSize(resp.Body, 1<<20)
	var data bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return true, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data.Len() > 0 {
				var ev model.Event
				if json.Unmarshal(data.Bytes(), &ev) == nil {
					if ev.Seq > *after {
						*after = ev.Seq
					}
					if !fn(ev) {
						return false, nil
					}
				}
				data.Reset()
			}
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

// FleetStream follows GET /v1/stream once (no reconnect), calling fn with
// each event's type ("hello", "session", "host", "message", or
// "session_removed" and "host_removed" with just an id) and JSON data,
// until ctx ends, the stream closes or fn returns false.
func (c *Client) FleetStream(ctx context.Context, fn func(typ string, data json.RawMessage) bool) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/v1/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	br := bufio.NewReaderSize(resp.Body, 1<<20)
	var typ string
	var data bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data.Len() > 0 {
				if !fn(typ, json.RawMessage(append([]byte(nil), data.Bytes()...))) {
					return nil
				}
				data.Reset()
			}
			typ = ""
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

// Health asks the coordinator's unauthenticated health endpoint for its
// version.
func (c *Client) Health(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/health", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

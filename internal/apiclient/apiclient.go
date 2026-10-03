// SPDX-License-Identifier: GPL-3.0-or-later

// Package apiclient talks to the coordinator's HTTPS API. It is used by the
// CLI and by tests.
package apiclient

import (
	"bufio"
	"bytes"
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

	"github.com/jm2/switchyard/internal/model"
	"github.com/jm2/switchyard/internal/pki"
)

// Config is the CLI's connection file (cli.json).
type Config struct {
	API   string `json:"api"`   // https://host:port
	CA    string `json:"ca"`    // PEM
	Token string `json:"token"` // admin bearer token
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
	tr := &http.Transport{
		TLSClientConfig:     tlsCfg,
		Proxy:               nil, // the coordinator is on the LAN; never route it through a proxy
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
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
}

func (c *Client) Hosts(ctx context.Context) ([]model.Host, error) {
	var out []model.Host
	return out, c.do(ctx, "GET", "/v1/hosts", nil, &out)
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
type SendResult struct {
	MessageID string `json:"message_id"`
}

func (c *Client) Send(ctx context.Context, id, text, mode, from string) (SendResult, error) {
	var out SendResult
	return out, c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/messages",
		map[string]string{"text": text, "mode": mode, "from": from}, &out)
}

func (c *Client) Interrupt(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/interrupt", nil, nil)
}

func (c *Client) Approve(ctx context.Context, id, approvalID, decision string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/approvals/"+url.PathEscape(approvalID),
		map[string]string{"decision": decision}, nil)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/stop", nil, nil)
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

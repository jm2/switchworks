// SPDX-License-Identifier: GPL-3.0-or-later

package apiclient

import (
	"context"
	"net/url"
	"time"
)

// API token scopes, from least to most.
const (
	ScopeRead    = "read"    // GET endpoints and event streams
	ScopeOperate = "operate" // read, plus messages, approvals, interrupt, stop and spawn
	ScopeAdmin   = "admin"   // everything, including enrollment, `hosts rm` and tokens
)

// TokenInfo describes an API token. The secret is never listed.
type TokenInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Scope    string    `json:"scope"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
	Expires  time.Time `json:"expires,omitzero"`
	// Owner: messages sent with this token reach agents as the owner's.
	Owner bool `json:"owner,omitempty"`
}

// NewToken is a token just created: the only time its secret is shown.
type NewToken struct {
	TokenInfo
	Secret string `json:"secret"`
}

// CreateTokenRequest is the body of POST /v1/tokens.
type CreateTokenRequest struct {
	Name       string `json:"name"`
	Scope      string `json:"scope"`
	TTLSeconds int64  `json:"ttl_seconds,omitempty"` // 0: never expires
	Owner      bool   `json:"owner,omitempty"`
}

// CreateToken creates a named API token with a scope (ttl 0: no expiry).
func (c *Client) CreateToken(ctx context.Context, name, scope string, ttl time.Duration) (NewToken, error) {
	return c.CreateTokenWith(ctx, CreateTokenRequest{Name: name, Scope: scope, TTLSeconds: int64(ttl / time.Second)})
}

// CreateTokenWith creates an API token as req describes.
func (c *Client) CreateTokenWith(ctx context.Context, req CreateTokenRequest) (NewToken, error) {
	var out NewToken
	return out, c.do(ctx, "POST", "/v1/tokens", req, &out)
}

// SetTokenOwner marks a token (by name or id) as the owner's, or not.
func (c *Client) SetTokenOwner(ctx context.Context, ref string, owner bool) (TokenInfo, error) {
	var out TokenInfo
	return out, c.do(ctx, "POST", "/v1/tokens/"+url.PathEscape(ref)+"/owner", map[string]bool{"owner": owner}, &out)
}

// Tokens lists the API tokens.
func (c *Client) Tokens(ctx context.Context) ([]TokenInfo, error) {
	var out []TokenInfo
	return out, c.do(ctx, "GET", "/v1/tokens", nil, &out)
}

// DeleteToken revokes an API token by id or name. Web sessions signed in
// with it, or with links it minted, end too.
func (c *Client) DeleteToken(ctx context.Context, idOrName string) error {
	return c.do(ctx, "DELETE", "/v1/tokens/"+url.PathEscape(idOrName), nil, nil)
}

// Config returns the configuration the client was built from.
func (c *Client) Config() Config { return c.cfg }

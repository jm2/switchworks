// SPDX-License-Identifier: GPL-3.0-or-later

package apiclient

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Dial connects elsewhere, but the certificate is still checked against
// the API's name.
func TestDialOverride(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"Switchyard","version":"9.9.9"}`))
	}))
	defer srv.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	addr := srv.Listener.Addr().String()

	// example.com is in httptest's certificate, and resolves nowhere useful.
	c, err := New(Config{API: "https://example.com:7480", CA: ca, Dial: addr})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := c.Health(context.Background()); err != nil || v != "9.9.9" {
		t.Fatalf("health through Dial: %q, %v", v, err)
	}
	// A name the certificate does not cover is refused, Dial or not.
	c, _ = New(Config{API: "https://coord.invalid:7480", CA: ca, Dial: addr})
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("a certificate for another name was accepted")
	}
	// $SWYD_DIAL does the same.
	t.Setenv("SWYD_DIAL", addr)
	c, _ = New(Config{API: "https://example.com:7480", CA: ca})
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("health through $SWYD_DIAL: %v", err)
	}
}

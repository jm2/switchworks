// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"errors"
	"testing"
	"time"
)

func TestAPITokens(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	a := APIToken{ID: "01J0000000000000000000000A", Name: "phone", Scope: "read", Hash: "aa", Created: now}
	b := APIToken{ID: "01J0000000000000000000000B", Name: "devbox", Scope: "operate", Hash: "bb", Created: now, Expires: now.Add(time.Hour)}
	old := APIToken{ID: "01J0000000000000000000000C", Name: "old", Scope: "admin", Hash: "cc", Created: now, Expires: now.Add(-time.Minute)}
	for _, tk := range []APIToken{a, b, old} {
		if err := s.AddAPIToken(tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddAPIToken(APIToken{ID: "01J0000000000000000000000D", Name: "phone", Scope: "read", Hash: "dd"}); !errors.Is(err, ErrNameInUse) {
		t.Fatalf("duplicate name: %v", err)
	}
	// An expired token's name can be reused.
	if err := s.AddAPIToken(APIToken{ID: "01J0000000000000000000000E", Name: "old", Scope: "read", Hash: "ee"}); err != nil {
		t.Fatalf("reuse an expired token's name: %v", err)
	}
	if got, ok := s.APITokenByHash("bb"); !ok || got.ID != b.ID {
		t.Fatalf("by hash: %+v %v", got, ok)
	}
	if _, ok := s.APITokenByHash("cc"); ok {
		t.Fatal("expired token found")
	}
	if _, ok := s.APITokenByHash(""); ok {
		t.Fatal("empty hash matched")
	}
	s.TouchAPIToken(a.ID, now)
	if got, _ := s.APIToken(a.ID); !got.LastUsed.Equal(now) {
		t.Fatalf("last used %v", got.LastUsed)
	}
	if err := s.DeleteAPIToken("01J0000000000000000000000E"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAPIToken("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Tokens survive a restart; revoked and expired ones do not.
	s, err = OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list := s.APITokens()
	if len(list) != 2 || list[0].Name != "devbox" || list[1].Name != "phone" || !list[1].LastUsed.Equal(now) || !list[0].Expires.Equal(b.Expires) {
		t.Fatalf("after reopen: %+v", list)
	}
}

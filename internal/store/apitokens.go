// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"crypto/subtle"
	"fmt"
	"sort"
	"time"
)

// apiTokenUseGranularity is how stale a token's last-used time may get
// before a use updates it.
const apiTokenUseGranularity = 5 * time.Minute

func (s *FileStore) AddAPIToken(t APIToken) error {
	if t.ID == "" || t.Hash == "" {
		return fmt.Errorf("token without id or hash")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, o := range s.apiToks {
		if o.Expired(now) {
			delete(s.apiToks, id)
			continue
		}
		if o.Name == t.Name {
			return fmt.Errorf("token %q: %w", t.Name, ErrNameInUse)
		}
	}
	if _, ok := s.apiToks[t.ID]; ok {
		return fmt.Errorf("token id %q is taken", t.ID)
	}
	s.apiToks[t.ID] = t
	s.dirty = true
	return s.saveLocked()
}

func (s *FileStore) APITokens() []APIToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := []APIToken{}
	for _, t := range s.apiToks {
		if !t.Expired(now) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *FileStore) APITokenByHash(hash string) (APIToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var found APIToken
	ok := false
	// Compare with every token in constant time, so the lookup reveals
	// nothing about how close a guess came.
	for _, t := range s.apiToks {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(hash)) == 1 && !t.Expired(now) {
			found, ok = t, true
		}
	}
	return found, ok
}

func (s *FileStore) APIToken(id string) (APIToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.apiToks[id]
	if !ok || t.Expired(time.Now()) {
		return APIToken{}, false
	}
	return t, true
}

func (s *FileStore) DeleteAPIToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.apiToks[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.apiToks, id)
	s.dirty = true
	if err := s.saveLocked(); err != nil {
		// Not on disk, so it would come back at the next start: keep it,
		// and let the caller say the revocation failed.
		s.apiToks[id] = t
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

func (s *FileStore) TouchAPIToken(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.apiToks[id]
	if !ok || now.Sub(t.LastUsed) < apiTokenUseGranularity {
		return
	}
	t.LastUsed = now.UTC().Truncate(time.Second)
	s.apiToks[id] = t
	s.dirty = true
}

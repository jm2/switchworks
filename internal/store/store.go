// SPDX-License-Identifier: GPL-3.0-or-later

// Package store persists the coordinator's state: hosts, sessions, the
// per-session event logs, and enrollment tokens.
//
// Store is an interface so the storage engine can change. The design calls
// for SQLite; this build ships FileStore (stdlib only) because third-party
// modules were unavailable when it was written. A SQLite implementation can
// satisfy the same interface without touching callers.
package store

import (
	"errors"
	"time"

	"github.com/jm2/switchyard/internal/model"
)

// Errors.
var (
	ErrNotFound   = errors.New("not found")
	ErrPathInUse  = errors.New("session path already in use")
	ErrBadToken   = errors.New("invalid or expired enrollment token")
	ErrSeqGap     = errors.New("event sequence gap")
	ErrNoSuchHost = errors.New("unknown host")
)

// Store is the coordinator's persistence boundary.
type Store interface {
	UpsertHost(h model.Host) error
	Host(id string) (model.Host, bool)
	Hosts() []model.Host

	// CreateSession fails with ErrPathInUse if a session with the same path
	// exists and is not stopped, gone or in error.
	CreateSession(s model.Session) error
	UpdateSession(id string, fn func(*model.Session)) (model.Session, error)
	Session(id string) (model.Session, bool)
	SessionByPath(path string) (model.Session, bool)
	Sessions() []model.Session

	// AppendEvents stores events idempotently: an event whose seq is not
	// above the session's last stored seq is ignored. It returns the events
	// actually stored. Events for one session must be in seq order.
	AppendEvents(evs []model.Event) ([]model.Event, error)
	// Events returns up to limit events with seq > after, in order.
	Events(sessionID string, after int64, limit int) ([]model.Event, error)
	LastSeq(sessionID string) int64

	AddToken(hash, host string, expires time.Time) error
	// ConsumeToken returns the host a token enrolls and deletes the token.
	ConsumeToken(hash string) (string, error)

	Close() error
}

// Reusable reports whether a session's path can be taken by a new session.
func Reusable(s model.Session) bool {
	switch s.Status {
	case model.StatusStopped, model.StatusGone, model.StatusError:
		return true
	}
	return false
}

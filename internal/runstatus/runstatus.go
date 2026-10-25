// SPDX-License-Identifier: GPL-3.0-or-later

// Package runstatus lets a running node or coordinator tell local tools, such
// as `swk service status` and the tray apps, that it is up and how it is
// doing, without opening a port.
//
// The running process writes status.json into its own directory (mode 0600)
// when something changes and every Heartbeat otherwise, and removes it when
// it stops. A reader treats a file older than Stale, or one whose process
// has exited, as a process that died without cleaning up. The file holds
// no secrets: process ID, version, host name, coordinator address and
// connection state.
package runstatus

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/paths"
	"github.com/jm2/switchworks/internal/version"
)

// File is the status file's name inside the node or coordinator directory.
const File = "status.json"

const (
	// Heartbeat is how often a running process rewrites its status file.
	Heartbeat = 20 * time.Second
	// Stale is the age after which a status file no longer counts as a
	// running process.
	Stale = 3 * Heartbeat
)

// Status is the content of status.json.
type Status struct {
	Role    string    `json:"role"` // "node" or "coordinator"
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`

	// Node only.
	Host           string    `json:"host,omitempty"`
	Coordinator    string    `json:"coordinator,omitempty"` // host:port of the event stream
	Connected      bool      `json:"connected"`
	ConnectedSince time.Time `json:"connected_since,omitzero"`
	LastError      string    `json:"last_error,omitempty"`

	// WebUI is the web UI's URL as browsers reach it (coordinator), or the
	// coordinator's API address as https://host:port (node).
	WebUI string `json:"web_ui,omitempty"`
}

// Fresh reports whether s was written recently enough to describe a running
// process.
func (s Status) Fresh(now time.Time) bool {
	return !s.Updated.IsZero() && now.Sub(s.Updated) < Stale && s.Updated.Sub(now) < Stale
}

// Running reports whether s is fresh and its process still runs, so a
// process killed before it could remove its file (by Task Scheduler, a
// crash or SIGKILL) counts as stopped at once.
func (s Status) Running(now time.Time) bool {
	return s.Fresh(now) && alive(s.PID, s.Started)
}

// RemoveDead removes dir's status file if the process that wrote it has
// exited. Best effort.
func RemoveDead(dir string) {
	if s, err := Read(dir); err == nil && !alive(s.PID, s.Started) {
		_ = os.Remove(filepath.Join(dir, File))
	}
}

// Read loads dir's status file.
func Read(dir string) (Status, error) {
	var s Status
	f, err := os.Open(filepath.Join(dir, File))
	if err != nil {
		return s, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return s, errors.New("status file is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil {
		return s, err
	}
	if len(b) > 64<<10 {
		return s, errors.New("status file too large")
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

// Reporter keeps a running process's status file current.
type Reporter struct {
	dir  string
	stop chan struct{}
	done chan struct{}

	mu      sync.Mutex
	s       Status
	stopped bool
}

// Start writes the first status file into dir and keeps it fresh until Stop.
// A nil Reporter is valid and does nothing, so callers need not check.
func Start(dir string, s Status) *Reporter {
	s.PID = os.Getpid()
	s.Version = version.Version
	s.Started = time.Now().UTC()
	r := &Reporter{dir: dir, s: s, stop: make(chan struct{}), done: make(chan struct{})}
	r.mu.Lock()
	r.writeLocked()
	r.mu.Unlock()
	go r.loop()
	return r
}

func (r *Reporter) loop() {
	defer close(r.done)
	t := time.NewTicker(Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.mu.Lock()
			r.writeLocked()
			r.mu.Unlock()
		case <-r.stop:
			return
		}
	}
}

func (r *Reporter) writeLocked() {
	if r.stopped {
		return // never recreate the file after Stop removed it
	}
	r.s.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(r.s, "", "  ")
	if err != nil {
		return
	}
	// Best effort: a full disk must not stop the node.
	_ = paths.WriteFile(filepath.Join(r.dir, File), append(b, '\n'), 0o600)
}

// Update changes the status and writes it out at once.
func (r *Reporter) Update(f func(*Status)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f(&r.s)
	r.writeLocked()
}

// Stop ends the heartbeat and removes the status file. It is safe to call
// more than once.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	_ = os.Remove(filepath.Join(r.dir, File))
	r.mu.Unlock()
	close(r.stop)
	<-r.done
}

// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jm2/switchworks/internal/model"
	"github.com/jm2/switchworks/internal/pki"
	"github.com/jm2/switchworks/internal/runstatus"
	"github.com/jm2/switchworks/internal/wire"
)

// Run keeps a link to the coordinator until ctx ends, reconnecting with
// backoff.
func (n *Node) Run(ctx context.Context) error {
	tlsCfg, err := n.clientTLS()
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		start := time.Now()
		err := n.connectOnce(ctx, tlsCfg)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		n.logf("coordinator link down: %v (retrying in %s)", err, backoff)
		n.status.Update(func(s *runstatus.Status) {
			s.Connected, s.ConnectedSince = false, time.Time{}
			if err != nil {
				s.LastError = err.Error()
			}
		})
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (n *Node) clientTLS() (*tls.Config, error) {
	ca, err := os.ReadFile(filepath.Join(n.dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	cert, err := os.ReadFile(filepath.Join(n.dir, "node.crt"))
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(filepath.Join(n.dir, "node.key"))
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(n.cfg.CoordinatorNode)
	if err != nil {
		return nil, err
	}
	return pki.ClientTLS(ca, cert, key, host)
}

func (n *Node) connectOnce(ctx context.Context, tlsCfg *tls.Config) error {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, Config: tlsCfg}
	raw, err := d.DialContext(ctx, "tcp", n.cfg.CoordinatorNode)
	if err != nil {
		return err
	}
	c := wire.NewConn(raw)
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()

	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	if err := c.Send(wire.THello, "", "", n.hello()); err != nil {
		return err
	}
	env, err := c.Recv()
	if err != nil {
		return err
	}
	if env.Type != wire.TWelcome {
		return errUnexpected(env.Type)
	}
	welcome, err := wire.Decode[wire.Welcome](env)
	if err != nil {
		return err
	}
	_ = raw.SetDeadline(time.Time{})
	n.applyAcks(welcome.Acked, true)

	n.connMu.Lock()
	n.conn = c
	n.connMu.Unlock()
	defer func() {
		n.connMu.Lock()
		if n.conn == c {
			n.conn = nil
		}
		n.connMu.Unlock()
		n.failRequests()
	}()
	n.logf("connected to coordinator %s", n.cfg.CoordinatorNode)
	n.status.Update(func(s *runstatus.Status) {
		s.Connected, s.ConnectedSince, s.LastError = true, time.Now().UTC(), ""
	})
	n.wakeSender()

	for {
		_ = raw.SetReadDeadline(time.Now().Add(75 * time.Second))
		env, err := c.Recv()
		if err != nil {
			return err
		}
		switch env.Type {
		case wire.TAck:
			a, _ := wire.Decode[wire.Ack](env)
			n.applyAcks(a.Acked, false)
		case wire.TCmd:
			cmd, err := wire.Decode[wire.Cmd](env)
			id := env.ID
			if err != nil {
				_ = c.Send(wire.TResult, "", id, wire.Result{OK: false, Error: "bad command: " + err.Error()})
				continue
			}
			go func() {
				r := n.handleCmd(cmd)
				_ = c.Send(wire.TResult, "", id, r)
			}()
		case wire.TResult:
			r, _ := wire.Decode[wire.Result](env)
			n.answerRequest(env.Re, r)
		case wire.TPing:
			_ = c.Send(wire.TPong, "", "", nil)
		case wire.TPong:
		default:
			n.logf("coordinator sent unexpected %q", env.Type)
		}
	}
}

type errUnexpected string

func (e errUnexpected) Error() string { return fmt.Sprintf("unexpected envelope %q", string(e)) }

// applyAcks drops acknowledged events. On a fresh connection (welcome) it
// also resets what has been sent, so everything unacked is resent.
func (n *Node) applyAcks(acked map[string]int64, welcome bool) {
	save := false
	for _, s := range n.sessionList() {
		s.mu.Lock()
		a, ok := acked[s.rec.ID]
		if welcome {
			if !ok {
				a = s.rec.AckedSeq
			}
			if a > s.seq {
				// The coordinator has events this node forgot (stale state):
				// never reuse their seq numbers.
				n.logf("session %s: coordinator has seq %d, node had %d; skipping ahead", s.rec.Path, a, s.seq)
				s.seq = a
			}
			s.sentSeq = a
		}
		if (ok || welcome) && a > s.rec.AckedSeq {
			s.rec.AckedSeq = a
			i := 0
			for i < len(s.unacked) && s.unacked[i].Seq <= a {
				i++
			}
			s.unacked = s.unacked[i:]
			if len(s.unacked) == 0 && s.spoolSize > compactBytes {
				save = true
			}
		}
		s.mu.Unlock()
	}
	if save {
		n.compact()
	}
}

// compact persists acked seqs, then empties fully acked large spools.
func (n *Node) compact() {
	if err := n.saveState(); err != nil {
		n.logf("save state before compaction: %v", err)
		return
	}
	for _, s := range n.sessionList() {
		s.mu.Lock()
		if len(s.unacked) == 0 && s.spoolSize > compactBytes && s.spool != nil {
			if err := s.spool.Truncate(0); err == nil {
				_, _ = s.spool.Seek(0, 0)
				s.spoolSize = 0
			}
		}
		s.mu.Unlock()
	}
}

// sender ships unsent events whenever woken.
func (n *Node) sender() {
	defer n.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-n.wake:
		case <-t.C:
		case <-n.closing:
			return
		}
		n.connMu.Lock()
		c := n.conn
		n.connMu.Unlock()
		if c == nil {
			continue
		}
		for _, s := range n.sessionList() {
			for {
				s.mu.Lock()
				var batch []model.Event
				size := 0
				for _, ev := range s.unacked {
					if ev.Seq > s.sentSeq {
						// Bounded by count and bytes, well inside the
						// coordinator's line limit, however big a backlog
						// of diffs has grown.
						if len(batch) > 0 && size+len(ev.Payload)+512 > maxBatchBytes {
							break
						}
						batch = append(batch, ev)
						size += len(ev.Payload) + 512
						if len(batch) == 500 {
							break
						}
					}
				}
				if len(batch) > 0 {
					s.sentSeq = batch[len(batch)-1].Seq
				}
				s.mu.Unlock()
				if len(batch) == 0 {
					break
				}
				if err := c.Send(wire.TEvents, "", "", wire.Events{Events: batch}); err != nil {
					_ = c.Close()
					break
				}
			}
		}
	}
}

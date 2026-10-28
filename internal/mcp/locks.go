// SPDX-License-Identifier: GPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jm2/switchworks/internal/fleet"
)

// The fleet lock tools: fleet_lock, fleet_unlock, fleet_locks.
var lockTools = []Tool{
	{Name: "fleet_lock", Title: "Take a fleet lock",
		Description: `Take a fleet-wide lock (a lease) on a shared resource before using it: a host's GPU, a LAN game-server port, a whole test host. Use this instead of messaging other agents to ask whether something is free or to say you are taking it. Names are free text, one namespace for the whole fleet; agree on a form such as "host:<host>:gpu", "host:<host>" or "port:<ip>:<port>".` +
			` Returns status "held" with a lease_id and expiry, or "busy" with who holds it, since when, their note and its expiry. With wait_seconds, waits up to that long for it to come free (waiters get it in the order they asked).` +
			` The lease lasts ttl_seconds (default 1800, at most 21600); call fleet_lock again on a lock you hold to renew it before then. Release it with fleet_unlock when done; it is also released when your session stops.`,
		InputSchema: obj(map[string]any{
			"name":         map[string]any{"type": "string", "maxLength": fleet.MaxLockNameBytes, "description": `The resource, e.g. "host:macmini-m1:gpu" or "port:192.168.1.10:28960".`},
			"ttl_seconds":  map[string]any{"type": "integer", "minimum": 1, "maximum": int(fleet.MaxLockTTL.Seconds()), "description": "How long the lease lasts unless renewed (default 1800)."},
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": int(fleet.MaxLockWait.Seconds()), "description": "How long to wait if it is busy (default 0: try once)."},
			"note":         map[string]any{"type": "string", "maxLength": fleet.MaxLockNoteBytes, "description": "What you use it for, shown to others who find it busy."},
		}, "name")},
	{Name: "fleet_unlock", Title: "Release a fleet lock",
		Description: "Release a fleet lock you hold (see fleet_lock); the next waiter, if any, gets it at once. Releasing a lock no one holds is not an error.",
		InputSchema: obj(map[string]any{"name": str("The lock's name.")}, "name")},
	{Name: "fleet_locks", Title: "List fleet locks", Annotations: map[string]bool{"readOnlyHint": true},
		Description: "List the fleet locks held now: name, holder, note, since, expiry and how many wait for each.",
		InputSchema: obj(map[string]any{"prefix": str(`Only locks whose name starts with this, e.g. "host:macmini-m1".`)})},
}

func init() { Tools = append(Tools, lockTools...) }

const lockInstructions = ` Before using a shared resource (a host's GPU, a LAN port, a whole test host), take it with fleet_lock and release it with fleet_unlock when done, instead of messaging other agents to ask whether it is free or to announce that you are taking or freeing it.`

// lockCall runs a lock tool; ok is false for other tools.
func (s *Server) lockCall(ctx context.Context, name string, raw json.RawMessage) (text string, structured any, ok bool, err error) {
	var op string
	var args any
	timeout := 3 * time.Minute
	switch name {
	case "fleet_lock":
		a, err := decode[fleet.LockArgs](raw)
		if err != nil {
			return "", nil, true, err
		}
		if a.WaitSeconds < 0 || a.TTLSeconds < 0 {
			return "", nil, true, fmt.Errorf("ttl_seconds and wait_seconds must not be negative")
		}
		timeout += min(time.Duration(a.WaitSeconds)*time.Second, fleet.MaxLockWait)
		op, args = "lock", a
	case "fleet_unlock":
		a, err := decode[fleet.UnlockArgs](raw)
		if err != nil {
			return "", nil, true, err
		}
		op, args = "unlock", a
	case "fleet_locks":
		a, err := decode[fleet.LocksArgs](raw)
		if err != nil {
			return "", nil, true, err
		}
		op, args = "locks", a
	default:
		return "", nil, false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := s.do(cctx, op, args)
	if err != nil {
		return "", nil, true, err
	}
	text, structured = pretty(data)
	return text, structured, true, nil
}

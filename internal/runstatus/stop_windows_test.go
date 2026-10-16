// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package runstatus

import (
	"context"
	"testing"
	"time"
)

// RequestStop ends the StopContext of the process running from the same
// directory: how `swk service stop` stops a node with no console.
func TestStopEventRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := StopContext(context.Background(), dir)
	defer cancel()
	if err := RequestStop(t.TempDir()); err == nil {
		t.Fatal("a stop request for another directory succeeded")
	}
	if ctx.Err() != nil {
		t.Fatal("stopped by a request for another directory")
	}
	if err := RequestStop(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stop request did not end the context")
	}
}

// A stop event someone else already created is not trusted: the second
// StopContext for the same directory ignores it.
func TestStopEventNotShared(t *testing.T) {
	dir := t.TempDir()
	ctx1, cancel1 := StopContext(context.Background(), dir)
	defer cancel1()
	ctx2, cancel2 := StopContext(context.Background(), dir)
	defer cancel2()
	if err := RequestStop(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx1.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the owner was not stopped")
	}
	time.Sleep(200 * time.Millisecond)
	if ctx2.Err() != nil {
		t.Fatal("the second listener used an event it did not create")
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package ulid

import (
	"testing"
	"time"
)

func TestMonotonicAndDecodable(t *testing.T) {
	var g Generator
	prev := ""
	start := time.Now().Add(-time.Second)
	for i := 0; i < 10000; i++ {
		id := g.New()
		if len(id) != 26 {
			t.Fatalf("len %d", len(id))
		}
		if id <= prev {
			t.Fatalf("not monotonic: %s <= %s", id, prev)
		}
		prev = id
	}
	ts, err := Time(prev)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Before(start) || ts.After(time.Now().Add(time.Second)) {
		t.Fatalf("decoded time %v out of range", ts)
	}
}

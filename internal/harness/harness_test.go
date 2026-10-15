// SPDX-License-Identifier: GPL-3.0-or-later

package harness

import (
	"os"
	"reflect"
	"testing"
)

func TestRegistry(t *testing.T) {
	self := os.Args[0] // any binary that exists
	o := Options{CodexBin: "/nonexistent/codex", AgyBin: "/nonexistent/agy", ACP: map[string]ACPAgent{
		"goose":    {Bin: self, Env: map[string]string{"GOOSE_PROVIDER": "deepseek"}},
		"opencode": {Bin: self, Args: []string{"acp"}},
		"crush":    {Bin: "/nonexistent/crush"},
		"codex":    {Bin: self}, // reserved: ignored
		"off":      {Bin: self, Disabled: true},
	}}
	if got, want := Names(o), []string{"codex", "agy", "crush", "goose", "opencode"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names %v, want %v", got, want)
	}
	if got, want := Available(o), []string{"goose", "opencode"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("available %v, want %v", got, want)
	}
	for _, n := range []string{"goose", "opencode", "codex", "agy"} {
		a, err := New(n, o)
		if err != nil || a.Name() != n {
			t.Fatalf("New(%s): %v %v", n, a, err)
		}
	}
	if _, err := New("off", o); err == nil {
		t.Fatal("disabled agent constructed")
	}
	o.ACP["goose"] = ACPAgent{Disabled: true}
	for _, n := range Names(o) {
		if n == "goose" {
			t.Fatal("disabled goose still listed")
		}
	}
	// Goose is built in when no config mentions it.
	if acpConfigs(Options{})["goose"].Bin != "goose" {
		t.Fatal("goose not built in")
	}
}

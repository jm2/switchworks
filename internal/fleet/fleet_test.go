// SPDX-License-Identifier: GPL-3.0-or-later

package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSendArgsTo(t *testing.T) {
	cases := []struct {
		in   string
		one  string
		many []string
		err  string
	}{
		{`{"to": "a", "text": "x"}`, "a", nil, ""},
		{`{"to": ["a"], "text": "x"}`, "a", nil, ""},
		{`{"to": ["a", "b"], "text": "x"}`, "", []string{"a", "b"}, ""},
		{`{"text": "x"}`, "", nil, ""},
		{`{"to": [], "text": "x"}`, "", nil, "empty list"},
		{`{"to": 3, "text": "x"}`, "", nil, "list of addresses"},
		{`{"to": "a", "txt": "x"}`, "", nil, "unknown field"},
		{`{"to": ["` + strings.Repeat(`a", "`, MaxRecipients) + `a"]}`, "", nil, "at most"},
	}
	for _, c := range cases {
		var a SendArgs
		err := json.Unmarshal([]byte(c.in), &a)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || a.To != c.one || strings.Join(a.ToList, ",") != strings.Join(c.many, ",") || a.Text != "x" {
			t.Errorf("%s: %+v %v", c.in, a, err)
		}
	}
	// One address is written as a string, as older coordinators read it.
	b, _ := json.Marshal(SendArgs{To: "a", Text: "x"})
	if !strings.Contains(string(b), `"to":"a"`) {
		t.Fatalf("one: %s", b)
	}
	b, _ = json.Marshal(SendArgs{ToList: []string{"a", "b"}, Text: "x"})
	var back SendArgs
	if err := json.Unmarshal(b, &back); err != nil || strings.Join(back.ToList, ",") != "a,b" {
		t.Fatalf("round trip %s: %+v %v", b, back, err)
	}
}

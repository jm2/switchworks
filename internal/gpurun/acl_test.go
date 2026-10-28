// SPDX-License-Identifier: GPL-3.0-or-later

package gpurun

import (
	"path/filepath"
	"testing"
)

func TestPlanDACL(t *testing.T) {
	sid, other := []byte{1, 2, 3}, []byte{9}
	owner := ACE{Type: aceAllowed, Flags: 0, Mask: fileAllAccess, SID: other}
	inh := ACE{Type: aceAllowed, Flags: inheritedACE | inheritFlags, Mask: fileAllAccess, SID: other}
	inhMine := ACE{Type: aceAllowed, Flags: inheritedACE, Mask: allowWrite, SID: sid} // from the workspace
	old := []ACE{owner, inh, inhMine}

	out, changed := PlanDACL(old, sid, ModeWrite, true)
	if !changed || len(out) != 4 || out[0].Mask != fileAllAccess || out[1].Mask != allowWrite || out[1].Flags != inheritFlags || out[2].Flags&inheritedACE == 0 {
		t.Fatalf("write: %+v", out)
	}
	if _, changed := PlanDACL(out, sid, ModeWrite, true); changed {
		t.Error("write applied twice")
	}

	// A .git under a writable directory: deny first, before everything,
	// so it wins over the ACE it inherited.
	out, changed = PlanDACL(old, sid, ModeRead, false)
	if !changed || len(out) != 5 || out[0].Type != aceDenied || out[0].Mask != denyChange || out[0].Flags != 0 || out[2].Mask != allowRead {
		t.Fatalf("read: %+v", out)
	}
	if _, changed := PlanDACL(out, sid, ModeRead, false); changed {
		t.Error("read applied twice")
	}
	// A path the deny list gained later: its old allow goes.
	w, _ := PlanDACL(old, sid, ModeWrite, false)
	out, changed = PlanDACL(w, sid, ModeHidden, false)
	if !changed || len(out) != 4 || out[0].Type != aceDenied || out[0].Mask != fileAllAccess {
		t.Fatalf("hidden: %+v", out)
	}
	for _, a := range out[1:] {
		if a.Flags&inheritedACE == 0 && a.forSID(sid) {
			t.Errorf("explicit ACE for the container left: %+v", a)
		}
	}
	// Denies everything ModeRead lets it change, and ModeWrite cannot
	// delete through the parent.
	if need := uint32(0x2 | 0x4 | 0x40 | accessDelete | 0x40000 | 0x80000); denyChange&need != need || allowWrite&0x40 != 0 || allowRead&(0x2|0x4|accessDelete) != 0 {
		t.Error("masks")
	}
}

func TestPlanDACLDenyFirst(t *testing.T) {
	sid, aap := []byte{1, 2, 3}, []byte{7}
	// An earlier allow for all AppContainers ahead of the deny: re-planned
	// so the deny leads.
	old := []ACE{{Type: aceAllowed, Mask: fileAllAccess, SID: aap},
		{Type: aceDenied, Mask: denyChange, SID: sid}, {Type: aceAllowed, Mask: allowRead, SID: sid}}
	out, changed := PlanDACL(old, sid, ModeRead, false)
	if !changed || out[0].Type != aceDenied || !out[0].forSID(sid) {
		t.Fatalf("%v %+v", changed, out)
	}
	// Revoke drops the container's explicit ACEs and keeps inherited ones.
	inh := ACE{Type: aceAllowed, Flags: inheritedACE, Mask: allowWrite, SID: sid}
	out, changed = PlanDACL(append(out, inh), sid, modeRevoke, false)
	if !changed || len(out) != 2 || out[0].forSID(sid) {
		t.Fatalf("revoke: %+v", out)
	}
	if _, changed := PlanDACL(out, sid, modeRevoke, false); changed {
		t.Error("revoke applied twice")
	}
}

func TestPathMode(t *testing.T) {
	ro := []string{filepath.FromSlash("/w/.git")}
	hid := []string{filepath.FromSlash("/w/keys"), filepath.FromSlash("/w/.git/secret")}
	for p, want := range map[string]Mode{"/w": ModeWrite, "/w/src/a.c": ModeWrite, "/w/.git": ModeRead, "/w/.git/config": ModeRead,
		"/w/keys": ModeHidden, "/w/keys/k": ModeHidden, "/w/.git/secret/x": ModeHidden, "/w/.gitignore": ModeWrite} {
		if got := PathMode(filepath.FromSlash(p), ro, hid); got != want {
			t.Errorf("%s: %d, want %d", p, got, want)
		}
	}
}

func TestACLRoundTrip(t *testing.T) {
	sid := []byte{1, 1, 0, 0, 0, 0, 0, 15, 2, 0, 0, 0} // S-1-15-2, one subauthority
	ownerSID := []byte{1, 2, 0, 0, 0, 0, 0, 5, 32, 0, 0, 0, 32, 2, 0, 0}
	audit := []byte{0x11, 0, 8, 0, 1, 2, 3, 4} // an ACE of another type, kept as is
	b := MarshalACL(2, []ACE{{Type: aceAllowed, Flags: inheritedACE, Mask: fileAllAccess, SID: ownerSID}, {Type: 0x11, Raw: audit}})
	rev, aces, err := ParseACL(b)
	if err != nil || rev != 2 || len(aces) != 2 || aces[0].Mask != fileAllAccess || string(aces[0].SID) != string(ownerSID) || string(aces[1].Raw) != string(audit) {
		t.Fatalf("%v %d %+v", err, rev, aces)
	}
	out, _ := PlanDACL(aces, sid, ModeRead, true)
	b2 := MarshalACL(rev, out)
	_, again, err := ParseACL(b2)
	if err != nil || len(again) != 4 || again[0].Type != aceDenied || string(again[0].SID) != string(sid) || again[0].Flags != inheritFlags {
		t.Fatalf("%v %+v", err, again)
	}
	if _, changed := PlanDACL(again, sid, ModeRead, true); changed {
		t.Error("parsed plan not recognised as applied")
	}
	for _, bad := range [][]byte{nil, {2, 0, 200, 0, 1, 0, 0, 0}, {2, 0, 12, 0, 1, 0, 0, 0, 0, 0, 99, 0}} {
		if _, _, err := ParseACL(bad); err == nil {
			t.Errorf("%v parsed", bad)
		}
	}
}

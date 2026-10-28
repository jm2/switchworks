// SPDX-License-Identifier: GPL-3.0-or-later

package gpurun

import "bytes"

// The AppContainer reaches the workspace through ACEs for its SID, which the
// node adds to each file and directory itself (aclwalk_windows.go): an
// explicit ACE per object, set through the object's handle, never through
// Windows' own propagation, which goes by path and so could be led out of the
// workspace by a junction an agent planted.

// Access modes of a path for the AppContainer.
type Mode int

const (
	ModeWrite  Mode = iota // read, write, create, delete
	ModeRead               // read and execute only: .git
	ModeHidden             // nothing: the deny list's entries
)

// ACE types and flags (winnt.h).
const (
	aceAllowed         = 0x0
	aceDenied          = 0x1
	objectInherit      = 0x1
	containerInherit   = 0x2
	inheritedACE       = 0x10
	inheritFlags       = objectInherit | containerInherit
	fileGenericRead    = 0x120089
	fileGenericWrite   = 0x120116
	fileGenericExecute = 0x1200A0
	accessDelete       = 0x10000
	fileAllAccess      = 0x1F01FF
	// Deny for ModeRead: every right that changes the object or its
	// children, its security or its name.
	denyChange = 0x2 | 0x4 | 0x10 | 0x40 | 0x100 | accessDelete | 0x40000 | 0x80000
	// ModeWrite has no FILE_DELETE_CHILD: entries are deleted through their
	// own DELETE right, so a ModeRead child (.git), denied DELETE, cannot be
	// deleted or replaced through its parent.
	allowWrite = fileGenericRead | fileGenericWrite | fileGenericExecute | accessDelete
	allowRead  = fileGenericRead | fileGenericExecute
)

// ACE is an access-allowed or access-denied ACE.
type ACE struct {
	Type  byte
	Flags byte
	Mask  uint32
	SID   []byte
	// Raw is the ACE as read, for ACEs kept unchanged (any type).
	Raw []byte
}

// wantACEs are the explicit ACEs a path in mode m gets for sid, deny first.
func wantACEs(m Mode, dir bool, sid []byte) []ACE {
	var fl byte
	if dir {
		fl = inheritFlags
	}
	switch m {
	case ModeRead:
		return []ACE{{Type: aceDenied, Flags: fl, Mask: denyChange, SID: sid}, {Type: aceAllowed, Flags: fl, Mask: allowRead, SID: sid}}
	case ModeHidden:
		return []ACE{{Type: aceDenied, Flags: fl, Mask: fileAllAccess, SID: sid}}
	default:
		return []ACE{{Type: aceAllowed, Flags: fl, Mask: allowWrite, SID: sid}}
	}
}

func (a ACE) forSID(sid []byte) bool {
	return (a.Type == aceAllowed || a.Type == aceDenied) && bytes.Equal(a.SID, sid)
}

// PlanDACL returns the ACEs of a DACL (old, in order) with this run's explicit
// ACEs for sid: its earlier explicit ACEs for sid are replaced, the wanted
// deny ACE goes first, the wanted allow ACE after the other explicit ACEs,
// and inherited ACEs stay last, as they were. changed is false when old
// already has exactly the wanted explicit ACEs for sid.
func PlanDACL(old []ACE, sid []byte, m Mode, dir bool) (out []ACE, changed bool) {
	want := wantACEs(m, dir, sid)
	var have []ACE
	for _, a := range old {
		if a.Flags&inheritedACE == 0 && a.forSID(sid) {
			have = append(have, a)
		}
	}
	if len(have) == len(want) {
		same := true
		for i := range want {
			// The deny and allow ACEs may be in either order among other
			// explicit ACEs; deny-first holds since deny ACEs sort first.
			found := false
			for _, h := range have {
				if h.Type == want[i].Type && h.Flags == want[i].Flags && h.Mask == want[i].Mask {
					found = true
				}
			}
			same = same && found
		}
		if same {
			return old, false
		}
	}
	var explicit, inherited []ACE
	for _, a := range old {
		switch {
		case a.Flags&inheritedACE != 0:
			inherited = append(inherited, a)
		case a.forSID(sid):
		default:
			explicit = append(explicit, a)
		}
	}
	for _, w := range want {
		if w.Type == aceDenied {
			out = append(out, w)
		}
	}
	out = append(out, explicit...)
	for _, w := range want {
		if w.Type == aceAllowed {
			out = append(out, w)
		}
	}
	return append(out, inherited...), true
}

// PathMode is the mode of a workspace entry: the deny list's entries and
// what is below them are ModeHidden, a .git and what is below it ModeRead,
// the rest ModeWrite.
func PathMode(path string, readOnly, hidden []string) Mode {
	for _, h := range hidden {
		if Within(h, path) {
			return ModeHidden
		}
	}
	for _, r := range readOnly {
		if Within(r, path) {
			return ModeRead
		}
	}
	return ModeWrite
}

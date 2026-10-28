// SPDX-License-Identifier: GPL-3.0-or-later

package gpurun

import (
	"bytes"
	"encoding/binary"
	"errors"
)

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

// ParseACL reads an ACL's ACEs and revision. ACEs other than plain allow
// and deny keep only Raw (and Type and Flags).
func ParseACL(b []byte) (rev byte, aces []ACE, err error) {
	if len(b) < 8 {
		return 0, nil, errors.New("ACL too short")
	}
	rev = b[0]
	size, count := int(binary.LittleEndian.Uint16(b[2:])), int(binary.LittleEndian.Uint16(b[4:]))
	if size > len(b) || size < 8 {
		return 0, nil, errors.New("bad ACL size")
	}
	b = b[8:size]
	for i := 0; i < count; i++ {
		if len(b) < 4 {
			return 0, nil, errors.New("ACL truncated")
		}
		n := int(binary.LittleEndian.Uint16(b[2:]))
		if n < 4 || n > len(b) {
			return 0, nil, errors.New("bad ACE size")
		}
		a := ACE{Type: b[0], Flags: b[1], Raw: append([]byte(nil), b[:n]...)}
		if (a.Type == aceAllowed || a.Type == aceDenied) && n >= 16 {
			a.Mask = binary.LittleEndian.Uint32(b[4:])
			sid := b[8:n]
			if sl := 8 + 4*int(sid[1]); sl <= len(sid) {
				a.SID = append([]byte(nil), sid[:sl]...)
			}
		}
		aces = append(aces, a)
		b = b[n:]
	}
	return rev, aces, nil
}

// MarshalACL builds an ACL of aces (Raw where set, else Type, Flags, Mask
// and SID). Revision 2 (ACL_REVISION) unless rev is higher.
func MarshalACL(rev byte, aces []ACE) []byte {
	if rev < 2 {
		rev = 2
	}
	body := []byte{}
	for _, a := range aces {
		if a.Raw != nil {
			body = append(body, a.Raw...)
			continue
		}
		n := 8 + len(a.SID)
		e := make([]byte, 8, n)
		e[0], e[1] = a.Type, a.Flags
		binary.LittleEndian.PutUint16(e[2:], uint16(n))
		binary.LittleEndian.PutUint32(e[4:], a.Mask)
		body = append(body, append(e, a.SID...)...)
	}
	h := make([]byte, 8, 8+len(body))
	h[0] = rev
	binary.LittleEndian.PutUint16(h[2:], uint16(8+len(body)))
	binary.LittleEndian.PutUint16(h[4:], uint16(len(aces)))
	return append(h, body...)
}

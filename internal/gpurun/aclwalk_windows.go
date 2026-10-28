// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

package gpurun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// maxEntries bounds the workspace walk.
const maxEntries = 1 << 20

// grant gives the AppContainer sid its access to the workspace: ModeWrite
// on everything, ModeRead on every .git (at any depth) and what is in it,
// ModeHidden on the hidden paths and what is in them.
//
// Each entry is opened without following reparse points and checked by its
// handle: not a link (symlink, junction, mount point), not a file with
// several hard links, and its final path is the one walked to (no directory
// on the way was swapped for a link). Then its DACL is set through that
// handle with SetKernelObjectSecurity, which does not propagate: Windows'
// own propagation (SetNamedSecurityInfo, icacls /T) goes by path and could be
// led out of the workspace by a junction an agent planted.
//
// An entry that fails those checks gets nothing, which is safe for ModeWrite
// (the container cannot reach it; counted in skipped). For a ModeRead or
// ModeHidden entry it is an error: the run is refused.
func grant(ws string, sid []byte, readOnly, hidden []string) (skipped int, err error) {
	root, err := openEntry(ws)
	if err != nil {
		return 0, err
	}
	rootFinal, rinfo, err := entryInfo(root)
	if err == nil && rinfo.FileAttributes&fileAttributeReparse != 0 {
		err = fmt.Errorf("%s is a link", ws)
	}
	if err == nil {
		err = setDACL(root, sid, ModeWrite, true)
	}
	syscall.CloseHandle(root)
	if err != nil {
		return 0, fmt.Errorf("workspace %s: %w", ws, err)
	}

	type item struct {
		path, final string
		mode        Mode
	}
	stack := []item{{ws, rootFinal, ModeWrite}}
	n := 0
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ents, err := os.ReadDir(dir.path)
		if err != nil {
			if dir.mode != ModeWrite {
				return skipped, fmt.Errorf("%s: %w", dir.path, err)
			}
			skipped++
			continue
		}
		for _, e := range ents {
			if n++; n > maxEntries {
				return skipped, fmt.Errorf("the workspace has over %d entries", maxEntries)
			}
			p := filepath.Join(dir.path, e.Name())
			m := max(dir.mode, PathMode(p, readOnly, hidden))
			if strings.EqualFold(e.Name(), ".git") {
				m = max(m, ModeRead)
			}
			final := strings.TrimRight(dir.final, `\`) + `\` + e.Name()
			isDir, err := grantEntry(p, final, sid, m)
			if err != nil {
				if m != ModeWrite {
					return skipped, fmt.Errorf("%s: %w; refusing the run", p, err)
				}
				skipped++
				continue
			}
			if isDir {
				stack = append(stack, item{p, final, m})
			}
		}
	}
	return skipped, nil
}

var errLink = errors.New("it is a link (symlink, junction or hard link)")

func grantEntry(p, final string, sid []byte, m Mode) (isDir bool, err error) {
	h, err := openEntry(p)
	if err != nil {
		return false, err
	}
	defer syscall.CloseHandle(h)
	got, fi, err := entryInfo(h)
	if err != nil {
		return false, err
	}
	if fi.FileAttributes&fileAttributeReparse != 0 {
		return false, errLink
	}
	isDir = fi.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0
	if !strings.EqualFold(got, final) {
		return false, fmt.Errorf("its path changed while it was opened (now %s)", got)
	}
	if !isDir && fi.NumberOfLinks > 1 {
		if m == ModeWrite {
			return false, errLink
		}
		// The deny reaches the file under all its names, which costs
		// nothing; an allow would give the container its other names.
		m = ModeHidden
	}
	return isDir, setDACL(h, sid, m, isDir)
}

func openEntry(p string) (syscall.Handle, error) {
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(u, readControl|writeDAC|fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, fileFlagBackupSemantics|fileFlagOpenReparsePoint, 0)
}

// entryInfo returns the final path of h (as a plain DOS path) and its
// attributes.
func entryInfo(h syscall.Handle) (string, syscall.ByHandleFileInformation, error) {
	var fi syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &fi); err != nil {
		return "", fi, err
	}
	buf := make([]uint16, 512)
	for {
		r, _, err := procGetFinalPathNameByHandleW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if r == 0 {
			return "", fi, fmt.Errorf("GetFinalPathNameByHandle: %w", err)
		}
		if int(r) < len(buf) {
			return dosPath(syscall.UTF16ToString(buf[:r])), fi, nil
		}
		buf = make([]uint16, r+1)
	}
}

// dosPath turns \\?\C:\x into C:\x and \\?\UNC\srv\share into \\srv\share.
func dosPath(p string) string {
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return `\\` + p[len(`\\?\UNC\`):]
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// setDACL gives h this run's explicit ACEs for sid (PlanDACL), through
// SetKernelObjectSecurity, which sets the object's own DACL and nothing
// else.
func setDACL(h syscall.Handle, sid []byte, m Mode, dir bool) error {
	var dacl, sd uintptr
	if r, _, _ := procGetSecurityInfo.Call(uintptr(h), seFileObject, daclSecurityInformation, 0, 0,
		uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd))); r != 0 {
		return fmt.Errorf("GetSecurityInfo: %w", syscall.Errno(r))
	}
	defer localFree(sd)
	if dacl == 0 {
		if m == ModeWrite {
			return nil // a NULL DACL lets everyone in, the container too
		}
		return errors.New("it has no DACL (everyone has access)")
	}
	size := *(*uint16)(at(dacl, 2))
	rev, aces, err := ParseACL(unsafe.Slice((*byte)(at(dacl, 0)), size))
	if err != nil {
		return err
	}
	out, changed := PlanDACL(aces, sid, m, dir)
	if !changed {
		return nil
	}
	acl := MarshalACL(rev, out)
	if len(acl) > 0xFFFC {
		return errors.New("its ACL would be too large")
	}
	var ctrl uint16
	var sdRev uint32
	if r, _, err := procGetSecurityDescriptorControl.Call(sd, uintptr(unsafe.Pointer(&ctrl)), uintptr(unsafe.Pointer(&sdRev))); r == 0 {
		return fmt.Errorf("GetSecurityDescriptorControl: %w", err)
	}
	mem, err := localAlloc(uintptr(securityDescriptorMinSize + len(acl)))
	if err != nil {
		return err
	}
	defer localFree(mem)
	aclMem := mem + securityDescriptorMinSize
	copy(unsafe.Slice((*byte)(at(aclMem, 0)), len(acl)), acl)
	if r, _, err := procInitializeSecurityDescriptor.Call(mem, securityDescriptorRev); r == 0 {
		return fmt.Errorf("InitializeSecurityDescriptor: %w", err)
	}
	if r, _, err := procSetSecurityDescriptorDacl.Call(mem, 1, aclMem, 0); r == 0 {
		return fmt.Errorf("SetSecurityDescriptorDacl: %w", err)
	}
	keep := uintptr(seDACLAutoInherited | seDACLProtected)
	if r, _, err := procSetSecurityDescriptorControl.Call(mem, keep, uintptr(ctrl)&keep); r == 0 {
		return fmt.Errorf("SetSecurityDescriptorControl: %w", err)
	}
	if r, _, err := procSetKernelObjectSecurity.Call(uintptr(h), daclSecurityInformation, mem); r == 0 {
		return fmt.Errorf("SetKernelObjectSecurity: %w", err)
	}
	return nil
}

// SPDX-License-Identifier: GPL-3.0-or-later

//go:build windows

// Package wintray is the Windows tray backend: a notification-area icon
// (Shell_NotifyIcon) with a popup menu, through user32, shell32 and gdi32
// directly, so it needs neither cgo nor third-party modules.
package wintray

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/jm2/switchyard/internal/tray"
	"github.com/jm2/switchyard/internal/version"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware     = user32.NewProc("SetProcessDPIAware")
	procCreateIconIndirect     = user32.NewProc("CreateIconIndirect")
	procDestroyIcon            = user32.NewProc("DestroyIcon")
	procGetDC                  = user32.NewProc("GetDC")
	procReleaseDC              = user32.NewProc("ReleaseDC")
	procShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	procCreateDIBSection       = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap           = gdi32.NewProc("CreateBitmap")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")
	procCreateMutexW           = kernel32.NewProc("CreateMutexW")
)

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmCommand     = 0x0111
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmApp         = 0x8000
	wmTrayIcon    = wmApp + 1
	wmApplyView   = wmApp + 2
	wmShowBalloon = wmApp + 3

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	niifWarning = 0x2

	mfString    = 0x0
	mfGrayed    = 0x1
	mfChecked   = 0x8
	mfSeparator = 0x800

	tpmRightButton = 0x2
	tpmNoNotify    = 0x80
	tpmReturnCmd   = 0x100

	smCxSmIcon = 49

	errorAlreadyExists syscall.Errno = 183
)

// MutexName is held while the tray runs. The installer's AppMutex names it,
// so setup can ask the user to close the tray before replacing it.
const MutexName = "SwitchyardTray"

// ErrAlreadyRunning is returned by Run when this session already has a tray.
var ErrAlreadyRunning = fmt.Errorf("another %s tray is already running", version.Name)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   syscall.Handle
	Icon       syscall.Handle
	Cursor     syscall.Handle
	Background syscall.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     syscall.Handle
}

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type notifyIconData struct {
	Size             uint32
	Wnd              uintptr
	ID               uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             syscall.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GuidItem         guid
	BalloonIcon      syscall.Handle
}

type point struct{ X, Y int32 }

type msg struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  syscall.Handle
	HbmColor syscall.Handle
}

// Backend implements tray.Backend.
type Backend struct {
	hwnd           uintptr
	taskbarCreated uintptr

	mu      sync.Mutex
	view    tray.View
	icons   map[tray.Icon]syscall.Handle
	balloon [2]string
	added   bool
}

// The window procedure is a process-wide callback; only one tray runs.
var (
	current   *Backend
	wndProcCB = syscall.NewCallback(wndProc)
)

// New returns the Windows tray backend.
func New() *Backend { return &Backend{icons: map[tray.Icon]syscall.Handle{}} }

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return p
}

// Run implements tray.Backend.
func (b *Backend) Run(ready func()) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if m, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(utf16(MutexName)))); m == 0 {
		return fmt.Errorf("create mutex: %w", err)
	} else if err == errorAlreadyExists {
		syscall.CloseHandle(syscall.Handle(m))
		return ErrAlreadyRunning
	}
	// The mutex stays held until the process exits.

	_, _, _ = procSetProcessDPIAware.Call()
	current = b
	inst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16("SwitchyardTrayWindow")
	wc := wndClassEx{WndProc: wndProcCB, Instance: syscall.Handle(inst), ClassName: className}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("register window class: %w", err)
	}
	// A hidden top-level window (not message-only, which would miss the
	// TaskbarCreated broadcast sent when Explorer restarts).
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16(version.Name))),
		0, 0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("create window: %w", err)
	}
	b.hwnd = hwnd
	b.taskbarCreated, _, _ = procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(utf16("TaskbarCreated"))))
	b.applyView()
	ready()

	var m msg
	for {
		r, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			return fmt.Errorf("message loop: %w", err)
		case 0:
			return nil // WM_QUIT
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// Update implements tray.Backend.
func (b *Backend) Update(v tray.View) {
	b.mu.Lock()
	b.view = v
	b.mu.Unlock()
	if b.hwnd != 0 {
		_, _, _ = procPostMessageW.Call(b.hwnd, wmApplyView, 0, 0)
	}
}

// Notify implements tray.Backend with a notification balloon.
func (b *Backend) Notify(title, text string) {
	b.mu.Lock()
	b.balloon = [2]string{title, text}
	b.mu.Unlock()
	if b.hwnd != 0 {
		_, _, _ = procPostMessageW.Call(b.hwnd, wmShowBalloon, 0, 0)
	}
}

// Quit implements tray.Backend.
func (b *Backend) Quit() {
	if b.hwnd != 0 {
		_, _, _ = procPostMessageW.Call(b.hwnd, wmClose, 0, 0)
	}
}

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(strings.ReplaceAll(s, "\x00", ""))
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

func (b *Backend) nid() notifyIconData {
	var d notifyIconData
	d.Size = uint32(unsafe.Sizeof(d))
	d.Wnd = b.hwnd
	d.ID = 1
	return d
}

// applyView runs on the UI thread.
func (b *Backend) applyView() {
	b.mu.Lock()
	v := b.view
	b.mu.Unlock()
	d := b.nid()
	d.Flags = nifMessage | nifIcon | nifTip
	d.CallbackMessage = wmTrayIcon
	d.Icon = b.icon(v.Icon)
	tip := v.Tooltip
	if tip == "" {
		tip = version.Name
	}
	copyUTF16(d.Tip[:], tip)
	op := uintptr(nimModify)
	if !b.added {
		op = nimAdd
	}
	if r, _, _ := procShellNotifyIconW.Call(op, uintptr(unsafe.Pointer(&d))); r != 0 {
		b.added = true
	} else if b.added {
		// Explorer may have restarted without us seeing TaskbarCreated.
		if r, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&d))); r != 0 {
			b.added = true
		}
	}
}

func (b *Backend) showBalloon() {
	b.mu.Lock()
	bl := b.balloon
	b.mu.Unlock()
	d := b.nid()
	d.Flags = nifInfo
	d.InfoFlags = niifWarning
	copyUTF16(d.InfoTitle[:], bl[0])
	copyUTF16(d.Info[:], bl[1])
	_, _, _ = procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&d)))
}

// icon returns (creating once) the HICON for a state at the small-icon size.
func (b *Backend) icon(ic tray.Icon) syscall.Handle {
	if h, ok := b.icons[ic]; ok {
		return h
	}
	want, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	size := tray.IconSizes[len(tray.IconSizes)-1]
	for _, s := range tray.IconSizes {
		if s >= int(want) {
			size = s
			break
		}
	}
	h, err := makeIcon(ic, size)
	if err != nil {
		return 0
	}
	b.icons[ic] = h
	return h
}

func makeIcon(ic tray.Icon, size int) (syscall.Handle, error) {
	img, err := tray.IconImage(ic, size)
	if err != nil {
		return 0, err
	}
	bi := bitmapInfoHeader{Width: int32(size), Height: -int32(size), Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	hdc, _, _ := procGetDC.Call(0)
	var bits unsafe.Pointer
	hbm, _, _ := procCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	_, _, _ = procReleaseDC.Call(0, hdc)
	if hbm == 0 || bits == nil {
		return 0, errors.New("CreateDIBSection failed")
	}
	defer procDeleteObject.Call(hbm)
	px := unsafe.Slice((*byte)(bits), size*size*4)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := img.NRGBAAt(x, y)
			i := (y*size + x) * 4
			px[i], px[i+1], px[i+2], px[i+3] = c.B, c.G, c.R, c.A
		}
	}
	mask, _, _ := procCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, 0)
	if mask == 0 {
		return 0, errors.New("CreateBitmap failed")
	}
	defer procDeleteObject.Call(mask)
	ii := iconInfo{FIcon: 1, HbmMask: syscall.Handle(mask), HbmColor: syscall.Handle(hbm)}
	h, _, err := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	if h == 0 {
		return 0, fmt.Errorf("CreateIconIndirect: %w", err)
	}
	return syscall.Handle(h), nil
}

// showMenu pops up the menu at the cursor and runs the chosen item.
func (b *Backend) showMenu() {
	b.mu.Lock()
	items := b.view.Items
	b.mu.Unlock()
	h, _, _ := procCreatePopupMenu.Call()
	if h == 0 {
		return
	}
	defer procDestroyMenu.Call(h)
	for i, it := range items {
		if it.Separator {
			_, _, _ = procAppendMenuW.Call(h, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if !it.Enabled {
			flags |= mfGrayed
		}
		if it.Checkable && it.Checked {
			flags |= mfChecked
		}
		label := strings.ReplaceAll(it.Label, "&", "&&")
		_, _, _ = procAppendMenuW.Call(h, flags, uintptr(i+1), uintptr(unsafe.Pointer(utf16(label))))
	}
	var pt point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Without this the menu does not close when the user clicks elsewhere.
	_, _, _ = procSetForegroundWindow.Call(b.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(h, tpmReturnCmd|tpmNoNotify|tpmRightButton,
		uintptr(pt.X), uintptr(pt.Y), 0, b.hwnd, 0)
	_, _, _ = procPostMessageW.Call(b.hwnd, wmNull, 0, 0)
	if i := int(cmd) - 1; i >= 0 && i < len(items) && items[i].Enabled && items[i].Action != nil {
		items[i].Action()
	}
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	b := current
	if b != nil && hwnd == b.hwnd {
		switch {
		case message == wmTrayIcon:
			switch lParam & 0xFFFF {
			case wmLButtonUp, wmRButtonUp:
				b.showMenu()
			}
			return 0
		case message == wmApplyView:
			b.applyView()
			return 0
		case message == wmShowBalloon:
			b.showBalloon()
			return 0
		case b.taskbarCreated != 0 && message == b.taskbarCreated:
			b.added = false
			b.applyView()
			return 0
		case message == wmClose:
			_, _, _ = procDestroyWindow.Call(hwnd)
			return 0
		case message == wmDestroy:
			d := b.nid()
			_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&d)))
			for _, h := range b.icons {
				_, _, _ = procDestroyIcon.Call(uintptr(h))
			}
			_, _, _ = procPostQuitMessage.Call(0)
			return 0
		case message == wmCommand:
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

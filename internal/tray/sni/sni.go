// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

// Package sni is the Linux tray backend: a StatusNotifierItem with a
// com.canonical.dbusmenu menu, on the session bus. KDE Plasma, Xfce, LXQt,
// Cinnamon, Budgie and others show these natively; GNOME needs the
// AppIndicator extension (packaged by most distributions as
// gnome-shell-extension-appindicator).
package sni

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/jm2/switchyard/internal/dbus"
	"github.com/jm2/switchyard/internal/tray"
	"github.com/jm2/switchyard/internal/version"
)

const (
	itemPath = dbus.ObjectPath("/StatusNotifierItem")
	menuPath = dbus.ObjectPath("/MenuBar")

	itemIface    = "org.kde.StatusNotifierItem"
	menuIface    = "com.canonical.dbusmenu"
	propsIface   = "org.freedesktop.DBus.Properties"
	introIface   = "org.freedesktop.DBus.Introspectable"
	peerIface    = "org.freedesktop.DBus.Peer"
	watcherName  = "org.kde.StatusNotifierWatcher"
	watcherPath  = dbus.ObjectPath("/StatusNotifierWatcher")
	busName      = "org.freedesktop.DBus"
	instanceName = "io.github.jm2.Switchyard.Tray"

	errUnknownMethod   = "org.freedesktop.DBus.Error.UnknownMethod"
	errUnknownObject   = "org.freedesktop.DBus.Error.UnknownObject"
	errUnknownProperty = "org.freedesktop.DBus.Error.UnknownProperty"
	errInvalidArgs     = "org.freedesktop.DBus.Error.InvalidArgs"
)

// ErrAlreadyRunning is returned by Run when another swyd-tray owns the bus
// name for this session.
var ErrAlreadyRunning = fmt.Errorf("another %s tray is already running in this session", version.Name)

// Backend implements tray.Backend over D-Bus.
type Backend struct {
	// Address overrides the session bus address (for tests).
	Address string
	// Logf receives diagnostics; nil logs to stderr.
	Logf func(string, ...any)

	conn *dbus.Conn
	name string
	quit chan struct{}
	once sync.Once

	mu       sync.Mutex
	view     tray.View
	revision uint32
	shown    map[int32]string // id -> label, as the host last read the menu
}

// New returns a Backend for the user's session bus.
func New() *Backend { return &Backend{quit: make(chan struct{})} }

func (b *Backend) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Run implements tray.Backend.
func (b *Backend) Run(ready func()) error {
	var err error
	if b.Address != "" {
		b.conn, err = dbus.Dial(b.Address)
	} else {
		b.conn, err = dbus.SessionBus()
	}
	if err != nil {
		return fmt.Errorf("connect to the session bus: %w", err)
	}
	defer b.conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// One tray per session.
	if r, err := b.conn.RequestName(ctx, instanceName); err != nil {
		return err
	} else if r != dbus.NamePrimaryOwner && r != dbus.NameAlreadyOwner {
		return ErrAlreadyRunning
	}
	b.name = fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	if _, err := b.conn.RequestName(ctx, b.name); err != nil {
		return err
	}
	b.conn.Handle(b.handle)
	// Register again whenever a watcher (the panel) appears or restarts.
	rule := "type='signal',sender='" + busName + "',interface='" + busName + "',member='NameOwnerChanged',arg0='" + watcherName + "'"
	if err := b.conn.AddMatch(ctx, rule); err != nil {
		return err
	}
	if err := b.register(ctx); err != nil {
		b.logf("no system tray yet (%v); the icon appears when one starts. On GNOME, enable the AppIndicator extension.", err)
	}
	ready()
	select {
	case <-b.quit:
		return nil
	case <-b.conn.Done():
		return fmt.Errorf("session bus connection lost: %w", b.conn.Err())
	}
}

func (b *Backend) register(ctx context.Context) error {
	_, err := b.conn.Call(ctx, watcherName, watcherPath, watcherName, "RegisterStatusNotifierItem", "s", b.name)
	return err
}

// Quit implements tray.Backend.
func (b *Backend) Quit() { b.once.Do(func() { close(b.quit) }) }

// Update implements tray.Backend.
func (b *Backend) Update(v tray.View) {
	b.mu.Lock()
	old := b.view
	// Always keep the new view: its actions are new closures. Tell the host
	// only about what it can see changing, so an open menu is not rebuilt
	// under the pointer on every poll.
	b.view = v
	first := b.revision == 0
	menuChanged := first || !sameItems(old.Items, v.Items)
	if menuChanged {
		b.revision++
	}
	rev := b.revision
	b.mu.Unlock()
	if b.conn == nil {
		return
	}
	if first || old.Icon != v.Icon {
		_ = b.conn.Emit(itemPath, itemIface, "NewIcon", "")
	}
	if old.Tooltip != v.Tooltip {
		_ = b.conn.Emit(itemPath, itemIface, "NewToolTip", "")
	}
	if menuChanged {
		_ = b.conn.Emit(menuPath, menuIface, "LayoutUpdated", "ui", rev, int32(0))
	}
}

// sameItems compares what a menu shows, ignoring the actions.
func sameItems(a, b []tray.Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Label != y.Label || x.Enabled != y.Enabled || x.Separator != y.Separator ||
			x.Checkable != y.Checkable || x.Checked != y.Checked {
			return false
		}
	}
	return true
}

// Notify implements tray.Backend with a desktop notification.
func (b *Backend) Notify(title, text string) {
	if b.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := b.conn.Call(ctx, "org.freedesktop.Notifications", "/org/freedesktop/Notifications",
		"org.freedesktop.Notifications", "Notify", "susssasa{sv}i",
		version.Name, uint32(0), tray.AppID, title, text, []string{}, map[string]dbus.Variant{}, int32(-1))
	if err != nil {
		b.logf("%s: %s", title, text)
	}
}

func (b *Backend) handle(m *dbus.Message) {
	if m.Type == dbus.TypeSignal {
		if m.Member == "NameOwnerChanged" && len(m.Body) == 3 && m.Body[0] == watcherName && m.Body[2] != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := b.register(ctx); err != nil {
				b.logf("register with the system tray: %v", err)
			}
		}
		return
	}
	switch {
	case m.Interface == peerIface && m.Member == "Ping":
		_ = b.conn.Reply(m, "")
	case m.Interface == peerIface && m.Member == "GetMachineId":
		id, _ := os.ReadFile("/etc/machine-id")
		_ = b.conn.Reply(m, "s", string(trimNL(id)))
	case m.Interface == introIface && m.Member == "Introspect":
		b.introspect(m)
	case m.Path == itemPath:
		b.handleItem(m)
	case m.Path == menuPath:
		b.handleMenu(m)
	default:
		_ = b.conn.ReplyError(m, errUnknownObject, "no object "+string(m.Path))
	}
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// --- StatusNotifierItem ---

func (b *Backend) itemProps() map[string]dbus.Variant {
	b.mu.Lock()
	v := b.view
	b.mu.Unlock()
	return map[string]dbus.Variant{
		"Category":            {Sig: "s", Value: "ApplicationStatus"},
		"Id":                  {Sig: "s", Value: version.Slug},
		"Title":               {Sig: "s", Value: version.Name},
		"Status":              {Sig: "s", Value: "Active"},
		"WindowId":            {Sig: "i", Value: int32(0)},
		"IconThemePath":       {Sig: "s", Value: ""},
		"IconName":            {Sig: "s", Value: ""},
		"IconPixmap":          {Sig: "a(iiay)", Value: pixmaps(v.Icon)},
		"OverlayIconName":     {Sig: "s", Value: ""},
		"OverlayIconPixmap":   {Sig: "a(iiay)", Value: []any{}},
		"AttentionIconName":   {Sig: "s", Value: ""},
		"AttentionIconPixmap": {Sig: "a(iiay)", Value: []any{}},
		"AttentionMovieName":  {Sig: "s", Value: ""},
		"ToolTip":             {Sig: "(sa(iiay)ss)", Value: []any{"", []any{}, version.Name, v.Tooltip}},
		"ItemIsMenu":          {Sig: "b", Value: true},
		"Menu":                {Sig: "o", Value: menuPath},
	}
}

// pixmaps renders the icon as ARGB32 in network byte order, at each size.
func pixmaps(ic tray.Icon) []any {
	var out []any
	for _, size := range []int{22, 32, 48, 64} {
		img, err := tray.IconImage(ic, size)
		if err != nil {
			continue
		}
		data := make([]byte, 0, size*size*4)
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				c := img.NRGBAAt(x, y)
				data = binary.BigEndian.AppendUint32(data, uint32(c.A)<<24|uint32(c.R)<<16|uint32(c.G)<<8|uint32(c.B))
			}
		}
		out = append(out, []any{int32(size), int32(size), data})
	}
	return out
}

func (b *Backend) handleItem(m *dbus.Message) {
	switch {
	case m.Interface == propsIface:
		b.handleProps(m, itemIface, b.itemProps())
	case m.Member == "Activate" || m.Member == "SecondaryActivate" || m.Member == "ContextMenu":
		// With ItemIsMenu set, hosts open the menu themselves; a host that
		// calls Activate anyway gets the web UI, the most useful default.
		if m.Member == "Activate" {
			b.activateLabel("Open web UI")
		}
		_ = b.conn.Reply(m, "")
	case m.Member == "Scroll":
		_ = b.conn.Reply(m, "")
	default:
		_ = b.conn.ReplyError(m, errUnknownMethod, m.Member)
	}
}

func (b *Backend) activateLabel(label string) {
	b.mu.Lock()
	var act func()
	for _, it := range b.view.Items {
		if it.Label == label && it.Enabled {
			act = it.Action
		}
	}
	b.mu.Unlock()
	if act != nil {
		act()
	}
}

func (b *Backend) handleProps(m *dbus.Message, iface string, props map[string]dbus.Variant) {
	switch m.Member {
	case "Get":
		if len(m.Body) != 2 {
			_ = b.conn.ReplyError(m, errInvalidArgs, "Get takes (ss)")
			return
		}
		name, _ := m.Body[1].(string)
		if in, _ := m.Body[0].(string); in != iface && in != "" {
			_ = b.conn.ReplyError(m, errUnknownProperty, in)
			return
		}
		v, ok := props[name]
		if !ok {
			_ = b.conn.ReplyError(m, errUnknownProperty, name)
			return
		}
		_ = b.conn.Reply(m, "v", v)
	case "GetAll":
		if len(m.Body) == 1 {
			if in, _ := m.Body[0].(string); in != iface && in != "" {
				_ = b.conn.Reply(m, "a{sv}", map[string]dbus.Variant{})
				return
			}
		}
		_ = b.conn.Reply(m, "a{sv}", props)
	case "Set":
		_ = b.conn.ReplyError(m, "org.freedesktop.DBus.Error.PropertyReadOnly", "read-only")
	default:
		_ = b.conn.ReplyError(m, errUnknownMethod, m.Member)
	}
}

// --- com.canonical.dbusmenu ---

func (b *Backend) menuProps() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Version":       {Sig: "u", Value: uint32(3)},
		"TextDirection": {Sig: "s", Value: "ltr"},
		"Status":        {Sig: "s", Value: "normal"},
		"IconThemePath": {Sig: "as", Value: []string{}},
	}
}

// itemProperties returns one menu item's properties; id 0 is the root.
func itemProperties(it tray.Item) map[string]dbus.Variant {
	if it.Separator {
		return map[string]dbus.Variant{"type": {Sig: "s", Value: "separator"}}
	}
	p := map[string]dbus.Variant{
		"label":   {Sig: "s", Value: escapeMnemonic(it.Label)},
		"enabled": {Sig: "b", Value: it.Enabled},
		"visible": {Sig: "b", Value: true},
	}
	if it.Checkable {
		state := int32(0)
		if it.Checked {
			state = 1
		}
		p["toggle-type"] = dbus.Variant{Sig: "s", Value: "checkmark"}
		p["toggle-state"] = dbus.Variant{Sig: "i", Value: state}
	}
	return p
}

// escapeMnemonic doubles underscores, which dbusmenu treats as access keys.
func escapeMnemonic(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			out = append(out, '_')
		}
		out = append(out, s[i])
	}
	return string(out)
}

func filterProps(p map[string]dbus.Variant, names []any) map[string]dbus.Variant {
	if len(names) == 0 {
		return p
	}
	out := map[string]dbus.Variant{}
	for _, n := range names {
		if s, ok := n.(string); ok {
			if v, ok := p[s]; ok {
				out[s] = v
			}
		}
	}
	return out
}

// snapshot returns the menu's items keyed by id (1-based) and the revision.
func (b *Backend) snapshot() ([]tray.Item, uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.view.Items
	// Remember what each id showed when the host last read the menu, so a
	// click is matched to the item the user saw even if the menu changed.
	b.shown = make(map[int32]string, len(items))
	for i, it := range items {
		if !it.Separator {
			b.shown[int32(i+1)] = it.Label
		}
	}
	return items, b.revision
}

func (b *Backend) layout(names []any) (uint32, []any) {
	items, rev := b.snapshot()
	children := make([]dbus.Variant, 0, len(items))
	for i, it := range items {
		children = append(children, dbus.Variant{Sig: "(ia{sv}av)", Value: []any{
			int32(i + 1), filterProps(itemProperties(it), names), []dbus.Variant{}}})
	}
	root := []any{int32(0), filterProps(map[string]dbus.Variant{"children-display": {Sig: "s", Value: "submenu"}}, names), children}
	return rev, root
}

func (b *Backend) handleMenu(m *dbus.Message) {
	if m.Interface == propsIface {
		b.handleProps(m, menuIface, b.menuProps())
		return
	}
	switch m.Member {
	case "GetLayout":
		if len(m.Body) != 3 {
			_ = b.conn.ReplyError(m, errInvalidArgs, "GetLayout takes (iias)")
			return
		}
		parent, _ := m.Body[0].(int32)
		names, _ := m.Body[2].([]any)
		rev, root := b.layout(names)
		if parent != 0 {
			// Items have no children: return the item itself, childless.
			items, _ := b.snapshot()
			if parent < 1 || int(parent) > len(items) {
				_ = b.conn.ReplyError(m, errInvalidArgs, "no such menu item")
				return
			}
			root = []any{parent, filterProps(itemProperties(items[parent-1]), names), []dbus.Variant{}}
		}
		_ = b.conn.Reply(m, "u(ia{sv}av)", rev, root)
	case "GetGroupProperties":
		if len(m.Body) != 2 {
			_ = b.conn.ReplyError(m, errInvalidArgs, "GetGroupProperties takes (aias)")
			return
		}
		ids, _ := m.Body[0].([]any)
		names, _ := m.Body[1].([]any)
		items, _ := b.snapshot()
		var out []any
		// Each item is returned once, whatever the request repeats.
		seen := map[int32]bool{}
		for _, x := range ids {
			if len(seen) >= len(items) {
				break
			}
			id, _ := x.(int32)
			if id >= 1 && int(id) <= len(items) && !seen[id] {
				seen[id] = true
				out = append(out, []any{id, filterProps(itemProperties(items[id-1]), names)})
			}
		}
		_ = b.conn.Reply(m, "a(ia{sv})", out)
	case "GetProperty":
		if len(m.Body) != 2 {
			_ = b.conn.ReplyError(m, errInvalidArgs, "GetProperty takes (is)")
			return
		}
		id, _ := m.Body[0].(int32)
		name, _ := m.Body[1].(string)
		items, _ := b.snapshot()
		if id < 1 || int(id) > len(items) {
			_ = b.conn.ReplyError(m, errInvalidArgs, "no such menu item")
			return
		}
		v, ok := itemProperties(items[id-1])[name]
		if !ok {
			_ = b.conn.ReplyError(m, errInvalidArgs, "no such property")
			return
		}
		_ = b.conn.Reply(m, "v", v)
	case "Event":
		if len(m.Body) == 4 {
			id, _ := m.Body[0].(int32)
			ev, _ := m.Body[1].(string)
			b.event(id, ev)
		}
		_ = b.conn.Reply(m, "")
	case "EventGroup":
		var bad []any
		if len(m.Body) == 1 {
			evs, _ := m.Body[0].([]any)
			for _, e := range evs {
				f, _ := e.([]any)
				if len(f) != 4 {
					continue
				}
				id, _ := f[0].(int32)
				ev, _ := f[1].(string)
				if !b.event(id, ev) {
					bad = append(bad, id)
				}
			}
		}
		_ = b.conn.Reply(m, "ai", bad)
	case "AboutToShow":
		_ = b.conn.Reply(m, "b", false)
	case "AboutToShowGroup":
		_ = b.conn.Reply(m, "aiai", []any{}, []any{})
	default:
		_ = b.conn.ReplyError(m, errUnknownMethod, m.Member)
	}
}

// event runs a clicked item's action; it reports whether id exists.
func (b *Backend) event(id int32, ev string) bool {
	b.mu.Lock()
	label, known := b.shown[id]
	var act func()
	if known {
		// The current item with that label (the actions are rebuilt on
		// every poll), preferring the same position.
		items := b.view.Items
		match := func(it tray.Item) bool { return it.Label == label && it.Enabled && it.Action != nil }
		if i := int(id) - 1; i < len(items) && match(items[i]) {
			act = items[i].Action
		} else {
			for _, it := range items {
				if match(it) {
					act = it.Action
					break
				}
			}
		}
	}
	b.mu.Unlock()
	if ev == "clicked" && act != nil {
		act()
	}
	return known || id == 0
}

func (b *Backend) introspect(m *dbus.Message) {
	var xml string
	switch m.Path {
	case "/":
		xml = `<node><node name="StatusNotifierItem"/><node name="MenuBar"/></node>`
	case itemPath:
		xml = `<node>
 <interface name="org.kde.StatusNotifierItem">
  <property name="Category" type="s" access="read"/>
  <property name="Id" type="s" access="read"/>
  <property name="Title" type="s" access="read"/>
  <property name="Status" type="s" access="read"/>
  <property name="WindowId" type="i" access="read"/>
  <property name="IconThemePath" type="s" access="read"/>
  <property name="IconName" type="s" access="read"/>
  <property name="IconPixmap" type="a(iiay)" access="read"/>
  <property name="OverlayIconName" type="s" access="read"/>
  <property name="OverlayIconPixmap" type="a(iiay)" access="read"/>
  <property name="AttentionIconName" type="s" access="read"/>
  <property name="AttentionIconPixmap" type="a(iiay)" access="read"/>
  <property name="AttentionMovieName" type="s" access="read"/>
  <property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
  <property name="ItemIsMenu" type="b" access="read"/>
  <property name="Menu" type="o" access="read"/>
  <method name="ContextMenu"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
  <method name="Activate"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
  <method name="SecondaryActivate"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
  <method name="Scroll"><arg name="delta" type="i" direction="in"/><arg name="orientation" type="s" direction="in"/></method>
  <signal name="NewTitle"/><signal name="NewIcon"/><signal name="NewAttentionIcon"/><signal name="NewOverlayIcon"/><signal name="NewToolTip"/>
  <signal name="NewStatus"><arg name="status" type="s"/></signal>
 </interface>
 <interface name="org.freedesktop.DBus.Properties">
  <method name="Get"><arg type="s" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
  <method name="GetAll"><arg type="s" direction="in"/><arg type="a{sv}" direction="out"/></method>
 </interface>
 <interface name="org.freedesktop.DBus.Introspectable"><method name="Introspect"><arg type="s" direction="out"/></method></interface>
</node>`
	case menuPath:
		xml = `<node>
 <interface name="com.canonical.dbusmenu">
  <property name="Version" type="u" access="read"/>
  <property name="TextDirection" type="s" access="read"/>
  <property name="Status" type="s" access="read"/>
  <property name="IconThemePath" type="as" access="read"/>
  <method name="GetLayout"><arg type="i" name="parentId" direction="in"/><arg type="i" name="recursionDepth" direction="in"/><arg type="as" name="propertyNames" direction="in"/><arg type="u" name="revision" direction="out"/><arg type="(ia{sv}av)" name="layout" direction="out"/></method>
  <method name="GetGroupProperties"><arg type="ai" name="ids" direction="in"/><arg type="as" name="propertyNames" direction="in"/><arg type="a(ia{sv})" name="properties" direction="out"/></method>
  <method name="GetProperty"><arg type="i" name="id" direction="in"/><arg type="s" name="name" direction="in"/><arg type="v" name="value" direction="out"/></method>
  <method name="Event"><arg type="i" name="id" direction="in"/><arg type="s" name="eventId" direction="in"/><arg type="v" name="data" direction="in"/><arg type="u" name="timestamp" direction="in"/></method>
  <method name="EventGroup"><arg type="a(isvu)" name="events" direction="in"/><arg type="ai" name="idErrors" direction="out"/></method>
  <method name="AboutToShow"><arg type="i" name="id" direction="in"/><arg type="b" name="needUpdate" direction="out"/></method>
  <method name="AboutToShowGroup"><arg type="ai" name="ids" direction="in"/><arg type="ai" name="updatesNeeded" direction="out"/><arg type="ai" name="idErrors" direction="out"/></method>
  <signal name="ItemsPropertiesUpdated"><arg type="a(ia{sv})" name="updatedProps"/><arg type="a(ias)" name="removedProps"/></signal>
  <signal name="LayoutUpdated"><arg type="u" name="revision"/><arg type="i" name="parent"/></signal>
  <signal name="ItemActivationRequested"><arg type="i" name="id"/><arg type="u" name="timestamp"/></signal>
 </interface>
 <interface name="org.freedesktop.DBus.Properties">
  <method name="Get"><arg type="s" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
  <method name="GetAll"><arg type="s" direction="in"/><arg type="a{sv}" direction="out"/></method>
 </interface>
 <interface name="org.freedesktop.DBus.Introspectable"><method name="Introspect"><arg type="s" direction="out"/></method></interface>
</node>`
	default:
		_ = b.conn.ReplyError(m, errUnknownObject, "no object "+string(m.Path))
		return
	}
	_ = b.conn.Reply(m, "s", `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN" "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">`+"\n"+xml)
}

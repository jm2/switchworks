// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package sni

import (
	"context"
	"testing"
	"time"

	"github.com/jm2/switchyard/internal/dbus"
	"github.com/jm2/switchyard/internal/dbus/dbustest"
	"github.com/jm2/switchyard/internal/tray"
)

// TestItemAndMenu plays the panel's side: a fake StatusNotifierWatcher
// receives the registration, then reads the item's properties and menu and
// clicks an item, the way KDE's and GNOME's hosts do.
func TestItemAndMenu(t *testing.T) {
	addr := dbustest.StartBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	host, err := dbus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	if _, err := host.RequestName(ctx, watcherName); err != nil {
		t.Fatal(err)
	}
	registered := make(chan string, 2)
	host.Handle(func(m *dbus.Message) {
		if m.Type == dbus.TypeMethodCall && m.Member == "RegisterStatusNotifierItem" {
			registered <- m.Body[0].(string)
			_ = host.Reply(m, "")
		}
	})

	b := New()
	b.Address = addr
	b.Logf = t.Logf
	clicked := make(chan struct{}, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- b.Run(func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("backend did not start")
	}
	b.Update(tray.View{Icon: tray.IconOK, Tooltip: "Node devbox: connected", Items: []tray.Item{
		{Label: "Node devbox: connected to coord.lan"},
		{Separator: true},
		{Label: "Open web_ui", Enabled: true, Action: func() { clicked <- struct{}{} }},
		{Label: "Start this tray at login", Enabled: true, Checkable: true, Checked: true, Action: func() {}},
	}})

	var svc string
	select {
	case svc = <-registered:
	case <-ctx.Done():
		t.Fatal("item never registered with the watcher")
	}

	rep, err := host.Call(ctx, svc, itemPath, propsIface, "GetAll", "s", itemIface)
	if err != nil {
		t.Fatal(err)
	}
	props := map[string]any{}
	for _, e := range rep.Body[0].([]any) {
		de := e.(dbus.DictEntry)
		props[de.Key.(string)] = de.Value.(dbus.Variant).Value
	}
	if props["Menu"] != menuPath || props["ItemIsMenu"] != true || props["Id"] != "switchyard" {
		t.Fatalf("item properties = %v", props)
	}
	pix := props["IconPixmap"].([]any)
	if len(pix) == 0 {
		t.Fatal("no icon pixmaps")
	}
	first := pix[0].([]any)
	w, h, data := first[0].(int32), first[1].(int32), first[2].([]byte)
	if int(w*h*4) != len(data) {
		t.Fatalf("pixmap %dx%d has %d bytes", w, h, len(data))
	}
	tip := props["ToolTip"].([]any)
	if tip[3] != "Node devbox: connected" {
		t.Fatalf("tooltip = %v", tip)
	}

	rep, err = host.Call(ctx, svc, menuPath, menuIface, "GetLayout", "iias", int32(0), int32(-1), []string{})
	if err != nil {
		t.Fatal(err)
	}
	root := rep.Body[1].([]any)
	children := root[2].([]any)
	if len(children) != 4 {
		t.Fatalf("menu has %d items", len(children))
	}
	item := func(i int) map[string]any {
		c := children[i].(dbus.Variant).Value.([]any)
		out := map[string]any{}
		for _, e := range c[1].([]any) {
			de := e.(dbus.DictEntry)
			out[de.Key.(string)] = de.Value.(dbus.Variant).Value
		}
		return out
	}
	if item(1)["type"] != "separator" || item(0)["enabled"] != false || item(2)["label"] != "Open web__ui" {
		t.Fatalf("menu items = %v %v %v", item(0), item(1), item(2))
	}
	if item(3)["toggle-type"] != "checkmark" || item(3)["toggle-state"] != int32(1) {
		t.Fatalf("checkbox item = %v", item(3))
	}

	// Clicking the disabled first line does nothing; clicking item 3 runs it.
	if _, err := host.Call(ctx, svc, menuPath, menuIface, "Event", "isvu", int32(1), "clicked", dbus.Variant{Sig: "i", Value: int32(0)}, uint32(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Call(ctx, svc, menuPath, menuIface, "Event", "isvu", int32(3), "clicked", dbus.Variant{Sig: "i", Value: int32(0)}, uint32(0)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clicked:
	case <-ctx.Done():
		t.Fatal("click did not run the action")
	}
	if _, err := host.Call(ctx, svc, itemPath, "org.freedesktop.DBus.Introspectable", "Introspect", ""); err != nil {
		t.Fatal(err)
	}

	// A second tray in the same session refuses to start.
	b2 := New()
	b2.Address = addr
	if err := b2.Run(func() {}); err != ErrAlreadyRunning {
		t.Fatalf("second tray: %v", err)
	}

	b.Quit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

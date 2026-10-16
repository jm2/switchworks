// SPDX-License-Identifier: GPL-3.0-or-later

// Package tray is the platform-neutral part of swk-tray: it reads the
// node's and coordinator's state through package service, turns it into a
// menu, and runs the menu's actions. A Backend draws the icon and menu with
// the platform's own tray API (StatusNotifierItem on Linux, Shell_NotifyIcon
// on Windows). The macOS menu-bar app is a separate Swift program that reads
// the same state from `swk service status --json`.
//
// The tray only reads local files and runs service-manager commands as the
// user; it opens no ports and never reads tokens.
package tray

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jm2/switchworks/internal/service"
	"github.com/jm2/switchworks/internal/version"
)

// Icon is the tray icon's state.
type Icon int

const (
	IconIdle Icon = iota // nothing set up on this machine
	IconOK               // node connected (or the coordinator running)
	IconWarn             // set up, but stopped or not connected
)

// Item is one menu entry. A zero Item with Separator set is a separator.
type Item struct {
	Label     string
	Enabled   bool
	Separator bool
	Checkable bool
	Checked   bool
	Action    func() `json:"-"`
}

// View is everything a backend shows.
type View struct {
	Icon    Icon
	Tooltip string
	Items   []Item
}

// Backend is a platform tray implementation.
type Backend interface {
	// Run shows the icon and blocks until Quit is called or the tray
	// cannot be shown. ready is called once the icon can be updated.
	Run(ready func()) error
	// Update replaces the icon, tooltip and menu. Safe from any goroutine.
	Update(View)
	// Notify shows a short message, such as a failed action.
	Notify(title, text string)
	// Quit ends Run.
	Quit()
}

// Platform hooks, set per OS.
var (
	openURL  = func(string) error { return fmt.Errorf("opening links is not supported here") }
	openPath = func(string) error { return fmt.Errorf("opening folders is not supported here") }
)

// Poll is how often the tray re-reads the state.
const Poll = 5 * time.Second

// App connects a Backend to Switchworks's state.
type App struct {
	b       Backend
	refresh chan struct{}
	quit    chan struct{}
	once    sync.Once

	busyMu sync.Mutex
	busy   string // the action in progress, shown in the menu
}

// NewApp returns an App drawing on b.
func NewApp(b Backend) *App {
	return &App{b: b, refresh: make(chan struct{}, 1), quit: make(chan struct{})}
}

// Run blocks until the user quits.
func (a *App) Run() error {
	return a.b.Run(func() { go a.loop() })
}

func (a *App) loop() {
	t := time.NewTicker(Poll)
	defer t.Stop()
	for {
		a.b.Update(a.view(service.Status()))
		select {
		case <-t.C:
		case <-a.refresh:
		case <-a.quit:
			return
		}
	}
}

func (a *App) poke() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

// do runs an action off the UI thread and refreshes when it is done.
func (a *App) do(label string, f func() error) func() {
	return func() {
		a.busyMu.Lock()
		if a.busy != "" {
			a.busyMu.Unlock()
			return
		}
		a.busy = label
		a.busyMu.Unlock()
		a.poke()
		go func() {
			err := f()
			a.busyMu.Lock()
			a.busy = ""
			a.busyMu.Unlock()
			if err != nil {
				a.b.Notify(version.Name+": "+label+" failed", err.Error())
			}
			a.poke()
		}()
	}
}

func (a *App) quitAll() {
	a.once.Do(func() {
		close(a.quit)
		a.b.Quit()
	})
}

// nodeLine describes the node for the menu's first line.
func nodeLine(in service.Info) string {
	s := in.Status
	switch {
	case !in.Configured:
		return "Node: not enrolled"
	case s != nil && s.Connected:
		return fmt.Sprintf("Node %s: connected to %s", s.Host, hostOnly(s.Coordinator))
	case s != nil:
		return fmt.Sprintf("Node %s: connecting to %s…", s.Host, hostOnly(s.Coordinator))
	case in.Running:
		return "Node: running"
	default:
		return "Node: stopped"
	}
}

func hostOnly(hostport string) string {
	if i := strings.LastIndexByte(hostport, ':'); i > 0 && !strings.HasSuffix(hostport, "]") {
		return strings.Trim(hostport[:i], "[]")
	}
	return hostport
}

func coordLine(in service.Info) string {
	if in.Running {
		return "Coordinator: running"
	}
	return "Coordinator: stopped"
}

func iconFor(o service.Overview) Icon {
	switch {
	case o.Node.Configured:
		if o.Node.Status != nil && o.Node.Status.Connected {
			return IconOK
		}
		return IconWarn
	case o.Coordinator.Configured:
		if o.Coordinator.Running {
			return IconOK
		}
		return IconWarn
	}
	return IconIdle
}

func roleItems(a *App, in service.Info, title string, busy bool) []Item {
	r := in.Role
	if !in.Configured {
		hint := "Enroll with: swk node enroll <token>"
		if r == service.Coordinator {
			return nil
		}
		return []Item{{Label: hint}}
	}
	if !in.Installed {
		return []Item{{Label: "Run " + title + " at login", Enabled: !busy && in.Error == "",
			Action: a.do("setting up the "+r+" service", func() error { return service.Install(r, "", false, io.Discard) })}}
	}
	return []Item{
		{Label: "Start " + title, Enabled: !busy && !in.Running, Action: a.do("starting the "+r, func() error { return service.Start(r) })},
		{Label: "Stop " + title, Enabled: !busy && in.Running, Action: a.do("stopping the "+r, func() error { return service.Stop(r) })},
		{Label: "Restart " + title, Enabled: !busy, Action: a.do("restarting the "+r, func() error { return service.Restart(r) })},
	}
}

func (a *App) view(o service.Overview) View {
	a.busyMu.Lock()
	busy := a.busy
	a.busyMu.Unlock()

	v := View{Icon: iconFor(o)}
	lines := []string{nodeLine(o.Node)}
	if o.Coordinator.Configured {
		lines = append(lines, coordLine(o.Coordinator))
	}
	v.Tooltip = version.Name + "\n" + strings.Join(lines, "\n")
	for _, l := range lines {
		v.Items = append(v.Items, Item{Label: l})
	}
	if busy != "" {
		v.Items = append(v.Items, Item{Label: strings.ToUpper(busy[:1]) + busy[1:] + "…"})
	}
	sep := Item{Separator: true}
	ui := o.WebUI
	v.Items = append(v.Items, sep, Item{Label: "Open web UI", Enabled: ui != "", Action: a.do("opening the web UI", func() error { return openURL(ui) })})

	v.Items = append(v.Items, sep)
	v.Items = append(v.Items, roleItems(a, o.Node, "node", busy != "")...)
	if o.Coordinator.Configured {
		v.Items = append(v.Items, sep)
		v.Items = append(v.Items, roleItems(a, o.Coordinator, "coordinator", busy != "")...)
	}

	logs, cfg := o.Node.LogDir, o.ConfigDir
	if !o.Node.Configured && o.Coordinator.Configured {
		logs = o.Coordinator.LogDir
	}
	v.Items = append(v.Items, sep,
		Item{Label: "Open logs folder", Enabled: true, Action: a.do("opening the logs folder", func() error {
			if err := os.MkdirAll(logs, 0o700); err != nil {
				return err
			}
			return openPath(logs)
		})},
		Item{Label: "Open config folder", Enabled: true, Action: a.do("opening the config folder", func() error {
			if err := os.MkdirAll(cfg, 0o700); err != nil {
				return err
			}
			return openPath(cfg)
		})},
	)
	if autostartSupported {
		on := autostartEnabled()
		v.Items = append(v.Items, sep, Item{Label: "Start this tray at login", Enabled: true, Checkable: true, Checked: on,
			Action: a.do("changing the login item", func() error { return setAutostart(!on) })})
	}
	v.Items = append(v.Items, sep, Item{Label: "Quit", Enabled: true, Action: a.quitAll})
	return v
}

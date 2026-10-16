// SPDX-License-Identifier: GPL-3.0-or-later

package tray

import (
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/runstatus"
	"github.com/jm2/switchworks/internal/service"
)

type nopBackend struct{}

func (nopBackend) Run(func()) error      { return nil }
func (nopBackend) Update(View)           {}
func (nopBackend) Notify(string, string) {}
func (nopBackend) Quit()                 {}

func labels(v View) string {
	var out []string
	for _, it := range v.Items {
		if it.Separator {
			out = append(out, "--")
			continue
		}
		l := it.Label
		if !it.Enabled {
			l = "(" + l + ")"
		}
		out = append(out, l)
	}
	return strings.Join(out, " | ")
}

func TestView(t *testing.T) {
	a := NewApp(nopBackend{})
	node := service.Info{Role: service.Node, Configured: true, Installed: true, Enabled: true, Running: true,
		Status: &runstatus.Status{Host: "devbox", Coordinator: "coord.lan:7481", Connected: true}}
	o := service.Overview{Node: node, Coordinator: service.Info{Role: service.Coordinator}, WebUI: "https://coord.lan:7480/"}
	v := a.view(o)
	got := labels(v)
	for _, want := range []string{"(Node devbox: connected to coord.lan)", "Open web UI", "(Start node)", "Stop node", "Restart node", "Quit"} {
		if !strings.Contains(got, want) {
			t.Errorf("menu %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "Coordinator") {
		t.Errorf("coordinator shown on a node-only host: %q", got)
	}
	if v.Icon != IconOK {
		t.Errorf("icon = %v, want OK", v.Icon)
	}

	o.Node.Status.Connected = false
	if v := a.view(o); v.Icon != IconWarn || !strings.Contains(labels(v), "connecting to coord.lan") {
		t.Errorf("disconnected: icon %v, menu %q", v.Icon, labels(v))
	}

	o.Node = service.Info{Role: service.Node, Configured: true}
	o.Coordinator = service.Info{Role: service.Coordinator, Configured: true, Installed: true, Running: true}
	got = labels(a.view(o))
	for _, want := range []string{"(Node: stopped)", "(Coordinator: running)", "Run node at login", "Stop coordinator"} {
		if !strings.Contains(got, want) {
			t.Errorf("menu %q lacks %q", got, want)
		}
	}

	o = service.Overview{Node: service.Info{Role: service.Node}, Coordinator: service.Info{Role: service.Coordinator}}
	v = a.view(o)
	if v.Icon != IconIdle || !strings.Contains(labels(v), "(Open web UI)") || !strings.Contains(labels(v), "Enroll with") {
		t.Errorf("fresh machine: icon %v, menu %q", v.Icon, labels(v))
	}
}

func TestIconImage(t *testing.T) {
	for _, size := range IconSizes {
		for _, ic := range []Icon{IconIdle, IconOK, IconWarn} {
			img, err := IconImage(ic, size)
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
				t.Fatalf("icon %d is %v", size, img.Bounds())
			}
		}
		ok, _ := IconImage(IconOK, size)
		idle, _ := IconImage(IconIdle, size)
		c := ok.NRGBAAt(size-size/5, size-size/5)
		if c == idle.NRGBAAt(size-size/5, size-size/5) {
			t.Errorf("%dpx: no status dot drawn", size)
		}
	}
	if _, err := IconImage(IconOK, 17); err == nil {
		t.Error("odd size accepted")
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{"coord.lan:7481": "coord.lan", "[fd00::1]:7481": "fd00::1", "coord.lan": "coord.lan"} {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

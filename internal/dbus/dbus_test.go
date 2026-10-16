// SPDX-License-Identifier: GPL-3.0-or-later

package dbus_test

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jm2/switchworks/internal/dbus"
	"github.com/jm2/switchworks/internal/dbus/dbustest"
)

func TestMarshalRoundTrip(t *testing.T) {
	layout := []any{int32(0), map[string]dbus.Variant{"children-display": {Sig: "s", Value: "submenu"}},
		[]dbus.Variant{{Sig: "(ia{sv}av)", Value: []any{int32(1), map[string]dbus.Variant{"label": {Sig: "s", Value: "Quit"}}, []dbus.Variant{}}}}}
	in := &dbus.Message{Type: dbus.TypeMethodReturn, ReplySerial: 7, Destination: ":1.9",
		Signature: "u(ia{sv}av)a(iiay)ybxtdgo",
		Body: []any{uint32(3), layout, []any{[]any{int32(2), int32(1), []byte{1, 2, 3, 4, 5, 6, 7, 8}}},
			byte(9), true, int64(-5), uint64(1 << 40), 1.5, dbus.Signature("a{sv}"), dbus.ObjectPath("/MenuBar")}}
	b, err := dbus.MarshalForTest(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dbus.ReadForTest(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if out.ReplySerial != 7 || out.Destination != ":1.9" || out.Signature != in.Signature {
		t.Fatalf("header = %+v", out)
	}
	if got := out.Body[0].(uint32); got != 3 {
		t.Fatalf("revision = %d", got)
	}
	l := out.Body[1].([]any)
	if l[0].(int32) != 0 {
		t.Fatalf("layout id = %v", l[0])
	}
	props := l[1].([]any)
	if len(props) != 1 || props[0].(dbus.DictEntry).Key != "children-display" || props[0].(dbus.DictEntry).Value.(dbus.Variant).Value != "submenu" {
		t.Fatalf("props = %#v", props)
	}
	child := l[2].([]any)[0].(dbus.Variant)
	if child.Sig != "(ia{sv}av)" || child.Value.([]any)[0].(int32) != 1 {
		t.Fatalf("child = %#v", child)
	}
	pix := out.Body[2].([]any)[0].([]any)
	if !bytes.Equal(pix[2].([]byte), []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("pixmap = %#v", pix)
	}
	rest := out.Body[3:]
	want := []any{byte(9), true, int64(-5), uint64(1 << 40), 1.5, dbus.Signature("a{sv}"), dbus.ObjectPath("/MenuBar")}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("scalars = %#v, want %#v", rest, want)
	}
}

func TestMarshalRejectsMismatch(t *testing.T) {
	for _, m := range []*dbus.Message{
		{Type: dbus.TypeSignal, Path: "/a", Interface: "a.b", Member: "C", Signature: "s", Body: []any{int32(1)}},
		{Type: dbus.TypeSignal, Path: "/a", Interface: "a.b", Member: "C", Signature: "ss", Body: []any{"x"}},
		{Type: dbus.TypeSignal, Path: "/a", Interface: "a.b", Member: "C", Signature: "v", Body: []any{dbus.Variant{Sig: "ss", Value: "x"}}},
		{Type: dbus.TypeSignal, Path: "/a", Interface: "a.b", Member: "C", Signature: "(s", Body: []any{[]any{"x"}}},
	} {
		if _, err := dbus.MarshalForTest(m); err == nil {
			t.Errorf("marshal %q %#v: no error", m.Signature, m.Body)
		}
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	good, err := dbus.MarshalForTest(&dbus.Message{Type: dbus.TypeSignal, Path: "/a", Interface: "a.b", Member: "C", Signature: "as", Body: []any{[]string{"x", "y"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(good); i++ {
		_, _ = dbus.ReadForTest(bytes.NewReader(good[:i])) // must not panic
	}
	bad := append([]byte(nil), good...)
	bad[0] = 'X'
	if _, err := dbus.ReadForTest(bytes.NewReader(bad)); err == nil {
		t.Fatal("bad endianness accepted")
	}
}

func TestBus(t *testing.T) {
	addr := dbustest.StartBus(t)
	server, err := dbus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := dbus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if n, err := server.RequestName(ctx, "dev.switchworks.Test"); err != nil || n != dbus.NamePrimaryOwner {
		t.Fatalf("RequestName = %d, %v", n, err)
	}
	if n, err := client.RequestName(ctx, "dev.switchworks.Test"); err != nil || n != dbus.NameExists {
		t.Fatalf("second RequestName = %d, %v", n, err)
	}
	server.Handle(func(m *dbus.Message) {
		if m.Type != dbus.TypeMethodCall {
			return
		}
		switch m.Member {
		case "Echo":
			_ = server.Reply(m, m.Signature, m.Body...)
		default:
			_ = server.ReplyError(m, "org.freedesktop.DBus.Error.UnknownMethod", "no "+m.Member)
		}
	})
	rep, err := client.Call(ctx, "dev.switchworks.Test", "/x", "dev.switchworks.Test", "Echo", "sa{sv}", "hi", map[string]dbus.Variant{"n": {Sig: "i", Value: int32(4)}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Body[0] != "hi" || rep.Body[1].([]any)[0].(dbus.DictEntry).Value.(dbus.Variant).Value != int32(4) {
		t.Fatalf("echo = %#v", rep.Body)
	}
	_, err = client.Call(ctx, "dev.switchworks.Test", "/x", "dev.switchworks.Test", "Nope", "")
	if e, ok := err.(*dbus.Error); !ok || e.Name != "org.freedesktop.DBus.Error.UnknownMethod" {
		t.Fatalf("error reply = %v", err)
	}

	got := make(chan *dbus.Message, 1)
	client.Handle(func(m *dbus.Message) {
		if m.Type == dbus.TypeSignal && m.Member == "Changed" {
			got <- m
		}
	})
	if err := client.AddMatch(ctx, "type='signal',interface='dev.switchworks.Test'"); err != nil {
		t.Fatal(err)
	}
	if err := server.Emit("/x", "dev.switchworks.Test", "Changed", "u", uint32(5)); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.Body[0].(uint32) != 5 || m.Sender != server.UniqueName() {
			t.Fatalf("signal = %+v", m)
		}
	case <-ctx.Done():
		t.Fatal("signal not received")
	}
}

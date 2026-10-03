package tray

import (
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ryanmwright/tether/internal/api"
)

// trayClient plays the desktop's tray on a private bus.
type trayClient struct {
	t       *testing.T
	conn    *dbus.Conn
	signals chan *dbus.Signal
}

func newTrayClient(t *testing.T, iface string) *trayClient {
	t.Helper()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.AddMatchSignal(dbus.WithMatchInterface(iface)); err != nil {
		t.Fatal(err)
	}
	c := &trayClient{t: t, conn: conn, signals: make(chan *dbus.Signal, 256)}
	conn.Signal(c.signals)
	return c
}

// next is the next signal, or nil if none comes soon.
func (c *trayClient) next(wait time.Duration) *dbus.Signal {
	select {
	case sig := <-c.signals:
		return sig
	case <-time.After(wait):
		return nil
	}
}

// layoutUpdates are the parents in the LayoutUpdated signals that come
// within wait, failing on any other signal but ItemsPropertiesUpdated, which
// it counts.
func (c *trayClient) layoutUpdates(wait time.Duration) (parents []int32, propUpdates int) {
	c.t.Helper()
	for {
		sig := c.next(wait)
		switch {
		case sig == nil:
			slices.Sort(parents)
			return parents, propUpdates
		case sig.Name == menuIface+".LayoutUpdated":
			parents = append(parents, sig.Body[1].(int32))
		case sig.Name == menuIface+".ItemsPropertiesUpdated":
			propUpdates++
		default:
			c.t.Fatalf("unexpected signal %s", sig.Name)
		}
	}
}

func newTestMenu(t *testing.T) (*menu, dbus.BusObject, *trayClient, chan *Action) {
	t.Helper()
	privateBus(t)
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	clicked := make(chan *Action, 1)
	m, err := newMenu(conn, func(a *Action) { clicked <- a }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c := newTrayClient(t, menuIface)
	return m, c.conn.Object(conn.Names()[0], menuPath), c, clicked
}

func getLayout(t *testing.T, obj dbus.BusObject, parent, depth int32) menuLayout {
	t.Helper()
	var rev uint32
	var l menuLayout
	if err := obj.Call(menuIface+".GetLayout", 0, parent, depth, []string{}).Store(&rev, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func children(t *testing.T, l menuLayout) []menuLayout {
	t.Helper()
	var out []menuLayout
	for _, v := range l.Children {
		var c menuLayout
		if err := dbus.Store([]any{v.Value()}, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// entry is the entry labelled label under l, at any depth.
func entry(t *testing.T, l menuLayout, label string) *menuLayout {
	t.Helper()
	for _, c := range children(t, l) {
		if c.Props["label"].Value() == label {
			return &c
		}
		if e := entry(t, c, label); e != nil {
			return e
		}
	}
	return nil
}

func entryID(t *testing.T, l menuLayout, label string) int32 {
	t.Helper()
	e := entry(t, l, label)
	if e == nil {
		t.Fatalf("no entry %q", label)
	}
	return e.ID
}

func withHost(st api.Status, i int, h api.HostStatus) api.Status {
	st.Hosts = slices.Clone(st.Hosts)
	st.Hosts[i] = h
	return st
}

func TestMenuLayout(t *testing.T) {
	m, obj, c, clicked := newTestMenu(t)
	m.Update(Build(sample))
	if parents, _ := c.layoutUpdates(100 * time.Millisecond); !slices.Equal(parents, []int32{0}) {
		t.Errorf("first menu: layout updates for %v", parents)
	}

	l := getLayout(t, obj, 0, -1)
	dev := entry(t, l, "● dev (up)")
	if dev == nil || dev.Props["children-display"].Value() != "submenu" || dev.Props["enabled"].Value() != true {
		t.Fatalf("dev = %+v", dev)
	}
	if gpg := entry(t, l, "Forward gpg-agent"); gpg == nil || gpg.Props["toggle-type"].Value() != "checkmark" || gpg.Props["toggle-state"].Value() != int32(1) {
		t.Errorf("gpg toggle = %+v", gpg)
	}
	if top := children(t, l); !slices.ContainsFunc(top, func(c menuLayout) bool { return c.Props["type"].Value() == "separator" }) {
		t.Error("no separator at the top level")
	}
	if p := itemProps(Item{Title: "my_box"}); p["label"].Value() != "my__box" {
		t.Errorf("label = %v", p["label"])
	}

	// A status change that only changes labels changes them in place.
	degraded := withHost(sample, 0, func() api.HostStatus { h := sample.Hosts[0]; h.State = api.StateDegraded; return h }())
	m.Update(Build(degraded))
	if parents, props := c.layoutUpdates(100 * time.Millisecond); len(parents) != 0 || props != 1 {
		t.Errorf("label change: layout updates for %v, %d property updates", parents, props)
	}
	l2 := getLayout(t, obj, 0, -1)
	if id := entryID(t, l2, "◐ dev (degraded)"); id != dev.ID {
		t.Errorf("dev's ID changed from %d to %d", dev.ID, id)
	}

	// A new forward changes the list in dev's submenu only.
	more := sample.Hosts[0]
	more.Forwards = append(slices.Clone(more.Forwards), api.ForwardStatus{Spec: "D:1080", AdHoc: true, State: api.StateUp})
	m.Update(Build(withHost(sample, 0, more)))
	if parents, _ := c.layoutUpdates(100 * time.Millisecond); !slices.Equal(parents, []int32{dev.ID}) {
		t.Errorf("new forward: layout updates for %v, want [%d]", parents, dev.ID)
	}

	// Clicks run the entry's action.
	reload := entryID(t, l, "Reload config")
	if err := obj.Call(menuIface+".Event", 0, reload, "clicked", dbus.MakeVariant(""), uint32(0)).Err; err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-clicked:
		if a.Method != api.MethodReload {
			t.Errorf("clicked ran %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("clicking did nothing")
	}
}

// TestMenuFetchesAhead checks that fetching a menu makes the tray fetch the
// submenus in it too, so they're ready before they're hovered.
func TestMenuFetchesAhead(t *testing.T) {
	m, obj, c, _ := newTestMenu(t)
	m.Update(Build(sample))
	c.layoutUpdates(100 * time.Millisecond)

	aboutToShow := func(id int32) bool {
		var need bool
		if err := obj.Call(menuIface+".AboutToShow", 0, id).Store(&need); err != nil {
			t.Fatal(err)
		}
		return need
	}
	// As KDE does: one level at a time.
	top := getLayout(t, obj, 0, 1)
	var submenus []int32
	for _, e := range children(t, top) {
		if e.Props["children-display"].Value() == "submenu" {
			submenus = append(submenus, e.ID)
		}
	}
	if len(submenus) != 3 {
		t.Fatalf("top-level submenus %v, want the 3 hosts", submenus)
	}
	if parents, _ := c.layoutUpdates(200 * time.Millisecond); !slices.Equal(parents, submenus) {
		t.Errorf("after fetching the top level: layout updates for %v, want %v", parents, submenus)
	}
	dev := entryID(t, top, "● dev (up)")
	if !aboutToShow(dev) {
		t.Error("dev's submenu not fetched yet, but AboutToShow says it's current")
	}
	getLayout(t, obj, dev, 1)
	if aboutToShow(dev) {
		t.Error("dev's submenu just fetched, but AboutToShow says it's out of date")
	}
	if parents, _ := c.layoutUpdates(200 * time.Millisecond); len(parents) == 0 || slices.Contains(parents, dev) || slices.Contains(parents, 0) {
		t.Errorf("after fetching dev: layout updates for %v, want dev's submenus", parents)
	}

	// Fetched whole, nothing needs fetching.
	getLayout(t, obj, 0, -1)
	if parents, _ := c.layoutUpdates(200 * time.Millisecond); len(parents) != 0 {
		t.Errorf("after fetching everything: layout updates for %v", parents)
	}
}

// TestMenuHoldsWhileOpen checks that entries aren't added or removed in a
// menu while it's open, which would lose the entry under the mouse, but
// labels still change.
func TestMenuHoldsWhileOpen(t *testing.T) {
	m, obj, c, _ := newTestMenu(t)
	m.Update(Build(sample))
	c.layoutUpdates(100 * time.Millisecond)
	event := func(id int32, name string) {
		if err := obj.Call(menuIface+".Event", 0, id, name, dbus.MakeVariant(""), uint32(0)).Err; err != nil {
			t.Fatal(err)
		}
	}

	event(0, "opened")
	fewer := withHost(sample, 0, func() api.HostStatus { h := sample.Hosts[0]; h.State = api.StateDegraded; return h }())
	fewer.Hosts = fewer.Hosts[:2]
	m.Update(Build(fewer))
	if parents, props := c.layoutUpdates(100 * time.Millisecond); len(parents) != 0 || props != 1 {
		t.Errorf("while open: layout updates for %v, %d property updates", parents, props)
	}
	l := getLayout(t, obj, 0, -1)
	if entry(t, l, "○ scratch") == nil || entry(t, l, "◐ dev (degraded)") == nil {
		t.Error("while open: want scratch still there and dev relabelled")
	}

	event(0, "closed")
	if parents, _ := c.layoutUpdates(100 * time.Millisecond); !slices.Equal(parents, []int32{0}) {
		t.Errorf("on closing: layout updates for %v", parents)
	}
	if entry(t, getLayout(t, obj, 0, -1), "○ scratch") != nil {
		t.Error("scratch still shown after closing")
	}

	// Changes elsewhere aren't held.
	event(0, "opened")
	m.Update(Build(withHost(fewer, 0, sample.Hosts[0])))
	if parents, _ := c.layoutUpdates(100 * time.Millisecond); len(parents) != 0 {
		t.Errorf("label change while open: layout updates for %v", parents)
	}
	more := sample.Hosts[0]
	more.Forwards = append(slices.Clone(more.Forwards), api.ForwardStatus{Spec: "D:1080", AdHoc: true, State: api.StateUp})
	m.Update(Build(withHost(fewer, 0, more)))
	dev := entryID(t, getLayout(t, obj, 0, 1), "● dev (up)")
	if parents, _ := c.layoutUpdates(100 * time.Millisecond); !slices.Contains(parents, dev) {
		t.Errorf("new forward in a closed submenu: layout updates for %v, want %d", parents, dev)
	}
}

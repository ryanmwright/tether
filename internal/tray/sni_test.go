package tray

import (
	"log/slog"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeWatcher is the desktop's tray, collecting the icons registered with it.
type fakeWatcher struct{ registered chan string }

func (w fakeWatcher) RegisterStatusNotifierItem(service string) *dbus.Error {
	w.registered <- service
	return nil
}

func startWatcher(t *testing.T) (*dbus.Conn, fakeWatcher) {
	t.Helper()
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	w := fakeWatcher{make(chan string, 4)}
	conn.Export(w, watcherPath, watcherName)
	if reply, err := conn.RequestName(watcherName, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("claiming the watcher name: %v %v", reply, err)
	}
	return conn, w
}

func (w fakeWatcher) expect(t *testing.T, name string) {
	t.Helper()
	select {
	case got := <-w.registered:
		if got != name {
			t.Errorf("registered %q, want %q", got, name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the icon wasn't registered")
	}
}

func TestStatusItem(t *testing.T) {
	privateBus(t)
	watcherConn, w := startWatcher(t)
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	activated := make(chan struct{}, 1)
	s, err := newStatusItem(conn, func() { activated <- struct{}{} }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.SetIcon(LookUp)
	s.SetToolTip("tether: 1 host: 1 up")
	if err := s.Show(); err != nil {
		t.Fatal(err)
	}
	w.expect(t, s.name)

	c := newTrayClient(t, itemIface)
	obj := c.conn.Object(s.name, itemPath)
	if v, err := obj.GetProperty(itemIface + ".Menu"); err != nil || v.Value() != menuPath {
		t.Errorf("Menu = %v, %v", v, err)
	}
	var icons []pixmap
	if v, err := obj.GetProperty(itemIface + ".IconPixmap"); err != nil || v.Store(&icons) != nil || len(icons) != 1 || icons[0].Width != 64 || len(icons[0].ARGB) != 64*64*4 {
		t.Errorf("IconPixmap = %v, %v", v, err)
	}
	var tip toolTip
	if v, err := obj.GetProperty(itemIface + ".ToolTip"); err != nil || v.Store(&tip) != nil || tip.Title != "tether: 1 host: 1 up" {
		t.Errorf("ToolTip = %v, %v", v, err)
	}

	if err := obj.Call(itemIface+".Activate", 0, int32(0), int32(0)).Err; err != nil {
		t.Fatal(err)
	}
	select {
	case <-activated:
	case <-time.After(2 * time.Second):
		t.Fatal("left click did nothing")
	}

	// Only changes are signalled.
	s.SetIcon(LookUp)
	s.SetToolTip("tether: 1 host: 1 up")
	if sig := c.next(100 * time.Millisecond); sig != nil {
		t.Errorf("unchanged icon signalled %s", sig.Name)
	}
	s.SetIcon(LookError)
	if sig := c.next(time.Second); sig == nil || sig.Name != itemIface+".NewIcon" {
		t.Errorf("changed icon signalled %v", sig)
	}

	// A tray that starts later, e.g. after plasmashell restarts, is told too.
	if _, err := watcherConn.ReleaseName(watcherName); err != nil {
		t.Fatal(err)
	}
	_, w2 := startWatcher(t)
	w2.expect(t, s.name)
}

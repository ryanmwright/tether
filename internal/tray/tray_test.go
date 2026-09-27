package tray

import (
	"bufio"
	"bytes"
	"image/png"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ryanmwright/tether/internal/api"
)

var sample = api.Status{
	Hosts: []api.HostStatus{
		{Name: "dev", SSH: "devbox", State: api.StateUp, Forwards: []api.ForwardStatus{
			{Spec: "L:5432:db:5432", Profiles: []string{"work"}, State: api.StateUp},
			{Spec: "gpg-agent", AdHoc: true, State: api.StateUp},
		}, Mounts: []api.MountStatus{{Key: "remote:~/src -> /home/me/src", State: api.StateError, Error: "sshfs not found"}}},
		{Name: "lab", SSH: "lab", State: api.StateError, Error: "Permission denied (publickey)"},
		{Name: "scratch", SSH: "me@10.0.0.5", AdHoc: true, State: api.StateDown},
	},
	Profiles: []api.ProfileStatus{
		{Name: "work", Host: "dev", Active: true, State: api.StateUp},
		{Name: "old", Host: "lab", State: api.StateDown},
	},
}

func find(items []Item, id string) *Item {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
		if it := find(items[i].Children, id); it != nil {
			return it
		}
	}
	return nil
}

func TestSummary(t *testing.T) {
	if state, text := Summary(sample); state != api.StateError || text != "1 up, 1 error" {
		t.Errorf("sample: %s %q", state, text)
	}
	if state, text := Summary(api.Status{Hosts: []api.HostStatus{{State: api.StateDown}}}); state != api.StateDown || text != "nothing connected" {
		t.Errorf("idle: %s %q", state, text)
	}
	if state, _ := Summary(api.Status{Hosts: []api.HostStatus{{State: api.StateUp}, {State: api.StatePending}}}); state != api.StateDegraded {
		t.Errorf("pending counts as not-yet-fine: %s", state)
	}
}

func TestBuild(t *testing.T) {
	items := Build(sample)

	if it := find(items, "summary"); it == nil || it.Title != "tether — 1 up, 1 error" || it.Enabled {
		t.Errorf("summary = %+v", it)
	}
	if it := find(items, "host:dev:down"); it == nil || it.Action.Method != api.MethodDown {
		t.Errorf("dev disconnect = %+v", it)
	}
	if it := find(items, "host:scratch:up"); it == nil || it.Action.Params != (api.TargetParams{Name: "scratch", Kind: api.TargetHost}) {
		t.Errorf("scratch connect = %+v", it)
	}
	if it := find(items, "host:lab:error"); it == nil || !strings.Contains(it.Title, "Permission denied") {
		t.Errorf("lab error label = %+v", it)
	}
	if find(items, "host:lab:retry") == nil || find(items, "host:dev:retry") != nil {
		t.Error("retry should appear only on hosts in error")
	}
	if find(items, "host:scratch:forget") == nil || find(items, "host:dev:forget") != nil {
		t.Error("forget should appear only on ad-hoc hosts")
	}
	if it := find(items, "host:dev:mount:remote:~/src -> /home/me/src"); it == nil || !strings.Contains(it.Title, "sshfs not found") {
		t.Errorf("mount row = %+v", it)
	}

	// gpg toggle reflects the ad-hoc gpg-agent forward.
	if it := find(items, "host:dev:gpg"); !it.Checked || it.Action.Method != api.MethodForwardRemove {
		t.Errorf("dev gpg = %+v", it)
	}
	if it := find(items, "host:lab:gpg"); it.Checked || it.Action.Method != api.MethodForwardAdd {
		t.Errorf("lab gpg = %+v", it)
	}

	if it := find(items, "profile:work"); !it.Checked || it.Action.Method != api.MethodDown {
		t.Errorf("active profile = %+v", it)
	}
	if it := find(items, "profile:old"); it.Checked || it.Action.Method != api.MethodUp {
		t.Errorf("inactive profile = %+v", it)
	}
	for _, id := range []string{"open-tui", "reload", "quit", "connect"} {
		if it := find(items, id); it == nil || it.Action == nil {
			t.Errorf("missing %s", id)
		}
	}
}

func TestShape(t *testing.T) {
	base := Shape(Build(sample))

	// State changes keep the shape (the menu is updated in place)...
	changed := sample
	changed.Profiles = []api.ProfileStatus{
		{Name: "work", Host: "dev", Active: false, State: api.StateDown},
		{Name: "old", Host: "lab", Active: true, State: api.StatePending},
	}
	if Shape(Build(changed)) != base {
		t.Error("profile state change altered the shape")
	}

	// ...new rows don't.
	more := sample
	more.Hosts = append([]api.HostStatus{}, sample.Hosts...)
	more.Hosts[0].Forwards = append(more.Hosts[0].Forwards, api.ForwardStatus{Spec: "D:1080", State: api.StateUp})
	if Shape(Build(more)) == base {
		t.Error("new forward didn't change the shape")
	}
}

func TestChanges(t *testing.T) {
	host := func(name string, state api.State, errMsg string) api.HostStatus {
		return api.HostStatus{Name: name, State: state, Error: errMsg}
	}
	st := func(hs ...api.HostStatus) api.Status { return api.Status{Hosts: hs} }

	lost := Changes(st(host("dev", api.StateUp, "")), st(host("dev", api.StateError, "Broken pipe")))
	if len(lost) != 1 || !lost[0].Urgent || !strings.Contains(lost[0].Body, "Connection lost: Broken pipe") {
		t.Errorf("lost = %+v", lost)
	}
	failed := Changes(st(host("dev", api.StatePending, "")), st(host("dev", api.StateError, "Permission denied")))
	if len(failed) != 1 || strings.Contains(failed[0].Body, "Connection lost") {
		t.Errorf("first connect failing = %+v", failed)
	}
	back := Changes(st(host("dev", api.StateError, "x")), st(host("dev", api.StateUp, "")))
	if len(back) != 1 || back[0].Summary != "dev: connected again" || back[0].Key != lost[0].Key {
		t.Errorf("back = %+v", back)
	}
	// Routine transitions aren't news.
	for _, pair := range [][2]api.State{{api.StateDown, api.StatePending}, {api.StatePending, api.StateUp}, {api.StateUp, api.StateDown}, {api.StateError, api.StateError}} {
		if n := Changes(st(host("dev", pair[0], "")), st(host("dev", pair[1], ""))); len(n) != 0 {
			t.Errorf("%s -> %s notified: %+v", pair[0], pair[1], n)
		}
	}
	// A new host (just added) isn't news either.
	if n := Changes(st(), st(host("new", api.StateError, "x"))); len(n) != 0 {
		t.Errorf("new host notified: %+v", n)
	}

	prev := st(api.HostStatus{Name: "dev", State: api.StateUp, Forwards: []api.ForwardStatus{{Spec: "D:1080", State: api.StateUp}}})
	cur := st(api.HostStatus{Name: "dev", State: api.StateDegraded, Forwards: []api.ForwardStatus{{Spec: "D:1080", State: api.StateError, Error: "port in use"}}})
	if n := Changes(prev, cur); len(n) != 1 || n[0].Summary != "dev: D:1080 failed" || n[0].Body != "port in use" {
		t.Errorf("forward failure = %+v", n)
	}
}

func TestIcon(t *testing.T) {
	for state, want := range iconColors {
		img, err := png.Decode(bytes.NewReader(Icon(state)))
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
			t.Errorf("%s: size %v", state, b)
		}
		// On the left ring's outline, and transparent in a corner.
		r, g, bl, a := img.At(7, 32).RGBA()
		if uint8(r>>8) != want.R || uint8(g>>8) != want.G || uint8(bl>>8) != want.B || a == 0 {
			t.Errorf("%s: ring pixel %d,%d,%d,%d", state, r>>8, g>>8, bl>>8, a>>8)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Errorf("%s: corner not transparent", state)
		}
	}
}

// fakeNotifications records Notify calls on a private session bus.
type fakeNotifications struct {
	mu    sync.Mutex
	calls []notifyCall
}

type notifyCall struct {
	replaces      uint32
	summary, body string
	urgency       byte
}

func (f *fakeNotifications) Notify(app string, replaces uint32, icon, summary, body string, actions []string, hints map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, _ := hints["urgency"].Value().(byte)
	f.calls = append(f.calls, notifyCall{replaces, summary, body, u})
	if replaces != 0 {
		return replaces, nil
	}
	return uint32(len(f.calls)), nil
}

// privateBus runs a session bus for this test only, so nothing reaches the
// user's desktop.
func privateBus(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not found")
	}
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", strings.TrimSpace(addr))
}

func TestNotifier(t *testing.T) {
	privateBus(t)
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fake := &fakeNotifications{}
	conn.Export(fake, "/org/freedesktop/Notifications", "org.freedesktop.Notifications")
	if reply, err := conn.RequestName("org.freedesktop.Notifications", dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("claiming the notification service: %v %v", reply, err)
	}

	n, err := NewNotifier()
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	for _, notice := range []Notice{
		{Key: "host:dev", Summary: "dev: not connected", Body: "Connection lost", Urgent: true},
		{Key: "host:dev", Summary: "dev: connected again"},
		{Key: "host:lab", Summary: "lab: not connected"},
	} {
		if err := n.Show(notice); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		calls := append([]notifyCall{}, fake.calls...)
		fake.mu.Unlock()
		if len(calls) == 3 {
			if calls[0].replaces != 0 || calls[0].urgency != 2 {
				t.Errorf("first = %+v", calls[0])
			}
			// The same key replaces the earlier notification instead of piling up.
			if calls[1].replaces != 1 || calls[1].urgency != 1 {
				t.Errorf("second = %+v", calls[1])
			}
			if calls[2].replaces != 0 {
				t.Errorf("third = %+v", calls[2])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("calls = %+v", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminalCommand(t *testing.T) {
	if got := terminalCommand([]string{"foot"}); len(got) != 1 || got[0] != "foot" {
		t.Errorf("preferred = %v", got)
	}
	t.Setenv("TERMINAL", "sh")
	if got := terminalCommand(nil); len(got) != 2 || got[0] != "sh" || got[1] != "-e" {
		t.Errorf("$TERMINAL = %v", got)
	}
}

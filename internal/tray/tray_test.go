package tray

import (
	"bufio"
	"bytes"
	"fmt"
	"image/png"
	"os/exec"
	"reflect"
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
	hosts := func(states ...api.State) api.Status {
		var st api.Status
		for i, s := range states {
			st.Hosts = append(st.Hosts, api.HostStatus{Name: fmt.Sprint("h", i), State: s})
		}
		return st
	}
	up, down, pending, degraded, failed := api.StateUp, api.StateDown, api.StatePending, api.StateDegraded, api.StateError
	tests := []struct {
		st   api.Status
		look Look
		text string
	}{
		{api.Status{}, LookIdle, "no hosts"},
		{hosts(down, down), LookIdle, "2 hosts: 2 disconnected"},
		{hosts(up, up), LookUp, "2 hosts: 2 up"},
		{hosts(up), LookUp, "1 host: 1 up"},
		// Green only when every host is connected.
		{hosts(up, down), LookPartial, "2 hosts: 1 up, 1 disconnected"},
		{hosts(up, pending, down), LookBusy, "3 hosts: 1 up, 1 connecting, 1 disconnected"},
		{hosts(up, degraded), LookBusy, "2 hosts: 1 up, 1 degraded"},
		{hosts(up, up, failed, down), LookError, "4 hosts: 2 up, 1 failed, 1 disconnected"},
	}
	for _, tt := range tests {
		look, text := Summary(tt.st)
		if look != tt.look || text != tt.text {
			t.Errorf("%v: got %s %q, want %s %q", tt.st.Hosts, look, text, tt.look, tt.text)
		}
	}

	if _, text := Summary(sample); text != "3 hosts: 1 up, 1 failed, 1 disconnected · 1/2 profiles active" {
		t.Errorf("with profiles: %q", text)
	}
}

func TestBuild(t *testing.T) {
	items := Build(sample)

	if it := find(items, "summary"); it == nil || it.Title != "tether — 3 hosts: 1 up, 1 failed, 1 disconnected · 1/2 profiles active" || it.Enabled {
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
	if it := find(items, "host:dev:mount:remote:~/src -> /home/me/src"); it == nil || !strings.Contains(it.Title, "sshfs not found") || len(it.Children) != 0 {
		t.Errorf("mount row = %+v", it)
	}
	if it := find(items, "host:lab:mount-here"); it == nil || *it.Action != (Action{Local: localMountHere, Host: "lab"}) {
		t.Errorf("lab mount here = %+v", it)
	}
	if it := find(items, "host:lab:mount-there"); it == nil || *it.Action != (Action{Local: localMountThere, Host: "lab"}) {
		t.Errorf("lab mount there = %+v", it)
	}

	// Ad-hoc mounts can be unmounted; profile mounts (above) can't.
	adhoc := api.Status{Hosts: []api.HostStatus{{Name: "dev", State: api.StateUp, Mounts: []api.MountStatus{
		{Key: "/home/me/proj -> remote:~/proj", Direction: "local-to-remote", Remote: "~/proj", Local: "/home/me/proj", AdHoc: true, State: api.StateUp},
	}}}}
	want := api.MountParams{Host: "dev", Direction: "local-to-remote", Remote: "~/proj", Local: "/home/me/proj"}
	if it := find(Build(adhoc), "host:dev:mount:/home/me/proj -> remote:~/proj:rm"); it == nil || it.Action.Method != api.MethodMountRemove || !reflect.DeepEqual(it.Action.Params, want) {
		t.Errorf("unmount = %+v", it)
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

func TestBuildUSB(t *testing.T) {
	st := api.Status{
		Hosts: []api.HostStatus{
			{Name: "dev", State: api.StateUp, USB: []api.USBStatus{
				{Device: "1-1", BusID: "1-1", Name: "Yubico YubiKey", AdHoc: true, State: api.StateUp},
			}},
			{Name: "lab", State: api.StateUp, USB: []api.USBStatus{
				{Device: "0627:0001", Profiles: []string{"work"}, State: api.StateError, Error: "no USB device 0627:0001 plugged in"},
			}},
		},
		USB: []api.USBDevice{
			{BusID: "1-1", ID: "1050:0407", Name: "Yubico YubiKey", Host: "dev"},
			{BusID: "1-2", ID: "046d:c52b", Name: "Logitech Receiver"},
		},
	}
	items := Build(st)

	if it := find(items, "usb:1-1"); it == nil || it.Title != "● Yubico YubiKey (1050:0407) — dev" {
		t.Errorf("shared device = %+v", it)
	}
	if it := find(items, "usb:1-1:dev"); it == nil || !it.Checked || it.Action.Method != api.MethodUSBDetach ||
		it.Action.Params != (api.USBParams{Host: "dev", Device: "1-1"}) {
		t.Errorf("unshare = %+v", it)
	}
	// It can't be shared with a second host while it's on dev.
	if it := find(items, "usb:1-1:lab"); it == nil || it.Checked || it.Enabled || it.Action != nil {
		t.Errorf("share elsewhere = %+v", it)
	}
	if it := find(items, "usb:1-2"); it == nil || it.Title != "○ Logitech Receiver (046d:c52b)" {
		t.Errorf("free device = %+v", it)
	}
	if it := find(items, "usb:1-2:lab"); it == nil || it.Checked || it.Action.Method != api.MethodUSBAttach ||
		it.Action.Params != (api.USBParams{Host: "lab", Device: "1-2"}) {
		t.Errorf("share = %+v", it)
	}
	// Per-host rows, including a profile device that isn't plugged in.
	if it := find(items, "host:dev:usb:1-1"); it == nil || it.Title != "● USB 1-1 — Yubico YubiKey" {
		t.Errorf("dev usb row = %+v", it)
	}
	if it := find(items, "host:lab:usb:0627:0001"); it == nil || !strings.Contains(it.Title, "no USB device 0627:0001 plugged in") {
		t.Errorf("lab usb row = %+v", it)
	}
	if find(items, "usb-unavailable") != nil {
		t.Error("warning shown with the helper available")
	}

	st.USB, st.USBUnavailable = nil, "the USB/IP helper isn't running"
	items = Build(st)
	if find(items, "usb-unavailable") == nil || find(items, "no-usb") == nil {
		t.Error("missing helper warning or empty-list label")
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

	prev = st(api.HostStatus{Name: "dev", State: api.StateUp, USB: []api.USBStatus{{Device: "1-1", State: api.StateUp}}})
	cur = st(api.HostStatus{Name: "dev", State: api.StateDegraded, USB: []api.USBStatus{{Device: "1-1", State: api.StateError, Error: "device unplugged"}}})
	if n := Changes(prev, cur); len(n) != 1 || n[0].Summary != "dev: USB device 1-1 failed" || n[0].Body != "device unplugged" {
		t.Errorf("USB failure = %+v", n)
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

func TestPVCMenu(t *testing.T) {
	st := api.Status{Hosts: []api.HostStatus{
		{Name: "jump", SSH: "jump", State: api.StateUp,
			Mounts: []api.MountStatus{{
				Key: "pvc:prod/db/data -> /mnt/pg", Direction: "pvc-to-local", Remote: "pvc:prod/db/data", Local: "/mnt/pg", AdHoc: true, State: api.StateUp,
				Kube: &api.KubeMountStatus{KubeMount: api.KubeMount{Context: "prod", Namespace: "db", PVC: "data"}, Pod: "tether-data-x", Node: "n1"},
			}},
			RecentPVCs: []api.RecentPVC{
				{KubeMount: api.KubeMount{Context: "prod", Namespace: "db", PVC: "data"}, Local: "/mnt/pg"}, // mounted now: left out
				{KubeMount: api.KubeMount{Context: "prod", Namespace: "web", PVC: "uploads", ReadOnly: true}, Local: "/mnt/up"},
			}},
		{Name: "local", Local: true, State: api.StateDown},
	}}
	items := Build(st)

	if it := find(items, "host:jump:pvc"); it == nil || *it.Action != (Action{Local: localPickPVC, Host: "jump"}) {
		t.Errorf("pick claim = %+v", it)
	}
	unmount := api.MountParams{Host: "jump", Direction: "pvc-to-local", Local: "/mnt/pg", Kube: &api.KubeMount{Context: "prod", Namespace: "db", PVC: "data"}}
	if it := find(items, "host:jump:mount:pvc:prod/db/data -> /mnt/pg:rm"); it == nil || !reflect.DeepEqual(it.Action.Params, unmount) {
		t.Errorf("unmount claim = %+v", it)
	}
	recent := find(items, "host:jump:recent")
	if recent == nil || len(recent.Children) != 1 {
		t.Fatalf("recent claims = %+v", recent)
	}
	it := recent.Children[0]
	want := api.MountParams{Host: "jump", Direction: "pvc-to-local", Local: "/mnt/up", Options: []string{"ro"},
		Kube: &api.KubeMount{Context: "prod", Namespace: "web", PVC: "uploads", ReadOnly: true}}
	if it.Title != "prod/web/uploads → /mnt/up (read-only)" || it.Action.Method != api.MethodMountAdd || !reflect.DeepEqual(it.Action.Params, want) {
		t.Errorf("recent claim = %+v %+v", it, it.Action)
	}

	// The local host only has claims.
	if find(items, "host:local:pvc") == nil || find(items, "host:local:gpg") != nil || find(items, "host:local:mount-here") != nil {
		t.Errorf("local host menu = %+v", find(items, "host:local"))
	}
	if find(items, "host:local:recent") != nil {
		t.Error("recent claims submenu without any")
	}
}

func TestSummaryIgnoresIdleLocalHost(t *testing.T) {
	st := api.Status{Hosts: []api.HostStatus{{Name: "dev", State: api.StateUp}, {Name: "local", Local: true, State: api.StateDown}}}
	if look, text := Summary(st); look != LookUp || text != "1 host: 1 up" {
		t.Errorf("summary = %s %q", look, text)
	}
	st.Hosts[1].State = api.StateUp
	if look, text := Summary(st); look != LookUp || text != "2 hosts: 2 up" {
		t.Errorf("summary with local in use = %s %q", look, text)
	}
}

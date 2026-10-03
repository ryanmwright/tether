package tray

import (
	"bufio"
	"bytes"
	"fmt"
	"image/png"
	"os/exec"
	"reflect"
	"slices"
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
	if it := find(items, "host:lab:add:mount-here"); it == nil || *it.Action != (Action{Local: localAdd, Host: "lab", Arg: addMountHere}) {
		t.Errorf("lab mount here = %+v", it)
	}
	if it := find(items, "host:lab:add:mount-there"); it == nil || *it.Action != (Action{Local: localAdd, Host: "lab", Arg: addMountThere}) {
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

// TestUniqueIDs checks that no two entries share an ID: the menu keeps an
// entry's place by it.
func TestUniqueIDs(t *testing.T) {
	st := sample
	st.Hosts = append([]api.HostStatus{}, sample.Hosts...)
	st.Hosts[0].Forwards = append(st.Hosts[0].Forwards, api.ForwardStatus{Spec: "D:1080", AdHoc: true, State: api.StateUp, Address: "127.0.0.1:1080", UsedBy: "office"})
	st.Hosts[0].USB = []api.USBStatus{{Device: "1-1", BusID: "1-1", AdHoc: true, State: api.StateUp}}
	st.Hosts[0].RecentForwards = []api.RecentForward{{Spec: "8080"}}
	st.USB = []api.USBDevice{{BusID: "1-1", ID: "1050:0407", Host: "dev"}, {BusID: "1-2", ID: "046d:c52b"}}
	st.ConfigError = "bad"
	for name, items := range map[string][]Item{"status": Build(st), "daemon gone": DisconnectedMenu()} {
		seen := map[string]bool{}
		var walk func([]Item)
		walk = func(items []Item) {
			for _, it := range items {
				if seen[it.ID] {
					t.Errorf("%s: ID %q used twice", name, it.ID)
				}
				seen[it.ID] = true
				walk(it.Children)
			}
		}
		walk(items)
	}
}

func TestChanges(t *testing.T) {
	host := func(name string, state api.State, errMsg string) api.HostStatus {
		return api.HostStatus{Name: name, State: state, Error: errMsg}
	}
	st := func(hs ...api.HostStatus) api.Status { return api.Status{Hosts: hs} }

	// Hosts' connections are connAlerts' job.
	if n := Changes(st(host("dev", api.StateUp, "")), st(host("dev", api.StateError, "Broken pipe"))); len(n) != 0 {
		t.Errorf("connection lost notified by Changes: %+v", n)
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

func TestConnAlerts(t *testing.T) {
	shown := make(chan Notice, 10)
	c := newConnAlerts(func(n Notice) { shown <- n })
	c.first, c.again = 300*time.Millisecond, 100*time.Millisecond
	st := func(state api.State, errMsg string) api.Status {
		return api.Status{Hosts: []api.HostStatus{{Name: "vm", State: state, Error: errMsg}}}
	}
	expect := func(what string, wait time.Duration, summary string) {
		t.Helper()
		select {
		case n := <-shown:
			if summary == "" || n.Summary != summary {
				t.Errorf("%s: got %+v, want %q", what, n, summary)
			}
		case <-time.After(wait):
			if summary != "" {
				t.Errorf("%s: nothing shown, want %q", what, summary)
			}
		}
	}

	// Booting along with this machine: failed attempts, then connected,
	// within the grace period, is nothing to report.
	for range 3 {
		c.update(st(api.StatePending, ""))
		c.update(st(api.StateError, "No route to host"))
		time.Sleep(50 * time.Millisecond)
	}
	c.update(st(api.StateUp, ""))
	expect("connected during the grace period", 400*time.Millisecond, "")

	// A dropped connection that comes back quickly isn't either.
	c.update(st(api.StateError, "connection lost: Broken pipe"))
	c.update(st(api.StatePending, ""))
	c.update(st(api.StateUp, ""))
	expect("quick reconnect", 200*time.Millisecond, "")

	// One that stays down is, once, with the latest error; then its return.
	c.update(st(api.StateError, "connection lost: Broken pipe"))
	c.update(st(api.StatePending, ""))
	c.update(st(api.StateError, "Connection refused"))
	select {
	case n := <-shown:
		if n.Summary != "vm: connection lost" || !strings.Contains(n.Body, "Connection refused") || !n.Urgent {
			t.Errorf("lost = %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("lost connection not reported")
	}
	c.update(st(api.StatePending, ""))
	c.update(st(api.StateError, "Connection refused"))
	expect("still down", 200*time.Millisecond, "")
	c.update(st(api.StateUp, ""))
	expect("back", 100*time.Millisecond, "vm: connected again")

	// Disconnecting on purpose cancels a pending report, and isn't news.
	c.update(st(api.StateError, "connection lost: Broken pipe"))
	c.update(st(api.StateDown, ""))
	expect("disconnected", 200*time.Millisecond, "")

	// Never connected: the longer grace period.
	c.update(st(api.StateError, "No route to host"))
	expect("first connect, before its grace period", 200*time.Millisecond, "")
	expect("first connect", 300*time.Millisecond, "vm: not connected")

	// The daemon going away forgets hosts, reported or not.
	c.update(api.Status{})
	c.update(st(api.StateUp, ""))
	expect("after the daemon restarted", 100*time.Millisecond, "")
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
		w, h, argb := IconPixmap(state)
		if px := argb[4*(32*w+7):][:4]; w != 64 || h != 64 || len(argb) != 4*w*h || px[0] == 0 || px[1] != want.R || px[2] != want.G || px[3] != want.B {
			t.Errorf("%s: pixmap %dx%d, ring pixel %v", state, w, h, px)
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
	actions       []string
}

func (f *fakeNotifications) Notify(app string, replaces uint32, icon, summary, body string, actions []string, hints map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, _ := hints["urgency"].Value().(byte)
	f.calls = append(f.calls, notifyCall{replaces, summary, body, u, actions})
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

	clicked := make(chan *Action, 1)
	n, err := NewNotifier(func(a *Action) { clicked <- a })
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	// A notice with a button runs its action when the button is clicked.
	claim := gpgClaimAction("dev")
	if err := n.Show(Notice{Key: "host:dev:gpg", Summary: "gpg on dev: in use by office", Button: "Use this machine's keys", Action: claim}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	got := fake.calls[0]
	fake.calls = nil
	fake.mu.Unlock()
	if !slices.Contains(got.actions, "Use this machine's keys") {
		t.Errorf("button not offered: %+v", got)
	}
	if err := conn.Emit("/org/freedesktop/Notifications", "org.freedesktop.Notifications.ActionInvoked", uint32(1), notificationAction); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-clicked:
		if a != claim {
			t.Errorf("clicked ran %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("clicking the button did nothing")
	}

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
			if calls[0].replaces != 0 || calls[0].urgency != 2 || len(calls[0].actions) != 0 {
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

	if it := find(items, "host:jump:add:pvc"); it == nil || *it.Action != (Action{Local: localAdd, Host: "jump", Arg: addPVC}) {
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
	if it.Title != "Mount prod/web/uploads → /mnt/up (read-only)" || it.Action.Method != api.MethodMountAdd || !reflect.DeepEqual(it.Action.Params, want) {
		t.Errorf("recent claim = %+v %+v", it, it.Action)
	}

	// The local host only has claims.
	if find(items, "host:local:add:pvc") == nil || find(items, "host:local:add:k8s") == nil || find(items, "host:local:gpg") != nil ||
		find(items, "host:local:add:mount-here") != nil || find(items, "host:local:add:socks") != nil {
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

func TestAddAndForwardMenus(t *testing.T) {
	st := api.Status{
		Hosts: []api.HostStatus{{Name: "dev", State: api.StateUp,
			Forwards: []api.ForwardStatus{
				{Spec: "L:5432:db:5432", Label: "pg", AdHoc: true, State: api.StateUp, Address: "localhost:5432",
					Description: "localhost:5432 here → db:5432, reached from dev", Target: "unreachable", TargetError: "nothing is answering at db:5432"},
				{Spec: "D:1080", Profiles: []string{"work"}, State: api.StateUp, Address: "localhost:1080"},
				{Spec: "R:8080:localhost:3000", AdHoc: true, State: api.StateUp},
			},
			RecentForwards: []api.RecentForward{{Spec: "L:5432:db:5432", Label: "pg"}, {Spec: "H:8080", Label: "web"}},
		}},
		USB: []api.USBDevice{{BusID: "1-2", ID: "1050:0407", Name: "YubiKey"}, {BusID: "1-3", ID: "aaaa:bbbb", Host: "dev"}},
	}
	items := Build(st)

	// Every kind of thing to add, named for the host.
	add := find(items, "host:dev:add")
	if add == nil {
		t.Fatal("no Add submenu")
	}
	for id, title := range map[string]string{
		"host:dev:add:local":  "Forward a local port to a port on dev…",
		"host:dev:add:rsocks": "SOCKS proxy on dev, connecting out from here…",
		"host:dev:add:k8s":    "Kubernetes service or pod…",
		"host:dev:add:tui":    "More in the terminal UI…",
	} {
		if it := find(items, id); it == nil || it.Title != title {
			t.Errorf("%s = %+v", id, it)
		}
	}
	if it := find(items, "host:dev:add:socks"); *it.Action != (Action{Local: localAdd, Host: "dev", Arg: addSOCKS}) {
		t.Errorf("socks action = %+v", it.Action)
	}
	// USB devices not shared elsewhere, one click each.
	share := find(items, "host:dev:add:usb")
	if share == nil || len(share.Children) != 1 || share.Children[0].Action.Params != (api.USBParams{Host: "dev", Device: "1-2"}) {
		t.Errorf("share USB = %+v", share)
	}

	// A forward says what it does, and can be copied, opened and removed.
	pg := find(items, "host:dev:fwd:L:5432:db:5432")
	if pg == nil || !strings.Contains(pg.Title, "pg (L:5432:db:5432)") || !strings.Contains(pg.Title, "target unreachable") {
		t.Fatalf("forward = %+v", pg)
	}
	for id, want := range map[string]string{
		":what":   "localhost:5432 here → db:5432, reached from dev",
		":target": "⚠ nothing is answering at db:5432",
		":copy":   "Copy address (localhost:5432)",
		":open":   "Open in browser",
		":rm":     "Remove",
	} {
		if it := find(items, pg.ID+id); it == nil || it.Title != want {
			t.Errorf("%s = %+v", id, it)
		}
	}
	if it := find(items, pg.ID+":copy"); *it.Action != (Action{Local: localCopy, Arg: "localhost:5432"}) {
		t.Errorf("copy action = %+v", it.Action)
	}
	// Profile forwards can't be removed on their own; SOCKS isn't a web page.
	if find(items, "host:dev:fwd:D:1080:rm") != nil || find(items, "host:dev:fwd:D:1080:open") != nil {
		t.Error("profile SOCKS forward has remove or open")
	}

	// Recent forwards not there now, one click to add again.
	recent := find(items, "host:dev:recent")
	if recent == nil || len(recent.Children) != 1 {
		t.Fatalf("recent = %+v", recent)
	}
	if it := recent.Children[0]; it.Title != "Forward web (H:8080)" || it.Action.Params != (api.ForwardParams{Host: "dev", Spec: "web=H:8080"}) {
		t.Errorf("recent forward = %+v %+v", it, it.Action)
	}
}

func TestGPGStandbyMenu(t *testing.T) {
	gpgFwd := func(usedBy string) api.Status {
		return api.Status{Hosts: []api.HostStatus{{Name: "dev", SSH: "devbox", State: api.StateUp, Forwards: []api.ForwardStatus{
			{Spec: "gpg-agent", AdHoc: true, State: api.StateUp, UsedBy: usedBy},
		}}}}
	}
	mine, standby := gpgFwd(""), gpgFwd("office")

	items := Build(standby)
	top := find(items, "gpg-claim:dev")
	if top == nil || !strings.Contains(top.Title, "in use by office") || top.Action == nil || top.Action.Method != api.MethodGPGClaim {
		t.Fatalf("top-level claim item = %+v", top)
	}
	if p, _ := top.Action.Params.(api.GPGClaimParams); p.Host != "dev" {
		t.Errorf("claims %+v", top.Action.Params)
	}
	if it := find(items, "host:dev:gpg-claim"); it == nil || it.Action == nil {
		t.Errorf("host submenu claim item = %+v", it)
	}
	if it := find(items, "host:dev:fwd:gpg-agent"); it == nil || !strings.Contains(it.Title, "standing by, in use by office") {
		t.Errorf("forward row = %+v", it)
	}
	if _, text := Summary(standby); !strings.Contains(text, "gpg on dev in use by office") {
		t.Errorf("summary = %q", text)
	}
	items = Build(mine)
	if find(items, "gpg-claim:dev") != nil || find(items, "host:dev:gpg-claim") != nil {
		t.Error("claim offered while this machine's keys are in use")
	}

	n := Changes(mine, standby)
	if len(n) != 1 || n[0].Action == nil || n[0].Action.Method != api.MethodGPGClaim || !strings.Contains(n[0].Summary, "in use by office") {
		t.Fatalf("taken over: %+v", n)
	}
	if n := Changes(standby, mine); len(n) != 1 || n[0].Action != nil || !strings.Contains(n[0].Body, "office") {
		t.Errorf("taken back: %+v", n)
	}
	// Turning gpg off isn't news, whoever had it.
	off := api.Status{Hosts: []api.HostStatus{{Name: "dev", SSH: "devbox", State: api.StateUp}}}
	if n := Changes(standby, off); len(n) != 0 {
		t.Errorf("gpg turned off: %+v", n)
	}

	// The tray doesn't confirm a claim the user just made from it.
	tr := &tray{claimed: map[string]time.Time{}}
	back := Changes(standby, mine)[0]
	if tr.justClaimed(back) {
		t.Error("unasked-for claim suppressed")
	}
	tr.claimed["dev"] = time.Now()
	if !tr.justClaimed(back) || tr.justClaimed(n[0]) {
		t.Error("claim from the tray not suppressed, or the takeover notice was")
	}
}

package tui

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/rpc"
)

type call struct {
	method string
	params any
}

type fakeClient struct {
	mu    sync.Mutex
	calls []call
	notes chan rpc.Notification
	err   error
}

func (f *fakeClient) Call(_ context.Context, method string, params, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{method, params})
	if method == api.MethodDoctor {
		*result.(*api.DoctorResult) = api.DoctorResult{Host: "dev", Checks: []api.Check{
			{Section: "local", Name: "ssh", Status: api.CheckOK, Detail: "OpenSSH_10"},
			{Section: "remote", Name: "public keys", Status: api.CheckWarn, Detail: "1 of 1 missing", Fix: "gpg --export X | ssh dev gpg --import"},
		}}
	}
	if method == api.MethodKubeList && f.err == nil {
		kctx := params.(api.KubeListParams).Context
		if kctx == "" {
			kctx = "prod"
		}
		*result.(*api.KubeListResult) = api.KubeListResult{
			Host: "dev", Context: kctx, Current: "prod", Contexts: []string{"prod", "staging"}, MountRoot: "/home/me/mnt/k8s",
			PVCs: []api.PVC{
				{Namespace: "db", Name: "data-pg-0", Phase: "Bound", Capacity: "10Gi", StorageClass: "fast", AccessModes: []string{"RWO"},
					UsedBy: []api.PVCConsumer{{Pod: "pg-0", Node: "n2"}}, Mountable: true, Node: "n2", Note: "in use by pg-0; helper runs on node n2"},
				{Namespace: "db", Name: "exclusive", Phase: "Bound", AccessModes: []string{"RWOP"}, Note: "in exclusive use (ReadWriteOncePod) by solo"},
				{Namespace: "web", Name: "uploads", Phase: "Pending", Request: "5Gi", StorageClass: "local", Mountable: true, Note: "not bound yet; mounting provisions its volume"},
			},
		}
	}
	return f.err
}

func (f *fakeClient) Notifications() <-chan rpc.Notification { return f.notes }

func (f *fakeClient) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return call{}
	}
	return f.calls[len(f.calls)-1]
}

var sampleStatus = api.Status{
	Version: "test", PID: 42, StartedAt: time.Now(), ConfigPath: "/cfg.toml",
	Hosts: []api.HostStatus{
		{Name: "dev", SSH: "devbox", State: api.StateUp, Forwards: []api.ForwardStatus{
			{Spec: "L:5432:db:5432", Profiles: []string{"work"}, State: api.StateUp},
			{Spec: "D:1080", AdHoc: true, State: api.StateError, Error: "local port 1080 is unavailable"},
			{Spec: "gpg-agent", AdHoc: true, State: api.StateUp, Resolved: "R:/run/user/1000/gnupg/S.gpg-agent:/local/S.gpg-agent.extra"},
		}, Mounts: []api.MountStatus{
			{Key: "remote:~/src -> /home/me/mnt/src", Direction: "remote-to-local", Remote: "~/src", Local: "/home/me/mnt/src", AdHoc: true, State: api.StateUp},
			{Key: "/home/me/proj -> remote:~/proj", Direction: "local-to-remote", Remote: "~/proj", Local: "/home/me/proj", Profiles: []string{"work"}, State: api.StateError, Error: "sshfs not found on the remote"},
		}},
		{Name: "lab", SSH: "lab", State: api.StateDown, Forwards: []api.ForwardStatus{}},
		{Name: "scratch", SSH: "me@10.0.0.5", AdHoc: true, State: api.StateDown},
	},
	Profiles: []api.ProfileStatus{
		{Name: "work", Host: "dev", Active: true, State: api.StateUp},
		{Name: "old", Host: "lab", State: api.StateDown},
	},
}

func newModel(t *testing.T) (Model, *fakeClient) {
	t.Helper()
	fc := &fakeClient{notes: make(chan rpc.Notification)}
	m := New(fc, []api.LogEntry{{Time: time.Now(), Level: "INFO", Message: "daemon started"}})
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = update(t, m, statusMsg(sampleStatus))
	return m, fc
}

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

// press sends a key and runs the command it returns, feeding the result
// back, like the real program would. Commands that don't finish promptly
// (cursor blink timers) are skipped; daemon calls return at once here.
func press(t *testing.T, m Model, key string) Model {
	t.Helper()
	next, cmd := m.Update(keyMsg(key))
	m = next.(Model)
	if cmd == nil {
		return m
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if _, isBatch := msg.(tea.BatchMsg); msg != nil && !isBatch {
			m = update(t, m, msg)
		}
	case <-time.After(50 * time.Millisecond):
	}
	return m
}

func keyMsg(key string) tea.KeyPressMsg {
	switch key {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "ctrl+o":
		return tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
	}
	r := []rune(key)[0]
	return tea.KeyPressMsg{Code: r, Text: key}
}

func screen(m Model) string { return ansi.Strip(m.View().Content) }

// moveTo moves the cursor, from the top, to the row named text.
func moveTo(t *testing.T, m Model, text string) Model {
	t.Helper()
	m.cursor = 0
	for range len(m.rows) {
		r, _ := m.selected()
		name := r.name
		if r.kind == rowHost {
			name = r.host
		}
		if name == text {
			return m
		}
		m = press(t, m, "down")
	}
	t.Fatalf("no row %q", text)
	return m
}

func TestRender(t *testing.T) {
	m, _ := newModel(t)
	s := screen(m)
	for _, want := range []string{
		"tether", "pid 42", "HOSTS", "dev", "ssh devbox", "L:5432:db:5432", "work",
		"D:1080", "local port 1080 is unavailable", "lab", "PROFILES", "on dev", "LOG", "daemon started",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("screen missing %q:\n%s", want, s)
		}
	}
	if lines := strings.Split(m.View().Content, "\n"); len(lines) != 40 {
		t.Errorf("screen has %d lines, want the window height 40", len(lines))
	}
	for _, l := range strings.Split(s, "\n") {
		if ansi.StringWidth(l) > 120 {
			t.Errorf("line wider than the window: %q", l)
		}
	}
}

func TestSelectionSurvivesUpdates(t *testing.T) {
	m, _ := newModel(t)
	m = moveTo(t, m, "lab")
	// A forward disappears above the selection.
	st := sampleStatus
	st.Hosts = []api.HostStatus{sampleStatus.Hosts[0], sampleStatus.Hosts[1]}
	st.Hosts[0].Forwards = st.Hosts[0].Forwards[:1]
	m = update(t, m, statusMsg(st))
	if r, _ := m.selected(); r.kind != rowHost || r.host != "lab" {
		t.Errorf("selection moved to %+v", r)
	}
}

func TestActions(t *testing.T) {
	tests := []struct {
		row, key string
		want     call
	}{
		{"lab", "enter", call{api.MethodUp, api.TargetParams{Name: "lab", Kind: api.TargetHost}}},
		{"dev", "enter", call{api.MethodDown, api.TargetParams{Name: "dev", Kind: api.TargetHost}}},
		{"dev", "u", call{api.MethodUp, api.TargetParams{Name: "dev", Kind: api.TargetHost}}},
		{"work", "enter", call{api.MethodDown, api.TargetParams{Name: "work", Kind: api.TargetProfile}}},
		{"old", "enter", call{api.MethodUp, api.TargetParams{Name: "old", Kind: api.TargetProfile}}},
		{"D:1080", "x", call{api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: "D:1080"}}},
		{"D:1080", "enter", call{api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: "D:1080"}}},
		{"dev", "g", call{api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: "gpg-agent"}}},
		{"lab", "g", call{api.MethodForwardAdd, api.ForwardParams{Host: "lab", Spec: "gpg-agent"}}},
		{"lab", "r", call{api.MethodReload, nil}},
		{"scratch", "x", call{api.MethodHostRemove, api.HostParams{Name: "scratch"}}},
		{"lab", "x", call{api.MethodDown, api.TargetParams{Name: "lab", Kind: api.TargetHost}}},
		{"remote:~/src -> /home/me/mnt/src", "x", call{api.MethodMountRemove, api.MountParams{Host: "dev", Direction: "remote-to-local", Remote: "~/src", Local: "/home/me/mnt/src"}}},
	}
	for _, tt := range tests {
		t.Run(tt.row+"/"+tt.key, func(t *testing.T) {
			m, fc := newModel(t)
			m = press(t, moveTo(t, m, tt.row), tt.key)
			if got := fc.last(); got.method != tt.want.method || !reflect.DeepEqual(got.params, tt.want.params) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if m.flash == "" || m.flashErr {
				t.Errorf("flash = %q (err %v)", m.flash, m.flashErr)
			}
		})
	}
}

func TestProfileForwardCantBeRemoved(t *testing.T) {
	m, fc := newModel(t)
	m = press(t, moveTo(t, m, "L:5432:db:5432"), "x")
	if len(fc.calls) != 0 {
		t.Errorf("unexpected call %+v", fc.calls)
	}
	if !m.flashErr || !strings.Contains(m.flash, "profile work") {
		t.Errorf("flash = %q", m.flash)
	}
}

func TestActionError(t *testing.T) {
	m, fc := newModel(t)
	fc.err = &rpc.Error{Code: api.CodeNotFound, Message: "no host named \"lab\""}
	m = press(t, moveTo(t, m, "lab"), "enter")
	if !m.flashErr || !strings.Contains(screen(m), `no host named "lab"`) {
		t.Errorf("error not shown:\n%s", screen(m))
	}
}

func TestAddForward(t *testing.T) {
	m, fc := newModel(t)
	m = press(t, moveTo(t, m, "D:1080"), "a")
	if m.mode != modeInput || m.inputHost != "dev" {
		t.Fatalf("mode %v host %q", m.mode, m.inputHost)
	}
	if !strings.Contains(screen(m), "add a forward to dev") {
		t.Errorf("prompt not shown:\n%s", screen(m))
	}
	for _, r := range "L:8080:localhost:80" {
		m = press(t, m, string(r))
	}
	// Keys typed into the prompt aren't actions.
	if len(fc.calls) != 0 {
		t.Fatalf("typing triggered calls: %+v", fc.calls)
	}
	m = press(t, m, "enter")
	want := call{api.MethodForwardAdd, api.ForwardParams{Host: "dev", Spec: "L:8080:localhost:80"}}
	if got := fc.last(); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if m.mode != modeNormal {
		t.Errorf("still in input mode")
	}

	// esc cancels without a call.
	m = press(t, press(t, m, "a"), "esc")
	if m.mode != modeNormal || len(fc.calls) != 1 {
		t.Errorf("esc: mode %v calls %d", m.mode, len(fc.calls))
	}
}

func TestDoctorView(t *testing.T) {
	m, fc := newModel(t)
	m = press(t, moveTo(t, m, "dev"), "d")
	if got := fc.last(); got.method != api.MethodDoctor || got.params != (api.DoctorParams{Host: "dev"}) {
		t.Errorf("call = %+v", got)
	}
	s := screen(m)
	for _, want := range []string{"DOCTOR dev", "OpenSSH_10", "WARN", "public keys", "fix: gpg --export X"} {
		if !strings.Contains(s, want) {
			t.Errorf("doctor view missing %q:\n%s", want, s)
		}
	}
	if m = press(t, m, "esc"); m.mode != modeNormal {
		t.Errorf("esc didn't close doctor")
	}
}

func TestEvents(t *testing.T) {
	m, fc := newModel(t)
	go func() {
		b, _ := json.Marshal(api.LogEntry{Time: time.Now(), Level: "WARN", Message: "connection lost"})
		fc.notes <- rpc.Notification{Method: api.EventLog, Params: b}
		close(fc.notes)
	}()
	msg := waitEvent(fc)()
	m = update(t, m, msg)
	if !strings.Contains(screen(m), "connection lost") {
		t.Errorf("log entry not shown:\n%s", screen(m))
	}
	m = update(t, m, waitEvent(fc)())
	if !strings.Contains(screen(m), "lost connection to the daemon") {
		t.Errorf("disconnect not shown:\n%s", screen(m))
	}
}

func TestHelpAndLogToggle(t *testing.T) {
	m, _ := newModel(t)
	if m = press(t, m, "l"); strings.Contains(screen(m), "LOG") {
		t.Error("log pane still shown after l")
	}
	m = press(t, m, "?")
	if !strings.Contains(screen(m), "KEYS") {
		t.Errorf("help not shown:\n%s", screen(m))
	}
	if m = press(t, m, "j"); m.mode != modeNormal {
		t.Error("any key should close help")
	}
}

func TestScrollKeepsCursorVisible(t *testing.T) {
	m, _ := newModel(t)
	m = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 12})
	m = moveTo(t, m, "old") // last row
	if !strings.Contains(screen(m), "▸") {
		t.Errorf("cursor scrolled out of view:\n%s", screen(m))
	}
}

func TestMounts(t *testing.T) {
	m, fc := newModel(t)
	s := screen(m)
	for _, want := range []string{"remote:~/src -> /home/me/mnt/src", "ad-hoc", "/home/me/proj -> remote:~/proj", "sshfs not found on the remote"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen missing %q:\n%s", want, s)
		}
	}

	// Profile mounts can't be removed on their own.
	m = press(t, moveTo(t, m, "/home/me/proj -> remote:~/proj"), "x")
	if len(fc.calls) != 0 || !strings.Contains(m.flash, "profile work") {
		t.Errorf("calls %+v, flash %q", fc.calls, m.flash)
	}

	m = press(t, moveTo(t, m, "dev"), "m")
	if !strings.Contains(screen(m), "add a mount on dev") {
		t.Fatalf("mount prompt not shown:\n%s", screen(m))
	}
	for _, r := range "remote:/srv/data /mnt/data" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")
	want := call{api.MethodMountAdd, api.MountParams{Host: "dev", Direction: "remote-to-local", Remote: "/srv/data", Local: "/mnt/data"}}
	if got := fc.last(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	m = press(t, m, "m")
	for _, r := range "/a /b" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")
	if !m.flashErr || !strings.Contains(m.flash, "remote:") {
		t.Errorf("bad mount input: flash %q", m.flash)
	}
}

func TestConnectPrompt(t *testing.T) {
	m, fc := newModel(t)
	m = press(t, m, "c")
	if !strings.Contains(screen(m), "connect to a host that isn't in the config") {
		t.Fatalf("connect prompt not shown:\n%s", screen(m))
	}
	for _, r := range "box me@10.0.0.9" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")
	want := call{api.MethodHostAdd, api.HostParams{Name: "box", SSH: "me@10.0.0.9"}}
	if got := fc.last(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// Works with nothing configured, too.
	empty := New(fc, nil)
	empty = update(t, empty, statusMsg(api.Status{Version: "test"}))
	if !strings.Contains(screen(empty), "press c to connect") {
		t.Errorf("empty screen:\n%s", screen(empty))
	}
	if empty = press(t, empty, "c"); empty.mode != modeInput {
		t.Error("c doesn't open the prompt when there are no hosts")
	}
}

func TestEnterHint(t *testing.T) {
	m, _ := newModel(t)
	for row, want := range map[string]string{
		"dev": "enter disconnect", "lab": "enter connect", "work": "enter deactivate",
		"old": "enter activate", "D:1080": "enter remove", "L:5432:db:5432": "enter —",
		"remote:~/src -> /home/me/mnt/src": "enter unmount",
	} {
		if s := screen(moveTo(t, m, row)); !strings.Contains(s, want+" ·") {
			t.Errorf("%s: footer lacks %q", row, want)
		}
	}
	if !strings.Contains(screen(moveTo(t, m, "scratch")), "ad-hoc") {
		t.Error("ad-hoc host not marked")
	}
}

func TestUSB(t *testing.T) {
	m, fc := newModel(t)
	st := sampleStatus
	st.Hosts = append([]api.HostStatus{}, sampleStatus.Hosts...)
	st.Hosts[0].USB = []api.USBStatus{
		{Device: "1050:0407", BusID: "1-1", Name: "Yubico YubiKey", AdHoc: true, State: api.StateUp},
		{Device: "0627:0001", Profiles: []string{"work"}, State: api.StateError, Error: "no USB device 0627:0001 plugged in"},
	}
	st.USB = []api.USBDevice{
		{BusID: "1-1", ID: "1050:0407", Name: "Yubico YubiKey", Host: "dev"},
		{BusID: "1-2", ID: "046d:c52b", Name: "Logitech Receiver"},
	}
	st.USBUnavailable = "the USB/IP helper isn't running"
	m = update(t, m, statusMsg(st))

	s := screen(m)
	for _, want := range []string{"USB DEVICES", "usb 1050:0407", "no USB device 0627:0001 plugged in",
		"Logitech Receiver (046d:c52b)", "Yubico YubiKey (1050:0407) · on dev", "⚠ the USB/IP helper isn't running"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen missing %q:\n%s", want, s)
		}
	}

	// Enter on a shared device stops sharing it, from either row.
	for _, row := range []string{"1-1", "1050:0407"} {
		m = press(t, moveTo(t, m, row), "enter")
		want := call{api.MethodUSBDetach, api.USBParams{Host: "dev", Device: "1050:0407"}}
		if got := fc.last(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v", row, got, want)
		}
	}
	// A profile's device stays.
	n := len(fc.calls)
	m = press(t, moveTo(t, m, "0627:0001"), "x")
	if len(fc.calls) != n || !strings.Contains(m.flash, "profile work") {
		t.Errorf("profile device: calls %+v, flash %q", fc.calls[n:], m.flash)
	}

	// With one host connected, enter shares with it...
	m = press(t, moveTo(t, m, "1-2"), "enter")
	if got, want := fc.last(), (call{api.MethodUSBAttach, api.USBParams{Host: "dev", Device: "1-2"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("share: got %+v, want %+v", got, want)
	}
	// ...with several, it asks which.
	st.Hosts[1].State = api.StateUp
	m = update(t, m, statusMsg(st))
	m = press(t, moveTo(t, m, "1-2"), "enter")
	if !strings.Contains(screen(m), "share Logitech Receiver (046d:c52b) with which host?") {
		t.Fatalf("share prompt not shown:\n%s", screen(m))
	}
	for _, r := range "lab" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")
	if got, want := fc.last(), (call{api.MethodUSBAttach, api.USBParams{Host: "lab", Device: "1-2"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("share with lab: got %+v, want %+v", got, want)
	}

	// Host keys do nothing on a local device.
	n = len(fc.calls)
	if m = press(t, moveTo(t, m, "1-2"), "u"); len(fc.calls) != n {
		t.Errorf("u on a device called %+v", fc.calls[n:])
	}
	if s := screen(moveTo(t, m, "1-1")); !strings.Contains(s, "enter stop sharing ·") {
		t.Error("footer lacks the stop-sharing hint")
	}
}

func TestPVCPicker(t *testing.T) {
	m, fc := newModel(t)
	m = press(t, moveTo(t, m, "dev"), "K")
	if m.mode != modePVC {
		t.Fatalf("mode = %v", m.mode)
	}
	if got := fc.last(); got.method != api.MethodKubeList || got.params.(api.KubeListParams).Host != "dev" {
		t.Fatalf("last call = %+v", got)
	}
	s := screen(m)
	for _, want := range []string{"CLAIMS on dev · context prod (1 of 2", "db/data-pg-0", "10Gi", "pg-0", "web/uploads", "5Gi", "helper runs on node n2"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen missing %q:\n%s", want, s)
		}
	}

	// Typing filters; claims that can't be mounted say why.
	for _, r := range "excl" {
		m = press(t, m, string(r))
	}
	if s := screen(m); strings.Contains(s, "data-pg-0") || !strings.Contains(s, "can't mount: in exclusive use") {
		t.Errorf("filtered screen:\n%s", s)
	}
	m = press(t, m, "enter")
	if m.mode != modePVC || !m.flashErr {
		t.Errorf("unmountable claim picked: mode %v flash %q", m.mode, m.flash)
	}
	m = press(t, m, "esc") // clears the filter
	if m.mode != modePVC || m.pvc.filter.Value() != "" {
		t.Fatalf("esc with a filter: mode %v filter %q", m.mode, m.pvc.filter.Value())
	}

	// Pick a claim: the mount point prompt offers the default.
	m = press(t, m, "ctrl+o")
	m = press(t, m, "enter")
	if !m.choosingPath() || m.input.Value() != "/home/me/mnt/k8s/prod/db/data-pg-0" {
		t.Fatalf("path prompt: mode %v value %q", m.mode, m.input.Value())
	}
	if s := screen(m); !strings.Contains(s, "mount db/data-pg-0 here at (read-only)") {
		t.Errorf("path prompt title missing:\n%s", s)
	}
	m = press(t, m, "enter")
	want := call{api.MethodMountAdd, api.MountParams{
		Host: "dev", Direction: "pvc-to-local", Local: "/home/me/mnt/k8s/prod/db/data-pg-0", Options: []string{"ro"},
		Kube: &api.KubeMount{Context: "prod", Namespace: "db", PVC: "data-pg-0", ReadOnly: true},
	}}
	if got := fc.last(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if m.mode != modeNormal {
		t.Errorf("mode after mounting = %v", m.mode)
	}

	// Tab moves to the next context.
	m = press(t, moveTo(t, m, "dev"), "K")
	m = press(t, m, "tab")
	if got := fc.last(); got.params.(api.KubeListParams).Context != "staging" || !strings.Contains(screen(m), "context staging (2 of 2") {
		t.Errorf("tab: last call %+v\n%s", got, screen(m))
	}
	m = press(t, m, "esc")
	if m.mode != modeNormal {
		t.Errorf("esc: mode %v", m.mode)
	}
}

func TestPVCMountRow(t *testing.T) {
	m, fc := newModel(t)
	st := sampleStatus
	st.Hosts = append([]api.HostStatus{{Name: "local", Local: true, State: api.StateUp, Mounts: []api.MountStatus{{
		Key: "pvc:prod/db/data -> /mnt/pg", Direction: "pvc-to-local", Remote: "pvc:prod/db/data", Local: "/mnt/pg", AdHoc: true, State: api.StateUp,
		Kube: &api.KubeMountStatus{KubeMount: api.KubeMount{Context: "prod", Namespace: "db", PVC: "data"}, Pod: "tether-data-x1", Node: "n1"},
	}}}}, st.Hosts...)
	m = update(t, m, statusMsg(st))
	if s := screen(m); !strings.Contains(s, "this machine") {
		t.Errorf("local host not shown as this machine:\n%s", s)
	}
	m = moveTo(t, m, "pvc:prod/db/data -> /mnt/pg")
	if s := screen(m); !strings.Contains(s, "helper pod db/tether-data-x1 on node n1") {
		t.Errorf("no pod detail:\n%s", s)
	}
	press(t, m, "x")
	want := call{api.MethodMountRemove, api.MountParams{Host: "local", Direction: "pvc-to-local", Local: "/mnt/pg",
		Kube: &api.KubeMount{Context: "prod", Namespace: "db", PVC: "data"}}}
	if got := fc.last(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

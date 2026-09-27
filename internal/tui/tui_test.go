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

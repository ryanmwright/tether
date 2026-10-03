package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/rpc"
)

// testLogger shows daemon logs in verbose test output.
func testLogger(t *testing.T) *slog.Logger {
	if !testing.Verbose() {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type harness struct {
	socket, config string
	done           chan error
}

// start runs a daemon. Unless sshOpts says otherwise, ssh gets an empty
// config file so tests never touch the user's ~/.ssh/config.
func start(t *testing.T, configTOML string, sshOpts ...openssh.Options) *harness {
	t.Helper()
	return startWith(t, configTOML, func(o *Options) {
		if len(sshOpts) > 0 {
			o.SSH = sshOpts[0]
		}
	})
}

// startWith runs a daemon with options adjusted by opt.
func startWith(t *testing.T, configTOML string, opt func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		socket: filepath.Join(dir, "run", "tether.sock"),
		config: filepath.Join(dir, "config.toml"),
		done:   make(chan error, 1),
	}
	if configTOML != "" {
		h.write(t, configTOML)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Only the USB test talks to a real USB/IP helper.
		helper := cmp.Or(os.Getenv("TETHER_TEST_USBIP_HELPER"), filepath.Join(dir, "no-usbip-helper.sock"))
		// Only the kube tests want the built-in local host.
		noLocal := os.Getenv("TETHER_TEST_LOCAL_HOST") == ""
		o := Options{SocketPath: h.socket, ConfigPath: h.config, Version: "test", Logger: testLogger(t),
			SSH: openssh.Options{ConfigFile: os.DevNull}, USBHelperSocket: helper, NoLocalHost: noLocal}
		opt(&o)
		h.done <- Run(ctx, o)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	h.waitReady(t)
	return h
}

func (h *harness) write(t *testing.T, s string) {
	t.Helper()
	if err := os.WriteFile(h.config, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-h.done:
			t.Fatalf("daemon exited: %v", err)
		default:
		}
		if c, err := rpc.Dial(context.Background(), h.socket); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon never became ready")
}

func (h *harness) client(t *testing.T) *rpc.Client {
	t.Helper()
	c, err := rpc.Dial(context.Background(), h.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestStatusAndReload(t *testing.T) {
	h := start(t, "[hosts.devbox]\nssh = \"devbox.invalid\"\ndisplay_name = \"Dev box\"\n[profiles.work]\nhost = \"devbox\"\n")
	c := h.client(t)
	ctx := context.Background()

	var st api.Status
	if err := c.Call(ctx, api.MethodStatus, nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.PID != os.Getpid() || st.ConfigError != "" {
		t.Errorf("status = %+v", st)
	}
	if len(st.Hosts) != 1 || st.Hosts[0].Name != "devbox" || st.Hosts[0].SSH != "devbox.invalid" || st.Hosts[0].State != api.StateDown ||
		st.Hosts[0].DisplayName != "Dev box" {
		t.Errorf("hosts = %+v", st.Hosts)
	}
	if len(st.Profiles) != 1 || st.Profiles[0].Host != "devbox" {
		t.Errorf("profiles = %+v", st.Profiles)
	}

	// An invalid config is rejected and the previous one kept.
	h.write(t, "[profiles.work]\nhost = \"gone\"\n")
	err := c.Call(ctx, api.MethodReload, nil, nil)
	if rpcErr, ok := errors.AsType[*rpc.Error](err); !ok || rpcErr.Code != api.CodeInvalidConfig {
		t.Fatalf("reload invalid: %v", err)
	}
	st = api.Status{}
	c.Call(ctx, api.MethodStatus, nil, &st)
	if st.ConfigError == "" || len(st.Hosts) != 1 {
		t.Errorf("after invalid reload: %+v", st)
	}

	h.write(t, "[hosts.a]\n[hosts.b]\n")
	var res api.ReloadResult
	if err := c.Call(ctx, api.MethodReload, nil, &res); err != nil || res.Hosts != 2 {
		t.Fatalf("reload valid: %+v %v", res, err)
	}
	st = api.Status{}
	c.Call(ctx, api.MethodStatus, nil, &st)
	if st.ConfigError != "" || len(st.Hosts) != 2 || len(st.Profiles) != 0 {
		t.Errorf("after valid reload: %+v", st)
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	h := start(t, "")
	err := Run(context.Background(), Options{SocketPath: h.socket, ConfigPath: h.config, Logger: slog.New(slog.DiscardHandler)})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("got %v, want ErrAlreadyRunning", err)
	}
	// The running daemon's socket must survive the failed attempt.
	h.client(t)
}

func TestShutdownRemovesSocket(t *testing.T) {
	h := start(t, "")
	c := h.client(t)
	if err := c.Call(context.Background(), api.MethodShutdown, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.done:
		h.done <- err // for cleanup
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, err := os.Stat(h.socket); !os.IsNotExist(err) {
		t.Errorf("socket still exists: %v", err)
	}
}

func TestSocketPermissions(t *testing.T) {
	h := start(t, "")
	fi, err := os.Stat(h.socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

func TestLogs(t *testing.T) {
	h := start(t, "[hosts.devbox]\nssh = \"devbox.invalid\"\n")
	c := h.client(t)
	ctx := context.Background()

	var entries []api.LogEntry
	if err := c.Call(ctx, api.MethodLogs, nil, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[len(entries)-1].Message != "daemon started" {
		t.Fatalf("recent logs = %+v", entries)
	}
	if err := c.Call(ctx, api.MethodLogs, api.LogsParams{Limit: 1}, &entries); err != nil || len(entries) != 1 {
		t.Fatalf("limit 1: %v %+v", err, entries)
	}

	if err := c.Call(ctx, api.MethodSubscribe, api.SubscribeParams{Logs: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, api.MethodReload, nil, nil); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case n := <-c.Notifications():
			if n.Method != api.EventLog {
				continue
			}
			var e api.LogEntry
			json.Unmarshal(n.Params, &e)
			if e.Message == "config loaded" {
				if len(e.Attrs) != 2 || e.Attrs[0] != (api.LogAttr{Key: "hosts", Value: "1"}) {
					t.Errorf("attrs = %+v", e.Attrs)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no log event for the reload")
		}
	}
}

func TestUSBRequests(t *testing.T) {
	h := start(t, "[hosts.devbox]\nssh = \"devbox.invalid\"\n[hosts.lab]\nssh = \"lab.invalid\"\n[profiles.work]\nhost = \"devbox\"\nusb = [\"1050:0407\"]\n")
	c := h.client(t)
	ctx := context.Background()
	code := func(err error) int {
		if rpcErr, ok := errors.AsType[*rpc.Error](err); ok {
			return rpcErr.Code
		}
		return 0
	}

	if err := c.Call(ctx, api.MethodUSBAttach, api.USBParams{Host: "devbox", Device: "yubikey"}, nil); code(err) != rpc.CodeInvalidParams {
		t.Errorf("bad device: %v", err)
	}
	if err := c.Call(ctx, api.MethodUSBAttach, api.USBParams{Host: "nope", Device: "1-1"}, nil); code(err) != api.CodeNotFound {
		t.Errorf("unknown host: %v", err)
	}
	if err := c.Call(ctx, api.MethodUSBDetach, api.USBParams{Host: "devbox", Device: "1-1"}, nil); code(err) != api.CodeNotFound {
		t.Errorf("detach unknown: %v", err)
	}

	var res api.USBResult
	if err := c.Call(ctx, api.MethodUSBAttach, api.USBParams{Host: "devbox", Device: "1050:ABCD"}, &res); err != nil || res.Device != "1050:abcd" {
		t.Fatalf("attach: %+v %v", res, err)
	}
	c.Call(ctx, api.MethodUp, api.TargetParams{Name: "work"}, nil)
	var st api.Status
	c.Call(ctx, api.MethodStatus, nil, &st)
	usb := st.Hosts[0].USB
	if len(usb) != 2 || usb[0].Device != "1050:0407" || len(usb[0].Profiles) != 1 || usb[1].Device != "1050:abcd" || !usb[1].AdHoc {
		t.Errorf("host USB = %+v", usb)
	}
	// This test's daemon has no helper (and in a build sandbox, no USB).
	if os.Getenv("TETHER_TEST_USBIP_HELPER") == "" && st.USBUnavailable == "" {
		t.Errorf("usb_unavailable = %q", st.USBUnavailable)
	}

	// A device is in one place: another host gets it only by moving it.
	usbOn := func(host string) []string {
		var st api.Status
		c.Call(ctx, api.MethodStatus, nil, &st)
		var devs []string
		for _, hs := range st.Hosts {
			if hs.Name == host {
				for _, u := range hs.USB {
					devs = append(devs, u.Device)
				}
			}
		}
		return devs
	}
	if err := c.Call(ctx, api.MethodUSBAttach, api.USBParams{Host: "lab", Device: "1050:abcd"}, nil); code(err) != api.CodeInvalidConfig || !strings.Contains(err.Error(), "already shared with devbox") {
		t.Errorf("second host: %v", err)
	}
	if err := c.Call(ctx, api.MethodUSBAttach, api.USBParams{Host: "lab", Device: "1050:abcd", Move: true}, nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	if on, lab := usbOn("devbox"), usbOn("lab"); slices.Contains(on, "1050:abcd") || !slices.Equal(lab, []string{"1050:abcd"}) {
		t.Errorf("after moving: devbox %v, lab %v", on, lab)
	}
	if err := c.Call(ctx, api.MethodUSBAttach, api.USBParams{Host: "lab", Device: "1050:0407", Move: true}, nil); code(err) != api.CodeInvalidConfig || !strings.Contains(err.Error(), "by profile work") {
		t.Errorf("moving a profile's device: %v", err)
	}
	if err := c.Call(ctx, api.MethodUSBDetach, api.USBParams{Host: "lab", Device: "1050:abcd"}, nil); err != nil {
		t.Fatal(err)
	}
	// Taking the profile down leaves nothing.
	c.Call(ctx, api.MethodDown, api.TargetParams{Name: "work"}, nil)
	st = api.Status{}
	c.Call(ctx, api.MethodStatus, nil, &st)
	if len(usbOn("devbox")) != 0 || len(usbOn("lab")) != 0 {
		t.Errorf("after down: %+v", st.Hosts)
	}
}

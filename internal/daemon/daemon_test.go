package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tether/internal/api"
	"tether/internal/rpc"
)

type harness struct {
	socket, config string
	done           chan error
}

func start(t *testing.T, configTOML string) *harness {
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
		h.done <- Run(ctx, Options{SocketPath: h.socket, ConfigPath: h.config, Version: "test", Logger: slog.New(slog.DiscardHandler)})
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
	h := start(t, "[hosts.devbox]\nautoconnect = true\n[profiles.work]\nhost = \"devbox\"\n")
	c := h.client(t)
	ctx := context.Background()

	var st api.Status
	if err := c.Call(ctx, api.MethodStatus, nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.PID != os.Getpid() || st.ConfigError != "" {
		t.Errorf("status = %+v", st)
	}
	if len(st.Hosts) != 1 || st.Hosts[0] != (api.HostStatus{Name: "devbox", SSH: "devbox", Autoconnect: true, State: api.StateDown}) {
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

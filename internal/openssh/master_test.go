package openssh

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/sshtest"
)

func startMaster(t *testing.T, srv *sshtest.Server) *Master {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := Start(ctx, Options{ConfigFile: srv.ConfigFile}, sshtest.HostAlias, filepath.Join(srv.Dir, "ctl"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m
}

func mustParse(t *testing.T, s string) forward.Spec {
	t.Helper()
	spec, err := forward.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestForwards(t *testing.T) {
	srv := sshtest.Start(t)
	m := startMaster(t, srv)
	ctx := context.Background()
	echo := sshtest.EchoServer(t)

	// Local forward: local port -> (via remote) echo server.
	lport := sshtest.FreePort(t)
	local := mustParse(t, fmt.Sprintf("L:127.0.0.1:%d:127.0.0.1:%d", lport, echo))
	if _, err := m.Forward(ctx, local); err != nil {
		t.Fatal(err)
	}
	if got, err := sshtest.Echo("tcp", fmt.Sprintf("127.0.0.1:%d", lport), "hello"); err != nil || got != "hello" {
		t.Fatalf("through L forward: %q %v", got, err)
	}

	// Remote forward on port 0: server allocates the port.
	remote := mustParse(t, fmt.Sprintf("R:127.0.0.1:0:127.0.0.1:%d", echo))
	port, err := m.Forward(ctx, remote)
	if err != nil || port == 0 {
		t.Fatalf("R forward: port %d, %v", port, err)
	}
	if got, err := sshtest.Echo("tcp", fmt.Sprintf("127.0.0.1:%d", port), "world"); err != nil || got != "world" {
		t.Fatalf("through R forward: %q %v", got, err)
	}

	// Unix socket listener.
	sock := filepath.Join(srv.Dir, "l.sock")
	unix := mustParse(t, fmt.Sprintf("L:%s:127.0.0.1:%d", sock, echo))
	if _, err := m.Forward(ctx, unix); err != nil {
		t.Fatal(err)
	}
	if got, err := sshtest.Echo("unix", sock, "sock"); err != nil || got != "sock" {
		t.Fatalf("through unix forward: %q %v", got, err)
	}

	// Cancel closes the listener; cancelling again is an error even though
	// ssh exits 0.
	if err := m.Cancel(ctx, local); err != nil {
		t.Fatal(err)
	}
	if _, err := sshtest.Echo("tcp", fmt.Sprintf("127.0.0.1:%d", lport), "x"); err == nil {
		t.Error("L forward still open after cancel")
	}
	if err := m.Cancel(ctx, local); err == nil || !strings.Contains(err.Error(), "not forwarded") {
		t.Errorf("second cancel: %v", err)
	}

	if err := m.Probe(ctx); err != nil {
		t.Errorf("probe: %v", err)
	}
}

func TestMasterExitsWhenServerDies(t *testing.T) {
	srv := sshtest.Start(t)
	m := startMaster(t, srv)
	srv.Stop()
	select {
	case <-m.Done():
		if m.Err() == nil {
			t.Error("expected an exit error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("master did not notice the server going away")
	}
}

func TestStartFailure(t *testing.T) {
	srv := sshtest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Authentication must fail fast rather than prompt.
	_, err := Start(ctx, Options{ConfigFile: srv.ConfigFile, ExtraArgs: []string{"-o", "User=tether-no-such-user"}},
		sshtest.HostAlias, filepath.Join(srv.Dir, "ctl"), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("got %v, want permission denied", err)
	}
}

func TestStopRemovesControlSocket(t *testing.T) {
	srv := sshtest.Start(t)
	m := startMaster(t, srv)
	m.Stop()
	if _, err := m.control(context.Background(), "check"); err == nil {
		t.Error("control socket still answering after Stop")
	}
}

// Dev-box entries often set RemoteCommand (e.g. tmux) and ControlMaster auto.
// The master and the probe must work regardless.
func TestUserConfigSessionOptions(t *testing.T) {
	srv := sshtest.Start(t)
	extra := fmt.Sprintf("  RemoteCommand exit 42\n  RequestTTY yes\n  ControlMaster auto\n  ControlPath %s/user-%%C\n  ControlPersist 10m\n", srv.Dir)
	appendFile(t, srv.ConfigFile, extra)
	m := startMaster(t, srv)

	// Probe treats remote command errors as "alive", so also check that the
	// probe's command really runs remotely instead of ssh rejecting it
	// locally ("Cannot execute command-line and remote command").
	if err := m.Probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	out, err := exec.Command("ssh", append(append([]string{"-F", srv.ConfigFile}, probeOptions...), "-S", m.ctl, "--", sshtest.HostAlias, "echo", "ran")...).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ran" {
		t.Fatalf("probe command did not run remotely: %v: %s", err, out)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

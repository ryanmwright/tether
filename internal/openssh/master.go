// Package openssh drives the system OpenSSH client: one ControlMaster
// connection per host, with forwards added and removed through its control
// socket.
package openssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/linelog"
)

type Options struct {
	Binary     string   // ssh executable; default "ssh"
	ConfigFile string   // passed as -F when set
	ExtraArgs  []string // added to every invocation, before the destination
}

func (o Options) binary() string {
	if o.Binary != "" {
		return o.Binary
	}
	return "ssh"
}

func (o Options) baseArgs() []string {
	var args []string
	if o.ConfigFile != "" {
		args = append(args, "-F", o.ConfigFile)
	}
	return append(args, o.ExtraArgs...)
}

// Master is a running `ssh -M` process.
type Master struct {
	opts Options
	dest string
	ctl  string
	log  *slog.Logger

	cmd    *exec.Cmd
	stderr *linelog.Writer
	done   chan struct{}
	err    error // valid after done is closed
}

// masterOptions keep the connection non-interactive and detect dead links.
// ClearAllForwardings drops forwards from the user's ssh_config; forwards
// added later through the control socket are unaffected.
var masterOptions = []string{
	"-M", "-N", "-T",
	"-o", "ControlPersist=no",
	"-o", "ClearAllForwardings=yes",
	"-o", "ExitOnForwardFailure=no",
	"-o", "BatchMode=yes",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
	"-o", "StreamLocalBindUnlink=yes",
}

// Start launches a master for dest with its control socket at ctlPath and
// waits until it is authenticated and accepting control requests.
func Start(ctx context.Context, opts Options, dest, ctlPath string, log *slog.Logger) (*Master, error) {
	m := &Master{opts: opts, dest: dest, ctl: ctlPath, log: log, stderr: linelog.New(log, "ssh output"), done: make(chan struct{})}
	// A failed connect is reported with its last line; don't log it twice.
	m.stderr.Quiet.Store(true)
	m.cleanupStale(ctx)

	args := append(opts.baseArgs(), masterOptions...)
	args = append(args, "-S", ctlPath, "--", dest)
	m.cmd = exec.Command(opts.binary(), args...)
	m.cmd.Stdin = nil
	m.cmd.Stdout = nil
	m.cmd.Stderr = m.stderr
	// Take the connection down with us if the daemon dies.
	m.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := m.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		err := m.cmd.Wait()
		m.err = m.exitError(err)
		close(m.done)
	}()

	if err := m.waitReady(ctx); err != nil {
		m.Stop()
		return nil, err
	}
	m.stderr.Quiet.Store(false)
	return m, nil
}

// cleanupStale removes a control socket left by a previous daemon, asking
// any master still listening on it to exit.
func (m *Master) cleanupStale(ctx context.Context) {
	if _, err := os.Stat(m.ctl); err != nil {
		return
	}
	m.control(ctx, "exit")
	os.Remove(m.ctl)
}

func (m *Master) waitReady(ctx context.Context) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-m.done:
			return m.err
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			// The control socket appears only after authentication.
			if _, err := os.Stat(m.ctl); err != nil {
				continue
			}
			if _, err := m.control(ctx, "check"); err == nil {
				return nil
			}
		}
	}
}

func (m *Master) exitError(err error) error {
	msg := m.stderr.Last()
	if msg == "" {
		if err == nil {
			return errors.New("connection closed")
		}
		return err
	}
	return errors.New(msg)
}

// Done is closed when the master process exits.
func (m *Master) Done() <-chan struct{} { return m.done }

// Err is why the master exited. Only valid after Done is closed.
func (m *Master) Err() error { return m.err }

// Stop terminates the master and waits for it to exit.
func (m *Master) Stop() {
	select {
	case <-m.done:
		return
	default:
	}
	m.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
		m.cmd.Process.Kill()
		<-m.done
	}
	os.Remove(m.ctl)
}

// Forward adds spec to the connection. For a remote forward on port 0 it
// returns the port the server allocated.
//
// ssh reports some failures only on stderr with exit status 0, so any
// "failed" message there is treated as an error.
func (m *Master) Forward(ctx context.Context, spec forward.Spec) (allocatedPort int, err error) {
	out, err := m.control(ctx, "forward", spec.Flag(), spec.Arg())
	if err != nil {
		return 0, err
	}
	if spec.Kind == forward.Remote && !spec.Listen.IsSocket() && spec.Listen.Port == 0 {
		port, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return 0, fmt.Errorf("unexpected reply for allocated port: %q", out)
		}
		return port, nil
	}
	return 0, nil
}

func (m *Master) Cancel(ctx context.Context, spec forward.Spec) error {
	_, err := m.control(ctx, "cancel", spec.Flag(), spec.Arg())
	return err
}

// probeOptions stop settings from the user's ssh_config (RemoteCommand,
// RequestTTY, ControlMaster auto) from rejecting the probe or turning it into
// a new connection.
var probeOptions = []string{
	"-T",
	"-o", "BatchMode=yes",
	"-o", "ControlMaster=no",
	"-o", "RemoteCommand=none",
	"-o", "RequestTTY=no",
}

// Probe runs a trivial remote command over the connection. It fails only if
// the connection doesn't answer within the context's deadline; a remote
// command error (e.g. no shell access) still proves the link is alive.
func (m *Master) Probe(ctx context.Context) error {
	args := append(m.opts.baseArgs(), probeOptions...)
	args = append(args, "-S", m.ctl, "--", m.dest, "true")
	cmd := exec.CommandContext(ctx, m.opts.binary(), args...)
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("no response from %s: %w", m.dest, ctx.Err())
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}

// Run runs command on the remote over the connection, feeding it stdin, and
// returns its standard output. On failure the error carries the first line
// of its standard error.
func (m *Master) Run(ctx context.Context, stdin, command string) ([]byte, error) {
	args := append(m.opts.baseArgs(), probeOptions...)
	args = append(args, "-S", m.ctl, "--", m.dest, command)
	cmd := exec.CommandContext(ctx, m.opts.binary(), args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(firstLine(msg))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// Command returns an unstarted `ssh [opts] dest [remote...]` that runs over
// the connection, e.g. Command([]string{"-s"}, "sftp") for a subsystem. It
// dies with the daemon.
func (m *Master) Command(opts []string, remote ...string) *exec.Cmd {
	args := append(m.opts.baseArgs(), probeOptions...)
	args = append(args, "-S", m.ctl)
	args = append(args, opts...)
	args = append(args, "--", m.dest)
	cmd := exec.Command(m.opts.binary(), append(args, remote...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	return cmd
}

// Dest is the ssh destination this master connects to.
func (m *Master) Dest() string { return m.dest }

// control runs `ssh -S ctl -O op [args] dest`.
func (m *Master) control(ctx context.Context, op string, args ...string) ([]byte, error) {
	full := append(m.opts.baseArgs(), "-S", m.ctl, "-O", op)
	full = append(full, args...)
	full = append(full, "--", m.dest)
	cmd := exec.CommandContext(ctx, m.opts.binary(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	msg := strings.TrimSpace(stderr.String())
	if err != nil || strings.Contains(msg, "failed") {
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(firstLine(msg))
	}
	return stdout.Bytes(), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

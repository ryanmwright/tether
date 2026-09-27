// Package client connects to the tether daemon, starting it if needed.
package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"tether/internal/paths"
	"tether/internal/rpc"
)

const systemdUnit = "tether.service"

type Options struct {
	SocketPath string
	ConfigPath string
	// Autostart launches the daemon when it isn't running: through systemd
	// if the user unit is installed and default paths are in use, otherwise
	// as a detached child process.
	Autostart bool
}

func Connect(ctx context.Context, opts Options) (*rpc.Client, error) {
	c, err := rpc.Dial(ctx, opts.SocketPath)
	if err == nil || !opts.Autostart || !notRunning(err) {
		return c, err
	}
	if err := start(ctx, opts); err != nil {
		return nil, fmt.Errorf("starting daemon: %w", err)
	}
	return rpc.Dial(ctx, opts.SocketPath)
}

func notRunning(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func start(ctx context.Context, opts Options) error {
	if opts.SocketPath == paths.SocketPath() && opts.ConfigPath == paths.ConfigFile() && haveSystemdUnit(ctx) {
		// Type=notify: returns once the daemon is ready.
		out, err := exec.CommandContext(ctx, "systemctl", "--user", "start", systemdUnit).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl --user start %s: %w: %s", systemdUnit, err, out)
		}
		return nil
	}
	return spawn(ctx, opts)
}

func haveSystemdUnit(ctx context.Context) bool {
	return exec.CommandContext(ctx, "systemctl", "--user", "cat", systemdUnit).Run() == nil
}

// spawn starts the daemon detached from this process and waits until its
// socket accepts connections.
func spawn(ctx context.Context, opts Options) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(opts.SocketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(dir, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "daemon", "--socket", opts.SocketPath, "--config", opts.ConfigPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("daemon exited early (%v); see %s", err, logPath)
		case <-ctx.Done():
			return fmt.Errorf("daemon not ready: %w; see %s", ctx.Err(), logPath)
		case <-tick.C:
			if c, err := rpc.Dial(ctx, opts.SocketPath); err == nil {
				c.Close()
				return nil
			}
		}
	}
}

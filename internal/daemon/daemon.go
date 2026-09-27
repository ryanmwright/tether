// Package daemon runs tether's background service: it owns the config and
// all connection state, and serves the RPC API on a Unix socket.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"tether/internal/api"
	"tether/internal/config"
	"tether/internal/rpc"
)

var ErrAlreadyRunning = errors.New("another tether daemon is already running")

type Options struct {
	SocketPath string
	ConfigPath string
	Version    string
	Logger     *slog.Logger
}

type Daemon struct {
	opts    Options
	log     *slog.Logger
	started time.Time

	mu     sync.RWMutex
	cfg    *config.Config
	cfgErr error // last reload failure; cfg keeps the last good config
}

// Run serves until ctx is cancelled or a client requests shutdown.
func Run(ctx context.Context, opts Options) error {
	d := &Daemon{opts: opts, log: opts.Logger, started: time.Now(), cfg: config.Default()}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(opts.SocketPath), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(opts.SocketPath + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	// Holding the lock means any existing socket is stale.
	if err := os.Remove(opts.SocketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	oldMask := syscall.Umask(0o177)
	l, err := net.Listen("unix", opts.SocketPath)
	syscall.Umask(oldMask)
	if err != nil {
		return err
	}
	defer os.Remove(opts.SocketPath)

	d.reload()

	srv := rpc.NewServer(d.log)
	srv.Handle(api.MethodStatus, d.handleStatus)
	srv.Handle(api.MethodReload, d.handleReload)
	srv.Handle(api.MethodShutdown, func(context.Context, json.RawMessage) (any, error) {
		d.log.Info("shutdown requested")
		cancel()
		return nil, nil
	})

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-hup:
				d.reload()
			case <-ctx.Done():
				return
			}
		}
	}()

	d.log.Info("daemon started", "version", opts.Version, "socket", opts.SocketPath, "config", opts.ConfigPath)
	if err := sdNotify("READY=1"); err != nil {
		d.log.Warn("sd_notify failed", "err", err)
	}
	err = srv.Serve(ctx, l)
	sdNotify("STOPPING=1")
	d.log.Info("daemon stopped")
	return err
}

// reload re-reads the config. An invalid config is reported and the previous
// one is kept.
func (d *Daemon) reload() error {
	cfg, err := config.Load(d.opts.ConfigPath)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfgErr = err
	if err != nil {
		d.log.Error("config invalid, keeping previous", "err", err)
		return err
	}
	d.cfg = cfg
	d.log.Info("config loaded", "hosts", len(cfg.Hosts), "profiles", len(cfg.Profiles))
	return nil
}

func (d *Daemon) handleReload(context.Context, json.RawMessage) (any, error) {
	if err := d.reload(); err != nil {
		return nil, &rpc.Error{Code: api.CodeInvalidConfig, Message: err.Error()}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return api.ReloadResult{Hosts: len(d.cfg.Hosts), Profiles: len(d.cfg.Profiles)}, nil
}

func (d *Daemon) handleStatus(context.Context, json.RawMessage) (any, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	st := api.Status{
		Version:    d.opts.Version,
		PID:        os.Getpid(),
		StartedAt:  d.started,
		ConfigPath: d.opts.ConfigPath,
		Hosts:      []api.HostStatus{},
		Profiles:   []api.ProfileStatus{},
	}
	if d.cfgErr != nil {
		st.ConfigError = d.cfgErr.Error()
	}
	for _, name := range slices.Sorted(maps.Keys(d.cfg.Hosts)) {
		h := d.cfg.Hosts[name]
		st.Hosts = append(st.Hosts, api.HostStatus{Name: name, SSH: h.SSH, Autoconnect: h.Autoconnect, State: api.StateDown})
	}
	for _, name := range slices.Sorted(maps.Keys(d.cfg.Profiles)) {
		p := d.cfg.Profiles[name]
		st.Profiles = append(st.Profiles, api.ProfileStatus{Name: name, Host: p.Host, Autoconnect: p.Autoconnect, State: api.StateDown})
	}
	return st, nil
}

// lockFile takes an exclusive, non-blocking flock on path. The lock is
// released when the process exits, even on a crash.
func lockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// sdNotify sends a readiness notification to systemd when running as a
// Type=notify service.
func sdNotify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	c, err := net.Dial("unixgram", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

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
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/config"
	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/gpg"
	"github.com/ryanmwright/tether/internal/mount"
	"github.com/ryanmwright/tether/internal/netwatch"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/rpc"
	"github.com/ryanmwright/tether/internal/usbip"
)

var ErrAlreadyRunning = errors.New("another tether daemon is already running")

type Options struct {
	SocketPath string
	ConfigPath string
	Version    string
	Logger     *slog.Logger
	SSH        openssh.Options
	// USBHelperSocket is the USB/IP helper's control socket.
	USBHelperSocket string
}

type Daemon struct {
	opts    Options
	log     *slog.Logger
	started time.Time
	ctx     context.Context // lifetime of sessions
	wg      sync.WaitGroup

	mu         sync.Mutex
	cfg        *config.Config
	cfgErr     error                             // last reload failure; cfg keeps the last good config
	active     map[string]bool                   // active profiles
	upHosts    map[string]bool                   // hosts brought up directly
	adhoc      map[string]map[string]wantForward // host -> forward key -> forward
	adhocMnt   map[string]map[string]wantMount   // host -> mount key -> mount
	adhocUSB   map[string]map[string]wantUSB     // host -> device spec -> device
	adhocHosts map[string]config.Host            // hosts added at runtime, not in the config
	home       string                            // for ~ in local mount paths
	autoSeen   map[string]bool                   // "host/x", "profile/x" -> autoconnect as last applied
	sessions   map[string]*sessionHandle
	usb        *localUSB

	gen    atomic.Uint64 // see api.Status.Generation
	logs   *logRing
	subsMu sync.Mutex
	subs   map[*subscriber]struct{}
}

type subscriber struct {
	status chan struct{}     // coalesced "status changed"
	logs   chan api.LogEntry // nil unless the client asked for logs
}

type sessionHandle struct {
	s      *session
	cancel context.CancelFunc
	done   chan struct{}
}

// Run serves until ctx is cancelled or a client requests shutdown.
func Run(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &Daemon{
		opts:       opts,
		started:    time.Now(),
		ctx:        ctx,
		cfg:        config.Default(),
		active:     map[string]bool{},
		upHosts:    map[string]bool{},
		adhoc:      map[string]map[string]wantForward{},
		adhocMnt:   map[string]map[string]wantMount{},
		adhocUSB:   map[string]map[string]wantUSB{},
		adhocHosts: map[string]config.Host{},
		autoSeen:   map[string]bool{},
		sessions:   map[string]*sessionHandle{},
		subs:       map[*subscriber]struct{}{},
		usb:        &localUSB{helper: &usbip.Client{Socket: opts.USBHelperSocket}},
	}
	if d.usb.helper.Socket == "" {
		d.usb.helper.Socket = usbip.DefaultHelperSocket
	}
	defer d.usb.helper.Close()
	d.home, _ = os.UserHomeDir()
	d.logs = &logRing{publish: d.publishLog}
	d.log = slog.New(&logTee{inner: opts.Logger.Handler(), ring: d.logs})

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

	d.ensureAuthSock()
	d.usb.poll()
	d.reload()
	go d.watchUSB(ctx)
	defer d.wg.Wait() // sessions stop their connections when ctx ends
	defer cancel()

	srv := rpc.NewServer(d.log)
	srv.Handle(api.MethodStatus, func(context.Context, json.RawMessage) (any, error) { return d.status(), nil })
	srv.Handle(api.MethodReload, d.handleReload)
	srv.Handle(api.MethodShutdown, func(context.Context, json.RawMessage) (any, error) {
		d.log.Info("shutdown requested")
		cancel()
		return nil, nil
	})
	srv.Handle(api.MethodUp, d.handleUp)
	srv.Handle(api.MethodDown, d.handleDown)
	srv.Handle(api.MethodForwardAdd, d.handleForwardAdd)
	srv.Handle(api.MethodForwardRemove, d.handleForwardRemove)
	srv.Handle(api.MethodSubscribe, d.handleSubscribe)
	srv.Handle(api.MethodDoctor, d.handleDoctor)
	srv.Handle(api.MethodMountAdd, d.handleMountAdd)
	srv.Handle(api.MethodHostAdd, d.handleHostAdd)
	srv.Handle(api.MethodHostRemove, d.handleHostRemove)
	srv.Handle(api.MethodMountRemove, d.handleMountRemove)
	srv.Handle(api.MethodUSBAttach, d.handleUSBAttach)
	srv.Handle(api.MethodUSBDetach, d.handleUSBDetach)
	srv.Handle(api.MethodLogs, func(_ context.Context, params json.RawMessage) (any, error) {
		p, err := decode[api.LogsParams](orEmpty(params))
		if err != nil {
			return nil, err
		}
		return d.logs.recent(p.Limit), nil
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

	if changes, err := netwatch.Watch(ctx, 2*time.Second); err != nil {
		d.log.Warn("network change detection unavailable", "err", err)
	} else {
		go func() {
			for range changes {
				d.log.Info("network changed")
				d.mu.Lock()
				for _, h := range d.sessions {
					h.s.networkChanged()
				}
				d.mu.Unlock()
			}
		}()
	}

	d.log.Info("daemon started", "version", opts.Version, "socket", opts.SocketPath, "config", opts.ConfigPath)
	if err := sdNotify("READY=1"); err != nil {
		d.log.Warn("sd_notify failed", "err", err)
	}
	err = srv.Serve(ctx, l)
	sdNotify("STOPPING=1")
	d.log.Info("daemon stopping")
	return err
}

// ensureAuthSock points ssh at gpg-agent's SSH socket when no agent is set.
// systemd user services often lack SSH_AUTH_SOCK even though the user's
// shells have it.
func (d *Daemon) ensureAuthSock() {
	if os.Getenv("SSH_AUTH_SOCK") != "" {
		return
	}
	out, err := exec.Command("gpgconf", "--list-dirs", "agent-ssh-socket").Output()
	if err != nil {
		return
	}
	sock := strings.TrimSpace(string(out))
	if fi, err := os.Stat(sock); err == nil && fi.Mode().Type() == os.ModeSocket {
		os.Setenv("SSH_AUTH_SOCK", sock)
		d.log.Info("SSH_AUTH_SOCK not set; using gpg-agent's SSH socket", "path", sock)
	}
}

// reload re-reads the config. An invalid config is reported and the previous
// one is kept.
func (d *Daemon) reload() error {
	cfg, err := config.Load(d.opts.ConfigPath)
	d.mu.Lock()
	d.cfgErr = err
	if err != nil {
		d.mu.Unlock()
		d.log.Error("config invalid, keeping previous", "err", err)
		d.notify()
		return err
	}
	d.applyConfig(cfg)
	d.mu.Unlock()
	d.log.Info("config loaded", "hosts", len(cfg.Hosts), "profiles", len(cfg.Profiles))
	d.notify()
	return nil
}

// applyConfig switches to cfg, keeping runtime state (active profiles,
// connected hosts, ad-hoc forwards) for everything that still exists.
// Autoconnect applies to items that are new or newly marked autoconnect.
// Callers hold d.mu.
func (d *Daemon) applyConfig(cfg *config.Config) {
	d.cfg = cfg
	for name := range cfg.Hosts {
		delete(d.adhocHosts, name) // the config now defines it
	}
	for name, h := range d.sessions {
		if _, ok := d.host(name); !ok {
			h.cancel()
			<-h.done
			delete(d.sessions, name)
			delete(d.upHosts, name)
			delete(d.adhoc, name)
			delete(d.adhocMnt, name)
			delete(d.adhocUSB, name)
		}
	}
	for name := range d.active {
		if _, ok := cfg.Profiles[name]; !ok {
			delete(d.active, name)
		}
	}

	seen := map[string]bool{}
	for name, h := range cfg.Hosts {
		key := "host/" + name
		if h.Autoconnect && !d.autoSeen[key] {
			d.upHosts[name] = true
		}
		seen[key] = h.Autoconnect
	}
	for name, p := range cfg.Profiles {
		key := "profile/" + name
		if p.Autoconnect && !d.autoSeen[key] {
			d.active[name] = true
		}
		seen[key] = p.Autoconnect
	}
	d.autoSeen = seen

	for _, name := range d.hostNames() {
		if _, ok := d.sessions[name]; !ok {
			d.startSession(name)
		}
	}
	d.recompute()
}

// host looks up a host in the config or among ad-hoc hosts. Callers hold
// d.mu.
func (d *Daemon) host(name string) (config.Host, bool) {
	if h, ok := d.cfg.Hosts[name]; ok {
		return h, true
	}
	h, ok := d.adhocHosts[name]
	return h, ok
}

// hostNames lists configured and ad-hoc hosts, sorted. Callers hold d.mu.
func (d *Daemon) hostNames() []string {
	names := slices.Collect(maps.Keys(d.cfg.Hosts))
	for name := range d.adhocHosts {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (d *Daemon) startSession(name string) {
	ctlPath := filepath.Join(filepath.Dir(d.opts.SocketPath), "ctl", name)
	s := newSession(name, ctlPath, d.opts.SSH, d.usb, d.log, d.notify)
	ctx, cancel := context.WithCancel(d.ctx)
	h := &sessionHandle{s: s, cancel: cancel, done: make(chan struct{})}
	d.sessions[name] = h
	d.wg.Go(func() {
		defer close(h.done)
		s.run(ctx)
	})
}

// recompute derives each host's want from the config and runtime state.
// Callers hold d.mu.
func (d *Daemon) recompute() {
	for name, h := range d.sessions {
		host, _ := d.host(name)
		w := want{backoff: d.cfg.Defaults.ReconnectBackoff, forwards: map[string]wantForward{}, mounts: map[string]wantMount{}, usb: map[string]wantUSB{}}
		wanted := d.upHosts[name]
		for _, pname := range slices.Sorted(maps.Keys(d.active)) {
			p := d.cfg.Profiles[pname]
			if p.Host != name {
				continue
			}
			wanted = true
			for key, wf := range profileForwards(p) {
				wf.profiles = append(w.forwards[key].profiles, pname)
				wf.adhoc = w.forwards[key].adhoc
				w.forwards[key] = wf
			}
			for key, wm := range profileMounts(p, d.home) {
				wm.profiles = append(w.mounts[key].profiles, pname)
				w.mounts[key] = wm
			}
			for key, wu := range profileUSB(p) {
				wu.profiles = append(w.usb[key].profiles, pname)
				w.usb[key] = wu
			}
		}
		for key, wu := range d.adhocUSB[name] {
			wanted = true
			wu.profiles = w.usb[key].profiles
			wu.adhoc = true
			w.usb[key] = wu
		}
		for key, wm := range d.adhocMnt[name] {
			wanted = true
			wm.profiles = w.mounts[key].profiles
			wm.adhoc = true
			w.mounts[key] = wm
		}
		for key, wf := range d.adhoc[name] {
			wanted = true
			wf.profiles = w.forwards[key].profiles
			wf.adhoc = true
			w.forwards[key] = wf
		}
		if wanted {
			w.dest = host.SSH
		} else {
			w.forwards, w.mounts, w.usb = nil, nil, nil
		}
		h.s.setWant(w)
	}
}

// Named forwards that stand for gpg-agent sockets, usable in `fwd add`.
const (
	gpgAgentForward = "gpg-agent"
	gpgSSHForward   = "gpg-ssh"
)

// parseForward parses a forward spec or a named forward, returning its key
// (the canonical spec, or the name).
func parseForward(s string) (string, wantForward, error) {
	switch s {
	case gpgAgentForward:
		return s, wantForward{gpgKind: gpg.KindAgent}, nil
	case gpgSSHForward:
		return s, wantForward{gpgKind: gpg.KindSSH}, nil
	}
	spec, err := forward.Parse(s)
	if err != nil {
		return "", wantForward{}, err
	}
	return spec.String(), wantForward{spec: spec}, nil
}

// profileForwards is every forward a profile asks for, by key.
func profileForwards(p config.Profile) map[string]wantForward {
	fwds := map[string]wantForward{}
	for _, f := range p.Forwards {
		key, wf, _ := parseForward(f) // validated with the config
		fwds[key] = wf
	}
	if p.GPG {
		fwds[gpgAgentForward] = wantForward{gpgKind: gpg.KindAgent}
	}
	if p.GPGSSH {
		fwds[gpgSSHForward] = wantForward{gpgKind: gpg.KindSSH}
	}
	return fwds
}

// profileMounts is every mount a profile asks for, by key.
func profileMounts(p config.Profile, home string) map[string]wantMount {
	mounts := map[string]wantMount{}
	for _, m := range p.Mounts {
		spec, _ := mount.Normalize(m.Spec(), home) // validated with the config
		mounts[spec.Key()] = wantMount{spec: spec}
	}
	return mounts
}

// profileUSB is every USB device a profile shares, by spec.
func profileUSB(p config.Profile) map[string]wantUSB {
	devs := map[string]wantUSB{}
	for _, u := range p.USB {
		spec, _ := usbip.ParseSpec(u) // validated with the config
		devs[string(spec)] = wantUSB{spec: spec}
	}
	return devs
}

// resolve finds the host or profile a request names. Callers hold d.mu.
func (d *Daemon) resolve(p api.TargetParams) (api.TargetResult, error) {
	prof, isProfile := d.cfg.Profiles[p.Name]
	_, isHost := d.host(p.Name)
	kind := p.Kind
	switch {
	case kind == "" && isProfile && isHost:
		return api.TargetResult{}, rpc.Errorf(api.CodeAmbiguous, "%q is both a host and a profile; specify which", p.Name)
	case kind == "" && isProfile:
		kind = api.TargetProfile
	case kind == "" && isHost:
		kind = api.TargetHost
	}
	switch {
	case kind == api.TargetProfile && isProfile:
		return api.TargetResult{Name: p.Name, Kind: kind, Host: prof.Host}, nil
	case kind == api.TargetHost && isHost:
		return api.TargetResult{Name: p.Name, Kind: kind, Host: p.Name}, nil
	case kind == "":
		return api.TargetResult{}, rpc.Errorf(api.CodeNotFound,
			"no host or profile named %q (to connect to a host that isn't in the config: tether host add %s [SSH-DEST])", p.Name, p.Name)
	default:
		return api.TargetResult{}, rpc.Errorf(api.CodeNotFound, "no %s named %q", kind, p.Name)
	}
}

func decode[T any](params json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(params, &v); err != nil {
		return v, rpc.Errorf(rpc.CodeInvalidParams, "invalid params: %v", err)
	}
	return v, nil
}

func (d *Daemon) handleUp(_ context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.TargetParams](params)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.resolve(p)
	if err != nil {
		return nil, err
	}
	if t.Kind == api.TargetProfile {
		d.active[t.Name] = true
	} else {
		d.upHosts[t.Name] = true
	}
	d.recompute()
	d.sessions[t.Host].s.retryNow()
	d.log.Info("up", "kind", t.Kind, "name", t.Name)
	t.Generation = d.gen.Load()
	return t, nil
}

func (d *Daemon) handleDown(_ context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.TargetParams](params)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.resolve(p)
	if err != nil {
		return nil, err
	}
	if t.Kind == api.TargetProfile {
		delete(d.active, t.Name)
	} else {
		// Taking a host down takes everything on it down.
		delete(d.upHosts, t.Name)
		delete(d.adhoc, t.Name)
		delete(d.adhocMnt, t.Name)
		delete(d.adhocUSB, t.Name)
		for name := range d.active {
			if d.cfg.Profiles[name].Host == t.Name {
				delete(d.active, name)
			}
		}
	}
	d.recompute()
	d.log.Info("down", "kind", t.Kind, "name", t.Name)
	t.Generation = d.gen.Load()
	return t, nil
}

func (d *Daemon) forwardParams(params json.RawMessage) (p api.ForwardParams, key string, wf wantForward, err error) {
	if p, err = decode[api.ForwardParams](params); err != nil {
		return
	}
	if _, ok := d.host(p.Host); !ok {
		err = rpc.Errorf(api.CodeNotFound, "no host named %q", p.Host)
		return
	}
	if key, wf, err = parseForward(p.Spec); err != nil {
		err = rpc.Errorf(rpc.CodeInvalidParams, "%v", err)
	}
	return
}

func (d *Daemon) handleForwardAdd(_ context.Context, params json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, key, wf, err := d.forwardParams(params)
	if err != nil {
		return nil, err
	}
	if d.adhoc[p.Host] == nil {
		d.adhoc[p.Host] = map[string]wantForward{}
	}
	d.adhoc[p.Host][key] = wf
	d.recompute()
	d.sessions[p.Host].s.retryNow()
	d.log.Info("ad-hoc forward added", "host", p.Host, "forward", key)
	return api.ForwardResult{Host: p.Host, Spec: key, Generation: d.gen.Load()}, nil
}

func (d *Daemon) handleForwardRemove(_ context.Context, params json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, key, _, err := d.forwardParams(params)
	if err != nil {
		return nil, err
	}
	if _, ok := d.adhoc[p.Host][key]; !ok {
		return nil, rpc.Errorf(api.CodeNotFound,
			"no ad-hoc forward %s on %s (profile forwards go away with `tether down <profile>`)", key, p.Host)
	}
	delete(d.adhoc[p.Host], key)
	d.recompute()
	d.log.Info("ad-hoc forward removed", "host", p.Host, "forward", key)
	return api.ForwardResult{Host: p.Host, Spec: key, Generation: d.gen.Load()}, nil
}

func (d *Daemon) mountParams(params json.RawMessage) (api.MountParams, mount.Spec, error) {
	p, err := decode[api.MountParams](params)
	if err != nil {
		return p, mount.Spec{}, err
	}
	if _, ok := d.host(p.Host); !ok {
		return p, mount.Spec{}, rpc.Errorf(api.CodeNotFound, "no host named %q", p.Host)
	}
	spec, err := mount.Normalize(mount.Spec{Direction: mount.Direction(p.Direction), Remote: p.Remote, Local: p.Local, Options: p.Options}, d.home)
	if err != nil {
		return p, spec, rpc.Errorf(rpc.CodeInvalidParams, "%v", err)
	}
	return p, spec, nil
}

func (d *Daemon) handleMountAdd(_ context.Context, params json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, spec, err := d.mountParams(params)
	if err != nil {
		return nil, err
	}
	if d.adhocMnt[p.Host] == nil {
		d.adhocMnt[p.Host] = map[string]wantMount{}
	}
	d.adhocMnt[p.Host][spec.Key()] = wantMount{spec: spec}
	d.recompute()
	d.sessions[p.Host].s.retryNow()
	d.log.Info("ad-hoc mount added", "host", p.Host, "mount", spec.Key())
	return api.MountResult{Host: p.Host, Key: spec.Key(), Generation: d.gen.Load()}, nil
}

func (d *Daemon) handleMountRemove(_ context.Context, params json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, spec, err := d.mountParams(params)
	if err != nil {
		return nil, err
	}
	if _, ok := d.adhocMnt[p.Host][spec.Key()]; !ok {
		return nil, rpc.Errorf(api.CodeNotFound,
			"no ad-hoc mount %s on %s (profile mounts go away with `tether down <profile>`)", spec.Key(), p.Host)
	}
	delete(d.adhocMnt[p.Host], spec.Key())
	d.recompute()
	d.log.Info("ad-hoc mount removed", "host", p.Host, "mount", spec.Key())
	return api.MountResult{Host: p.Host, Key: spec.Key(), Generation: d.gen.Load()}, nil
}

func (d *Daemon) usbParams(params json.RawMessage) (api.USBParams, usbip.Spec, error) {
	p, err := decode[api.USBParams](params)
	if err != nil {
		return p, "", err
	}
	if _, ok := d.host(p.Host); !ok {
		return p, "", rpc.Errorf(api.CodeNotFound, "no host named %q", p.Host)
	}
	spec, err := usbip.ParseSpec(p.Device)
	if err != nil {
		return p, "", rpc.Errorf(rpc.CodeInvalidParams, "%v", err)
	}
	return p, spec, nil
}

func (d *Daemon) handleUSBAttach(_ context.Context, params json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, spec, err := d.usbParams(params)
	if err != nil {
		return nil, err
	}
	// A device can only be in one place: refuse a second host up front
	// rather than failing later.
	if dev, err := spec.Find(d.usb.Devices()); err == nil {
		for name, h := range d.sessions {
			if name == p.Host {
				continue
			}
			for _, u := range h.s.status(config.Host{}).USB {
				if u.BusID == dev.BusID || spec.Matches(dev) && u.Device == string(spec) {
					return nil, rpc.Errorf(api.CodeInvalidConfig, "%s is already shared with %s", dev.Title(), name)
				}
			}
		}
	}
	if d.adhocUSB[p.Host] == nil {
		d.adhocUSB[p.Host] = map[string]wantUSB{}
	}
	d.adhocUSB[p.Host][string(spec)] = wantUSB{spec: spec}
	d.recompute()
	d.sessions[p.Host].s.retryNow()
	d.log.Info("ad-hoc USB device added", "host", p.Host, "usb", spec)
	return api.USBResult{Host: p.Host, Device: string(spec), Generation: d.gen.Load()}, nil
}

func (d *Daemon) handleUSBDetach(_ context.Context, params json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, spec, err := d.usbParams(params)
	if err != nil {
		return nil, err
	}
	if _, ok := d.adhocUSB[p.Host][string(spec)]; !ok {
		return nil, rpc.Errorf(api.CodeNotFound,
			"no ad-hoc USB device %s on %s (profile devices go away with `tether down <profile>`)", spec, p.Host)
	}
	delete(d.adhocUSB[p.Host], string(spec))
	d.recompute()
	d.log.Info("ad-hoc USB device removed", "host", p.Host, "usb", spec)
	return api.USBResult{Host: p.Host, Device: string(spec), Generation: d.gen.Load()}, nil
}

func (d *Daemon) handleHostAdd(_ context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.HostParams](params)
	if err != nil {
		return nil, err
	}
	if p.SSH == "" {
		p.SSH = p.Name
	}
	if err := config.ValidateHost(p.Name, config.Host{SSH: p.SSH}); err != nil {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "%v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.cfg.Hosts[p.Name]; ok {
		return nil, rpc.Errorf(api.CodeInvalidConfig, "%q is already in the config; use `tether up %s`", p.Name, p.Name)
	}
	if _, ok := d.cfg.Profiles[p.Name]; ok {
		return nil, rpc.Errorf(api.CodeAmbiguous, "%q is a profile name; pick another name for the host", p.Name)
	}
	if h, ok := d.adhocHosts[p.Name]; ok && h.SSH != p.SSH {
		return nil, rpc.Errorf(api.CodeInvalidConfig, "ad-hoc host %q already connects to %s", p.Name, h.SSH)
	}
	d.adhocHosts[p.Name] = config.Host{SSH: p.SSH}
	if _, ok := d.sessions[p.Name]; !ok {
		d.startSession(p.Name)
	}
	d.upHosts[p.Name] = true
	d.recompute()
	d.sessions[p.Name].s.retryNow()
	d.log.Info("ad-hoc host added", "host", p.Name, "ssh", p.SSH)
	return api.TargetResult{Name: p.Name, Kind: api.TargetHost, Host: p.Name, Generation: d.gen.Load()}, nil
}

func (d *Daemon) handleHostRemove(_ context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.HostParams](params)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.adhocHosts[p.Name]; !ok {
		if _, inConfig := d.cfg.Hosts[p.Name]; inConfig {
			return nil, rpc.Errorf(api.CodeNotFound, "%q is in the config, not ad hoc; use `tether down %s`", p.Name, p.Name)
		}
		return nil, rpc.Errorf(api.CodeNotFound, "no ad-hoc host named %q", p.Name)
	}
	delete(d.adhocHosts, p.Name)
	delete(d.upHosts, p.Name)
	delete(d.adhoc, p.Name)
	delete(d.adhocMnt, p.Name)
	delete(d.adhocUSB, p.Name)
	h := d.sessions[p.Name]
	h.cancel() // disconnects, unmounting first
	<-h.done
	delete(d.sessions, p.Name)
	d.recompute()
	d.log.Info("ad-hoc host removed", "host", p.Name)
	d.notify()
	return api.TargetResult{Name: p.Name, Kind: api.TargetHost, Host: p.Name, Generation: d.gen.Load()}, nil
}

func (d *Daemon) handleReload(context.Context, json.RawMessage) (any, error) {
	if err := d.reload(); err != nil {
		return nil, &rpc.Error{Code: api.CodeInvalidConfig, Message: err.Error()}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return api.ReloadResult{Hosts: len(d.cfg.Hosts), Profiles: len(d.cfg.Profiles)}, nil
}

// handleSubscribe pushes a status snapshot now and after every change until
// the client disconnects. Snapshots are coalesced, so a slow client skips
// intermediate states rather than falling behind. Log entries, if requested,
// are buffered; a client that falls far behind misses some.
func (d *Daemon) handleSubscribe(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.SubscribeParams](orEmpty(params))
	if err != nil {
		return nil, err
	}
	conn := rpc.ConnFromContext(ctx)
	sub := &subscriber{status: make(chan struct{}, 1)}
	sub.status <- struct{}{}
	if p.Logs {
		sub.logs = make(chan api.LogEntry, 256)
	}
	d.subsMu.Lock()
	d.subs[sub] = struct{}{}
	d.subsMu.Unlock()
	go func() {
		defer func() {
			d.subsMu.Lock()
			delete(d.subs, sub)
			d.subsMu.Unlock()
		}()
		for {
			var err error
			select {
			case <-ctx.Done():
				return
			case <-sub.status:
				err = conn.Notify(api.EventStatus, d.status())
			case e := <-sub.logs:
				err = conn.Notify(api.EventLog, e)
			}
			if err != nil {
				return
			}
		}
	}()
	return nil, nil
}

func (d *Daemon) notify() {
	d.gen.Add(1)
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for sub := range d.subs {
		poke(sub.status)
	}
}

func (d *Daemon) publishLog(e api.LogEntry) {
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for sub := range d.subs {
		if sub.logs == nil {
			continue
		}
		select {
		case sub.logs <- e:
		default:
		}
	}
}

// orEmpty lets handlers with optional params accept a call without any.
func orEmpty(params json.RawMessage) json.RawMessage {
	if len(params) == 0 {
		return json.RawMessage("{}")
	}
	return params
}

func (d *Daemon) status() api.Status {
	// Read the generation first: the snapshot is at least that new.
	gen := d.gen.Load()
	d.mu.Lock()
	defer d.mu.Unlock()
	st := api.Status{
		Generation: gen,
		Protocol:   api.ProtocolVersion,
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
	hosts := map[string]api.HostStatus{}
	for _, name := range d.hostNames() {
		h, _ := d.host(name)
		hs := d.sessions[name].s.status(h)
		_, hs.AdHoc = d.adhocHosts[name]
		hosts[name] = hs
		st.Hosts = append(st.Hosts, hs)
	}
	st.USB, st.USBUnavailable = d.usbStatus(st.Hosts)
	for _, name := range slices.Sorted(maps.Keys(d.cfg.Profiles)) {
		p := d.cfg.Profiles[name]
		ps := api.ProfileStatus{Name: name, Host: p.Host, Autoconnect: p.Autoconnect, Active: d.active[name]}
		ps.State, ps.Error = profileState(ps.Active, p, hosts[p.Host], d.home)
		st.Profiles = append(st.Profiles, ps)
	}
	return st
}

// profileState summarizes an active profile from its host and forwards.
func profileState(active bool, p config.Profile, hs api.HostStatus, home string) (api.State, string) {
	if !active {
		return api.StateDown, ""
	}
	switch hs.State {
	case api.StateError:
		return api.StateError, hs.Error
	case api.StateUp, api.StateDegraded:
	default:
		return api.StatePending, ""
	}
	byspec := map[string]api.ForwardStatus{}
	for _, f := range hs.Forwards {
		byspec[f.Spec] = f
	}
	state := api.StateUp
	for _, key := range slices.Sorted(maps.Keys(profileForwards(p))) {
		fs := byspec[key]
		switch fs.State {
		case api.StateError:
			return api.StateDegraded, fmt.Sprintf("%s: %s", key, fs.Error)
		case api.StateUp:
		default:
			state = api.StatePending
		}
	}
	bykey := map[string]api.MountStatus{}
	for _, m := range hs.Mounts {
		bykey[m.Key] = m
	}
	for _, key := range slices.Sorted(maps.Keys(profileMounts(p, home))) {
		switch ms := bykey[key]; ms.State {
		case api.StateError:
			return api.StateDegraded, fmt.Sprintf("%s: %s", key, ms.Error)
		case api.StateUp:
		default:
			state = api.StatePending
		}
	}
	usb := map[string]api.USBStatus{}
	for _, u := range hs.USB {
		usb[u.Device] = u
	}
	for _, key := range slices.Sorted(maps.Keys(profileUSB(p))) {
		switch us := usb[key]; us.State {
		case api.StateError:
			return api.StateDegraded, fmt.Sprintf("USB %s: %s", key, us.Error)
		case api.StateUp:
		default:
			state = api.StatePending
		}
	}
	return state, ""
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

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/config"
	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/gpg"
	"github.com/ryanmwright/tether/internal/mount"
	"github.com/ryanmwright/tether/internal/openssh"
)

const (
	connectTimeout       = 30 * time.Second
	controlTimeout       = 10 * time.Second
	probeTimeout         = 10 * time.Second
	forwardRetryInterval = 15 * time.Second
)

// want is what the daemon wants from one host's connection.
type want struct {
	dest     string // ssh destination; empty means disconnected
	forwards map[string]wantForward
	mounts   map[string]wantMount
	usb      map[string]wantUSB
	backoff  config.Backoff
}

type wantForward struct {
	spec forward.Spec
	// gpgKind marks a gpg-agent forward (gpg.KindAgent or gpg.KindSSH). Its
	// spec depends on socket paths on both ends, so it is resolved each time
	// the forward is added.
	gpgKind  string
	profiles []string
	adhoc    bool
}

type forwardState struct {
	state    api.State
	err      string
	port     int
	resolved string // the spec a gpg forward resolved to
}

// session keeps one host's connection and forwards matching its want. All
// SSH work happens on the goroutine running run; other methods only update
// state and wake it.
type session struct {
	name    string
	ctlPath string
	ssh     openssh.Options
	usb     usbBackend
	log     *slog.Logger
	changed func()

	wake      chan struct{} // want changed
	kick      chan struct{} // retry a failed connection now
	netChange chan struct{}
	mountDied chan struct{} // a running mount went away

	mu             sync.Mutex
	want           want
	state          api.State
	err            string
	retryAt        time.Time
	forwards       map[string]*forwardState
	mounts         map[string]*mountState
	usbStates      map[string]*usbState
	master         *openssh.Master // the live connection, if any
	cancelConnect  context.CancelFunc
	connectingDest string
}

func newSession(name, ctlPath string, ssh openssh.Options, usb usbBackend, log *slog.Logger, changed func()) *session {
	return &session{
		name:      name,
		ctlPath:   ctlPath,
		ssh:       ssh,
		usb:       usb,
		log:       log.With("host", name),
		changed:   changed,
		wake:      make(chan struct{}, 1),
		kick:      make(chan struct{}, 1),
		netChange: make(chan struct{}, 1),
		mountDied: make(chan struct{}, 1),
		state:     api.StateDown,
		forwards:  map[string]*forwardState{},
		mounts:    map[string]*mountState{},
		usbStates: map[string]*usbState{},
	}
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// setWant records the desired state. Visible state is updated immediately
// (new forwards show as pending, a newly wanted host as pending) so clients
// never observe a stale result for a request they just made.
func (s *session) setWant(w want) {
	s.mu.Lock()
	if w.dest != s.connectingDest && s.cancelConnect != nil {
		s.cancelConnect()
	}
	s.want = w
	for key := range s.forwards {
		if _, ok := w.forwards[key]; !ok {
			delete(s.forwards, key)
		}
	}
	for key := range s.mounts {
		if _, ok := w.mounts[key]; !ok {
			delete(s.mounts, key)
		}
	}
	for key := range s.usbStates {
		if _, ok := w.usb[key]; !ok {
			delete(s.usbStates, key)
		}
	}
	if w.dest != "" {
		for key := range w.forwards {
			if s.forwards[key] == nil {
				s.forwards[key] = &forwardState{state: api.StatePending}
			}
		}
		for key := range w.mounts {
			if s.mounts[key] == nil {
				s.mounts[key] = &mountState{state: api.StatePending}
			}
		}
		for key := range w.usb {
			if s.usbStates[key] == nil {
				s.usbStates[key] = &usbState{state: api.StatePending}
			}
		}
		if s.state == api.StateDown {
			s.state = api.StatePending
		}
	}
	s.mu.Unlock()
	poke(s.wake)
	s.changed()
}

// retryNow skips any reconnect backoff.
func (s *session) retryNow() {
	s.mu.Lock()
	if s.state == api.StateError && s.want.dest != "" {
		s.state, s.err, s.retryAt = api.StatePending, "", time.Time{}
	}
	s.mu.Unlock()
	poke(s.kick)
	s.changed()
}

func (s *session) networkChanged() { poke(s.netChange) }

func (s *session) currentWant() want {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.want
}

func (s *session) setState(state api.State, err string, retryAt time.Time) {
	s.mu.Lock()
	s.state, s.err, s.retryAt = state, err, retryAt
	if state != api.StateUp {
		for _, f := range s.forwards {
			*f = forwardState{state: api.StatePending}
		}
		for _, ms := range s.mounts {
			*ms = mountState{state: api.StatePending}
		}
		for _, us := range s.usbStates {
			*us = usbState{state: api.StatePending}
		}
	}
	if s.want.dest == "" {
		clear(s.forwards)
		clear(s.mounts)
		clear(s.usbStates)
	}
	s.mu.Unlock()
	s.changed()
}

// setForward records the outcome of adding a forward and returns its
// previous error message.
func (s *session) setForward(key string, err error, port int, resolved string) (prevErr string) {
	s.mu.Lock()
	if f := s.forwards[key]; f != nil {
		prevErr = f.err
		if err != nil {
			*f = forwardState{state: api.StateError, err: err.Error()}
		} else {
			*f = forwardState{state: api.StateUp, port: port, resolved: resolved}
		}
	}
	s.mu.Unlock()
	s.changed()
	return prevErr
}

func (s *session) setMaster(m *openssh.Master) {
	s.mu.Lock()
	s.master = m
	s.mu.Unlock()
}

// liveMaster returns the current connection, or nil when not connected.
func (s *session) liveMaster() *openssh.Master {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.master
}

func (s *session) run(ctx context.Context) {
	var (
		m        *openssh.Master
		dest     string
		applied  = map[string]forward.Spec{}
		running  = map[string]*mount.Running{}
		usbRT    = newUSBRuntime()
		delay    time.Duration
		retry    *time.Timer // pending reconnect; nil when not waiting
		fwdRetry *time.Timer
	)
	defer func() {
		if m != nil {
			s.stopUSB(m, usbRT)
			s.stopMounts(running)
			s.setMaster(nil)
			m.Stop()
		}
	}()
	disconnect := func() {
		if m != nil {
			s.stopUSB(m, usbRT) // detach and unmount cleanly while still connected
			s.stopMounts(running)
			s.setMaster(nil)
			m.Stop()
			m = nil
			clear(applied)
		}
	}
	scheduleRetry := func(w want, reason string) {
		delay = nextDelay(delay, w.backoff)
		retry = time.NewTimer(delay)
		s.setState(api.StateError, reason, time.Now().Add(delay))
	}

	for {
		w := s.currentWant()
		if m != nil && w.dest != dest {
			s.log.Info("disconnecting")
			disconnect()
		}
		switch {
		case w.dest == "":
			stopTimer(retry)
			retry, delay = nil, 0
			s.setState(api.StateDown, "", time.Time{})
		case m == nil && retry == nil:
			s.setState(api.StatePending, "", time.Time{})
			s.log.Info("connecting", "dest", w.dest)
			var err error
			m, err = s.connect(ctx, w.dest)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				s.log.Warn("connect failed", "err", err)
				if s.currentWant().dest == w.dest {
					scheduleRetry(w, err.Error())
				}
				continue
			}
			s.log.Info("connected")
			s.setMaster(m)
			dest, delay = w.dest, 0
			s.setState(api.StateUp, "", time.Time{})
		}
		if m != nil {
			fwdFailed := s.syncForwards(ctx, m, w, applied)
			mountFailed := s.syncMounts(ctx, m, w, running)
			usbFailed := s.syncUSB(ctx, m, w, usbRT)
			if (fwdFailed || mountFailed || usbFailed) && fwdRetry == nil {
				fwdRetry = time.NewTimer(forwardRetryInterval)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-s.kick:
			if retry != nil {
				stopTimer(retry)
				retry, delay = nil, 0
			}
		case <-timerC(retry):
			retry = nil
		case <-timerC(fwdRetry):
			fwdRetry = nil
		case <-s.mountDied:
			if s.reapMounts(running) && fwdRetry == nil {
				fwdRetry = time.NewTimer(forwardRetryInterval)
			}
		case <-doneC(m):
			err := m.Err()
			s.log.Warn("connection lost", "err", err)
			s.stopMounts(running) // they die with the connection; clean up after them
			s.stopUSB(nil, usbRT)
			s.setMaster(nil)
			m = nil
			clear(applied)
			scheduleRetry(w, fmt.Sprintf("connection lost: %v", err))
		case <-s.netChange:
			if retry != nil {
				s.log.Info("network changed, reconnecting now")
				stopTimer(retry)
				retry, delay = nil, 0
			} else if m != nil {
				// The TCP connection may be dead without ssh knowing yet.
				pctx, cancel := context.WithTimeout(ctx, probeTimeout)
				if err := m.Probe(pctx); err != nil {
					s.log.Warn("network changed and connection is unresponsive", "err", err)
					m.Stop() // handled as a lost connection on the next pass
				}
				cancel()
			}
		}
	}
}

func (s *session) connect(ctx context.Context, dest string) (*openssh.Master, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	s.mu.Lock()
	s.cancelConnect, s.connectingDest = cancel, dest
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cancelConnect, s.connectingDest = nil, ""
		s.mu.Unlock()
	}()
	if err := os.MkdirAll(filepath.Dir(s.ctlPath), 0o700); err != nil {
		return nil, err
	}
	return openssh.Start(ctx, s.ssh, dest, s.ctlPath, s.log)
}

// syncForwards cancels forwards no longer wanted and adds missing ones. It
// reports whether any failed, so the caller can schedule a retry.
func (s *session) syncForwards(ctx context.Context, m *openssh.Master, w want, applied map[string]forward.Spec) (failed bool) {
	for _, key := range slices.Sorted(maps.Keys(applied)) {
		if _, ok := w.forwards[key]; ok {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, controlTimeout)
		err := m.Cancel(cctx, applied[key])
		cancel()
		if err != nil {
			s.log.Warn("cancel forward failed", "forward", key, "err", err)
		} else {
			s.log.Info("forward removed", "forward", key)
		}
		delete(applied, key)
	}
	for _, key := range slices.Sorted(maps.Keys(w.forwards)) {
		if _, ok := applied[key]; ok {
			continue
		}
		spec, port, err := s.addWanted(ctx, m, w.forwards[key])
		resolved := ""
		if w.forwards[key].gpgKind != "" {
			resolved = spec.String()
		}
		if prev := s.setForward(key, err, port, resolved); err != nil {
			// Failing forwards are retried often; log each distinct failure once.
			if prev != err.Error() {
				s.log.Warn("forward failed", "forward", key, "err", err)
			}
			failed = true
			continue
		}
		s.log.Info("forward added", "forward", key)
		applied[key] = spec
	}
	return failed
}

// addWanted resolves wf if it's a gpg forward, then adds it.
func (s *session) addWanted(ctx context.Context, m *openssh.Master, wf wantForward) (forward.Spec, int, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	spec := wf.spec
	if wf.gpgKind != "" {
		var err error
		if spec, err = resolveGPG(ctx, m, wf.gpgKind); err != nil {
			return spec, 0, err
		}
	}
	port, err := addForward(ctx, m, spec)
	return spec, port, err
}

// resolveGPG builds the remote forward for a gpg socket kind: from the
// remote's standard socket (cleared first) to the matching local one.
func resolveGPG(ctx context.Context, m *openssh.Master, kind string) (forward.Spec, error) {
	extra, sshSock, err := gpg.LocalSockets(ctx, kind == gpg.KindSSH)
	if err != nil {
		return forward.Spec{}, err
	}
	local := extra
	if kind == gpg.KindSSH {
		local = sshSock
	}
	out, err := m.Run(ctx, gpg.PrepareRemoteScript, "sh -s -- "+kind)
	if err != nil {
		return forward.Spec{}, fmt.Errorf("preparing remote gpg socket: %w", err)
	}
	remote := gpg.ParsePrepareOutput(string(out))[kind]
	if remote == "" {
		return forward.Spec{}, fmt.Errorf("remote gpgconf reported no %s socket", kind)
	}
	return forward.Spec{
		Kind:   forward.Remote,
		Listen: forward.Endpoint{Socket: remote},
		Target: forward.Endpoint{Socket: local},
	}, nil
}

// addForward adds spec and, for listeners on this machine, checks that the
// port or socket really is ssh's: ssh reports a failed local bind only in
// its own log.
func addForward(ctx context.Context, m *openssh.Master, spec forward.Spec) (int, error) {
	if spec.ListensLocally() {
		if err := checkLocalFree(spec.Listen); err != nil {
			return 0, err
		}
	}
	port, err := m.Forward(ctx, spec)
	if err != nil {
		return 0, err
	}
	if spec.ListensLocally() && !localBound(spec.Listen) {
		m.Cancel(ctx, spec)
		return 0, fmt.Errorf("ssh could not listen on %s", spec.Listen)
	}
	return port, nil
}

func localAddr(e forward.Endpoint) string {
	host := e.Host
	switch host {
	case "":
		host = "localhost" // ssh's default bind address
	case "*":
		host = ""
	}
	return net.JoinHostPort(host, strconv.Itoa(e.Port))
}

func checkLocalFree(e forward.Endpoint) error {
	if e.IsSocket() {
		fi, err := os.Stat(e.Socket)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode().Type() != os.ModeSocket {
			return fmt.Errorf("%s exists and is not a socket", e.Socket)
		}
		if c, err := net.Dial("unix", e.Socket); err == nil {
			c.Close()
			return fmt.Errorf("%s is in use", e.Socket)
		}
		return nil // stale; ssh replaces it (StreamLocalBindUnlink)
	}
	l, err := net.Listen("tcp", localAddr(e))
	if err != nil {
		return fmt.Errorf("local port %d is unavailable: %w", e.Port, unwrapSyscall(err))
	}
	l.Close()
	return nil
}

// localBound reports whether something is listening on e. It checks by
// trying to listen rather than connecting, so no connection is forwarded.
func localBound(e forward.Endpoint) bool {
	if e.IsSocket() {
		fi, err := os.Stat(e.Socket)
		return err == nil && fi.Mode().Type() == os.ModeSocket
	}
	l, err := net.Listen("tcp", localAddr(e))
	if err != nil {
		return true
	}
	l.Close()
	return false
}

func unwrapSyscall(err error) error {
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return sysErr.Err
	}
	return err
}

func (s *session) status(host config.Host) api.HostStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs := api.HostStatus{
		Name:        s.name,
		SSH:         host.SSH,
		Autoconnect: host.Autoconnect,
		State:       s.state,
		Error:       s.err,
		Forwards:    []api.ForwardStatus{},
	}
	if !s.retryAt.IsZero() {
		t := s.retryAt
		hs.RetryAt = &t
	}
	for _, key := range slices.Sorted(maps.Keys(s.forwards)) {
		f := s.forwards[key]
		wf := s.want.forwards[key]
		hs.Forwards = append(hs.Forwards, api.ForwardStatus{
			Spec:          key,
			Profiles:      wf.profiles,
			AdHoc:         wf.adhoc,
			State:         f.state,
			Error:         f.err,
			AllocatedPort: f.port,
			Resolved:      f.resolved,
		})
	}
	hs.Mounts = s.mountStatuses()
	hs.USB = s.usbStatuses()
	if hs.State == api.StateUp {
		for _, f := range hs.Forwards {
			if f.State == api.StateError {
				hs.State = api.StateDegraded
			}
		}
		for _, ms := range hs.Mounts {
			if ms.State == api.StateError {
				hs.State = api.StateDegraded
			}
		}
		for _, us := range hs.USB {
			if us.State == api.StateError {
				hs.State = api.StateDegraded
			}
		}
	}
	return hs
}

func nextDelay(prev time.Duration, b config.Backoff) time.Duration {
	if prev == 0 {
		return b.Min
	}
	return min(prev*2, b.Max)
}

func stopTimer(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}

func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func doneC(m *openssh.Master) <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.Done()
}

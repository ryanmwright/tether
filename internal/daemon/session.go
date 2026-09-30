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
	"github.com/ryanmwright/tether/internal/kube"
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
	// checkTargets checks that forwards' targets answer.
	checkTargets bool
}

type wantForward struct {
	spec  forward.Spec
	label string // a name given to it, e.g. "postgres"
	// gpgKind marks a gpg-agent forward (gpg.KindAgent or gpg.KindSSH). Its
	// spec depends on socket paths on both ends, so it is resolved each time
	// the forward is added.
	gpgKind  string
	kube     kube.Options // for a Kubernetes forward: how to run kubectl
	profiles []string
	adhoc    bool
}

type forwardState struct {
	state    api.State
	err      string
	port     int
	resolved string // the spec a gpg forward resolved to
	// actual is the forward with the ports chosen for it, once up.
	actual forward.Spec
	// lastPort is the local port picked for a forward on port 0, kept
	// across reconnects so it stays the same while it's free.
	lastPort          int
	target, targetErr string // the last target check
	// usedBy, for a gpg forward, names the machine whose agent gpg on the
	// remote uses instead of this one's; empty while it uses this one's.
	usedBy string
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
	fwdDied   chan struct{} // a forward's process (kubectl port-forward) exited
	checkNow  chan struct{} // check forward targets now
	gpgID     string        // this machine's name in remote gpg socket paths

	mu             sync.Mutex
	want           want
	state          api.State
	err            string
	retryAt        time.Time
	forwards       map[string]*forwardState
	mounts         map[string]*mountState
	usbStates      map[string]*usbState
	checks         map[string]forward.Spec // forwards whose targets to check, as applied
	master         *openssh.Master         // the live connection, if any
	gpgClaim       bool                    // claim the gpg sockets at the next sync: the user asked for this host
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
		fwdDied:   make(chan struct{}, 1),
		checkNow:  make(chan struct{}, 1),
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
			*f = forwardState{state: api.StatePending, lastPort: f.lastPort}
		}
		s.checks = nil
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
// previous error message. af is the forward as applied, when it worked.
func (s *session) setForward(key string, af *appliedForward, err error, port int, resolved string) (prevErr string) {
	s.mu.Lock()
	if f := s.forwards[key]; f != nil {
		prevErr = f.err
		last := f.lastPort
		if err != nil {
			*f = forwardState{state: api.StateError, err: err.Error(), lastPort: last}
		} else {
			*f = forwardState{state: api.StateUp, port: port, resolved: resolved, actual: af.actual, lastPort: last}
			if af.actual.AutoPort() || s.want.forwards[key].spec.AutoPort() {
				f.lastPort = af.actual.Listen.Port
			}
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
		applied  = map[string]*appliedForward{}
		running  = map[string]*mount.Running{}
		usbRT    = newUSBRuntime()
		delay    time.Duration
		retry    *time.Timer // pending reconnect; nil when not waiting
		fwdRetry *time.Timer
		gpgRT    gpgRuntime
		gpgTick  = time.NewTicker(gpgCheckInterval)
	)
	defer gpgTick.Stop()
	go s.checkLoop(ctx)
	defer func() {
		if m != nil {
			// Shutting down: hand the gpg sockets on while still connected.
			rctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
			s.releaseGPG(rctx, m, applied)
			cancel()
		}
		s.stopForwards(applied)
		if m != nil {
			s.stopUSB(m, usbRT)
			s.stopMounts(running)
			s.setMaster(nil)
			m.Stop()
		}
	}()
	disconnect := func() {
		if m != nil {
			s.releaseGPG(ctx, m, applied)
			s.stopUSB(m, usbRT) // detach and unmount cleanly while still connected
			s.stopMounts(running)
			s.stopForwards(applied)
			s.setMaster(nil)
			m.Stop()
			m = nil
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
			claim := s.takeGPGClaim()
			fwdFailed := s.syncForwards(ctx, m, w, applied)
			fwdFailed = s.syncGPG(ctx, m, w, applied, claim, &gpgRT) || fwdFailed
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
		case <-gpgTick.C:
		case <-s.mountDied:
			if s.reapMounts(running) && fwdRetry == nil {
				fwdRetry = time.NewTimer(forwardRetryInterval)
			}
		case <-s.fwdDied:
			if retryIn, failed := s.reapForwards(ctx, m, applied); failed {
				// A forward whose kubectl ran a while (its pod was replaced)
				// comes back at once; one that keeps failing waits.
				stopTimer(fwdRetry)
				fwdRetry = time.NewTimer(retryIn)
			}
		case <-doneC(m):
			err := m.Err()
			s.log.Warn("connection lost", "err", err)
			s.stopMounts(running) // they die with the connection; clean up after them
			s.stopUSB(nil, usbRT)
			s.stopForwards(applied)
			s.setMaster(nil)
			m = nil
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
	if dest == openssh.LocalDest {
		return openssh.StartLocal(s.log), nil
	}
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

// resolveGPG builds the remote forward for a gpg socket kind: from this
// machine's socket beside the remote's standard one (cleared first) to the
// matching local one. syncGPG decides whether the standard one leads to it.
func (s *session) resolveGPG(ctx context.Context, m *openssh.Master, kind string) (forward.Spec, error) {
	extra, sshSock, err := gpg.LocalSockets(ctx, kind == gpg.KindSSH)
	if err != nil {
		return forward.Spec{}, err
	}
	local := extra
	if kind == gpg.KindSSH {
		local = sshSock
	}
	socks, err := s.gpgScript(ctx, m, gpg.ModeBind, kind)
	if err != nil {
		return forward.Spec{}, fmt.Errorf("preparing remote gpg socket: %w", err)
	}
	return forward.Spec{
		Kind:   forward.Remote,
		Listen: forward.Endpoint{Socket: socks[kind].Ours},
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
		Local:       host.Local,
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
		fs := api.ForwardStatus{
			Spec:          key,
			Label:         wf.label,
			Profiles:      wf.profiles,
			AdHoc:         wf.adhoc,
			State:         f.state,
			Error:         f.err,
			AllocatedPort: f.port,
			Resolved:      f.resolved,
			Target:        f.target,
			TargetError:   f.targetErr,
			UsedBy:        f.usedBy,
		}
		if wf.gpgKind == "" {
			spec := wf.spec
			if f.state == api.StateUp {
				spec = f.actual
			}
			fs.Description = forward.Describe(spec, s.name)
			fs.Address = connectAddress(spec)
		}
		hs.Forwards = append(hs.Forwards, fs)
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

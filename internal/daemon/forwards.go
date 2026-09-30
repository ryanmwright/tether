package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/kube"
	"github.com/ryanmwright/tether/internal/openssh"
)

const (
	checkInterval = 30 * time.Second
	checkTimeout  = 3 * time.Second
	// A kubectl port-forward that ran this long before exiting (its pod was
	// replaced, say) is restarted at once rather than after the retry delay.
	stableRunner = 10 * time.Second
)

// appliedForward is a forward as set up on the connection.
type appliedForward struct {
	// ssh is the forward on the master, to cancel when removing it (with the
	// port the server picked, for R:0); unset for forwards that have none
	// (Kubernetes ones on the local host).
	ssh forward.Spec
	// actual is what the user asked for, with the ports chosen for it.
	actual forward.Spec
	// runner is what runs besides ssh: the HTTP proxy, or kubectl.
	runner  forwardRunner
	noCheck bool   // not worth checking (gpg-agent's sockets)
	gpgKind string // for a gpg forward, its socket kind
}

type forwardRunner interface {
	Stop()
	Done() <-chan struct{}
	Err() error
}

// syncForwards cancels forwards no longer wanted and adds missing ones. It
// reports whether any failed, so the caller can schedule a retry.
func (s *session) syncForwards(ctx context.Context, m *openssh.Master, w want, applied map[string]*appliedForward) (failed bool) {
	for _, key := range slices.Sorted(maps.Keys(applied)) {
		if _, ok := w.forwards[key]; !ok {
			s.removeForward(ctx, m, key, applied)
			s.log.Info("forward removed", "forward", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(w.forwards)) {
		if _, ok := applied[key]; ok {
			continue
		}
		wf := w.forwards[key]
		af, port, resolved, err := s.addWanted(ctx, m, key, wf)
		if prev := s.setForward(key, af, err, port, resolved); err != nil {
			// Failing forwards are retried often; log each distinct failure once.
			if prev != err.Error() {
				s.log.Warn("forward failed", "forward", key, "err", err)
			}
			failed = true
			continue
		}
		s.log.Info("forward added", "forward", key)
		applied[key] = af
		if r := af.runner; r != nil {
			go func() {
				<-r.Done()
				poke(s.fwdDied)
			}()
		}
	}
	s.setChecks(applied, w.checkTargets)
	return failed
}

// removeForward takes a forward off the connection.
func (s *session) removeForward(ctx context.Context, m *openssh.Master, key string, applied map[string]*appliedForward) {
	af := applied[key]
	delete(applied, key)
	if af.runner != nil {
		af.runner.Stop()
	}
	if af.ssh.Kind == 0 || m == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	if af.gpgKind != "" {
		s.releaseGPG(cctx, m, map[string]*appliedForward{key: af})
	}
	if err := m.Cancel(cctx, af.ssh); err != nil {
		s.log.Warn("cancel forward failed", "forward", key, "err", err)
	}
}

// stopForwards stops what runs besides ssh for every forward, when the
// connection goes (the forwards on it go with it).
func (s *session) stopForwards(applied map[string]*appliedForward) {
	for _, af := range applied {
		if af.runner != nil {
			af.runner.Stop()
		}
	}
	clear(applied)
	s.setChecks(nil, false)
}

// reapForwards notices forwards whose process exited (kubectl port-forward
// when its pod goes away) and marks them failed, so they're added again. It
// says how soon to retry.
func (s *session) reapForwards(ctx context.Context, m *openssh.Master, applied map[string]*appliedForward) (retryIn time.Duration, failed bool) {
	retryIn = forwardRetryInterval
	for _, key := range slices.Sorted(maps.Keys(applied)) {
		af := applied[key]
		if af.runner == nil {
			continue
		}
		select {
		case <-af.runner.Done():
		default:
			continue
		}
		if pf, ok := af.runner.(*kube.PortForward); ok && pf.Uptime() > stableRunner {
			retryIn = time.Second
		}
		err := af.runner.Err()
		s.log.Warn("forward lost", "forward", key, "err", err)
		s.removeForward(ctx, m, key, applied)
		s.setForward(key, nil, fmt.Errorf("%w (retrying)", err), 0, "")
		failed = true
	}
	return retryIn, failed
}

// addWanted sets up one forward: resolving a gpg one, starting a proxy or
// kubectl for tether's own kinds, picking a free port for port 0.
func (s *session) addWanted(ctx context.Context, m *openssh.Master, key string, wf wantForward) (af *appliedForward, port int, resolved string, err error) {
	timeout := controlTimeout
	if wf.spec.Kind == forward.Kube {
		timeout += 30 * time.Second // kubectl finding the pod
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	spec := wf.spec
	switch {
	case wf.gpgKind != "":
		if spec, err = s.resolveGPG(ctx, m, wf.gpgKind); err != nil {
			return nil, 0, "", err
		}
		if _, err := addForward(ctx, m, spec); err != nil {
			return nil, 0, "", err
		}
		return &appliedForward{ssh: spec, actual: spec, noCheck: true, gpgKind: wf.gpgKind}, 0, spec.String(), nil
	case spec.Kind == forward.HTTP:
		return s.startHTTPProxy(ctx, m, key, spec)
	case spec.Kind == forward.Kube:
		return s.startKubeForward(ctx, m, key, wf)
	case m.IsLocal():
		return nil, 0, "", errors.New("only Kubernetes forwards work on the local host")
	}
	if spec.AutoPort() {
		if spec.Listen.Port, err = s.pickPort(key, spec.Listen); err != nil {
			return nil, 0, "", err
		}
		port = spec.Listen.Port
	}
	allocated, err := addForward(ctx, m, spec)
	if err != nil {
		return nil, 0, "", err
	}
	af = &appliedForward{ssh: spec, actual: spec}
	if allocated != 0 {
		// R:0 with a target is cancelled by the port the server picked; a
		// reverse SOCKS proxy on port 0 only by port 0 (OpenSSH's mux).
		af.actual.Listen.Port, port = allocated, allocated
		if !spec.IsReverseSOCKS() {
			af.ssh.Listen.Port = allocated
		}
	}
	return af, port, "", nil
}

// pickPort chooses a free local port for a forward on port 0: the one it
// had before if that's still free, so it stays put across reconnects.
func (s *session) pickPort(key string, e forward.Endpoint) (int, error) {
	s.mu.Lock()
	prev := 0
	if f := s.forwards[key]; f != nil {
		prev = f.lastPort
	}
	s.mu.Unlock()
	if prev != 0 {
		e.Port = prev
		if checkLocalFree(e) == nil {
			return prev, nil
		}
	}
	e.Port = 0
	l, err := net.Listen("tcp", localAddr(e))
	if err != nil {
		return 0, fmt.Errorf("no free local port: %w", unwrapSyscall(err))
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// proxyRunner is tether's HTTP proxy, serving on its listener.
type proxyRunner struct {
	l    net.Listener
	done chan struct{}
}

func (p *proxyRunner) Stop()                 { p.l.Close(); <-p.done }
func (p *proxyRunner) Done() <-chan struct{} { return p.done }
func (p *proxyRunner) Err() error            { return errors.New("proxy stopped") }

// startHTTPProxy serves an HTTP (and SOCKS5) proxy here that connects out
// through a hidden dynamic forward on the master.
func (s *session) startHTTPProxy(ctx context.Context, m *openssh.Master, key string, spec forward.Spec) (*appliedForward, int, string, error) {
	if m.IsLocal() {
		return nil, 0, "", errors.New("proxies need an SSH connection; the local host has none")
	}
	listen, port := spec.Listen, 0
	if spec.AutoPort() {
		var err error
		if listen.Port, err = s.pickPort(key, listen); err != nil {
			return nil, 0, "", err
		}
		port = listen.Port
	}
	if err := checkLocalFree(listen); err != nil {
		return nil, 0, "", err
	}
	socks := forward.Spec{Kind: forward.Dynamic, Listen: forward.Endpoint{Host: "127.0.0.1"}}
	var err error
	if socks.Listen.Port, err = s.pickPort("", socks.Listen); err != nil {
		return nil, 0, "", err
	}
	if _, err := addForward(ctx, m, socks); err != nil {
		return nil, 0, "", fmt.Errorf("starting the SOCKS proxy behind it: %w", err)
	}
	l, err := net.Listen("tcp", localAddr(listen))
	if err != nil {
		m.Cancel(ctx, socks)
		return nil, 0, "", fmt.Errorf("local port %d is unavailable: %w", listen.Port, unwrapSyscall(err))
	}
	r := &proxyRunner{l: l, done: make(chan struct{})}
	p := &forward.Proxy{SOCKS: net.JoinHostPort("127.0.0.1", strconv.Itoa(socks.Listen.Port)), Log: s.log.With("forward", key)}
	go func() {
		p.Serve(l)
		close(r.done)
	}()
	actual := spec
	actual.Listen = listen
	return &appliedForward{ssh: socks, actual: actual, runner: r}, port, "", nil
}

// startKubeForward runs kubectl port-forward on the host and brings its
// port here. On the local host kubectl listens on the forward's port itself.
func (s *session) startKubeForward(ctx context.Context, m *openssh.Master, key string, wf wantForward) (*appliedForward, int, string, error) {
	spec, ref := wf.spec, wf.spec.KubeRef()
	listen, port := spec.Listen, 0
	if spec.AutoPort() {
		var err error
		if listen.Port, err = s.pickPort(key, listen); err != nil {
			return nil, 0, "", err
		}
		port = listen.Port
	}
	if err := checkLocalFree(listen); err != nil {
		return nil, 0, "", err
	}
	log := s.log.With("forward", key)
	actual := spec
	actual.Listen = listen
	if m.IsLocal() {
		bind := listen.Host
		switch bind {
		case "":
			bind = "localhost"
		case "*":
			bind = "0.0.0.0"
		}
		pf, err := kube.StartPortForward(ctx, m, wf.kube, ref, bind, listen.Port, spec.Target.Port, log)
		if err != nil {
			return nil, 0, "", err
		}
		return &appliedForward{actual: actual, runner: pf}, port, "", nil
	}
	pf, err := kube.StartPortForward(ctx, m, wf.kube, ref, "127.0.0.1", 0, spec.Target.Port, log)
	if err != nil {
		return nil, 0, "", err
	}
	l := forward.Spec{Kind: forward.Local, Listen: listen, Target: forward.Endpoint{Host: "127.0.0.1", Port: pf.Port}}
	if _, err := addForward(ctx, m, l); err != nil {
		pf.Stop()
		return nil, 0, "", err
	}
	return &appliedForward{ssh: l, actual: actual, runner: pf}, port, "", nil
}

// connectAddress is where to connect to a forward listening here:
// "localhost:5432", a bind address and port, or a socket path. It's empty
// for forwards listening on the remote, and for port 0 before it's picked.
func connectAddress(spec forward.Spec) string {
	if !spec.ListensLocally() {
		return ""
	}
	e := spec.Listen
	switch {
	case e.IsSocket():
		return e.Socket
	case e.Port == 0:
		return ""
	case e.Host == "" || e.Host == "*" || e.Host == "0.0.0.0" || e.Host == "::":
		return "localhost:" + strconv.Itoa(e.Port)
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// checkable reports whether a forward's target can be checked: a forward
// listening here (through it), or one reaching a target here (directly).
// Proxies have no single target.
func checkable(spec forward.Spec) bool {
	return !spec.IsProxy() && spec.Kind != 0
}

// setChecks records which forwards' targets to check, as applied, and
// checks new ones at once.
func (s *session) setChecks(applied map[string]*appliedForward, enabled bool) {
	checks := map[string]forward.Spec{}
	if enabled {
		for key, af := range applied {
			if checkable(af.actual) && !af.noCheck {
				checks[key] = af.actual
			}
		}
	}
	s.mu.Lock()
	added := false
	for key, spec := range checks {
		if s.checks[key] != spec {
			added = true
		}
	}
	for key, f := range s.forwards {
		if _, ok := checks[key]; !ok {
			f.target, f.targetErr = "", ""
		}
	}
	s.checks = checks
	s.mu.Unlock()
	if added {
		poke(s.checkNow)
	}
}

// checkLoop checks forward targets every checkInterval, and when asked.
func (s *session) checkLoop(ctx context.Context) {
	tick := time.NewTicker(checkInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.checkNow:
		}
		s.mu.Lock()
		checks := maps.Clone(s.checks)
		s.mu.Unlock()
		var wg sync.WaitGroup
		for key, spec := range checks {
			wg.Go(func() {
				errMsg := probeTarget(ctx, spec)
				s.mu.Lock()
				f := s.forwards[key]
				if f == nil || s.checks[key] != spec {
					s.mu.Unlock()
					return // changed meanwhile
				}
				prev := f.target + f.targetErr
				if errMsg == "" {
					f.target, f.targetErr = "ok", ""
				} else {
					f.target, f.targetErr = "unreachable", errMsg
				}
				changed := prev != f.target+f.targetErr
				s.mu.Unlock()
				if changed {
					if errMsg != "" {
						s.log.Warn("forward target unreachable", "forward", key, "err", errMsg)
					}
					s.changed()
				}
			})
		}
		wg.Wait()
	}
}

// probeTarget checks that a forward's target answers, returning why not.
//
// A forward reaching a target here (R) is checked by connecting to the
// target. One listening here (L, K) is checked through the forward: ssh (or
// kubectl) accepts the connection, then closes it at once if it can't reach
// the target. A target that answers, or says nothing but keeps the
// connection open, counts as reachable; so does one that drops packets
// (the connection attempt just waits), which this can't tell apart.
func probeTarget(ctx context.Context, spec forward.Spec) string {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var d net.Dialer
	if spec.Kind == forward.Remote {
		network, addr := "tcp", net.JoinHostPort(spec.Target.Host, strconv.Itoa(spec.Target.Port))
		if spec.Target.IsSocket() {
			network, addr = "unix", spec.Target.Socket
		}
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return fmt.Sprintf("nothing answering at %s here: %v", spec.Target, unwrapSyscall(err))
		}
		c.Close()
		return ""
	}
	network, addr := "tcp", localAddr(spec.Listen)
	if spec.Listen.IsSocket() {
		network, addr = "unix", spec.Listen.Socket
	}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return fmt.Sprintf("can't connect to the forward at %s: %v", addr, unwrapSyscall(err))
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(checkTimeout))
	var buf [1]byte
	n, err := c.Read(buf[:])
	var nerr net.Error
	if n > 0 || errors.As(err, &nerr) && nerr.Timeout() {
		return ""
	}
	target := spec.Target.String()
	if spec.Kind == forward.Kube {
		target = fmt.Sprintf("%s port %d", spec.KubeRef().Object(), spec.Target.Port)
	}
	return "connections are closed at once: nothing is answering at " + target
}

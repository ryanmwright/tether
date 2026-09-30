package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/gpg"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/rpc"
)

// Several machines can forward gpg-agent to the same host; gpg there uses
// the one holding the standard socket path (see the gpg package). A gpg
// forward is up once this machine's socket is on the host, holding the path
// or not; syncGPG decides whether it should.

var (
	// gpgCheckInterval is how often to check who holds the gpg sockets.
	gpgCheckInterval = 30 * time.Second
	// gpgGrace is how long the machine holding them may go without
	// answering (a network blip, say) before one standing by takes over.
	gpgGrace = 60 * time.Second
)

// Taking the sockets back from the host's own gpg-agent this many times in
// ownAgentWindow means something there keeps starting it (see `tether
// doctor`), so tether stops and leaves it to the user.
const (
	ownAgentLimit  = 3
	ownAgentWindow = 10 * time.Minute
)

// gpgRuntime is what syncGPG keeps between checks, on the run goroutine.
type gpgRuntime struct {
	deadSince map[string]time.Time // by kind: since when the holder hasn't answered
	ownAgent  []time.Time          // when the sockets were taken back from the host's own agent
}

// claimGPG takes this host's gpg sockets at the next sync, from any machine:
// the user asked for this host here.
func (s *session) claimGPG() {
	s.mu.Lock()
	s.gpgClaim = true
	s.mu.Unlock()
	poke(s.wake)
}

func (s *session) takeGPGClaim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim := s.gpgClaim
	s.gpgClaim = false
	return claim
}

// gpgScript runs gpg.RemoteScript in mode for kinds.
func (s *session) gpgScript(ctx context.Context, m *openssh.Master, mode string, kinds ...string) (map[string]gpg.Socket, error) {
	out, err := m.Run(ctx, gpg.RemoteScript, gpg.RemoteCommand(mode, s.gpgID, kinds...))
	if err != nil {
		return nil, err
	}
	socks := gpg.ParseSockets(string(out))
	for _, kind := range kinds {
		if socks[kind].Path == "" {
			return nil, fmt.Errorf("remote gpgconf reported no %s socket", kind)
		}
	}
	return socks, nil
}

// syncGPG checks who holds the gpg forwards' sockets on the host, and takes
// them when this machine should: when claim is set, when nothing holds
// them, when the host's own agent does, or when the machine holding them
// hasn't answered for gpgGrace. Otherwise the forward stays up, standing by,
// and shows who holds them. It reports whether a forward must be added
// again.
func (s *session) syncGPG(ctx context.Context, m *openssh.Master, w want, applied map[string]*appliedForward, claim bool, rt *gpgRuntime) (failed bool) {
	keys := map[string]string{} // kind -> forward key
	for key, af := range applied {
		if af.gpgKind != "" {
			keys[af.gpgKind] = key
		}
	}
	if claim {
		// Keep the request for gpg forwards not added yet.
		for key, wf := range w.forwards {
			if _, ok := applied[key]; wf.gpgKind != "" && !ok {
				s.mu.Lock()
				s.gpgClaim = true
				s.mu.Unlock()
				break
			}
		}
	}
	if len(keys) == 0 {
		return false
	}
	kinds := slices.Sorted(maps.Keys(keys))
	cctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	socks, err := s.gpgScript(cctx, m, gpg.ModeStatus, kinds...)
	if err != nil {
		s.log.Warn("checking the remote gpg sockets failed", "err", err)
		return false
	}
	if rt.deadSince == nil {
		rt.deadSince = map[string]time.Time{}
	}
	now := time.Now()
	var take, fromOwnAgent []string
	for _, kind := range kinds {
		sock, key := socks[kind], keys[kind]
		if !sock.Bound {
			s.log.Warn("remote gpg socket went away; adding the forward again", "forward", key)
			s.removeForward(ctx, m, key, applied)
			s.setForward(key, nil, errors.New("its socket on the host went away (retrying)"), 0, "")
			delete(keys, kind)
			failed = true
			continue
		}
		if sock.Holder == s.gpgID || sock.Holder == "" || sock.Alive {
			delete(rt.deadSince, kind)
		}
		switch {
		case sock.Holder == s.gpgID:
		case claim, sock.Holder == "":
			take = append(take, kind)
		case sock.Holder == gpg.HolderOther:
			fromOwnAgent = append(fromOwnAgent, kind)
		case !sock.Alive:
			if rt.deadSince[kind].IsZero() {
				rt.deadSince[kind] = now
			}
			if now.Sub(rt.deadSince[kind]) >= gpgGrace {
				s.log.Info("the machine holding the gpg socket stopped answering; taking over",
					"socket", kind, "from", gpg.HolderName(sock.Holder))
				take = append(take, kind)
			}
		}
	}
	if len(fromOwnAgent) > 0 {
		rt.ownAgent = slices.DeleteFunc(rt.ownAgent, func(t time.Time) bool { return now.Sub(t) > ownAgentWindow })
		if len(rt.ownAgent) < ownAgentLimit {
			rt.ownAgent = append(rt.ownAgent, now)
			take = append(take, fromOwnAgent...)
		} else if len(rt.ownAgent) == ownAgentLimit {
			rt.ownAgent = append(rt.ownAgent, now) // warn once per window
			s.log.Warn("the host keeps starting its own gpg-agent; not taking its socket back again (see tether doctor)")
		}
	}
	if len(take) > 0 {
		slices.Sort(take)
		claimed, err := s.gpgScript(cctx, m, gpg.ModeClaim, take...)
		if err != nil {
			s.log.Warn("taking the remote gpg sockets failed", "err", err)
		} else {
			maps.Copy(socks, claimed)
			for _, kind := range take {
				delete(rt.deadSince, kind)
			}
			s.log.Info("gpg on the host now uses this machine's agent", "sockets", take)
		}
	}

	s.mu.Lock()
	changed := false
	for kind, key := range keys {
		f := s.forwards[key]
		if f == nil || f.state != api.StateUp {
			continue
		}
		usedBy := ""
		if h := socks[kind].Holder; h != "" && h != s.gpgID {
			usedBy = gpg.HolderName(h)
		}
		if f.usedBy != usedBy {
			if usedBy != "" {
				s.log.Info("gpg on the host uses another agent", "forward", key, "by", usedBy)
			}
			f.usedBy, changed = usedBy, true
		}
	}
	s.mu.Unlock()
	if changed {
		s.changed()
	}
	return failed
}

// releaseGPG passes the host's gpg sockets held by applied gpg forwards to
// another machine's live forward, or removes them, before the forwards go.
func (s *session) releaseGPG(ctx context.Context, m *openssh.Master, applied map[string]*appliedForward) {
	var kinds []string
	for _, af := range applied {
		if af.gpgKind != "" {
			kinds = append(kinds, af.gpgKind)
		}
	}
	if len(kinds) == 0 {
		return
	}
	slices.Sort(kinds)
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	socks, err := s.gpgScript(ctx, m, gpg.ModeRelease, kinds...)
	if err != nil {
		s.log.Warn("releasing the remote gpg sockets failed", "err", err)
		return
	}
	for _, kind := range kinds {
		if h := socks[kind].Holder; h != "" {
			s.log.Info("gpg socket on the host left to another agent", "socket", kind, "to", gpg.HolderName(h))
		}
	}
}

func (d *Daemon) handleGPGClaim(_ context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.GPGClaimParams](params)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.sessions[p.Host]
	if _, ok := d.host(p.Host); !ok || h == nil {
		return nil, rpc.Errorf(api.CodeNotFound, "no host named %q", p.Host)
	}
	gpgOn := false
	for _, wf := range h.s.currentWant().forwards {
		gpgOn = gpgOn || wf.gpgKind != ""
	}
	if !gpgOn {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "gpg-agent isn't forwarded to %s; turn it on with `tether gpg on %s`", p.Host, p.Host)
	}
	h.s.claimGPG()
	d.log.Info("gpg claimed", "host", p.Host)
	return api.TargetResult{Name: p.Host, Kind: api.TargetHost, Host: p.Host, Generation: d.gen.Load()}, nil
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/authkeys"
	"github.com/ryanmwright/tether/internal/config"
	"github.com/ryanmwright/tether/internal/openssh"
)

var (
	// authKeyLease is how long a host accepts a lent key without its lease
	// being renewed: how long it may outlive this machine going away.
	authKeyLease = 15 * time.Minute
	// authKeyRenew is how often the lease is renewed.
	authKeyRenew = 5 * time.Minute
	// keyFetchMaxAge is how long a key read from another machine is used
	// before it's read again. A failed read keeps the last one.
	keyFetchMaxAge = time.Hour
)

// errKeyPending is a key being read from another machine for the first
// time.
var errKeyPending = errors.New("reading the key")

type wantAuthKey struct {
	src      config.AuthorizedKey // File is absolute
	profiles []string
}

type authKeyState struct {
	state       api.State
	err         string
	fingerprint string
	comment     string
	present     bool
	expires     time.Time
}

// authKeyRuntime is what the session's run loop knows about lent keys on
// the current connection.
type authKeyRuntime struct {
	applied []string  // "fingerprint options" of what the last sync lent, sorted
	synced  time.Time // when; zero before the first sync on this connection
	failed  bool      // the last sync failed
}

// profileAuthKeys is every key a profile lends, by ID.
func profileAuthKeys(p config.Profile, home string) map[string]wantAuthKey {
	keys := map[string]wantAuthKey{}
	for _, a := range p.AuthorizedKeys {
		id := a.ID()
		if rest, ok := strings.CutPrefix(a.File, "~/"); ok {
			a.File = filepath.Join(home, rest)
		}
		keys[id] = wantAuthKey{src: a}
	}
	return keys
}

// resolveAuthKey finds the key a source names: from the config, a file
// here, or another machine (through f, which reads it in the background).
func resolveAuthKey(src config.AuthorizedKey, f *keyFetcher) (authkeys.Key, error) {
	var (
		text string
		err  error
	)
	switch {
	case src.Key != "":
		text = src.Key
	case src.File != "":
		var data []byte
		data, err = os.ReadFile(src.File)
		text = string(data)
	default:
		text, err = f.get(src.Host, src.Path)
	}
	if err != nil {
		return authkeys.Key{}, err
	}
	k, err := authkeys.Parse(text)
	if err != nil {
		return authkeys.Key{}, err
	}
	k.From, k.Restrict = src.From, src.Restrict
	return k, nil
}

// syncAuthKeys lends the host exactly the wanted keys, renewing the lease
// once it's due. It reports whether anything failed, so the caller can
// schedule a retry.
func (s *session) syncAuthKeys(ctx context.Context, m *openssh.Master, w want, rt *authKeyRuntime) (failed bool) {
	if len(w.authKeys) == 0 && len(rt.applied) == 0 {
		return false
	}
	var (
		keys    []authkeys.Key
		lent    = map[string]authkeys.Key{} // by want ID
		applied []string
	)
	for _, id := range slices.Sorted(maps.Keys(w.authKeys)) {
		k, err := resolveAuthKey(w.authKeys[id].src, s.keys)
		if err != nil {
			if !errors.Is(err, errKeyPending) {
				failed = true
			}
			s.setAuthKey(id, err, k, authkeys.Result{})
			continue
		}
		lent[id] = k
		entry := k.Fingerprint + " " + k.From + " " + fmt.Sprint(k.Restrict)
		if !slices.Contains(applied, entry) {
			keys = append(keys, k)
			applied = append(applied, entry)
		}
	}
	slices.Sort(applied)
	if !rt.failed && slices.Equal(applied, rt.applied) && !rt.synced.IsZero() && time.Since(rt.synced) < authKeyRenew {
		return failed
	}

	cctx, cancel := context.WithTimeout(ctx, controlTimeout)
	out, err := m.Run(cctx, authkeys.Script, authkeys.SyncCommand(s.gpgID, int(authKeyLease/time.Second), keys))
	cancel()
	if err != nil {
		err = fmt.Errorf("updating authorized_keys: %w", err)
		if !rt.failed {
			s.log.Warn("lending keys failed", "err", err)
		}
		rt.failed = true
		for id, k := range lent {
			s.setAuthKey(id, err, k, authkeys.Result{})
		}
		return true
	}
	res := authkeys.ParseResult(string(out))
	if !slices.Equal(applied, rt.applied) || rt.failed {
		s.log.Info("lent keys", "keys", len(keys), "expires", res.Expires.Format(time.DateTime))
	}
	rt.applied, rt.synced, rt.failed = applied, time.Now(), false
	for id, k := range lent {
		s.setAuthKey(id, nil, k, res)
	}
	return failed
}

// releaseAuthKeys takes back every key lent over m.
func (s *session) releaseAuthKeys(ctx context.Context, m *openssh.Master, rt *authKeyRuntime) {
	defer func() { *rt = authKeyRuntime{} }()
	if len(rt.applied) == 0 && !rt.failed {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	if _, err := m.Run(ctx, authkeys.Script, authkeys.ReleaseCommand(s.gpgID)); err != nil {
		s.log.Warn("taking back lent keys failed; they expire with their lease", "err", err)
		return
	}
	s.log.Info("took back lent keys")
}

// setAuthKey records the outcome for one wanted key.
func (s *session) setAuthKey(id string, err error, k authkeys.Key, res authkeys.Result) {
	s.mu.Lock()
	if ks := s.authKeyStates[id]; ks != nil {
		*ks = authKeyState{fingerprint: k.Fingerprint, comment: k.Comment}
		switch {
		case errors.Is(err, errKeyPending):
			ks.state = api.StatePending
		case err != nil:
			ks.state, ks.err = api.StateError, err.Error()
		default:
			ks.state, ks.present = api.StateUp, res.Present[k.Fingerprint]
			if !ks.present {
				ks.expires = res.Expires
			}
		}
	}
	s.mu.Unlock()
	s.changed()
}

// authKeyStatuses reports lent keys for status. Callers hold s.mu.
func (s *session) authKeyStatuses() []api.AuthKeyStatus {
	var out []api.AuthKeyStatus
	for _, id := range slices.Sorted(maps.Keys(s.authKeyStates)) {
		ks, wk := s.authKeyStates[id], s.want.authKeys[id]
		var opts []string
		if wk.src.From != "" {
			opts = append(opts, "from="+wk.src.From)
		}
		if wk.src.Restrict {
			opts = append(opts, "restrict")
		}
		var expires *time.Time
		if !ks.expires.IsZero() {
			expires = &ks.expires
		}
		out = append(out, api.AuthKeyStatus{
			Source:      wk.src.Source(),
			Options:     strings.Join(opts, " "),
			Fingerprint: ks.fingerprint,
			Comment:     ks.comment,
			Profiles:    wk.profiles,
			State:       ks.state,
			Present:     ks.present,
			Error:       ks.err,
			Expires:     expires,
		})
	}
	return out
}

// keyFetcher reads public keys from other machines, each over a connection
// of its own, in the background, and keeps them.
type keyFetcher struct {
	ssh     openssh.Options
	changed func() // a read finished
	retry   time.Duration

	mu      sync.Mutex
	entries map[[2]string]*fetchEntry // by destination and path
}

type fetchEntry struct {
	key  string // the last key read; empty until one is
	err  error  // the last read's error
	at   time.Time
	busy bool
}

func newKeyFetcher(ssh openssh.Options, changed func()) *keyFetcher {
	return &keyFetcher{ssh: ssh, changed: changed, retry: forwardRetryInterval, entries: map[[2]string]*fetchEntry{}}
}

// get returns the key at path on dest: the last one read, if any, else the
// last read's error, or errKeyPending while the first read runs. It starts
// a read when there's none yet, the last one failed a while ago, or the key
// is older than keyFetchMaxAge.
func (f *keyFetcher) get(dest, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[[2]string{dest, path}]
	if e == nil {
		e = &fetchEntry{}
		f.entries[[2]string{dest, path}] = e
	}
	age := time.Since(e.at)
	if !e.busy && (e.at.IsZero() || e.err != nil && age >= f.retry || age >= keyFetchMaxAge) {
		e.busy = true
		go f.fetch(dest, path, e)
	}
	switch {
	case e.key != "":
		return e.key, nil
	case e.err != nil:
		return "", fmt.Errorf("can't read the key from %s: %w", dest, e.err)
	default:
		return "", errKeyPending
	}
}

func (f *keyFetcher) fetch(dest, path string, e *fetchEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	out, err := openssh.RunOnce(ctx, f.ssh, dest, authkeys.FetchCommand(path))
	cancel()
	if err == nil {
		_, err = authkeys.Parse(string(out))
	}
	f.mu.Lock()
	e.busy, e.at, e.err = false, time.Now(), err
	if err == nil {
		e.key = strings.TrimSpace(string(out))
	}
	f.mu.Unlock()
	f.changed()
}

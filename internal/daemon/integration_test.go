package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/rpc"
	"github.com/ryanmwright/tether/internal/sshtest"
)

// sshHarness is a daemon wired to a throwaway sshd, reachable as host "dev".
type sshHarness struct {
	*harness
	srv *sshtest.Server
	c   *rpc.Client
}

func startWithSSH(t *testing.T, extraTOML string) *sshHarness {
	t.Helper()
	srv := sshtest.Start(t)
	cfg := fmt.Sprintf("[defaults]\nreconnect_backoff = \"100ms..500ms\"\n\n[hosts.dev]\nssh = %q\n%s", sshtest.HostAlias, extraTOML)
	h := start(t, cfg, openssh.Options{ConfigFile: srv.ConfigFile})
	return &sshHarness{harness: h, srv: srv, c: h.client(t)}
}

func (h *sshHarness) call(t *testing.T, method string, params, result any) {
	t.Helper()
	if err := h.c.Call(context.Background(), method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func (h *sshHarness) status(t *testing.T) api.Status {
	t.Helper()
	var st api.Status
	h.call(t, api.MethodStatus, nil, &st)
	return st
}

// waitFor polls status until cond holds.
func (h *sshHarness) waitFor(t *testing.T, what string, cond func(api.Status) bool) api.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := h.status(t)
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			b, _ := json.MarshalIndent(st, "", "  ")
			t.Fatalf("timed out waiting for %s; status:\n%s", what, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func host(st api.Status, name string) api.HostStatus {
	for _, h := range st.Hosts {
		if h.Name == name {
			return h
		}
	}
	return api.HostStatus{}
}

func profile(st api.Status, name string) api.ProfileStatus {
	for _, p := range st.Profiles {
		if p.Name == name {
			return p
		}
	}
	return api.ProfileStatus{}
}

func fwd(st api.Status, hostName, spec string) api.ForwardStatus {
	for _, f := range host(st, hostName).Forwards {
		if f.Spec == spec {
			return f
		}
	}
	return api.ForwardStatus{}
}

func echoWorks(port int) bool {
	got, err := sshtest.Echo("tcp", fmt.Sprintf("127.0.0.1:%d", port), "ping")
	return err == nil && got == "ping"
}

func TestProfileUpDown(t *testing.T) {
	echo := sshtest.EchoServer(t)
	lport, rport := sshtest.FreePort(t), sshtest.FreePort(t)
	h := startWithSSH(t, fmt.Sprintf(`
[profiles.work]
host = "dev"
forwards = ["L:127.0.0.1:%d:127.0.0.1:%d", "R:127.0.0.1:%d:127.0.0.1:%d"]
`, lport, echo, rport, echo))

	if st := h.status(t); host(st, "dev").State != api.StateDown || profile(st, "work").Active {
		t.Fatalf("expected everything down initially: %+v", st)
	}

	var res api.TargetResult
	h.call(t, api.MethodUp, api.TargetParams{Name: "work"}, &res)
	if res.Kind != api.TargetProfile || res.Host != "dev" {
		t.Errorf("up result = %+v", res)
	}
	st := h.waitFor(t, "profile up", func(st api.Status) bool { return profile(st, "work").State == api.StateUp })
	if f := fwd(st, "dev", fmt.Sprintf("L:127.0.0.1:%d:127.0.0.1:%d", lport, echo)); f.State != api.StateUp || len(f.Profiles) != 1 || f.Profiles[0] != "work" {
		t.Errorf("L forward status = %+v", f)
	}
	if !echoWorks(lport) || !echoWorks(rport) {
		t.Fatal("forwards not passing traffic")
	}

	h.call(t, api.MethodDown, api.TargetParams{Name: "work"}, nil)
	h.waitFor(t, "host down", func(st api.Status) bool {
		return host(st, "dev").State == api.StateDown && len(host(st, "dev").Forwards) == 0
	})
	if echoWorks(lport) {
		t.Error("L forward still open after down")
	}
}

func TestAdHocForward(t *testing.T) {
	echo := sshtest.EchoServer(t)
	lport := sshtest.FreePort(t)
	h := startWithSSH(t, "")

	// Non-canonical input is accepted and normalized.
	var res api.ForwardResult
	h.call(t, api.MethodForwardAdd, api.ForwardParams{Host: "dev", Spec: fmt.Sprintf("l:%d:127.0.0.1:%d", lport, echo)}, &res)
	spec := fmt.Sprintf("L:%d:127.0.0.1:%d", lport, echo)
	if res.Spec != spec {
		t.Errorf("canonical spec = %q", res.Spec)
	}
	h.waitFor(t, "forward up", func(st api.Status) bool {
		f := fwd(st, "dev", spec)
		return f.State == api.StateUp && f.AdHoc
	})
	if !echoWorks(lport) {
		t.Fatal("ad-hoc forward not passing traffic")
	}

	// The host came up only for this forward, so removing it disconnects.
	h.call(t, api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: spec}, nil)
	h.waitFor(t, "host down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })

	err := h.c.Call(context.Background(), api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: spec}, nil)
	if rpcErr, ok := errors.AsType[*rpc.Error](err); !ok || rpcErr.Code != api.CodeNotFound {
		t.Errorf("removing a missing forward: %v", err)
	}
}

func TestReconnectAfterServerRestart(t *testing.T) {
	echo := sshtest.EchoServer(t)
	lport := sshtest.FreePort(t)
	h := startWithSSH(t, "")
	h.call(t, api.MethodForwardAdd, api.ForwardParams{Host: "dev", Spec: fmt.Sprintf("L:%d:127.0.0.1:%d", lport, echo)}, nil)
	h.waitFor(t, "host up", func(st api.Status) bool {
		return host(st, "dev").State == api.StateUp && len(host(st, "dev").Forwards) == 1 && host(st, "dev").Forwards[0].State == api.StateUp
	})

	h.srv.Stop()
	st := h.waitFor(t, "connection lost", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	if hs := host(st, "dev"); hs.RetryAt == nil || !strings.Contains(hs.Error, "connection lost") {
		t.Errorf("after server stop: %+v", hs)
	}

	h.srv.Restart(t)
	h.waitFor(t, "reconnected with forward", func(st api.Status) bool {
		hs := host(st, "dev")
		return hs.State == api.StateUp && len(hs.Forwards) == 1 && hs.Forwards[0].State == api.StateUp
	})
	if !echoWorks(lport) {
		t.Fatal("forward not restored after reconnect")
	}
}

func TestLocalPortInUse(t *testing.T) {
	echo := sshtest.EchoServer(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := busy.Addr().(*net.TCPAddr).Port
	spec := fmt.Sprintf("L:127.0.0.1:%d:127.0.0.1:%d", port, echo)
	h := startWithSSH(t, fmt.Sprintf("[profiles.work]\nhost = \"dev\"\nforwards = [%q]\n", spec))

	h.call(t, api.MethodUp, api.TargetParams{Name: "work"}, nil)
	st := h.waitFor(t, "degraded", func(st api.Status) bool { return profile(st, "work").State == api.StateDegraded })
	if f := fwd(st, "dev", spec); !strings.Contains(f.Error, "unavailable") {
		t.Errorf("forward error = %q", f.Error)
	}
	if hs := host(st, "dev"); hs.State != api.StateDegraded {
		t.Errorf("host state = %s", hs.State)
	}

	// Once the port is free, bringing the profile up again retries it.
	busy.Close()
	h.call(t, api.MethodUp, api.TargetParams{Name: "work"}, nil)
	h.waitFor(t, "profile up", func(st api.Status) bool { return profile(st, "work").State == api.StateUp })
	if !echoWorks(port) {
		t.Fatal("forward not working after retry")
	}
}

func TestReloadChangesForwardsLive(t *testing.T) {
	echo := sshtest.EchoServer(t)
	p1, p2 := sshtest.FreePort(t), sshtest.FreePort(t)
	profileTOML := func(port int) string {
		return fmt.Sprintf("[profiles.work]\nhost = \"dev\"\nautoconnect = true\nforwards = [\"L:%d:127.0.0.1:%d\"]\n", port, echo)
	}
	h := startWithSSH(t, profileTOML(p1))

	// autoconnect brings the profile up at start.
	h.waitFor(t, "profile up", func(st api.Status) bool { return profile(st, "work").State == api.StateUp })
	if !echoWorks(p1) {
		t.Fatal("first forward not working")
	}

	h.write(t, fmt.Sprintf("[defaults]\nreconnect_backoff = \"100ms..500ms\"\n\n[hosts.dev]\nssh = %q\n%s", sshtest.HostAlias, profileTOML(p2)))
	h.call(t, api.MethodReload, nil, nil)
	h.waitFor(t, "new forward up", func(st api.Status) bool {
		return fwd(st, "dev", fmt.Sprintf("L:%d:127.0.0.1:%d", p2, echo)).State == api.StateUp && len(host(st, "dev").Forwards) == 1
	})
	if echoWorks(p1) || !echoWorks(p2) {
		t.Error("reload did not swap the forward")
	}
}

// TestReconnectWhenReachable checks that a host whose SSH port wasn't
// answering (a VM still booting) is reconnected as soon as it answers, not
// at the end of a long backoff.
func TestReconnectWhenReachable(t *testing.T) {
	srv := sshtest.Start(t)
	h := start(t, fmt.Sprintf("[defaults]\nreconnect_backoff = \"30s..60s\"\n[hosts.dev]\nssh = %q\n", sshtest.HostAlias),
		openssh.Options{ConfigFile: srv.ConfigFile})
	sh := &sshHarness{harness: h, srv: srv, c: h.client(t)}

	srv.Stop()
	sh.call(t, api.MethodUp, api.TargetParams{Name: "dev"}, nil)
	st := sh.waitFor(t, "error", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	if at := host(st, "dev").RetryAt; at == nil || time.Until(*at) < 20*time.Second {
		t.Fatalf("retry at %v, want the 30s backoff", at)
	}
	srv.Restart(t)
	sh.waitFor(t, "reconnected before the backoff ended", func(st api.Status) bool { return host(st, "dev").State == api.StateUp })
}

func TestConnectFailure(t *testing.T) {
	srv := sshtest.Start(t)
	h := start(t, fmt.Sprintf("[defaults]\nreconnect_backoff = \"1s..5s\"\n[hosts.dev]\nssh = %q\n", sshtest.HostAlias),
		openssh.Options{ConfigFile: srv.ConfigFile, ExtraArgs: []string{"-o", "User=tether-no-such-user"}})
	sh := &sshHarness{harness: h, srv: srv, c: h.client(t)}

	sh.call(t, api.MethodUp, api.TargetParams{Name: "dev"}, nil)
	st := sh.waitFor(t, "error", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	if hs := host(st, "dev"); !strings.Contains(hs.Error, "Permission denied") || hs.RetryAt == nil {
		t.Errorf("host = %+v", hs)
	}
}

func TestAmbiguousAndUnknownTargets(t *testing.T) {
	h := start(t, "[hosts.x]\nssh = \"x.invalid\"\n[profiles.x]\nhost = \"x\"\n")
	c := h.client(t)
	ctx := context.Background()

	err := c.Call(ctx, api.MethodUp, api.TargetParams{Name: "x"}, nil)
	if rpcErr, ok := errors.AsType[*rpc.Error](err); !ok || rpcErr.Code != api.CodeAmbiguous {
		t.Errorf("ambiguous: %v", err)
	}
	err = c.Call(ctx, api.MethodDown, api.TargetParams{Name: "x", Kind: api.TargetProfile}, nil)
	if err != nil {
		t.Errorf("explicit kind: %v", err)
	}
	err = c.Call(ctx, api.MethodUp, api.TargetParams{Name: "nope"}, nil)
	if rpcErr, ok := errors.AsType[*rpc.Error](err); !ok || rpcErr.Code != api.CodeNotFound {
		t.Errorf("unknown: %v", err)
	}
	err = c.Call(ctx, api.MethodForwardAdd, api.ForwardParams{Host: "x", Spec: "L:bogus"}, nil)
	if rpcErr, ok := errors.AsType[*rpc.Error](err); !ok || rpcErr.Code != rpc.CodeInvalidParams {
		t.Errorf("bad spec: %v", err)
	}
}

func TestSubscribe(t *testing.T) {
	h := startWithSSH(t, "")
	c := h.client(t)
	ctx := context.Background()
	if err := c.Call(ctx, api.MethodSubscribe, nil, nil); err != nil {
		t.Fatal(err)
	}
	next := func() api.Status {
		t.Helper()
		select {
		case n := <-c.Notifications():
			var st api.Status
			if n.Method != api.EventStatus || json.Unmarshal(n.Params, &st) != nil {
				t.Fatalf("bad notification %s %s", n.Method, n.Params)
			}
			return st
		case <-time.After(15 * time.Second):
			t.Fatal("no notification")
			return api.Status{}
		}
	}

	if st := next(); host(st, "dev").State != api.StateDown {
		t.Errorf("initial snapshot: %+v", host(st, "dev"))
	}
	h.call(t, api.MethodUp, api.TargetParams{Name: "dev"}, nil)
	for {
		if st := next(); host(st, "dev").State == api.StateUp {
			break
		}
	}
}

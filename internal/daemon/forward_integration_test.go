package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/kubetest"
	"github.com/ryanmwright/tether/internal/sshtest"
)

// addFwd adds an ad-hoc forward and waits for it to settle, returning its
// status.
func (h *sshHarness) addFwd(t *testing.T, host, spec string) api.ForwardStatus {
	t.Helper()
	var res api.ForwardResult
	h.call(t, api.MethodForwardAdd, api.ForwardParams{Host: host, Spec: spec}, &res)
	st := h.waitFor(t, spec+" up", func(st api.Status) bool {
		f := fwd(st, host, res.Spec)
		return f.State == api.StateUp || f.State == api.StateError
	})
	f := fwd(st, host, res.Spec)
	if f.State != api.StateUp {
		t.Fatalf("%s: %s", spec, f.Error)
	}
	return f
}

// echoVia sends a line through conn and says whether it came back.
func echoVia(c net.Conn) bool {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "ping\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && line == "ping\n"
}

func TestProxies(t *testing.T) {
	h := startWithSSH(t, "")
	echo := fmt.Sprintf("127.0.0.1:%d", sshtest.EchoServer(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A reverse SOCKS proxy on the remote, connecting out from here. The
	// "remote" is this machine too, so it's reachable on 127.0.0.1.
	f := h.addFwd(t, "dev", "back=R:127.0.0.1:0")
	if f.Label != "back" || f.AllocatedPort == 0 || !strings.Contains(f.Description, "connecting out from this machine") {
		t.Errorf("reverse SOCKS = %+v", f)
	}
	c, err := forward.DialSOCKS(ctx, fmt.Sprintf("127.0.0.1:%d", f.AllocatedPort), echo)
	if err != nil || !echoVia(c) {
		t.Fatalf("through the reverse SOCKS proxy: %v", err)
	}

	// The HTTP proxy here, on a port tether picks.
	f = h.addFwd(t, "dev", "H:127.0.0.1:0")
	if f.AllocatedPort == 0 || f.Address != fmt.Sprintf("127.0.0.1:%d", f.AllocatedPort) {
		t.Fatalf("HTTP proxy = %+v", f)
	}
	pc, err := net.Dial("tcp", f.Address)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(pc, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo, echo)
	br := bufio.NewReader(pc)
	if line, _ := br.ReadString('\n'); !strings.Contains(line, " 200 ") {
		t.Fatalf("CONNECT reply %q", line)
	}
	br.ReadString('\n')
	fmt.Fprint(pc, "ping\n")
	if line, _ := br.ReadString('\n'); line != "ping\n" {
		t.Errorf("through CONNECT: %q", line)
	}
	pc.Close()
	// ...which also speaks SOCKS5.
	c, err = forward.DialSOCKS(ctx, f.Address, echo)
	if err != nil || !echoVia(c) {
		t.Fatalf("SOCKS through the HTTP proxy port: %v", err)
	}

	// Removing a forward on port 0 frees what the server picked.
	var rev api.ForwardStatus
	for _, x := range h.status(t).Hosts[0].Forwards {
		if x.Label == "back" {
			rev = x
		}
	}
	h.call(t, api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: "R:127.0.0.1:0"}, nil)
	h.waitFor(t, "reverse proxy gone", func(api.Status) bool {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", rev.AllocatedPort), time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

func TestTargetChecks(t *testing.T) {
	h := startWithSSH(t, "")
	echo := sshtest.EchoServer(t)
	closed := sshtest.FreePort(t)
	good := h.addFwd(t, "dev", fmt.Sprintf("L:0:127.0.0.1:%d", echo))
	bad := h.addFwd(t, "dev", fmt.Sprintf("L:0:127.0.0.1:%d", closed))
	back := h.addFwd(t, "dev", fmt.Sprintf("R:0:127.0.0.1:%d", closed))
	st := h.waitFor(t, "targets checked", func(st api.Status) bool {
		return fwd(st, "dev", good.Spec).Target != "" && fwd(st, "dev", bad.Spec).Target != "" && fwd(st, "dev", back.Spec).Target != ""
	})
	if f := fwd(st, "dev", good.Spec); f.Target != "ok" {
		t.Errorf("reachable target = %+v", f)
	}
	if f := fwd(st, "dev", bad.Spec); f.Target != "unreachable" || !strings.Contains(f.TargetError, "closed at once") {
		t.Errorf("unreachable through L = %+v", f)
	}
	if f := fwd(st, "dev", back.Spec); f.Target != "unreachable" || !strings.Contains(f.TargetError, "nothing answering") {
		t.Errorf("unreachable through R = %+v", f)
	}
	if host(st, "dev").State != api.StateUp {
		t.Errorf("unreachable targets shouldn't degrade the host: %s", host(st, "dev").State)
	}
}

func TestAutoPortKeptAcrossReconnect(t *testing.T) {
	h := startWithSSH(t, "")
	echo := sshtest.EchoServer(t)
	f := h.addFwd(t, "dev", fmt.Sprintf("L:0:127.0.0.1:%d", echo))
	if f.AllocatedPort == 0 || !echoWorks(f.AllocatedPort) {
		t.Fatalf("L:0 = %+v", f)
	}
	h.srv.Stop()
	h.waitFor(t, "connection lost", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	h.srv.Restart(t)
	st := h.waitFor(t, "back up", func(st api.Status) bool { return fwd(st, "dev", f.Spec).State == api.StateUp })
	if again := fwd(st, "dev", f.Spec); again.AllocatedPort != f.AllocatedPort || !echoWorks(again.AllocatedPort) {
		t.Errorf("port after reconnect = %d, before %d", again.AllocatedPort, f.AllocatedPort)
	}
}

func TestShorthandsAndLabelsInProfiles(t *testing.T) {
	echo := sshtest.EchoServer(t)
	// "0:127.0.0.1:PORT" is L:0:127.0.0.1:PORT (the same port here would
	// clash with the echo server, on this same machine).
	h := startWithSSH(t, fmt.Sprintf("[profiles.p]\nhost = \"dev\"\nforwards = [\"echo=0:127.0.0.1:%d\"]\n", echo))
	h.call(t, api.MethodUp, api.TargetParams{Name: "p"}, nil)
	st := h.waitFor(t, "profile up", func(st api.Status) bool { return profile(st, "p").State == api.StateUp })
	f := fwd(st, "dev", fmt.Sprintf("L:0:127.0.0.1:%d", echo))
	if f.Label != "echo" || f.Profiles[0] != "p" {
		t.Errorf("forward = %+v", f)
	}
}

func testKubeForward(t *testing.T, h *sshHarness, fake *kubetest.Fake, hostName string) {
	t.Helper()
	echo := fmt.Sprintf("127.0.0.1:%d", sshtest.EchoServer(t))
	fake.AddTarget(t, "web", "svc", "frontend", echo)
	f := h.addFwd(t, hostName, "web=K:0:web/svc/frontend:80")
	if f.Spec != "K:0:"+kubetest.Context+"/web/svc/frontend:80" || f.Label != "web" || f.Address == "" {
		t.Fatalf("kube forward = %+v", f)
	}
	c, err := net.Dial("tcp", f.Address)
	if err != nil || !echoVia(c) {
		t.Fatalf("through the kube forward: %v", err)
	}

	// The pod goes away: kubectl exits and the forward is marked failed;
	// once it's back, the forward comes back on the same local port.
	fake.RemoveTarget("web", "svc", "frontend")
	h.waitFor(t, "forward lost", func(st api.Status) bool { return fwd(st, hostName, f.Spec).State == api.StateError })
	fake.AddTarget(t, "web", "svc", "frontend", echo)
	h.call(t, api.MethodUp, api.TargetParams{Name: hostName, Kind: api.TargetHost}, nil) // retry now
	st := h.waitFor(t, "forward back", func(st api.Status) bool { return fwd(st, hostName, f.Spec).State == api.StateUp })
	if again := fwd(st, hostName, f.Spec); again.Address != f.Address {
		t.Errorf("address after restart = %s, before %s", again.Address, f.Address)
	}
	c, err = net.Dial("tcp", f.Address)
	if err != nil || !echoVia(c) {
		t.Fatalf("through the restarted kube forward: %v", err)
	}

	// Removing it (without the context) stops kubectl.
	h.call(t, api.MethodForwardRemove, api.ForwardParams{Host: hostName, Spec: "K:0:web/svc/frontend:80"}, nil)
	h.waitFor(t, "forward gone", func(api.Status) bool {
		c, err := net.DialTimeout("tcp", f.Address, time.Second)
		if err == nil {
			c.Close()
		}
		return err != nil
	})
}

func TestKubeForwardViaSSH(t *testing.T) {
	fake := kubetest.Install(t)
	h := startWithMounts(t, "")
	h.call(t, api.MethodUp, api.TargetParams{Name: "dev"}, nil)
	h.waitFor(t, "connected", func(st api.Status) bool { return host(st, "dev").State == api.StateUp })
	testKubeForward(t, h, fake, "dev")
}

func TestKubeForwardLocal(t *testing.T) {
	fake := kubetest.Install(t)
	t.Setenv("TETHER_TEST_LOCAL_HOST", "1")
	hh := start(t, "")
	h := &sshHarness{harness: hh, c: hh.client(t)}
	h.call(t, api.MethodUp, api.TargetParams{Name: "local"}, nil)
	testKubeForward(t, h, fake, "local")
	if err := h.c.Call(context.Background(), api.MethodForwardAdd, api.ForwardParams{Host: "local", Spec: "D:1080"}, nil); err == nil {
		t.Error("SOCKS proxy accepted on the local host")
	}
}

func TestKubeTargets(t *testing.T) {
	kubetest.Install(t)
	t.Setenv("TETHER_TEST_LOCAL_HOST", "1")
	hh := start(t, "")
	h := &sshHarness{harness: hh, c: hh.client(t)}
	var res api.KubeTargetsResult
	h.call(t, api.MethodKubeTargets, api.KubeTargetsParams{Host: "local"}, &res)
	if res.Context != kubetest.Context {
		t.Errorf("kube.targets = %+v", res)
	}
}

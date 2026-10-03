package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/rpc"
	"github.com/ryanmwright/tether/internal/sshtest"
)

func TestAdHocHost(t *testing.T) {
	srv := sshtest.Start(t)
	h := start(t, "[hosts.cfg]\nssh = \"cfg.invalid\"\n[profiles.p]\nhost = \"cfg\"\n", openssh.Options{ConfigFile: srv.ConfigFile})
	sh := &sshHarness{harness: h, srv: srv, c: h.client(t)}
	echo := sshtest.EchoServer(t)
	lport := sshtest.FreePort(t)

	// Not in the config: up says how to add it.
	err := sh.c.Call(context.Background(), api.MethodUp, api.TargetParams{Name: "scratch"}, nil)
	if rpcErr, ok := errors.AsType[*rpc.Error](err); !ok || rpcErr.Code != api.CodeNotFound {
		t.Fatalf("up unknown: %v", err)
	}

	var res api.TargetResult
	sh.call(t, api.MethodHostAdd, api.HostParams{Name: "scratch", SSH: sshtest.HostAlias, DisplayName: "Scratch box"}, &res)
	st := sh.waitFor(t, "ad-hoc host up", func(st api.Status) bool { return host(st, "scratch").State == api.StateUp })
	if hs := host(st, "scratch"); !hs.AdHoc || hs.SSH != sshtest.HostAlias || hs.Title() != "Scratch box" {
		t.Errorf("host = %+v", hs)
	}
	if host(st, "cfg").AdHoc {
		t.Error("config host marked ad-hoc")
	}

	// It works like any host.
	sh.call(t, api.MethodForwardAdd, api.ForwardParams{Host: "scratch", Spec: fmt.Sprintf("L:%d:127.0.0.1:%d", lport, echo)}, nil)
	sh.waitFor(t, "forward up", func(st api.Status) bool {
		f := host(st, "scratch").Forwards
		return len(f) == 1 && f[0].State == api.StateUp
	})
	if !echoWorks(lport) {
		t.Fatal("forward on ad-hoc host not working")
	}
	var doc api.DoctorResult
	sh.call(t, api.MethodDoctor, api.DoctorParams{Host: "scratch"}, &doc)
	if c := findCheck(doc, "connect"); c.Status != api.CheckOK {
		t.Errorf("doctor connect = %+v", c)
	}

	// Down keeps it listed; remove forgets it and closes everything.
	sh.call(t, api.MethodDown, api.TargetParams{Name: "scratch"}, nil)
	sh.waitFor(t, "down", func(st api.Status) bool { return host(st, "scratch").State == api.StateDown })
	sh.call(t, api.MethodHostAdd, api.HostParams{Name: "scratch", SSH: sshtest.HostAlias}, nil)
	sh.waitFor(t, "up again", func(st api.Status) bool { return host(st, "scratch").State == api.StateUp })
	sh.call(t, api.MethodHostRemove, api.HostParams{Name: "scratch"}, nil)
	st = sh.status(t)
	if host(st, "scratch").Name != "" {
		t.Errorf("removed host still listed: %+v", host(st, "scratch"))
	}
	if echoWorks(lport) {
		t.Error("forward still open after the host was removed")
	}
}

func TestAdHocHostErrors(t *testing.T) {
	h := start(t, "[hosts.cfg]\nssh = \"cfg.invalid\"\n[profiles.p]\nhost = \"cfg\"\n")
	c := h.client(t)
	ctx := context.Background()
	for _, p := range []api.HostParams{
		{Name: "evil", SSH: "-oProxyCommand=touch /tmp/pwned"},
		{Name: "bad name"},
		{Name: "cfg"}, // in the config
		{Name: "p"},   // a profile's name
	} {
		if err := c.Call(ctx, api.MethodHostAdd, p, nil); err == nil {
			t.Errorf("host.add %+v accepted", p)
		}
	}
	for _, name := range []string{"cfg", "nope"} {
		if err := c.Call(ctx, api.MethodHostRemove, api.HostParams{Name: name}, nil); err == nil {
			t.Errorf("host.remove %q accepted", name)
		}
	}
}

func TestConfigTakesOverAdHocHost(t *testing.T) {
	h := start(t, "")
	c := h.client(t)
	ctx := context.Background()
	if err := c.Call(ctx, api.MethodHostAdd, api.HostParams{Name: "box", SSH: "box.invalid"}, nil); err != nil {
		t.Fatal(err)
	}
	h.write(t, "[hosts.box]\nssh = \"box2.invalid\"\n")
	if err := c.Call(ctx, api.MethodReload, nil, nil); err != nil {
		t.Fatal(err)
	}
	var st api.Status
	c.Call(ctx, api.MethodStatus, nil, &st)
	if hs := host(st, "box"); hs.AdHoc || hs.SSH != "box2.invalid" || len(st.Hosts) != 1 {
		t.Errorf("after reload: %+v", st.Hosts)
	}
}

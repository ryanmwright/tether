package main

import (
	"strings"
	"testing"

	"github.com/ryanmwright/tether/internal/api"
)

func TestStatusDisplayNames(t *testing.T) {
	st := api.Status{
		Hosts: []api.HostStatus{
			{Name: "devbox", SSH: "devbox", State: api.StateUp, Forwards: []api.ForwardStatus{{Spec: "D:1080", AdHoc: true, State: api.StateUp}}},
			{Name: "192.168.122.10", SSH: "192.168.122.10", State: api.StateUp, Forwards: []api.ForwardStatus{{Spec: "L:5432:localhost:5432", AdHoc: true, State: api.StateUp}}},
		},
		Profiles: []api.ProfileStatus{{Name: "build", Host: "192.168.122.10", State: api.StateUp}},
	}
	render := func(st api.Status) []string {
		var b strings.Builder
		printStatus(&b, &st)
		return strings.Split(b.String(), "\n")
	}
	line := func(lines []string, prefix string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return strings.Join(strings.Fields(l), " ")
			}
		}
		return ""
	}

	// Without display names, nothing changes.
	plain := render(st)
	if got := line(plain, "HOST"); got != "HOST SSH AUTO STATE DETAIL" {
		t.Errorf("header = %q", got)
	}
	if got := line(plain, "L:5432"); !strings.HasPrefix(got, "L:5432:localhost:5432 192.168.122.10 ") {
		t.Errorf("forward = %q", got)
	}

	st.Hosts[1].DisplayName = "Build VM"
	named := render(st)
	if got := line(named, "HOST"); got != "HOST DISPLAY NAME SSH AUTO STATE DETAIL" {
		t.Errorf("header = %q", got)
	}
	if got := line(named, "192.168.122.10"); !strings.HasPrefix(got, "192.168.122.10 Build VM 192.168.122.10 ") {
		t.Errorf("host = %q", got)
	}
	if got := line(named, "devbox"); !strings.HasPrefix(got, "devbox - devbox ") {
		t.Errorf("host without one = %q", got)
	}
	if got := line(named, "L:5432"); !strings.HasPrefix(got, "L:5432:localhost:5432 Build VM ") {
		t.Errorf("forward = %q", got)
	}
	if got := line(named, "build"); !strings.HasPrefix(got, "build Build VM ") {
		t.Errorf("profile = %q", got)
	}

	var b strings.Builder
	printTarget(&b, st, api.TargetResult{Name: "192.168.122.10", Kind: api.TargetHost, Host: "192.168.122.10"})
	if b.String() != "Build VM: up\n" {
		t.Errorf("target = %q", b.String())
	}
}

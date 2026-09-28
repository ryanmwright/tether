package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/sshtest"
	"github.com/ryanmwright/tether/internal/usbip"
)

func usbOf(st api.Status, hostName, device string) api.USBStatus {
	for _, u := range host(st, hostName).USB {
		if u.Device == device {
			return u
		}
	}
	return api.USBStatus{}
}

func localUSBOf(st api.Status, busid string) api.USBDevice {
	for _, d := range st.USB {
		if d.BusID == busid {
			return d
		}
	}
	return api.USBDevice{}
}

// TestUSBAttach shares a real device with the test sshd's "remote", which is
// this machine. It needs root in several places, so it only runs when asked:
//
//	sudo tether usbip-helper --allow-user $USER --socket /tmp/helper.sock &
//	TETHER_TEST_USBIP_HELPER=/tmp/helper.sock TETHER_TEST_USBIP_DEVICE=1-1 go test -run USB ./internal/daemon
//
// with usbip on PATH and passwordless sudo. The device disappears from this
// machine while the test runs.
func TestUSBAttach(t *testing.T) {
	busid := os.Getenv("TETHER_TEST_USBIP_DEVICE")
	if os.Getenv("TETHER_TEST_USBIP_HELPER") == "" || busid == "" {
		t.Skip("set TETHER_TEST_USBIP_HELPER and TETHER_TEST_USBIP_DEVICE to test USB/IP")
	}
	usbipBin, err := exec.LookPath("usbip")
	if err != nil {
		t.Skip("usbip not found")
	}
	path := strings.ReplaceAll(os.Getenv("PATH"), "'", "")
	srv := sshtest.StartWithConfig(t, fmt.Sprintf(`ForceCommand env PATH='%s' /bin/sh -c "$SSH_ORIGINAL_COMMAND"`, path))
	cfg := fmt.Sprintf("[defaults]\nreconnect_backoff = \"100ms..500ms\"\n\n[hosts.dev]\nssh = %q\n", sshtest.HostAlias)
	hh := start(t, cfg, openssh.Options{ConfigFile: srv.ConfigFile})
	h := &sshHarness{harness: hh, srv: srv, c: hh.client(t)}
	t.Cleanup(func() {
		h.c.Call(context.Background(), api.MethodUSBDetach, api.USBParams{Host: "dev", Device: busid}, nil)
	})

	attached := func(what string) {
		t.Helper()
		st := h.waitFor(t, what, func(st api.Status) bool {
			s := usbOf(st, "dev", busid).State
			return s == api.StateUp || s == api.StateError
		})
		if u := usbOf(st, "dev", busid); u.State != api.StateUp {
			t.Fatalf("%s: %+v", what, u)
		}
		if d := localUSBOf(st, busid); d.Host != "dev" {
			t.Errorf("%s: local device = %+v, want it shared with dev", what, d)
		}
		if d, _ := usbip.Read(busid); d.Status != usbip.StatusUsed {
			t.Errorf("%s: stub status = %d, want in use", what, d.Status)
		}
	}

	h.call(t, api.MethodUSBAttach, api.USBParams{Host: "dev", Device: busid}, nil)
	attached("attach")

	// The remote letting go of the device is noticed and undone.
	// usbip port lists the device once the remote has enumerated it.
	port, found := -1, false
	var out []byte
	for deadline := time.Now().Add(10 * time.Second); !found && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		// While the remote enumerates the device, usbip port can fail.
		if out, err = exec.Command("sudo", "-n", usbipBin, "port").CombinedOutput(); err != nil {
			continue
		}
		for line := range strings.Lines(string(out)) {
			line = strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(line, "Port "); ok {
				fmt.Sscanf(rest, "%d:", &port)
			} else if strings.HasSuffix(line, "/"+busid) && strings.Contains(line, "usbip://127.0.0.1:") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("device not in usbip port output:\n%s", out)
	}
	if out, err := exec.Command("sudo", "-n", usbipBin, "detach", "-p", fmt.Sprint(port)).CombinedOutput(); err != nil {
		t.Fatalf("usbip detach: %v: %s", err, out)
	}
	h.waitFor(t, "the drop to be noticed", func(st api.Status) bool {
		d, _ := usbip.Read(busid)
		return usbOf(st, "dev", busid).State == api.StateUp && d.Status == usbip.StatusUsed
	})
	attached("reattach")

	h.call(t, api.MethodUSBDetach, api.USBParams{Host: "dev", Device: busid}, nil)
	st := h.waitFor(t, "detach", func(st api.Status) bool {
		d, _ := usbip.Read(busid)
		return localUSBOf(st, busid).Host == "" && d.Driver != "usbip-host"
	})
	if len(host(st, "dev").USB) != 0 {
		t.Errorf("USB after detach: %+v", host(st, "dev").USB)
	}
}

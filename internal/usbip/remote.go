package usbip

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// findUsbip sets $usbip to the remote's usbip, looking in the sbin
// directories too: Debian installs it in /usr/sbin, which isn't on a
// regular user's PATH.
const findUsbip = `usbip=$(command -v usbip) || for d in /usr/local/sbin /usr/sbin /sbin; do [ -x "$d/usbip" ] && usbip=$d/usbip && break; done
`

// remotePrelude finds usbip on the remote and defines `run`, which runs a
// command as root: directly when already root, else with passwordless sudo.
const remotePrelude = findUsbip + `[ -n "$usbip" ] || { echo "usbip not found on the remote; install it there (Debian: apt install usbip; NixOS: tether.remote.usb.enable)" >&2; exit 3; }
if [ "$(id -u)" = 0 ]; then run() { "$@"; }; else run() { sudo -n "$@"; }; fi
`

// AttachScript attaches bus ID $2 from the USB/IP server on 127.0.0.1:$1.
const AttachScript = remotePrelude + `if [ ! -d /sys/devices/platform/vhci_hcd.0 ]; then
  run modprobe vhci-hcd 2>/dev/null || { echo "the vhci-hcd kernel module isn't loaded on the remote; load it there (modprobe vhci-hcd; NixOS: tether.remote.usb.enable)" >&2; exit 3; }
fi
run "$usbip" --tcp-port "$1" attach -r 127.0.0.1 -b "$2"
`

// PortScript lists the remote's attached devices (`usbip port`).
const PortScript = remotePrelude + `run "$usbip" port`

// DetachScript detaches vhci port $1.
const DetachScript = remotePrelude + `run "$usbip" detach -p "$1"`

// CheckScript reports, without changing anything, what the remote has for
// attaching devices: key=value lines.
const CheckScript = findUsbip + `echo "usbip=$usbip"
[ -d /sys/devices/platform/vhci_hcd.0 ] && echo vhci=yes
[ -e /sys/module/vhci_hcd ] && echo vhci=yes
if [ "$(id -u)" = 0 ]; then echo root=yes
elif [ -n "$usbip" ] && sudo -n -l "$usbip" >/dev/null 2>&1; then echo sudo=yes; fi
`

// RemoteCommand is a command line that runs script under sh with args.
func RemoteCommand(script string, args ...string) string {
	cmd := "sh -c " + shellQuote(script) + " tether-usb"
	for _, a := range args {
		cmd += " " + shellQuote(a)
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// FindPort finds the vhci port where bus ID busid from 127.0.0.1:port is
// attached, in `usbip port` output:
//
//	Port 00: <Port in Use> at Full Speed(12Mbps)
//	       QEMU : unknown product (0627:0001)
//	       3-1 -> usbip://127.0.0.1:41234/1-1
//	           -> remote bus/dev 001/002
func FindPort(output string, port int, busid string) (int, bool) {
	want := fmt.Sprintf("usbip://127.0.0.1:%d/%s", port, busid)
	current := -1
	sc := bufio.NewScanner(strings.NewReader(output))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "Port "); ok {
			num, _, _ := strings.Cut(rest, ":")
			if n, err := strconv.Atoi(num); err == nil {
				current = n
			}
			continue
		}
		if current >= 0 && strings.HasSuffix(line, "-> "+want) {
			return current, true
		}
	}
	return 0, false
}

// RemoteError adds a hint to a failed remote usbip command.
func RemoteError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "password is required") || strings.Contains(msg, "a terminal is required") {
		return fmt.Errorf("%s: usbip needs root on the remote; allow it with passwordless sudo there (NixOS: tether.remote.usb.enable)", msg)
	}
	return err
}

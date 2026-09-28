package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/gpg"
	"github.com/ryanmwright/tether/internal/mount"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/rpc"
	"github.com/ryanmwright/tether/internal/usbip"
)

const doctorTimeout = 30 * time.Second

// doctor collects checks for one host. Nothing it runs changes anything,
// locally or on the remote.
type doctor struct {
	host, dest     string
	useGPG, gpgSSH bool
	mountHere      bool // remote directories mounted on this machine
	mountThere     bool // local directories mounted on the remote
	useUSB         bool // USB devices shared with the host
	usbHelper      func(context.Context) (int, error)
	checks         []api.Check
}

func (dr *doctor) add(section, name string, status api.CheckStatus, detail, fix string) {
	dr.checks = append(dr.checks, api.Check{Section: section, Name: name, Status: status, Detail: detail, Fix: fix})
}

func (d *Daemon) handleDoctor(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.DoctorParams](params)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	host, ok := d.host(p.Host)
	var s *session
	dr := &doctor{host: p.Host, dest: host.SSH, usbHelper: d.usb.Port}
	if ok {
		s = d.sessions[p.Host].s
		// Check gpg if anything could forward it to this host.
		for _, prof := range d.cfg.Profiles {
			if prof.Host == p.Host {
				dr.useGPG = dr.useGPG || prof.GPG || prof.GPGSSH
				dr.gpgSSH = dr.gpgSSH || prof.GPGSSH
			}
		}
		for key := range d.adhoc[p.Host] {
			dr.useGPG = dr.useGPG || key == gpgAgentForward || key == gpgSSHForward
			dr.gpgSSH = dr.gpgSSH || key == gpgSSHForward
		}
		var specs []mount.Spec
		for _, prof := range d.cfg.Profiles {
			if prof.Host == p.Host {
				for _, m := range prof.Mounts {
					specs = append(specs, m.Spec())
				}
			}
		}
		for _, wm := range d.adhocMnt[p.Host] {
			specs = append(specs, wm.spec)
		}
		for _, spec := range specs {
			dr.mountHere = dr.mountHere || spec.Direction == mount.RemoteToLocal
			dr.mountThere = dr.mountThere || spec.Direction == mount.LocalToRemote
		}
		dr.useUSB = len(d.adhocUSB[p.Host]) > 0
		for _, prof := range d.cfg.Profiles {
			dr.useUSB = dr.useUSB || prof.Host == p.Host && len(prof.USB) > 0
		}
	}
	d.mu.Unlock()
	if !ok {
		return nil, rpc.Errorf(api.CodeNotFound, "no host named %q", p.Host)
	}

	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	dr.checkLocal(ctx)

	m := s.liveMaster()
	if m != nil {
		dr.add("connection", "connect", api.CheckOK, "using the running connection to "+dr.dest, "")
	} else {
		ctl := filepath.Join(filepath.Dir(d.opts.SocketPath), "doctor", p.Host)
		os.MkdirAll(filepath.Dir(ctl), 0o700)
		tmp, err := openssh.Start(ctx, d.opts.SSH, dr.dest, ctl, d.log.With("host", p.Host, "doctor", true))
		if err != nil {
			dr.add("connection", "connect", api.CheckFail, err.Error(), connectFix(err.Error(), dr.dest))
			return api.DoctorResult{Host: p.Host, Checks: dr.checks}, nil
		}
		defer tmp.Stop()
		m = tmp
		dr.add("connection", "connect", api.CheckOK, "connected to "+dr.dest, "")
	}
	dr.checkRemote(ctx, m)
	return api.DoctorResult{Host: p.Host, Checks: dr.checks}, nil
}

func (dr *doctor) checkLocal(ctx context.Context) {
	if out, err := exec.CommandContext(ctx, "ssh", "-V").CombinedOutput(); err != nil {
		dr.add("local", "ssh", api.CheckFail, "ssh not found on the daemon's PATH", "install OpenSSH, or add it to the daemon's PATH")
	} else {
		dr.add("local", "ssh", api.CheckOK, strings.TrimSpace(string(out)), "")
	}

	sock := os.Getenv("SSH_AUTH_SOCK")
	switch out, err := exec.CommandContext(ctx, "ssh-add", "-l").CombinedOutput(); {
	case sock == "":
		dr.add("local", "ssh agent", api.CheckWarn, "the daemon has no SSH_AUTH_SOCK",
			"keys with passphrases need an agent; start gpg-agent with enable-ssh-support or ssh-agent, then restart the daemon")
	case err == nil:
		n := len(strings.Split(strings.TrimSpace(string(out)), "\n"))
		dr.add("local", "ssh agent", api.CheckOK, fmt.Sprintf("%s (%d keys)", sock, n), "")
	case exitCode(err) == 1:
		dr.add("local", "ssh agent", api.CheckWarn, sock+" has no keys loaded", "add a key with ssh-add")
	default:
		dr.add("local", "ssh agent", api.CheckFail, fmt.Sprintf("can't reach the agent at %s", sock), "start the agent, or fix SSH_AUTH_SOCK for the daemon")
	}

	dr.checkLocalMounts()
	dr.checkLocalUSB(ctx)
	if !dr.useGPG {
		dr.add("local", "gpg-agent", api.CheckSkip, "no profile on "+dr.host+" forwards gpg", "")
		return
	}
	extra, sshSock, err := gpg.LocalSockets(ctx, dr.gpgSSH)
	if err != nil {
		dr.add("local", "gpg-agent", api.CheckFail, err.Error(), "")
	} else {
		detail := "extra socket " + extra
		if sshSock != "" {
			detail += ", SSH socket " + sshSock
		}
		dr.add("local", "gpg-agent", api.CheckOK, detail, "")
	}
}

func (dr *doctor) checkRemote(ctx context.Context, m *openssh.Master) {
	dr.checkRemoteMounts(ctx, m)
	dr.checkRemoteUSB(ctx, m)
	dr.checkRemoteGPG(ctx, m)
}

// checkLocalUSB checks that the USB/IP helper answers.
func (dr *doctor) checkLocalUSB(ctx context.Context) {
	if !dr.useUSB {
		return
	}
	port, err := dr.usbHelper(ctx)
	if err != nil {
		dr.add("local", "usb", api.CheckFail, err.Error(),
			"set it up with `tether usbip-helper install` (NixOS: services.tether-usbip.enable)")
		return
	}
	dr.add("local", "usb", api.CheckOK, fmt.Sprintf("USB/IP helper running, port %d", port), "")
}

// checkRemoteUSB checks what attaching devices needs on the remote.
func (dr *doctor) checkRemoteUSB(ctx context.Context, m *openssh.Master) {
	if !dr.useUSB {
		return
	}
	out, err := m.Run(ctx, usbip.CheckScript, "sh -s")
	if err != nil {
		dr.add("remote", "usb", api.CheckFail, "inspecting the remote failed: "+err.Error(), "")
		return
	}
	facts := map[string]string{}
	for line := range strings.Lines(string(out)) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		facts[k] = v
	}
	var problems, fixes []string
	if facts["usbip"] == "" {
		problems = append(problems, "usbip not found")
		fixes = append(fixes, "install it (Debian: apt install usbip)")
	}
	if facts["vhci"] != "yes" {
		problems = append(problems, "vhci-hcd module not loaded")
		fixes = append(fixes, "load it at boot: echo vhci-hcd | sudo tee /etc/modules-load.d/vhci-hcd.conf; sudo modprobe vhci-hcd")
	}
	if facts["usbip"] != "" && facts["root"] != "yes" && facts["sudo"] != "yes" {
		problems = append(problems, "no passwordless sudo for usbip")
		fixes = append(fixes, fmt.Sprintf("allow it in sudoers: YOUR-USER ALL=(root) NOPASSWD: %s", facts["usbip"]))
	}
	if len(problems) > 0 {
		dr.add("remote", "usb", api.CheckFail, strings.Join(problems, "; ")+" on "+dr.host,
			strings.Join(fixes, "; ")+" (NixOS: tether.remote.usb.enable)")
		return
	}
	dr.add("remote", "usb", api.CheckOK, "usbip, vhci-hcd and root access available", "")
}

// checkLocalMounts checks what mounting remote directories here needs.
func (dr *doctor) checkLocalMounts() {
	if !dr.mountHere {
		return
	}
	var problems, fixes []string
	if _, err := exec.LookPath("sshfs"); err != nil {
		problems = append(problems, "sshfs not found")
		fixes = append(fixes, "install sshfs (Fedora: fuse-sshfs, Debian: sshfs)")
	}
	if !setuidFusermount() {
		problems = append(problems, "no setuid fusermount3 on the daemon's PATH")
		fixes = append(fixes, "install fuse3 from your distribution; a Nix-built fusermount3 isn't setuid, so put /usr/bin before Nix profiles in the daemon's PATH")
	}
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		problems = append(problems, "can't open /dev/fuse: "+err.Error())
		fixes = append(fixes, "load the fuse kernel module")
	} else {
		f.Close()
	}
	if len(problems) > 0 {
		dr.add("local", "mounts", api.CheckFail, strings.Join(problems, "; "), strings.Join(fixes, "; "))
		return
	}
	dr.add("local", "mounts", api.CheckOK, "sshfs and FUSE available for remote directories", "")
}

// setuidFusermount reports whether a usable (setuid) fusermount is around.
func setuidFusermount() bool {
	for _, name := range []string{"/run/wrappers/bin/fusermount3", "fusermount3", "fusermount"} {
		if p, err := exec.LookPath(name); err == nil {
			if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSetuid != 0 {
				return true
			}
		}
	}
	return false
}

// remoteMountScript reports, without changing anything, what the remote
// has for mounting local directories there.
const remoteMountScript = `echo "sshfs=$(command -v sshfs)"
f=$(command -v fusermount3 || command -v fusermount)
echo "fusermount=$f"
if [ -n "$f" ] && [ -u "$(readlink -f "$f")" ]; then echo setuid=yes; fi
if [ -r /dev/fuse ] && [ -w /dev/fuse ]; then echo fuse=yes; fi
`

// checkRemoteMounts checks what mounting local directories on the remote
// needs there.
func (dr *doctor) checkRemoteMounts(ctx context.Context, m *openssh.Master) {
	if !dr.mountThere {
		return
	}
	out, err := m.Run(ctx, remoteMountScript, "sh -s")
	if err != nil {
		dr.add("remote", "mounts", api.CheckFail, "inspecting the remote failed: "+err.Error(), "")
		return
	}
	facts := map[string]string{}
	for line := range strings.Lines(string(out)) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		facts[k] = v
	}
	var problems []string
	if facts["sshfs"] == "" {
		problems = append(problems, "sshfs not found")
	}
	if facts["fusermount"] == "" || facts["setuid"] != "yes" {
		problems = append(problems, "no setuid fusermount")
	}
	if facts["fuse"] != "yes" {
		problems = append(problems, "/dev/fuse not usable")
	}
	if len(problems) > 0 {
		dr.add("remote", "mounts", api.CheckFail, strings.Join(problems, "; ")+" on "+dr.host,
			"install sshfs and fuse3 there (Debian: apt install sshfs fuse3; NixOS: enable tether.remote)")
		return
	}
	dr.add("remote", "mounts", api.CheckOK, "sshfs and FUSE available for local directories", "")
}

func (dr *doctor) checkRemoteGPG(ctx context.Context, m *openssh.Master) {
	if !dr.useGPG {
		dr.add("remote", "gpg", api.CheckSkip, "no profile on "+dr.host+" forwards gpg", "")
		return
	}
	out, err := m.Run(ctx, gpg.RemoteInfoScript, "sh -s")
	if err != nil {
		dr.add("remote", "gpg", api.CheckFail, "inspecting the remote failed: "+err.Error(), "")
		return
	}
	info := gpg.ParseRemoteInfo(string(out))
	if info.Gpgconf == "" {
		dr.add("remote", "gpg", api.CheckFail, "GnuPG is not installed on "+dr.host,
			"install it (Debian: apt install gnupg; NixOS: enable tether.remote)")
		return
	}
	dr.add("remote", "gpg", api.CheckOK, info.Version, "")
	dr.add("remote", "agent socket", api.CheckOK, info.Dirs["agent-socket"], "")

	if units := info.CompetingUnits(); len(units) > 0 {
		dr.add("remote", "remote gpg-agent", api.CheckWarn,
			"these units can start a gpg-agent on "+dr.host+" that takes over the forwarded socket: "+strings.Join(units, ", "),
			fmt.Sprintf("ssh %s systemctl --user mask --now %s   (NixOS: enable tether.remote)", dr.dest, strings.Join(gpg.AgentUnits(), " ")))
	} else {
		dr.add("remote", "remote gpg-agent", api.CheckOK, "no gpg-agent units will compete for the socket", "")
	}

	if local, err := gpg.LocalSecretKeys(ctx); err != nil {
		dr.add("remote", "public keys", api.CheckWarn, "couldn't list local secret keys: "+err.Error(), "")
	} else {
		var missing []string
		for _, fpr := range local {
			if !info.Fingerprints[fpr] {
				missing = append(missing, fpr)
			}
		}
		switch {
		case len(local) == 0:
			dr.add("remote", "public keys", api.CheckWarn, "no local secret keys to use remotely", "")
		case len(missing) > 0:
			dr.add("remote", "public keys", api.CheckWarn,
				fmt.Sprintf("%d of %d local keys are missing from %s's keyring; gpg there can't use them", len(missing), len(local), dr.host),
				fmt.Sprintf("gpg --export %s | ssh %s gpg --import", strings.Join(missing, " "), dr.dest))
		default:
			dr.add("remote", "public keys", api.CheckOK, fmt.Sprintf("all %d local keys are known on %s", len(local), dr.host), "")
		}
	}

	if dr.gpgSSH {
		dr.add("remote", "ssh socket", api.CheckOK, info.Dirs["agent-ssh-socket"],
			"remote shells need SSH_AUTH_SOCK set to this path: export SSH_AUTH_SOCK=\"$(gpgconf --list-dirs agent-ssh-socket)\" (the NixOS module does this)")
	}
}

// connectFix suggests a fix for a failed connection from ssh's message.
func connectFix(msg, dest string) string {
	switch {
	case strings.Contains(msg, "Host key verification failed"), strings.Contains(msg, "IDENTIFICATION HAS CHANGED"):
		return fmt.Sprintf("run `ssh %s` once and check/accept the host key; the daemon can't prompt", dest)
	case strings.Contains(msg, "Permission denied"):
		return "load the key into your agent (ssh-add); the daemon can't prompt for passphrases or passwords"
	case strings.Contains(msg, "Could not resolve hostname"):
		return "check the host name, or the alias in ~/.ssh/config"
	case strings.Contains(msg, "Connection refused"), strings.Contains(msg, "timed out"), strings.Contains(msg, "No route"):
		return fmt.Sprintf("check that %s is reachable and sshd is running", dest)
	}
	return fmt.Sprintf("try `ssh %s` by hand to see what's wrong", dest)
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

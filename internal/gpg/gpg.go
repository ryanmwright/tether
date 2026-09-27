// Package gpg finds gpg-agent sockets locally and on remote hosts, and
// gathers what `tether doctor` needs to check a host's gpg setup.
package gpg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// Remote socket kinds, as understood by PrepareRemoteScript.
const (
	KindAgent = "agent" // the remote's standard agent socket
	KindSSH   = "ssh"   // the remote's SSH agent socket
)

// ParseDirs parses `gpgconf --list-dirs` output ("name:value" lines, with
// values percent-escaped).
func ParseDirs(out string) map[string]string {
	dirs := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		if v, err := url.PathUnescape(value); err == nil {
			value = v
		}
		dirs[name] = value
	}
	return dirs
}

// LocalSockets starts the local gpg-agent if needed and returns the sockets
// to forward: the restricted "extra" socket, and the SSH socket when
// wantSSH is set.
func LocalSockets(ctx context.Context, wantSSH bool) (extra, ssh string, err error) {
	if _, err := exec.LookPath("gpgconf"); err != nil {
		return "", "", errors.New("gpgconf not found locally; is GnuPG installed?")
	}
	if out, err := exec.CommandContext(ctx, "gpgconf", "--launch", "gpg-agent").CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("starting local gpg-agent: %v: %s", err, strings.TrimSpace(string(out)))
	}
	out, err := exec.CommandContext(ctx, "gpgconf", "--list-dirs").Output()
	if err != nil {
		return "", "", fmt.Errorf("gpgconf --list-dirs: %w", err)
	}
	dirs := ParseDirs(string(out))
	extra = dirs["agent-extra-socket"]
	if !isSocket(extra) {
		return "", "", fmt.Errorf("local gpg-agent has no extra socket at %q; with systemd socket activation, "+
			"enable gpg-agent-extra.socket (home-manager: services.gpg-agent.enableExtraSocket = true), "+
			"otherwise check extra-socket in gpg-agent.conf", extra)
	}
	if wantSSH {
		ssh = dirs["agent-ssh-socket"]
		if !isSocket(ssh) {
			return "", "", fmt.Errorf("local gpg-agent has no SSH socket at %q; add enable-ssh-support to gpg-agent.conf", ssh)
		}
	}
	return extra, ssh, nil
}

func isSocket(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Type() == os.ModeSocket
}

// PrepareRemoteScript runs on the remote under `sh -s -- KIND...`. For each
// kind it prints "kind=path" for the socket to forward to, after clearing
// the way:
//
//   - It stops the remote's own gpg-agent. Merely removing its socket isn't
//     enough: the agent notices, exits, and deletes the socket path on the
//     way out, taking the forward with it. (If the socket is instead a stale
//     forward from an earlier connection, the kill request reaches the local
//     agent's restricted extra socket, which refuses it.)
//   - It removes whatever is left at the path, e.g. a stale socket.
const PrepareRemoteScript = `
command -v gpgconf >/dev/null 2>&1 || { echo "gpgconf not found on the remote; install GnuPG there" >&2; exit 3; }
gpgconf --create-socketdir >/dev/null 2>&1
if command -v timeout >/dev/null 2>&1; then timeout 5 gpgconf --kill gpg-agent; else gpgconf --kill gpg-agent; fi >/dev/null 2>&1
for kind in "$@"; do
  case $kind in
    agent) p=$(gpgconf --list-dirs agent-socket) ;;
    ssh) p=$(gpgconf --list-dirs agent-ssh-socket) ;;
    *) continue ;;
  esac
  [ -n "$p" ] || { echo "gpgconf gave no socket path for $kind" >&2; exit 3; }
  rm -f "$p"
  echo "$kind=$p"
done
`

// ParsePrepareOutput maps each kind to its remote socket path.
func ParsePrepareOutput(out string) map[string]string {
	paths := map[string]string{}
	for line := range strings.Lines(out) {
		if kind, path, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			if p, err := url.PathUnescape(path); err == nil {
				path = p
			}
			paths[kind] = path
		}
	}
	return paths
}

// agentUnits are the systemd user units distributions ship for gpg-agent.
// If any is active on the remote, it can start an agent that takes over the
// forwarded socket.
var agentUnits = []string{
	"gpg-agent.socket", "gpg-agent-extra.socket", "gpg-agent-ssh.socket",
	"gpg-agent-browser.socket", "gpg-agent.service",
}

// RemoteInfoScript runs on the remote under `sh -s` and reports its gpg
// setup as key=value lines. It changes nothing.
var RemoteInfoScript = `
if ! command -v gpgconf >/dev/null 2>&1; then echo "gpgconf="; exit 0; fi
echo "gpgconf=$(command -v gpgconf)"
echo "version=$(gpg --version 2>/dev/null | head -n 1)"
gpgconf --list-dirs | sed 's/^/dir./'
if command -v systemctl >/dev/null 2>&1; then
  for u in ` + strings.Join(agentUnits, " ") + `; do
    echo "unit.$u=$(systemctl --user is-enabled "$u" 2>/dev/null)/$(systemctl --user is-active "$u" 2>/dev/null)"
  done
fi
gpg --list-keys --with-colons 2>/dev/null | awk -F: '$1=="pub"{p=1;next} p&&$1=="fpr"{print "fpr="$10;p=0}'
`

type RemoteInfo struct {
	Gpgconf      string // empty if GnuPG isn't installed
	Version      string
	Dirs         map[string]string
	Units        map[string]UnitState
	Fingerprints map[string]bool // primary keys in the public keyring
}

type UnitState struct {
	Enabled, Active string // as printed by systemctl is-enabled / is-active
}

// Competing reports whether the unit could start a remote gpg-agent. Static
// units (e.g. gpg-agent.service) only start when a socket unit pulls them
// in, so they don't count on their own.
func (u UnitState) Competing() bool {
	return u.Active == "active" || strings.HasPrefix(u.Enabled, "enabled")
}

func ParseRemoteInfo(out string) RemoteInfo {
	info := RemoteInfo{Dirs: map[string]string{}, Units: map[string]UnitState{}, Fingerprints: map[string]bool{}}
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "dir."):
			for k, v := range ParseDirs(strings.TrimPrefix(line, "dir.")) {
				info.Dirs[k] = v
			}
		case strings.HasPrefix(line, "unit."):
			name, state, _ := strings.Cut(strings.TrimPrefix(line, "unit."), "=")
			enabled, active, _ := strings.Cut(state, "/")
			info.Units[name] = UnitState{Enabled: enabled, Active: active}
		default:
			key, value, _ := strings.Cut(line, "=")
			switch key {
			case "gpgconf":
				info.Gpgconf = value
			case "version":
				info.Version = value
			case "fpr":
				info.Fingerprints[value] = true
			}
		}
	}
	return info
}

// CompetingUnits lists the remote's gpg-agent units that could start an
// agent of its own.
func (r RemoteInfo) CompetingUnits() []string {
	var units []string
	for _, u := range agentUnits {
		if s, ok := r.Units[u]; ok && s.Competing() {
			units = append(units, u)
		}
	}
	return units
}

// AgentUnits is every unit to mask so the remote never starts its own agent.
func AgentUnits() []string { return agentUnits }

// LocalSecretKeys returns the fingerprints of local secret (primary) keys.
func LocalSecretKeys(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "gpg", "--list-secret-keys", "--with-colons").Output()
	if err != nil {
		return nil, fmt.Errorf("gpg --list-secret-keys: %w", err)
	}
	var fprs []string
	primary := false
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSpace(line), ":")
		switch {
		case fields[0] == "sec":
			primary = true
		case fields[0] == "fpr" && primary && len(fields) > 9:
			fprs = append(fprs, fields[9])
			primary = false
		}
	}
	return fprs, nil
}

// Package gpg finds gpg-agent sockets locally and on remote hosts, and
// gathers what `tether doctor` needs to check a host's gpg setup.
package gpg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// Remote socket kinds, as understood by RemoteScript.
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

// Several machines can forward their agents to the same remote, but gpg
// there talks to one socket. So each machine's forward gets a path of its
// own beside the standard one (S.gpg-agent.tether.ID), and the standard path
// is a symlink to the forward in use: the machine holding it. Switching
// machines relinks it, and the others' forwards stay up, ready.

// Script modes, as understood by RemoteScript.
const (
	ModeBind    = "bind"    // clear the way for this machine's forward
	ModeStatus  = "status"  // just report
	ModeClaim   = "claim"   // link the standard path to this machine's forward
	ModeRelease = "release" // hand the standard path to another live forward, or remove it
)

// HolderOther is Socket.Holder for a socket that isn't a tether forward:
// normally the remote's own gpg-agent.
const HolderOther = "(remote)"

// RemoteScript runs on the remote under the command RemoteCommand gives.
// Whatever the mode, it then prints each kind's Socket as "KIND.FIELD=VALUE"
// lines. In status mode it changes nothing.
//
//   - bind removes a stale socket of this machine's, so ssh can listen there.
//   - claim first stops the remote's own gpg-agent if it has the standard
//     path. Merely removing its socket isn't enough: the agent notices,
//     exits, and deletes the path on the way out, taking the link with it.
//     (If the path is instead a stale forward from an older tether, the kill
//     request reaches that machine's restricted extra socket, which refuses
//     it.) The link is replaced atomically, so gpg never finds no socket.
//   - release removes this machine's socket and, if it held the standard
//     path, passes it to another machine's live forward or removes it.
//
// A socket is alive if it answers within 3s: one whose machine has gone to
// sleep still accepts a connection (sshd does) but never answers.
const RemoteScript = `
mode=$1 id=$2
shift 2
command -v gpgconf >/dev/null 2>&1 || { echo "gpgconf not found on the remote; install GnuPG there" >&2; exit 3; }
[ "$mode" = status ] || gpgconf --create-socketdir >/dev/null 2>&1
t=
command -v timeout >/dev/null 2>&1 && t="timeout 3"
alive() {
  [ -S "$2" ] || return 1
  case $1 in
    agent) $t gpg-connect-agent --no-autostart --raw-socket "$2" /bye >/dev/null 2>&1 ;;
    ssh) command -v ssh-add >/dev/null 2>&1 || return 0
      SSH_AUTH_SOCK=$2 $t ssh-add -l >/dev/null 2>&1; [ $? -le 1 ] ;;
  esac
}
relink() { ln -sf "$2" "$1.tmp.$$" && mv -f "$1.tmp.$$" "$1"; }
killed=
for kind in "$@"; do
  case $kind in
    agent) p=$(gpgconf --list-dirs agent-socket) ;;
    ssh) p=$(gpgconf --list-dirs agent-ssh-socket) ;;
    *) continue ;;
  esac
  [ -n "$p" ] || { echo "gpgconf gave no socket path for $kind" >&2; exit 3; }
  ours=$p.tether.$id
  case $mode in
    bind) rm -f "$ours" ;;
    claim)
      if [ ! -L "$p" ] && [ -e "$p" ]; then
        if [ -z "$killed" ]; then
          $t gpgconf --kill gpg-agent >/dev/null 2>&1
          killed=1
        fi
        rm -f "$p"
      fi
      relink "$p" "$ours" ;;
    release)
      rm -f "$ours"
      if [ "$(readlink "$p")" = "$ours" ]; then
        next=
        for s in "$p".tether.*; do
          if alive "$kind" "$s"; then next=$s; break; fi
        done
        if [ -n "$next" ]; then relink "$p" "$next"; else rm -f "$p"; fi
      fi ;;
  esac
  holder= ok=0 mine=0
  if [ -L "$p" ]; then
    l=$(readlink "$p")
    case ${l##*/} in
      "${p##*/}".tether.*) holder=${l##*/}; holder=${holder#"${p##*/}".tether.} ;;
      *) holder="` + HolderOther + `" ;;
    esac
  elif [ -e "$p" ]; then
    holder="` + HolderOther + `"
  fi
  [ -n "$holder" ] && alive "$kind" "$p" && ok=1
  [ -S "$ours" ] && mine=1
  echo "$kind.path=$p"
  echo "$kind.ours=$ours"
  echo "$kind.holder=$holder"
  echo "$kind.alive=$ok"
  echo "$kind.bound=$mine"
done
`

// RemoteCommand is the command to run RemoteScript with, on its stdin, for
// machine id.
func RemoteCommand(mode, id string, kinds ...string) string {
	return "sh -s -- " + mode + " " + id + " " + strings.Join(kinds, " ")
}

// Socket is a kind's standard socket path on the remote, as RemoteScript
// found it.
type Socket struct {
	Path string // what gpg uses
	Ours string // this machine's forward, beside it
	// Holder is the ID of the machine whose forward Path leads to,
	// HolderOther if it's something else, or empty if there's nothing.
	Holder string
	Alive  bool // Path answers
	Bound  bool // something (our forward, once added) is at Ours
}

// ParseSockets reads RemoteScript's output, by kind.
func ParseSockets(out string) map[string]Socket {
	socks := map[string]Socket{}
	for line := range strings.Lines(out) {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\n"), "=")
		kind, field, ok2 := strings.Cut(key, ".")
		if !ok || !ok2 {
			continue
		}
		s := socks[kind]
		switch field {
		case "path":
			s.Path = value
		case "ours":
			s.Ours = value
		case "holder":
			s.Holder = value
		case "alive":
			s.Alive = value == "1"
		case "bound":
			s.Bound = value == "1"
		default:
			continue
		}
		socks[kind] = s
	}
	return socks
}

// MachineID names this machine in remote socket paths: its hostname, cut
// short to keep paths under the socket path limit, and a hash of its
// machine ID so two machines with the same hostname don't collide.
func MachineID() string {
	host, _ := os.Hostname()
	unique, err := os.ReadFile("/etc/machine-id")
	if err != nil || len(bytes.TrimSpace(unique)) == 0 {
		unique = []byte(host)
	}
	return machineID(host, bytes.TrimSpace(unique))
}

func machineID(host string, unique []byte) string {
	host, _, _ = strings.Cut(host, ".")
	var b strings.Builder
	for _, r := range host {
		if b.Len() >= 16 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "machine"
	}
	sum := sha256.Sum256(unique)
	return name + "-" + hex.EncodeToString(sum[:3])
}

// HolderName is how to show Socket.Holder: the machine's hostname part.
func HolderName(holder string) string {
	if holder == HolderOther {
		return "the remote's own gpg-agent"
	}
	if i := strings.LastIndexByte(holder, '-'); i > 0 {
		return holder[:i]
	}
	return holder
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

// Package authkeys lends public keys to a remote: lines in its
// ~/.ssh/authorized_keys that tether adds, refreshes and removes. Each line
// carries an OpenSSH expiry-time a short lease ahead, so a key whose machine
// went away (crashed, offline) stops working on its own; the lease is
// renewed while the key is wanted. Keys the user put there themselves are
// never touched.
package authkeys

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
)

// Key is a public key to lend, with the options its line gets.
type Key struct {
	Type        string // e.g. "ssh-ed25519"
	Blob        string // base64
	Fingerprint string // "SHA256:…"
	Comment     string
	// From and Restrict become the line's from="…" and restrict options.
	From     string
	Restrict bool
}

// Parse reads one public key in authorized_keys / .pub form ("type blob
// [comment]"). Options in front of it are refused: they're given with
// From and Restrict instead.
func Parse(s string) (Key, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Key{}, errors.New("no public key")
	}
	pk, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(s))
	if err != nil {
		return Key{}, fmt.Errorf("not a public key: %w", err)
	}
	if len(options) > 0 {
		return Key{}, errors.New("public key has options; use from and restrict instead")
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return Key{}, errors.New("more than one public key")
	}
	return Key{
		Type:        pk.Type(),
		Blob:        base64.StdEncoding.EncodeToString(pk.Marshal()),
		Fingerprint: ssh.FingerprintSHA256(pk),
		Comment:     comment,
	}, nil
}

// ValidateFrom checks a from="…" pattern list.
func ValidateFrom(from string) error {
	if from == "" || strings.ContainsFunc(from, func(r rune) bool {
		return r == '"' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return fmt.Errorf("invalid from pattern %q", from)
	}
	return nil
}

// options is the key's options for its line, without expiry-time.
func (k Key) options() string {
	var opts []string
	if k.From != "" {
		opts = append(opts, `from="`+k.From+`"`)
	}
	if k.Restrict {
		opts = append(opts, "restrict")
	}
	return strings.Join(opts, ",")
}

// Script modes, as understood by Script.
const (
	ModeSync    = "sync"    // lend exactly the keys given, renewing their lease
	ModeRelease = "release" // remove every key this machine lent
	ModeCheck   = "check"   // report, change nothing
)

// MinOpenSSH is the first OpenSSH with expiry-time, which the lease needs.
const MinOpenSSH = "8.2"

// Script runs on the remote, on its stdin, under the command SyncCommand,
// ReleaseCommand or CheckCommand gives. It rewrites ~/.ssh/authorized_keys
// atomically (under flock when there is one), only when something changed:
//
//   - Lines marked "tether:ID:EXPIRY:FP" for this machine are dropped, then
//     the wanted keys are added back with an expiry-time LEASE seconds from
//     the remote's clock, unless a line of the user's already has the key:
//     that key is "present" and left alone.
//   - Lines another machine lent are kept until their EXPIRY (epoch
//     seconds) passes, then dropped.
//
// sshd reads an expiry-time as UTC when it ends in Z (OpenSSH 9.1 on), and
// before that in the system's standard time, ignoring daylight saving: an
// expiry written in local time would be an hour late in summer. So older
// versions get the lease end in standard time; were an sshd to apply
// daylight saving after all, the key would expire early rather than late.
//
// It prints "present FP" or "ok FP" per key, then "expires EPOCH". In check
// mode it prints "version V", "ours N" (this machine's lines) and "writable
// yes|no|symlink" instead. It refuses to touch a symlinked authorized_keys
// (managed elsewhere, e.g. by home-manager) or to lend keys to an sshd
// older than MinOpenSSH.
const Script = `
mode=$1 id=$2 lease=$3
shift 3
umask 077
dir=$HOME/.ssh f=$dir/authorized_keys
version=
for b in sshd /usr/sbin/sshd /usr/local/sbin/sshd ssh; do
  command -v "$b" >/dev/null 2>&1 || continue
  version=$("$b" -V 2>&1 </dev/null | sed -n 's/^OpenSSH_\([0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' | head -n 1)
  [ -n "$version" ] && break
done
if [ "$mode" = check ]; then
  echo "version $version"
  n=0
  [ -f "$f" ] && n=$(grep -c " tether:$id:" "$f")
  echo "ours $n"
  if [ -L "$f" ]; then echo "writable symlink"
  elif [ -e "$f" ] && [ ! -w "$f" ]; then echo "writable no"
  elif [ ! -e "$f" ] && [ -e "$dir" ] && [ ! -w "$dir" ]; then echo "writable no"
  else echo "writable yes"; fi
  exit 0
fi
major=0 minor=0
if [ -n "$version" ]; then major=${version%%.*} minor=${version#*.}; fi
if [ "$mode" = sync ] && [ $# -gt 0 ]; then
  [ -n "$version" ] || { echo "can't find OpenSSH on the remote; lending keys needs OpenSSH >= ` + MinOpenSSH + `" >&2; exit 4; }
  if [ "$major" -lt 8 ] || { [ "$major" -eq 8 ] && [ "$minor" -lt 2 ]; }; then
    echo "OpenSSH $version on the remote is too old to lend keys (needs expiry-time, OpenSSH >= ` + MinOpenSSH + `)" >&2; exit 4
  fi
fi
[ -L "$f" ] && { echo "$f is a symlink (managed elsewhere?); not changing it" >&2; exit 5; }
if [ ! -e "$f" ] && [ "$mode" = release ]; then exit 0; fi
mkdir -p "$dir" && chmod 700 "$dir" || exit 6
if command -v flock >/dev/null 2>&1; then
  exec 9>"$dir/authorized_keys.tether.lock"
  flock -w 10 9 || { echo "timed out waiting for $dir/authorized_keys.tether.lock" >&2; exit 7; }
fi
now=$(date +%s) || exit 8
exp=$((now + lease))
d() { date "$@" -d "@$e" 2>/dev/null || date "$@" -r "$e"; }
if [ "$major" -gt 9 ] || { [ "$major" -eq 9 ] && [ "$minor" -ge 1 ]; }; then
  e=$exp; expts=$(d -u +%Y%m%d%H%M%SZ) || exit 8
else
  # The standard offset is the smallest over a year, sampled quarterly.
  offs=
  for q in 0 1 2 3; do
    e=$((now + q * 7889400)); o=$(d +%z) || exit 8; offs="$offs$o "
  done
  std=$(printf '%s\n' $offs | awk '{ v = substr($0, 2, 2) * 3600 + substr($0, 4, 2) * 60; if (substr($0, 1, 1) == "-") v = -v; if (NR == 1 || v < m) m = v } END { print m }')
  e=$((exp + std)); expts=$(d -u +%Y%m%d%H%M%S) || exit 8
fi
want=
while [ $# -ge 4 ]; do
  want="$want$1	$2	$3	$4
"
  shift 4
done
tmp=$(mktemp "$dir/authorized_keys.tether.XXXXXX") || exit 9
st=$tmp.status
touch "$f" 2>/dev/null
TETHER_WANT=$want awk -v id="$id" -v now="$now" -v exp_="$exp" -v expts="$expts" -v st="$st" '
BEGIN {
  n = split(ENVIRON["TETHER_WANT"], w, "\n")
  for (i = 1; i <= n; i++) {
    if (w[i] == "") continue
    split(w[i], p, "\t")
    k++; fp[k] = p[1]; opts[k] = p[2]; type[k] = p[3]; blob[k] = p[4]
  }
}
{
  mark = $NF
  if (NF >= 2 && mark ~ /^tether:[^:]+:[0-9]+:SHA256:/) {
    split(mark, m, ":")
    if (m[2] == id || m[3] + 0 < now + 0) next
    print; next
  }
  print
  for (i = 1; i <= k; i++)
    for (j = 1; j < NF; j++)
      if ($j == type[i] && $(j + 1) == blob[i]) present[i] = 1
}
END {
  for (i = 1; i <= k; i++) {
    if (present[i]) { print "present " fp[i] > st; continue }
    o = "expiry-time=\"" expts "\""
    if (opts[i] != "") o = o "," opts[i]
    print o " " type[i] " " blob[i] " tether:" id ":" exp_ ":" fp[i]
    print "ok " fp[i] > st
  }
  print "expires " exp_ > st
}' "$f" >"$tmp" || { rm -f "$tmp" "$st"; exit 10; }
if cmp -s "$tmp" "$f"; then
  rm -f "$tmp"
else
  chmod 600 "$tmp" && mv -f "$tmp" "$f" || { rm -f "$tmp" "$st"; exit 11; }
fi
cat "$st"
rm -f "$st"
`

// SyncCommand is the command to run Script with to lend exactly keys, for
// machine id, with a lease of leaseSecs.
func SyncCommand(id string, leaseSecs int, keys []Key) string {
	args := []string{"sh", "-s", "--", ModeSync, quote(id), strconv.Itoa(leaseSecs)}
	for _, k := range keys {
		args = append(args, quote(k.Fingerprint), quote(k.options()), quote(k.Type), quote(k.Blob))
	}
	return strings.Join(args, " ")
}

// ReleaseCommand is the command to run Script with to remove machine id's
// keys.
func ReleaseCommand(id string) string {
	return "sh -s -- " + ModeRelease + " " + quote(id) + " 0"
}

// CheckCommand is the command to run Script with to report on machine
// id's keys.
func CheckCommand(id string) string {
	return "sh -s -- " + ModeCheck + " " + quote(id) + " 0"
}

// DefaultPaths are where FetchCommand looks for a public key when given no
// path, in order.
var DefaultPaths = []string{"~/.ssh/id_ed25519.pub", "~/.ssh/id_ecdsa.pub", "~/.ssh/id_rsa.pub"}

// FetchCommand is the command that prints the public key at path on a
// remote (relative to the home directory, or starting with ~/), or the
// first of DefaultPaths there if path is empty. It runs under sh whatever
// the login shell.
func FetchCommand(path string) string {
	paths := DefaultPaths
	if path != "" {
		paths = []string{path}
	}
	var words []string
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			words = append(words, `"$HOME"/`+quote(rest))
		} else {
			words = append(words, quote(p))
		}
	}
	msg := "no public key at " + strings.Join(paths, ", ")
	script := "for f in " + strings.Join(words, " ") + `; do [ -f "$f" ] && exec cat -- "$f"; done; echo ` + quote(msg) + " >&2; exit 1"
	return "sh -c " + quote(script)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Result is what a sync reported.
type Result struct {
	Present map[string]bool // by fingerprint: the user's own line has it
	Expires time.Time       // when the lease ends; zero if not reported
}

// ParseResult reads Script's sync output.
func ParseResult(out string) Result {
	r := Result{Present: map[string]bool{}}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		word, rest, _ := strings.Cut(sc.Text(), " ")
		switch word {
		case "present":
			r.Present[rest] = true
		case "expires":
			if secs, err := strconv.ParseInt(rest, 10, 64); err == nil {
				r.Expires = time.Unix(secs, 0)
			}
		}
	}
	return r
}

// Check is what Script reported in check mode.
type Check struct {
	Version  string // OpenSSH version, e.g. "9.6"; empty if not found
	Ours     int    // lines this machine lent
	Writable string // "yes", "no" or "symlink"
}

// ParseCheck reads Script's check output.
func ParseCheck(out string) Check {
	var c Check
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		word, rest, _ := strings.Cut(sc.Text(), " ")
		switch word {
		case "version":
			c.Version = rest
		case "ours":
			c.Ours, _ = strconv.Atoi(rest)
		case "writable":
			c.Writable = rest
		}
	}
	return c
}

// VersionOK reports whether OpenSSH version v ("9.6") has expiry-time.
func VersionOK(v string) bool {
	major, minor, ok := strings.Cut(v, ".")
	ma, err1 := strconv.Atoi(major)
	mi, err2 := strconv.Atoi(minor)
	if !ok || err1 != nil || err2 != nil {
		return false
	}
	return ma > 8 || ma == 8 && mi >= 2
}

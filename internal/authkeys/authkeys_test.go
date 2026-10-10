package authkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newKey(t *testing.T, comment string) (string, Key) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment
	k, err := Parse(line)
	if err != nil {
		t.Fatal(err)
	}
	return line, k
}

// run runs Script locally with HOME at home, as sshd would run the command.
func run(t *testing.T, home, command string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin = strings.NewReader(Script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Logf("stderr: %s", ee.Stderr)
	}
	return string(out), err
}

func haveOpenSSH(t *testing.T) {
	t.Helper()
	out, _ := exec.Command("ssh", "-V").CombinedOutput()
	if !strings.HasPrefix(string(out), "OpenSSH_") {
		t.Skip("needs OpenSSH")
	}
}

func readKeys(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParse(t *testing.T) {
	line, k := newKey(t, "me@c")
	if k.Type != "ssh-ed25519" || !strings.HasPrefix(k.Fingerprint, "SHA256:") || k.Comment != "me@c" {
		t.Fatalf("Parse = %+v", k)
	}
	if !strings.Contains(line, k.Blob) {
		t.Fatalf("blob %q not in %q", k.Blob, line)
	}
	for _, bad := range []string{"", "nonsense", `restrict ` + line, line + "\n" + line} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestValidateFrom(t *testing.T) {
	for _, ok := range []string{"10.0.0.7", "10.0.0.0/8,!10.0.0.1", "*.example.com"} {
		if err := ValidateFrom(ok); err != nil {
			t.Errorf("ValidateFrom(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", `a"b`, "a b", `a\b`} {
		if err := ValidateFrom(bad); err == nil {
			t.Errorf("ValidateFrom(%q) succeeded", bad)
		}
	}
}

func TestVersionOK(t *testing.T) {
	for v, want := range map[string]bool{"8.2": true, "8.1": false, "9.0": true, "10.5": true, "7.9": false, "": false, "x": false} {
		if got := VersionOK(v); got != want {
			t.Errorf("VersionOK(%q) = %v", v, got)
		}
	}
}

func TestSyncAndRelease(t *testing.T) {
	haveOpenSSH(t)
	home := t.TempDir()
	userLine, userKey := newKey(t, "user's own")
	_, lent := newKey(t, "c")
	lent.From, lent.Restrict = "10.0.0.7", true
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "# my keys\n" + userLine + "\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, home, SyncCommand("m1", 600, []Key{userKey, lent}))
	if err != nil {
		t.Fatal(err)
	}
	r := ParseResult(out)
	if !r.Present[userKey.Fingerprint] || r.Present[lent.Fingerprint] || time.Until(r.Expires) < 9*time.Minute || time.Until(r.Expires) > 11*time.Minute {
		t.Fatalf("result %+v from %q", r, out)
	}
	got := readKeys(t, home)
	if !strings.HasPrefix(got, original) {
		t.Fatalf("user's lines changed:\n%s", got)
	}
	want := regexp.MustCompile(`^expiry-time="` + r.Expires.UTC().Format("20060102150405") + `Z",from="10\.0\.0\.7",restrict ssh-ed25519 ` +
		regexp.QuoteMeta(lent.Blob) + " tether:m1:" + strconv.FormatInt(r.Expires.Unix(), 10) + ":" + regexp.QuoteMeta(lent.Fingerprint) + "\n$")
	if !want.MatchString(strings.TrimPrefix(got, original)) {
		t.Fatalf("authorized_keys =\n%s\nwant the user's lines then a line matching\n%s", got, want)
	}
	fi, _ := os.Stat(filepath.Join(home, ".ssh", "authorized_keys"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if err := exec.Command("ssh-keygen", "-l", "-f", filepath.Join(home, ".ssh", "authorized_keys")).Run(); err != nil {
		t.Errorf("ssh-keygen can't read the result: %v", err)
	}

	// Syncing again keeps one line for the key.
	if _, err := run(t, home, SyncCommand("m1", 600, []Key{lent})); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readKeys(t, home), lent.Blob); n != 1 {
		t.Fatalf("%d lines for the key after a second sync", n)
	}

	// Another machine's line stays when this one releases.
	_, other := newKey(t, "other")
	if _, err := run(t, home, SyncCommand("m2", 600, []Key{other})); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, home, CheckCommand("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if c := ParseCheck(out); c.Ours != 1 || c.Writable != "yes" || !VersionOK(c.Version) {
		t.Fatalf("check = %+v", c)
	}
	if _, err := run(t, home, ReleaseCommand("m1")); err != nil {
		t.Fatal(err)
	}
	got = readKeys(t, home)
	if strings.Contains(got, lent.Blob) || !strings.Contains(got, other.Blob) || !strings.HasPrefix(got, original) {
		t.Fatalf("after release:\n%s", got)
	}
}

func TestExpiredLinesPruned(t *testing.T) {
	haveOpenSSH(t)
	home := t.TempDir()
	_, old := newKey(t, "old")
	stale := `expiry-time="20200101000000Z" ssh-ed25519 ` + old.Blob + " tether:gone:1577836800:" + old.Fingerprint + "\n"
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(stale), 0o600)
	_, k := newKey(t, "k")
	if _, err := run(t, home, SyncCommand("m1", 600, []Key{k})); err != nil {
		t.Fatal(err)
	}
	if got := readKeys(t, home); strings.Contains(got, old.Blob) || !strings.Contains(got, k.Blob) {
		t.Fatalf("authorized_keys =\n%s", got)
	}
}

func TestNoFile(t *testing.T) {
	haveOpenSSH(t)
	home := t.TempDir()
	if _, err := run(t, home, ReleaseCommand("m1")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); err == nil {
		t.Fatal("release created ~/.ssh")
	}
	_, k := newKey(t, "k")
	if _, err := run(t, home, SyncCommand("m1", 600, []Key{k})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readKeys(t, home), k.Blob) {
		t.Fatal("key not added")
	}
}

func TestSymlinkRefused(t *testing.T) {
	haveOpenSSH(t)
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	target := filepath.Join(home, "managed")
	os.WriteFile(target, nil, 0o600)
	os.Symlink(target, filepath.Join(home, ".ssh", "authorized_keys"))
	_, k := newKey(t, "k")
	if _, err := run(t, home, SyncCommand("m1", 600, []Key{k})); err == nil {
		t.Fatal("sync through a symlink succeeded")
	}
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Fatal("managed file changed")
	}
}

func TestConcurrentSyncs(t *testing.T) {
	haveOpenSSH(t)
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("needs flock")
	}
	home := t.TempDir()
	keys := make([]Key, 8)
	for i := range keys {
		_, keys[i] = newKey(t, "k")
	}
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Go(func() {
			if _, err := run(t, home, SyncCommand("m"+string(rune('a'+i)), 600, []Key{k})); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got := readKeys(t, home)
	for _, k := range keys {
		if !strings.Contains(got, k.Blob) {
			t.Errorf("lost a key:\n%s", got)
		}
	}
}

// TestStandardTime checks that before OpenSSH 9.1 the expiry is written in
// the remote's standard time, as sshd reads it, whatever the season.
func TestStandardTime(t *testing.T) {
	haveOpenSSH(t)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "sshd"), []byte("#!/bin/sh\necho 'OpenSSH_8.9p1, OpenSSL 3.0' >&2\n"), 0o755)
	for _, tz := range []string{"America/New_York", "Australia/Sydney", "UTC", "Asia/Kolkata"} {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			t.Skipf("no zoneinfo: %v", err)
		}
		home := t.TempDir()
		_, k := newKey(t, "k")
		cmd := exec.Command("/bin/sh", "-c", SyncCommand("m1", 600, []Key{k}))
		cmd.Stdin = strings.NewReader(Script)
		cmd.Env = append(os.Environ(), "HOME="+home, "TZ="+tz, "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		r := ParseResult(string(out))
		// Standard time: the smaller offset of January and July.
		_, jan := time.Date(r.Expires.Year(), 1, 1, 0, 0, 0, 0, loc).Zone()
		_, jul := time.Date(r.Expires.Year(), 7, 1, 0, 0, 0, 0, loc).Zone()
		std := time.FixedZone("std", min(jan, jul))
		want := `expiry-time="` + r.Expires.In(std).Format("20060102150405") + `" `
		if got := readKeys(t, home); !strings.HasPrefix(got, want) {
			t.Errorf("TZ=%s: got %q, want it to start with %q", tz, got, want)
		}
	}
}

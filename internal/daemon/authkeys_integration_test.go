package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/sshtest"
)

// TestAuthorizedKeysLend lends host "dev" a key read from another machine
// (the same sshd, standing in for C), checks that dev then accepts it, and
// that it stops accepting it when the profile goes down.
func TestAuthorizedKeysLend(t *testing.T) {
	remoteHome, err := os.MkdirTemp("", "tether-akhome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(remoteHome) })
	srv := startAuthKeysSSHD(t, remoteHome, "")
	cKey := newSSHKey(t)
	cfg := fmt.Sprintf(`[defaults]
reconnect_backoff = "100ms..500ms"

[hosts.dev]
ssh = %q

[profiles.lend]
host = "dev"
authorized_keys = [{ host = %q, path = %q, from = "127.0.0.1", restrict = true }]
`, sshtest.HostAlias, sshtest.HostAlias, cKey+".pub")
	h := start(t, cfg, openssh.Options{ConfigFile: srv.ConfigFile})
	sh := &sshHarness{harness: h, srv: srv, c: h.client(t)}

	if err := loginWith(srv, cKey); err == nil {
		t.Fatal("dev accepts C's key before it's lent")
	}
	sh.call(t, api.MethodUp, api.TargetParams{Name: "lend"}, nil)
	st := sh.waitFor(t, "key lent", func(st api.Status) bool {
		ks := host(st, "dev").AuthorizedKeys
		return len(ks) == 1 && ks[0].State == api.StateUp
	})
	ks := host(st, "dev").AuthorizedKeys[0]
	if ks.Present || ks.Expires == nil || ks.Comment != "me@c" || ks.Options != "from=127.0.0.1 restrict" || !strings.HasPrefix(ks.Fingerprint, "SHA256:") {
		t.Fatalf("status = %+v", ks)
	}
	if err := loginWith(srv, cKey); err != nil {
		t.Fatalf("dev doesn't accept the lent key: %v", err)
	}

	sh.call(t, api.MethodDown, api.TargetParams{Name: "lend"}, nil)
	sh.waitFor(t, "dev down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })
	data, _ := os.ReadFile(filepath.Join(remoteHome, ".ssh", "authorized_keys"))
	if strings.Contains(string(data), "tether:") {
		t.Fatalf("key still lent after down:\n%s", data)
	}
	if err := loginWith(srv, cKey); err == nil {
		t.Fatal("dev still accepts C's key after down")
	}
}

// TestAuthorizedKeysPresent leaves a key the user authorized themselves
// alone, and reports it as present.
func TestAuthorizedKeysPresent(t *testing.T) {
	remoteHome, err := os.MkdirTemp("", "tether-akhome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(remoteHome) })
	srv := startAuthKeysSSHD(t, remoteHome, "")
	cKey := newSSHKey(t)
	pub, _ := os.ReadFile(cKey + ".pub")
	original := "# mine\n" + string(pub)
	os.MkdirAll(filepath.Join(remoteHome, ".ssh"), 0o700)
	if err := os.WriteFile(filepath.Join(remoteHome, ".ssh", "authorized_keys"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[hosts.dev]\nssh = %q\n\n[profiles.lend]\nhost = \"dev\"\nauthorized_keys = [{ file = %q }]\n",
		sshtest.HostAlias, cKey+".pub")
	h := start(t, cfg, openssh.Options{ConfigFile: srv.ConfigFile})
	sh := &sshHarness{harness: h, srv: srv, c: h.client(t)}

	sh.call(t, api.MethodUp, api.TargetParams{Name: "lend"}, nil)
	sh.waitFor(t, "key present", func(st api.Status) bool {
		ks := host(st, "dev").AuthorizedKeys
		return len(ks) == 1 && ks[0].State == api.StateUp && ks[0].Present
	})
	sh.call(t, api.MethodDown, api.TargetParams{Name: "lend"}, nil)
	sh.waitFor(t, "dev down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })
	if data, _ := os.ReadFile(filepath.Join(remoteHome, ".ssh", "authorized_keys")); string(data) != original {
		t.Fatalf("the user's authorized_keys changed:\n%s", data)
	}
}

// startAuthKeysSSHD runs sshd with remoteHome as the remote's ~, reading
// ~/.ssh/authorized_keys there as well as the harness's own.
func startAuthKeysSSHD(t *testing.T, remoteHome, fakeVersion string) *sshtest.Server {
	t.Helper()
	path := os.Getenv("PATH")
	if fakeVersion != "" {
		bin := t.TempDir()
		script := "#!/bin/sh\necho 'OpenSSH_" + fakeVersion + "p1, OpenSSL 3.0' >&2\n"
		if err := os.WriteFile(filepath.Join(bin, "sshd"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		path = bin + ":" + path
	}
	return sshtest.StartWithConfig(t, fmt.Sprintf(`Match all
  AuthorizedKeysFile {dir}/authorized_keys %[1]s/.ssh/authorized_keys
  ForceCommand env HOME=%[1]s PATH='%[2]s' /bin/sh -c "$SSH_ORIGINAL_COMMAND"`,
		remoteHome, path))
}

// loginWith tries to log in to srv with only key.
func loginWith(srv *sshtest.Server, key string) error {
	u, _ := user.Current()
	out, err := exec.Command("ssh", "-F", os.DevNull, "-p", strconv.Itoa(srv.Port), "-i", key,
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
		u.Username+"@127.0.0.1", "true").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// TestAuthorizedKeysLease checks the lease: renewed, the key keeps working;
// not renewed (as when this machine goes away), sshd stops accepting it on
// its own.
func TestAuthorizedKeysLease(t *testing.T) {
	// Before 9.1 the expiry is written in standard time, not UTC; this
	// (newer) sshd reads it the same way, so it can check both.
	for _, version := range []string{"", "8.9"} {
		t.Run("version="+version, func(t *testing.T) { testAuthorizedKeysLease(t, version) })
	}
}

func testAuthorizedKeysLease(t *testing.T, fakeVersion string) {
	oldLease, oldRenew := authKeyLease, authKeyRenew
	t.Cleanup(func() { authKeyLease, authKeyRenew = oldLease, oldRenew })
	authKeyLease, authKeyRenew = 3*time.Second, time.Second

	remoteHome, err := os.MkdirTemp("", "tether-akhome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(remoteHome) })
	srv := startAuthKeysSSHD(t, remoteHome, fakeVersion)
	cKey := newSSHKey(t)
	cfg := fmt.Sprintf("[hosts.dev]\nssh = %q\n\n[profiles.lend]\nhost = \"dev\"\nauthorized_keys = [{ file = %q }]\n",
		sshtest.HostAlias, cKey+".pub")
	h := start(t, cfg, openssh.Options{ConfigFile: srv.ConfigFile})
	sh := &sshHarness{harness: h, srv: srv, c: h.client(t)}

	sh.call(t, api.MethodUp, api.TargetParams{Name: "lend"}, nil)
	sh.waitFor(t, "key lent", func(st api.Status) bool {
		ks := host(st, "dev").AuthorizedKeys
		return len(ks) == 1 && ks[0].State == api.StateUp
	})
	time.Sleep(5 * time.Second) // past the first lease
	if err := loginWith(srv, cKey); err != nil {
		t.Fatalf("renewed key no longer accepted: %v", err)
	}

	// This machine stops renewing, as if it went away without taking the
	// key back.
	authKeyRenew = time.Hour
	time.Sleep(authKeyLease + 2*time.Second)
	if err := loginWith(srv, cKey); err == nil {
		data, _ := os.ReadFile(filepath.Join(remoteHome, ".ssh", "authorized_keys"))
		t.Fatalf("expired key still accepted:\n%s", data)
	}
}

// newSSHKey makes a key pair, commented "me@c", and returns its private
// key's path.
func newSSHKey(t *testing.T) string {
	t.Helper()
	key := filepath.Join(t.TempDir(), "id_c")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "me@c", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	return key
}

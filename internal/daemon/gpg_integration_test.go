package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/gpg"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/sshtest"
)

// gpgHarness gives "local" (this process) and "remote" (sessions of the
// test sshd) separate throwaway GnuPG homes, so the test never touches the
// user's real keys or agent sockets.
type gpgHarness struct {
	*sshHarness
	localHome, remoteHome string
	fpr                   string
}

func startGPG(t *testing.T, profileTOML string) *gpgHarness {
	t.Helper()
	for _, bin := range []string{"gpg", "gpgconf", "gpg-agent", "ssh-add"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	// Short paths: agent sockets may live in the home directory.
	localHome := mkdirTemp(t, "tether-gpgl-")
	remoteHome := mkdirTemp(t, "tether-gpgr-")
	os.WriteFile(filepath.Join(localHome, "gpg-agent.conf"), []byte("enable-ssh-support\n"), 0o600)
	t.Setenv("GNUPGHOME", localHome)
	t.Cleanup(func() {
		for _, home := range []string{localHome, remoteHome} {
			gpgconfIn(home, "--kill", "all")
			gpgconfIn(home, "--remove-socketdir")
		}
	})

	gpgRun(t, localHome, "", "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
		"--quick-gen-key", "tether test <test@example.invalid>", "ed25519", "sign", "never")
	fpr := ""
	for line := range strings.Lines(gpgRun(t, localHome, "", "--list-secret-keys", "--with-colons")) {
		if f := strings.Split(line, ":"); f[0] == "fpr" {
			fpr = f[9]
			break
		}
	}

	// "Remote" sessions are this user on this machine, and shell startup
	// files may set GNUPGHOME (overriding sshd's SetEnv). Force it after
	// they run, so tether's remote gpg setup can't reach the real sockets.
	srv := sshtest.StartWithConfig(t, fmt.Sprintf(`ForceCommand env GNUPGHOME=%s /bin/sh -c "$SSH_ORIGINAL_COMMAND"`, remoteHome))
	g := &gpgHarness{localHome: localHome, remoteHome: remoteHome, fpr: fpr}
	g.sshHarness = &sshHarness{srv: srv}
	g.requireIsolated(t)

	cfg := fmt.Sprintf("[defaults]\nreconnect_backoff = \"100ms..500ms\"\n\n[hosts.dev]\nssh = %q\n%s", sshtest.HostAlias, profileTOML)
	h := start(t, cfg, openssh.Options{ConfigFile: srv.ConfigFile})
	g.harness, g.c = h, h.client(t)
	return g
}

// requireIsolated stops the test unless both sides use the throwaway homes.
func (g *gpgHarness) requireIsolated(t *testing.T) {
	t.Helper()
	local, err := exec.Command("gpgconf", "--list-dirs", "homedir").Output()
	if err != nil || strings.TrimSpace(string(local)) != g.localHome {
		t.Fatalf("local gpg homedir is %q, not the test's %q; refusing to continue", local, g.localHome)
	}
	remote, err := g.remote(t, "gpgconf --list-dirs homedir")
	if err != nil || strings.TrimSpace(remote) != g.remoteHome {
		t.Fatalf("test sshd sessions use gpg homedir %q, not the test's %q (%v); refusing to continue", remote, g.remoteHome, err)
	}
}

// importPublicKey copies the local public key to the remote keyring.
func (g *gpgHarness) importPublicKey(t *testing.T) {
	t.Helper()
	pub := gpgRun(t, g.localHome, "", "--export", "--armor", g.fpr)
	gpgRun(t, g.remoteHome, pub, "--batch", "--import")
}

// remote runs a shell command in a session on the test sshd.
func (g *gpgHarness) remote(t *testing.T, command string) (string, error) {
	t.Helper()
	out, err := exec.Command("ssh", "-F", g.srv.ConfigFile, "-o", "IdentityAgent=none", sshtest.HostAlias, command).CombinedOutput()
	return string(out), err
}

func TestGPGForwarding(t *testing.T) {
	g := startGPG(t, "[profiles.work]\nhost = \"dev\"\ngpg = true\ngpg_ssh = true\n")

	// Doctor flags the missing public key before we fix it.
	var res api.DoctorResult
	g.call(t, api.MethodDoctor, api.DoctorParams{Host: "dev"}, &res)
	if c := findCheck(res, "public keys"); c.Status != api.CheckWarn || !strings.Contains(c.Fix, g.fpr) {
		t.Errorf("public keys check before import = %+v", c)
	}
	for _, name := range []string{"connect", "gpg-agent", "gpg", "remote gpg-agent"} {
		if c := findCheck(res, name); c.Status != api.CheckOK {
			t.Errorf("%s check = %+v", name, c)
		}
	}

	g.importPublicKey(t)
	g.call(t, api.MethodDoctor, api.DoctorParams{Host: "dev"}, &res)
	if c := findCheck(res, "public keys"); c.Status != api.CheckOK {
		t.Errorf("public keys check after import = %+v", c)
	}

	g.call(t, api.MethodUp, api.TargetParams{Name: "work"}, nil)
	st := g.waitFor(t, "profile up", func(st api.Status) bool { return profile(st, "work").State == api.StateUp })
	if f := fwd(st, "dev", "gpg-agent"); !strings.HasPrefix(f.Resolved, "R:/") || f.Profiles[0] != "work" {
		t.Errorf("gpg-agent forward = %+v", f)
	}
	g.call(t, api.MethodDoctor, api.DoctorParams{Host: "dev"}, &res)
	if c := findCheck(res, "gpg uses"); c.Status != api.CheckOK || c.Detail != "this machine's agent" {
		t.Errorf("gpg uses check = %+v", c)
	}

	// The remote keyring has only the public key: signing works only
	// through the forwarded agent.
	if out, err := g.remote(t, "echo hello | gpg --batch --clearsign"); err != nil || !strings.Contains(out, "BEGIN PGP SIGNATURE") {
		t.Fatalf("remote signing failed: %v\n%s", err, out)
	}
	// A remote `gpgconf --kill` goes through the forward to the local agent's
	// restricted extra socket, which must refuse it.
	localPID := agentPID(t)
	g.remote(t, "gpgconf --kill gpg-agent")
	if pid := agentPID(t); pid != localPID {
		t.Fatalf("remote gpgconf --kill reached the local agent (pid %s -> %s)", localPID, pid)
	}
	if out, err := g.remote(t, "echo hello | gpg --batch --clearsign"); err != nil {
		t.Fatalf("remote signing failed after remote --kill: %v\n%s", err, out)
	}

	// ssh-add exits 1 for "agent has no identities" and 2 if it can't reach
	// the agent at all.
	out, err := g.remote(t, `SSH_AUTH_SOCK="$(gpgconf --list-dirs agent-ssh-socket)" ssh-add -l`)
	if code := exitCode(err); code != 0 && code != 1 {
		t.Fatalf("remote ssh-add can't reach the forwarded agent (exit %d): %s", code, out)
	}

	// Taking the profile down removes the forward.
	g.call(t, api.MethodDown, api.TargetParams{Name: "work"}, nil)
	g.waitFor(t, "host down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })
	if out, err := g.remote(t, "echo hello | gpg --batch --no-autostart --clearsign"); err == nil {
		t.Errorf("remote signing still works after down:\n%s", out)
	}
}

func TestGPGAdHoc(t *testing.T) {
	g := startGPG(t, "")
	g.importPublicKey(t)
	// Like a real remote with gpg in use: the remote's own agent is running
	// and owns the socket path when tether connects.
	gpgRun(t, g.remoteHome, "", "--batch", "--list-secret-keys")
	gpgconfIn(g.remoteHome, "--launch", "gpg-agent")

	var res api.ForwardResult
	g.call(t, api.MethodForwardAdd, api.ForwardParams{Host: "dev", Spec: "gpg-agent"}, &res)
	g.waitFor(t, "gpg forward up", func(st api.Status) bool { return fwd(st, "dev", "gpg-agent").State == api.StateUp })
	if out, err := g.remote(t, "echo hello | gpg --batch --clearsign"); err != nil {
		t.Fatalf("remote signing failed: %v\n%s", err, out)
	}

	// A stale socket left behind (e.g. by a remote gpg-agent) is replaced
	// when the connection comes back.
	g.srv.Stop()
	g.waitFor(t, "connection lost", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	g.srv.Restart(t)
	g.waitFor(t, "gpg forward back", func(st api.Status) bool { return fwd(st, "dev", "gpg-agent").State == api.StateUp })
	if out, err := g.remote(t, "echo hello | gpg --batch --clearsign"); err != nil {
		t.Fatalf("remote signing failed after reconnect: %v\n%s", err, out)
	}

	g.call(t, api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: "gpg-agent"}, nil)
	g.waitFor(t, "host down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })
}

func TestDoctorConnectFailure(t *testing.T) {
	srv := sshtest.Start(t)
	h := start(t, fmt.Sprintf("[hosts.dev]\nssh = %q\n", sshtest.HostAlias),
		openssh.Options{ConfigFile: srv.ConfigFile, ExtraArgs: []string{"-o", "User=tether-no-such-user"}})
	var res api.DoctorResult
	if err := h.client(t).Call(context.Background(), api.MethodDoctor, api.DoctorParams{Host: "dev"}, &res); err != nil {
		t.Fatal(err)
	}
	c := findCheck(res, "connect")
	if c.Status != api.CheckFail || !strings.Contains(c.Fix, "ssh-add") {
		t.Errorf("connect check = %+v", c)
	}
	if c := findCheck(res, "gpg-agent"); c.Status != api.CheckSkip {
		t.Errorf("gpg-agent check without gpg profiles = %+v", c)
	}
}

// agentPID asks the local test agent for its pid, without starting one.
func agentPID(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("gpg-connect-agent", "--no-autostart", "getinfo pid", "/bye").Output()
	if err != nil {
		t.Fatalf("local agent not running: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func findCheck(res api.DoctorResult, name string) api.Check {
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	return api.Check{Name: name, Status: "missing"}
}

func mkdirTemp(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func gpgRun(t *testing.T, home, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("gpg", args...)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gpg %v: %v", args, err)
	}
	return string(out)
}

func gpgconfIn(home string, args ...string) {
	cmd := exec.Command("gpgconf", args...)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	cmd.Run()
}

// TestGPGSwitchMachines has two machines ("home", this test's default, and
// "office") forward their agents to one host, and checks which one gpg
// there uses as they come, go and take over.
func TestGPGSwitchMachines(t *testing.T) {
	interval, grace := gpgCheckInterval, gpgGrace
	gpgCheckInterval, gpgGrace = 200*time.Millisecond, 3*time.Second
	t.Cleanup(func() { gpgCheckInterval, gpgGrace = interval, grace })

	home := startGPG(t, "")
	home.importPublicKey(t)
	const officeID = "office-bbbbbb"
	oh := startWith(t, fmt.Sprintf("[defaults]\nreconnect_backoff = \"100ms..500ms\"\n\n[hosts.dev]\nssh = %q\n", sshtest.HostAlias),
		func(o *Options) { o.SSH, o.GPGMachine = openssh.Options{ConfigFile: home.srv.ConfigFile}, officeID })
	office := &sshHarness{harness: oh, srv: home.srv, c: oh.client(t)}
	homeName := gpg.HolderName(gpg.MachineID())

	out, err := home.remote(t, "gpgconf --list-dirs agent-socket")
	if err != nil {
		t.Fatal(err)
	}
	socket := strings.TrimSpace(out)
	holder := func() string {
		l, _ := os.Readlink(socket)
		_, id, _ := strings.Cut(filepath.Base(l), ".tether.")
		return id
	}
	waitHolder := func(when, want string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); holder() != want; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: socket leads to %q, not %q", when, holder(), want)
			}
		}
	}
	sign := func(when string) {
		t.Helper()
		if out, err := home.remote(t, "echo hello | gpg --batch --no-autostart --clearsign"); err != nil || !strings.Contains(out, "BEGIN PGP SIGNATURE") {
			t.Fatalf("remote signing failed %s: %v\n%s", when, err, out)
		}
	}
	usedBy := func(who string) func(api.Status) bool {
		return func(st api.Status) bool {
			f := fwd(st, "dev", "gpg-agent")
			return f.State == api.StateUp && f.UsedBy == who
		}
	}

	home.call(t, api.MethodForwardAdd, api.ForwardParams{Host: "dev", Spec: "gpg-agent"}, nil)
	home.waitFor(t, "home holds", usedBy(""))
	waitHolder("home turned gpg on", gpg.MachineID())
	sign("from home")

	// Turning gpg on at the office takes it over; home stands by.
	office.call(t, api.MethodForwardAdd, api.ForwardParams{Host: "dev", Spec: "gpg-agent"}, nil)
	office.waitFor(t, "office holds", usedBy(""))
	home.waitFor(t, "home sees office", usedBy("office"))
	waitHolder("office turned gpg on", officeID)
	sign("from office")
	var res api.DoctorResult
	home.call(t, api.MethodDoctor, api.DoctorParams{Host: "dev"}, &res)
	if c := findCheck(res, "gpg uses"); c.Status != api.CheckOK || !strings.HasPrefix(c.Detail, "office's agent") {
		t.Errorf("home's doctor: gpg uses = %+v", c)
	}

	// Both reconnect on their own: neither takes over from the other, even
	// if home comes back first and finds office's socket dead for a moment.
	home.srv.Stop()
	home.waitFor(t, "connection lost", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	home.srv.Restart(t)
	home.waitFor(t, "home back, standing by", usedBy("office"))
	office.waitFor(t, "office back, holding", usedBy(""))
	time.Sleep(gpgGrace + 4*gpgCheckInterval)
	if id := holder(); id != officeID {
		t.Fatalf("after reconnecting, socket leads to %q, not office", id)
	}
	sign("after reconnecting")

	// Home asks for it back.
	home.call(t, api.MethodGPGClaim, api.GPGClaimParams{Host: "dev"}, nil)
	home.waitFor(t, "home holds again", usedBy(""))
	waitHolder("home claimed", gpg.MachineID())
	office.waitFor(t, "office sees home", usedBy(homeName))
	sign("after home claims")

	// Home turning gpg off hands it to office, at once.
	home.call(t, api.MethodForwardRemove, api.ForwardParams{Host: "dev", Spec: "gpg-agent"}, nil)
	waitHolder("home let go", officeID)
	office.waitFor(t, "office holds after handover", usedBy(""))
	sign("after handover")

	// A machine that vanishes without letting go (asleep, off the network)
	// is replaced once it has been silent for the grace period.
	ghost := filepath.Join(filepath.Dir(socket), filepath.Base(socket)+".tether.ghost-cccccc")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: ghost, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	t.Cleanup(func() { os.Remove(ghost) })
	if err := os.Symlink(ghost, socket+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(socket+".tmp", socket); err != nil {
		t.Fatal(err)
	}
	office.waitFor(t, "office sees the ghost", usedBy("ghost"))
	start := time.Now()
	office.waitFor(t, "office takes over from the ghost", usedBy(""))
	if waited := time.Since(start); waited < gpgGrace-time.Second {
		t.Errorf("took over after %v, before the grace period", waited)
	}
	sign("after taking over from a vanished machine")
}

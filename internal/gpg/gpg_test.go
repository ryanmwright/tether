package gpg

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseDirs(t *testing.T) {
	dirs := ParseDirs("sysconfdir:/etc/gnupg\nagent-socket:/run/user/1000/gnupg/S.gpg-agent\nhomedir:/home/a%3ab/.gnupg\nbogus\n")
	if dirs["agent-socket"] != "/run/user/1000/gnupg/S.gpg-agent" || dirs["homedir"] != "/home/a:b/.gnupg" || len(dirs) != 3 {
		t.Errorf("dirs = %v", dirs)
	}
}

func TestParseSockets(t *testing.T) {
	got := ParseSockets("agent.path=/run/user/1000/gnupg/S.gpg-agent\nagent.ours=/run/user/1000/gnupg/S.gpg-agent.tether.me-abc123\n" +
		"agent.holder=office-0a1b2c\nagent.alive=1\nagent.bound=0\nssh.holder=(remote)\nnoise\n")
	want := map[string]Socket{
		KindAgent: {Path: "/run/user/1000/gnupg/S.gpg-agent", Ours: "/run/user/1000/gnupg/S.gpg-agent.tether.me-abc123", Holder: "office-0a1b2c", Alive: true},
		KindSSH:   {Holder: HolderOther},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestMachineID(t *testing.T) {
	for _, c := range []struct{ host, want string }{
		{"office-pc.corp.example", "office-pc-"},
		{"My_Laptop", "my-laptop-"},
		{"a-very-long-hostname-indeed", "a-very-long-host-"},
		{"", "machine-"},
	} {
		id := machineID(c.host, []byte("x"))
		if !strings.HasPrefix(id, c.want) || len(id) != len(c.want)+6 {
			t.Errorf("machineID(%q) = %q", c.host, id)
		}
		if name := HolderName(id); name+"-" != c.want {
			t.Errorf("HolderName(%q) = %q", id, name)
		}
	}
	if machineID("a", []byte("1")) == machineID("a", []byte("2")) {
		t.Error("same hostname, different machines: same ID")
	}
}

// fakeAgent listens at path. It greets each connection like gpg-agent,
// unless mute, which is how a machine gone to sleep behind sshd looks.
func fakeAgent(t *testing.T, path string, mute bool) *net.UnixListener {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			if !mute {
				c.Write([]byte("OK Pleased to meet you\n"))
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return l
}

// TestRemoteScript runs the script against a throwaway GnuPG home, with
// fake agents standing in for two machines' forwards.
func TestRemoteScript(t *testing.T) {
	for _, bin := range []string{"gpgconf", "gpg-connect-agent", "timeout"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	home, err := os.MkdirTemp("", "tether-gpgs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GNUPGHOME", home)
	run := func(mode, id string) Socket {
		t.Helper()
		cmd := exec.Command("sh", "-c", RemoteCommand(mode, id, KindAgent))
		cmd.Stdin = strings.NewReader(RemoteScript)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", mode, id, err, out)
		}
		return ParseSockets(string(out))[KindAgent]
	}
	const home1, office = "home-aaaaaa", "office-bbbbbb"
	s := run(ModeBind, home1)
	t.Cleanup(func() {
		files, _ := filepath.Glob(s.Path + "*")
		for _, f := range files {
			os.Remove(f)
		}
		exec.Command("gpgconf", "--remove-socketdir").Run()
		os.RemoveAll(home)
	})
	if s.Holder != "" || s.Bound || s.Ours != s.Path+".tether."+home1 {
		t.Fatalf("bind on a fresh remote = %+v", s)
	}

	homeAgent := fakeAgent(t, s.Ours, false)
	if s = run(ModeClaim, home1); s.Holder != home1 || !s.Alive || !s.Bound {
		t.Fatalf("home claims: %+v", s)
	}
	if l, _ := os.Readlink(s.Path); l != s.Ours {
		t.Fatalf("link = %q", l)
	}

	o := run(ModeBind, office)
	fakeAgent(t, o.Ours, false)
	if o = run(ModeStatus, office); o.Holder != home1 || !o.Alive || !o.Bound {
		t.Fatalf("office sees: %+v", o)
	}
	if o = run(ModeClaim, office); o.Holder != office || !o.Alive {
		t.Fatalf("office claims: %+v", o)
	}
	// Releasing hands the path to the other live forward.
	if o = run(ModeRelease, office); o.Holder != home1 || o.Bound {
		t.Fatalf("office releases: %+v", o)
	}

	// Home's machine vanishes without a word: its socket stays, dead.
	homeAgent.SetUnlinkOnClose(false)
	homeAgent.Close()
	if s = run(ModeStatus, home1); s.Holder != home1 || s.Alive {
		t.Fatalf("home gone: %+v", s)
	}

	// The remote's own agent (here: a plain socket) is replaced on claim.
	os.Remove(s.Path)
	fakeAgent(t, s.Path, false)
	o = run(ModeBind, office)
	fakeAgent(t, o.Ours, true) // office's machine is asleep
	if o = run(ModeStatus, office); o.Holder != HolderOther || !o.Alive {
		t.Fatalf("remote's own agent: %+v", o)
	}
	start := time.Now()
	if o = run(ModeClaim, office); o.Holder != office || o.Alive {
		t.Fatalf("claim from the remote's agent, asleep: %+v", o)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("checking an unresponsive socket took %v", time.Since(start))
	}

	// Releasing when nothing else answers removes the link.
	if s = run(ModeRelease, office); s.Holder != "" {
		t.Fatalf("last release: %+v", s)
	}
	if _, err := os.Lstat(s.Path); !os.IsNotExist(err) {
		t.Errorf("link left behind: %v", err)
	}
}

func TestParseRemoteInfo(t *testing.T) {
	info := ParseRemoteInfo(`gpgconf=/usr/bin/gpgconf
version=gpg (GnuPG) 2.4.7
dir.agent-socket:/run/user/1000/gnupg/S.gpg-agent
dir.agent-ssh-socket:/run/user/1000/gnupg/S.gpg-agent.ssh
unit.gpg-agent.socket=enabled/active
unit.gpg-agent-extra.socket=masked/inactive
unit.gpg-agent-ssh.socket=/inactive
unit.gpg-agent-browser.socket=static/inactive
unit.gpg-agent.service=/inactive
fpr=AAAA
fpr=BBBB
`)
	if info.Gpgconf != "/usr/bin/gpgconf" || info.Version != "gpg (GnuPG) 2.4.7" {
		t.Errorf("info = %+v", info)
	}
	if info.Dirs["agent-socket"] != "/run/user/1000/gnupg/S.gpg-agent" {
		t.Errorf("dirs = %v", info.Dirs)
	}
	if got := info.CompetingUnits(); !slices.Equal(got, []string{"gpg-agent.socket"}) {
		t.Errorf("competing = %v", got)
	}
	if !info.Fingerprints["AAAA"] || !info.Fingerprints["BBBB"] {
		t.Errorf("fingerprints = %v", info.Fingerprints)
	}

	if none := ParseRemoteInfo("gpgconf=\n"); none.Gpgconf != "" {
		t.Errorf("missing gpg: %+v", none)
	}
}

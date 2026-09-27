package mount

import (
	"strings"
	"testing"
)

func TestParseAndNormalize(t *testing.T) {
	tests := []struct {
		src, dst string
		want     Spec
		key      string
	}{
		{"remote:~/src", "~/mnt/src", Spec{Direction: RemoteToLocal, Remote: "~/src", Local: "/home/me/mnt/src"}, "remote:~/src -> /home/me/mnt/src"},
		{"remote:src/", "/mnt/x", Spec{Direction: RemoteToLocal, Remote: "~/src", Local: "/mnt/x"}, ""},
		{"remote:~", "/mnt/home", Spec{Direction: RemoteToLocal, Remote: "~", Local: "/mnt/home"}, ""},
		{"remote:/var//log/", "/mnt/log", Spec{Direction: RemoteToLocal, Remote: "/var/log", Local: "/mnt/log"}, ""},
		{"~/proj", "remote:~/proj", Spec{Direction: LocalToRemote, Remote: "~/proj", Local: "/home/me/proj"}, "/home/me/proj -> remote:~/proj"},
		{"/data/../data/x", "remote:/srv/x", Spec{Direction: LocalToRemote, Remote: "/srv/x", Local: "/data/x"}, ""},
	}
	for _, tt := range tests {
		spec, err := Parse(tt.src, tt.dst)
		if err != nil {
			t.Fatalf("%s %s: %v", tt.src, tt.dst, err)
		}
		got, err := Normalize(spec, "/home/me")
		if err != nil {
			t.Fatalf("%s %s: %v", tt.src, tt.dst, err)
		}
		if got.Direction != tt.want.Direction || got.Remote != tt.want.Remote || got.Local != tt.want.Local {
			t.Errorf("%s %s = %+v, want %+v", tt.src, tt.dst, got, tt.want)
		}
		if tt.key != "" && got.Key() != tt.key {
			t.Errorf("key = %q, want %q", got.Key(), tt.key)
		}
	}
}

func TestInvalid(t *testing.T) {
	for _, args := range [][2]string{{"a", "b"}, {"remote:a", "remote:b"}} {
		if _, err := Parse(args[0], args[1]); err == nil {
			t.Errorf("Parse(%q, %q) accepted", args[0], args[1])
		}
	}
	bad := []Spec{
		{Direction: "sideways", Remote: "x", Local: "/x"},
		{Direction: RemoteToLocal, Remote: "x", Local: "relative/path"},
		{Direction: RemoteToLocal, Remote: "", Local: "/x"},
		{Direction: RemoteToLocal, Remote: "x", Local: "/x", Options: []string{"allow_other;rm -rf /"}},
		{Direction: RemoteToLocal, Remote: "x", Local: "/x", Options: []string{"-o"}},
	}
	for _, s := range bad {
		if _, err := Normalize(s, "/home/me"); err == nil {
			t.Errorf("Normalize(%+v) accepted", s)
		}
	}
}

func TestSSHFSOptions(t *testing.T) {
	if got := sshfsOptions(nil); got != "passive,idmap=user" {
		t.Errorf("defaults = %q", got)
	}
	if got := sshfsOptions([]string{"idmap=none", "ro"}); got != "passive,idmap=none,ro" {
		t.Errorf("with user idmap = %q", got)
	}
}

func TestSFTPPath(t *testing.T) {
	for in, want := range map[string]string{"~": ".", "~/src": "src", "/srv": "/srv"} {
		if got := sftpPath(in); got != want {
			t.Errorf("sftpPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnescapeMountinfo(t *testing.T) {
	if got := unescapeMountinfo(`/mnt/with\040space\134x`); got != `/mnt/with space\x` {
		t.Errorf("got %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	cmd := remoteCommand("echo $1", "it's ~/x")
	if !strings.Contains(cmd, `'it'"'"'s ~/x'`) || !strings.HasPrefix(cmd, "sh -c 'echo $1' tether-mount") {
		t.Errorf("command = %s", cmd)
	}
}

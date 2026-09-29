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

func TestParsePVC(t *testing.T) {
	spec, err := Parse("pvc:prod/db/data-pg-0", "~/mnt/pg")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Normalize(spec, "/home/me")
	if err != nil {
		t.Fatal(err)
	}
	if got.Direction != PVCToLocal || got.Kube.Context != "prod" || got.Kube.Namespace != "db" || got.Kube.PVC != "data-pg-0" || got.Local != "/home/me/mnt/pg" {
		t.Errorf("spec = %+v, kube = %+v", got, got.Kube)
	}
	if got.Key() != "pvc:prod/db/data-pg-0 -> /home/me/mnt/pg" || got.Remote != "pvc:prod/db/data-pg-0" {
		t.Errorf("key = %q, remote = %q", got.Key(), got.Remote)
	}

	for _, args := range [][2]string{{"pvc:data", "/mnt/x"}, {"pvc:db/data", "remote:/x"}, {"pvc:db/Bad", "/mnt/x"}} {
		if _, err := Parse(args[0], args[1]); err == nil {
			t.Errorf("Parse(%q, %q) succeeded", args[0], args[1])
		}
	}
	if _, err := Normalize(Spec{Direction: PVCToLocal, Local: "/mnt/x"}, "/home/me"); err == nil {
		t.Error("PVC spec without a claim accepted")
	}

	spec, _ = Parse("pvc:arn:aws:eks:x:1:cluster/prod/db/data", "")
	if got := DefaultPVCLocal("~/mnt/k8s", *spec.Kube); got != "~/mnt/k8s/arn:aws:eks:x:1:cluster_prod/db/data" {
		t.Errorf("default mount point = %q", got)
	}
}

func TestPVCCaching(t *testing.T) {
	if got := strings.Join(withPVCCaching(nil), ","); got != "auto_cache,attr_timeout=10,entry_timeout=10,negative_timeout=5,dcache_timeout=60" {
		t.Errorf("defaults = %s", got)
	}
	got := strings.Join(withPVCCaching([]string{"ro", "attr_timeout=1", "kernel_cache"}), ",")
	if got != "entry_timeout=10,negative_timeout=5,dcache_timeout=60,ro,attr_timeout=1,kernel_cache" {
		t.Errorf("with user options = %s", got)
	}
}

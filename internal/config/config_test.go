package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFull(t *testing.T) {
	c, err := Parse([]byte(`
[defaults]
reconnect_backoff = "2s..30s"

[hosts.devbox]
ssh = "devbox.lan"
autoconnect = true

[hosts.bastion]

[profiles.work]
host = "devbox"
gpg = true
forwards = ["L:5432:db.internal:5432", "D:1080"]

[[profiles.work.mounts]]
direction = "remote-to-local"
remote = "~/src"
local = "~/mnt/src"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Defaults.ReconnectBackoff; got.Min != 2*time.Second || got.Max != 30*time.Second {
		t.Errorf("backoff = %+v", got)
	}
	if h := c.Hosts["devbox"]; h.SSH != "devbox.lan" || !h.Autoconnect {
		t.Errorf("devbox = %+v", h)
	}
	if h := c.Hosts["bastion"]; h.SSH != "bastion" {
		t.Errorf("ssh should default to the host name, got %q", h.SSH)
	}
	p := c.Profiles["work"]
	if !p.GPG || len(p.Forwards) != 2 || len(p.Mounts) != 1 || p.Mounts[0].Direction != RemoteToLocal {
		t.Errorf("work = %+v", p)
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := c.Defaults.ReconnectBackoff; b.Min != time.Second || b.Max != time.Minute {
		t.Errorf("default backoff = %+v", b)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, input string
		want        []string
	}{
		{"unknown key", "[hosts.a]\nsssh = \"x\"", []string{"unknown keys: hosts.a.sssh"}},
		{"bad backoff", `[defaults]` + "\n" + `reconnect_backoff = "60s..1s"`, []string{"invalid backoff"}},
		{"option injection", "[hosts.a]\nssh = \"-oProxyCommand=evil\"", []string{"hosts.a.ssh: invalid destination"}},
		{"whitespace in ssh", "[hosts.a]\nssh = \"a b\"", []string{"hosts.a.ssh: invalid destination"}},
		{"bad name", "[hosts.\"a b\"]", []string{`hosts.a b: name must match`}},
		{
			"collects all profile errors",
			"[profiles.p]\nforwards = [\"\"]\n[[profiles.p.mounts]]\ndirection = \"sideways\"",
			[]string{"profiles.p.host: required", "forwards[0]: invalid forward", "mounts[0]: remote and local are required"},
		},
		{"unknown host", "[profiles.p]\nhost = \"nope\"", []string{`unknown host "nope"`}},
		{"bad mount", "[hosts.h]\n[profiles.p]\nhost = \"h\"\n[[profiles.p.mounts]]\ndirection = \"sideways\"\nremote = \"x\"\nlocal = \"rel/path\"\n", []string{"mounts[0]: direction must be"}},
		{"bad usb", "[hosts.h]\n[profiles.p]\nhost = \"h\"\nusb = [\"1-1\", \"yubikey\"]\n", []string{`profiles.p.usb[1]: invalid USB device "yubikey"`}},
		{"relative mount", "[hosts.h]\n[profiles.p]\nhost = \"h\"\n[[profiles.p.mounts]]\ndirection = \"remote-to-local\"\nremote = \"x\"\nlocal = \"rel/path\"\n", []string{"must be absolute or start with ~/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.input))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()

	c, err := Load(filepath.Join(dir, "missing.toml"))
	if err != nil || len(c.Hosts) != 0 {
		t.Fatalf("missing file: %v, %+v", err, c)
	}

	bad := filepath.Join(dir, "bad.toml")
	os.WriteFile(bad, []byte("[profiles.p]\n"), 0o600)
	if _, err := Load(bad); err == nil || !strings.HasPrefix(err.Error(), bad+":") {
		t.Errorf("error should be prefixed with the path, got %v", err)
	}
}

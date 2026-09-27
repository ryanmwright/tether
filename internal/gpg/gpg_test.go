package gpg

import (
	"slices"
	"testing"
)

func TestParseDirs(t *testing.T) {
	dirs := ParseDirs("sysconfdir:/etc/gnupg\nagent-socket:/run/user/1000/gnupg/S.gpg-agent\nhomedir:/home/a%3ab/.gnupg\nbogus\n")
	if dirs["agent-socket"] != "/run/user/1000/gnupg/S.gpg-agent" || dirs["homedir"] != "/home/a:b/.gnupg" || len(dirs) != 3 {
		t.Errorf("dirs = %v", dirs)
	}
}

func TestParsePrepareOutput(t *testing.T) {
	got := ParsePrepareOutput("agent=/run/user/1000/gnupg/S.gpg-agent\nssh=/run/user/1000/gnupg/S.gpg-agent.ssh\n")
	if got[KindAgent] != "/run/user/1000/gnupg/S.gpg-agent" || got[KindSSH] != "/run/user/1000/gnupg/S.gpg-agent.ssh" {
		t.Errorf("got %v", got)
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

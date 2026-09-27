// Package mount mounts directories between this machine and a remote over an
// existing SSH connection, with sshfs in passive mode.
package mount

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

type Direction string

const (
	RemoteToLocal Direction = "remote-to-local" // a remote directory mounted here
	LocalToRemote Direction = "local-to-remote" // a local directory mounted there
)

type Spec struct {
	Direction Direction
	Remote    string // path on the remote; relative paths and ~ are from the remote home
	Local     string // absolute path here
	Options   []string
}

// RemotePrefix marks the remote side in "SRC DST" arguments, scp-style.
const RemotePrefix = "remote:"

// Key identifies a mount and reads like the arguments that create it:
// "remote:~/src -> /home/me/mnt/src" or "/home/me/proj -> remote:~/proj".
func (s Spec) Key() string {
	if s.Direction == LocalToRemote {
		return s.Local + " -> " + RemotePrefix + s.Remote
	}
	return RemotePrefix + s.Remote + " -> " + s.Local
}

// Parse builds a spec from a source and a mount point, one of which is
// prefixed with "remote:".
func Parse(src, dst string) (Spec, error) {
	srcRemote, dstRemote := strings.HasPrefix(src, RemotePrefix), strings.HasPrefix(dst, RemotePrefix)
	switch {
	case srcRemote && !dstRemote:
		return Spec{Direction: RemoteToLocal, Remote: strings.TrimPrefix(src, RemotePrefix), Local: dst}, nil
	case dstRemote && !srcRemote:
		return Spec{Direction: LocalToRemote, Remote: strings.TrimPrefix(dst, RemotePrefix), Local: src}, nil
	}
	return Spec{}, fmt.Errorf("exactly one side must start with %q: %q %q", RemotePrefix, src, dst)
}

var optionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_=.,:/+-]*$`)

// Normalize validates s and puts its paths in canonical form, expanding ~ in
// the local path with home.
func Normalize(s Spec, home string) (Spec, error) {
	if s.Direction != RemoteToLocal && s.Direction != LocalToRemote {
		return s, fmt.Errorf("direction must be %q or %q", RemoteToLocal, LocalToRemote)
	}

	local := s.Local
	switch {
	case local == "~":
		local = home
	case strings.HasPrefix(local, "~/"):
		local = filepath.Join(home, local[2:])
	}
	if !filepath.IsAbs(local) {
		return s, fmt.Errorf("local path %q must be absolute or start with ~/", s.Local)
	}
	s.Local = filepath.Clean(local)

	remote := strings.TrimSpace(s.Remote)
	if remote == "" || strings.ContainsAny(remote, "\x00\n") {
		return s, fmt.Errorf("invalid remote path %q", s.Remote)
	}
	// Keep ~ for the remote to expand; relative paths are from the home too.
	switch {
	case path.IsAbs(remote):
		remote = path.Clean(remote)
	case remote == "~" || strings.HasPrefix(remote, "~/"):
		remote = "~" + path.Clean("/"+strings.TrimPrefix(remote, "~"))
	default:
		remote = "~" + path.Clean("/"+remote)
	}
	s.Remote = strings.TrimSuffix(remote, "/") // "~/" -> "~"

	for _, o := range s.Options {
		if !optionPattern.MatchString(o) {
			return s, fmt.Errorf("invalid sshfs option %q", o)
		}
	}
	return s, nil
}

// sftpPath is the remote path as SFTP sees it: relative paths start in the
// remote home.
func sftpPath(remote string) string {
	switch {
	case remote == "~":
		return "."
	case strings.HasPrefix(remote, "~/"):
		return remote[2:]
	}
	return remote
}

// sshfsOptions adds tether's defaults to the user's options: passive mode
// (talk SFTP over stdin/stdout), and mapping the other side's owner to the
// local user unless the user chose an idmap.
func sshfsOptions(user []string) string {
	opts := []string{"passive"}
	idmap := false
	for _, o := range user {
		idmap = idmap || strings.HasPrefix(o, "idmap=")
	}
	if !idmap {
		opts = append(opts, "idmap=user")
	}
	return strings.Join(append(opts, user...), ",")
}

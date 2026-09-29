// Package mount mounts directories between this machine and a remote over an
// existing SSH connection, with sshfs in passive mode.
package mount

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ryanmwright/tether/internal/kube"
)

type Direction string

const (
	RemoteToLocal Direction = "remote-to-local" // a remote directory mounted here
	LocalToRemote Direction = "local-to-remote" // a local directory mounted there
	PVCToLocal    Direction = "pvc-to-local"    // a Kubernetes claim mounted here, via kubectl on the host
)

type Spec struct {
	Direction Direction
	Remote    string // path on the remote; relative paths and ~ are from the remote home
	Local     string // absolute path here
	Options   []string
	Kube      *kube.Source // the claim, for PVCToLocal
}

// RemotePrefix marks the remote side in "SRC DST" arguments, scp-style.
const RemotePrefix = "remote:"

// PVCPrefix marks a Kubernetes claim as the source: "pvc:[context/]ns/claim".
const PVCPrefix = "pvc:"

// Key identifies a mount and reads like the arguments that create it:
// "remote:~/src -> /home/me/mnt/src" or "/home/me/proj -> remote:~/proj".
func (s Spec) Key() string {
	if s.Direction == PVCToLocal && s.Kube != nil {
		return PVCPrefix + s.Kube.Ref() + " -> " + s.Local
	}
	if s.Direction == LocalToRemote {
		return s.Local + " -> " + RemotePrefix + s.Remote
	}
	return RemotePrefix + s.Remote + " -> " + s.Local
}

// Parse builds a spec from a source and a mount point, one of which is
// prefixed with "remote:"; or from a "pvc:" source and a mount point, which
// may be empty for the default.
func Parse(src, dst string) (Spec, error) {
	if ref, ok := strings.CutPrefix(src, PVCPrefix); ok {
		k, err := kube.ParseRef(ref)
		if err != nil {
			return Spec{}, err
		}
		if strings.HasPrefix(dst, RemotePrefix) {
			return Spec{}, fmt.Errorf("claims can only be mounted here, not at %q", dst)
		}
		return Spec{Direction: PVCToLocal, Local: dst, Kube: &k}, nil
	}
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
	switch s.Direction {
	case RemoteToLocal, LocalToRemote:
		s.Kube = nil
	case PVCToLocal:
		if s.Kube == nil {
			return s, errors.New("no claim given")
		}
		if err := s.Kube.Validate(); err != nil {
			return s, err
		}
		k := *s.Kube
		s.Kube = &k
		s.Remote = PVCPrefix + k.Ref()
	default:
		return s, fmt.Errorf("direction must be %q, %q or %q", RemoteToLocal, LocalToRemote, PVCToLocal)
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

	for _, o := range s.Options {
		if !optionPattern.MatchString(o) {
			return s, fmt.Errorf("invalid sshfs option %q", o)
		}
	}
	if s.Direction == PVCToLocal {
		return s, nil
	}

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

// pvcCaching is extra caching for PVC mounts, where every request goes
// through ssh, kubectl exec, the API server and the kubelet, so round trips
// are slow: attributes and names are kept 10s (not 1s), lookups of missing
// files 5s, directory listings 60s, and file contents across opens until the
// file's mtime changes. Changes made in the cluster can take that long to
// show up here.
var pvcCaching = []string{"auto_cache", "attr_timeout=10", "entry_timeout=10", "negative_timeout=5", "dcache_timeout=60"}

// withPVCCaching adds pvcCaching to the user's options, except for what
// they set themselves.
func withPVCCaching(user []string) []string {
	key := func(o string) string { k, _, _ := strings.Cut(o, "="); return k }
	set := map[string]bool{}
	for _, o := range user {
		set[key(o)] = true
	}
	// Options that decide file caching another way replace auto_cache.
	contentSet := set["kernel_cache"] || set["noauto_cache"] || set["auto_cache"] || set["direct_io"]
	var opts []string
	for _, o := range pvcCaching {
		if o == "auto_cache" && contentSet || set[key(o)] {
			continue
		}
		opts = append(opts, o)
	}
	return append(opts, user...)
}

// DefaultPVCLocal is where a claim is mounted when no mount point is given:
// root/<context>/<namespace>/<claim>, with slashes in the context name
// replaced.
func DefaultPVCLocal(root string, k kube.Source) string {
	return path.Join(root, strings.ReplaceAll(k.Context, "/", "_"), k.Namespace, k.PVC)
}

// Package kube mounts Kubernetes PersistentVolumeClaims. kubectl runs on a
// host tether is connected to (or on this machine, for the local host); a
// short-lived helper pod mounts the claim and serves it over SFTP through
// `kubectl exec`, so nothing listens on a port and nothing is installed in
// the cluster.
package kube

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ryanmwright/tether/internal/openssh"
)

// Defaults for Options.
const (
	DefaultKubectl      = "kubectl"
	DefaultImage        = "docker.io/atmoz/sftp:alpine"
	DefaultSFTPServer   = "/usr/lib/ssh/sftp-server"
	DefaultMountRoot    = "~/mnt/k8s"
	DefaultStartTimeout = 2 * time.Minute
)

// Options says how to run kubectl on a host and what helper pods look like.
// The zero value uses the defaults.
type Options struct {
	Kubectl    string // kubectl on that host: a name on its PATH, an absolute path, or ~/...
	Kubeconfig string // passed as --kubeconfig when set
	Image      string // helper image; needs sh, cat and an sftp-server
	SFTPServer string // sftp-server's path in the image
	RunAsUser  *int64 // helper pod securityContext
	RunAsGroup *int64
	FSGroup    *int64
	// MountRoot is where PVCs are mounted by default, as
	// MountRoot/<context>/<namespace>/<claim>.
	MountRoot    string
	StartTimeout time.Duration // for the helper pod to become ready
	// Env is set for kubectl, e.g. KUBECTL_REMOTE_COMMAND_WEBSOCKETS=true,
	// which can make exec streams (and so mounts) faster.
	Env map[string]string
}

func (o Options) WithDefaults() Options {
	if o.Kubectl == "" {
		o.Kubectl = DefaultKubectl
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.SFTPServer == "" {
		o.SFTPServer = DefaultSFTPServer
	}
	if o.MountRoot == "" {
		o.MountRoot = DefaultMountRoot
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = DefaultStartTimeout
	}
	return o
}

// Validate checks values that end up on command lines and in manifests.
func (o Options) Validate() error {
	var errs []error
	for name, v := range map[string]string{"kubectl": o.Kubectl, "kubeconfig": o.Kubeconfig, "image": o.Image, "sftp_server": o.SFTPServer, "mount_root": o.MountRoot} {
		if strings.ContainsFunc(v, unicode.IsControl) {
			errs = append(errs, fmt.Errorf("%s: invalid value %q", name, v))
		}
	}
	for k, v := range o.Env {
		if !envName.MatchString(k) || strings.ContainsFunc(v, unicode.IsControl) {
			errs = append(errs, fmt.Errorf("env: invalid variable %s=%q", k, v))
		}
	}
	if o.SFTPServer != "" && !strings.HasPrefix(o.SFTPServer, "/") {
		errs = append(errs, fmt.Errorf("sftp_server: %q must be an absolute path", o.SFTPServer))
	}
	return errors.Join(errs...)
}

// Source is a claim to mount.
type Source struct {
	Context   string // kubectl context; empty for the current one (resolved before mounting)
	Namespace string
	PVC       string
	SubPath   string // directory inside the volume to mount instead of its root
	ReadOnly  bool

	Opts Options // how to reach the cluster; not part of the mount's identity
}

// Ref is "context/namespace/claim".
func (s Source) Ref() string {
	if s.Context == "" {
		return s.Namespace + "/" + s.PVC
	}
	return s.Context + "/" + s.Namespace + "/" + s.PVC
}

// ParseRef parses "[context/]namespace/claim". Context names may themselves
// contain slashes (EKS ARNs do), so the claim and namespace are taken from
// the end.
func ParseRef(ref string) (Source, error) {
	parts := strings.Split(ref, "/")
	if len(parts) < 2 {
		return Source{}, fmt.Errorf("%q: want [CONTEXT/]NAMESPACE/CLAIM", ref)
	}
	n := len(parts)
	s := Source{Context: strings.Join(parts[:n-2], "/"), Namespace: parts[n-2], PVC: parts[n-1]}
	return s, s.Validate()
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

func (s Source) Validate() error {
	var errs []error
	if strings.HasPrefix(s.Context, "-") || strings.ContainsFunc(s.Context, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		errs = append(errs, fmt.Errorf("invalid context %q", s.Context))
	}
	if len(s.Namespace) > 63 || !dnsLabel.MatchString(s.Namespace) {
		errs = append(errs, fmt.Errorf("invalid namespace %q", s.Namespace))
	}
	if len(s.PVC) > 253 || !dnsSubdomain.MatchString(s.PVC) {
		errs = append(errs, fmt.Errorf("invalid claim name %q", s.PVC))
	}
	if s.SubPath != "" {
		bad := strings.HasPrefix(s.SubPath, "/") || strings.ContainsFunc(s.SubPath, unicode.IsControl)
		for _, p := range strings.Split(s.SubPath, "/") {
			bad = bad || p == ".."
		}
		if bad {
			errs = append(errs, fmt.Errorf("invalid sub-path %q: must be relative, without \"..\"", s.SubPath))
		}
	}
	return errors.Join(errs...)
}

// kubectl is a shell command line running kubectl with opts, in context kctx
// if set, with args.
func kubectl(opts Options, kctx string, args ...string) string {
	opts = opts.WithDefaults()
	// The environment goes through env(1), not a VAR=value prefix, so the
	// command can be exec'd.
	var cmd []string
	if len(opts.Env) > 0 {
		cmd = append(cmd, "env")
	}
	for _, k := range slices.Sorted(maps.Keys(opts.Env)) {
		cmd = append(cmd, Quote(k+"="+opts.Env[k]))
	}
	cmd = append(cmd, shellPath(opts.Kubectl))
	if opts.Kubeconfig != "" {
		cmd = append(cmd, "--kubeconfig", shellPath(opts.Kubeconfig))
	}
	if kctx != "" {
		cmd = append(cmd, "--context", Quote(kctx))
	}
	for _, a := range args {
		cmd = append(cmd, Quote(a))
	}
	return strings.Join(cmd, " ")
}

// Quote quotes s for a POSIX shell.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// shellPath quotes a path, leaving a leading ~/ for the shell to expand.
func shellPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return `"$HOME"/` + Quote(rest)
	}
	return Quote(p)
}

// run runs kubectl over m and returns its output, with kubectl's error
// message on failure.
func run(ctx context.Context, m *openssh.Master, opts Options, kctx, stdin string, args ...string) ([]byte, error) {
	out, err := m.Run(ctx, stdin, kubectl(opts, kctx, args...))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("kubectl %s: %w", args[0], ctx.Err())
		}
		msg := strings.TrimPrefix(err.Error(), "error: ")
		if strings.Contains(msg, "command not found") || strings.Contains(msg, ": not found") && strings.Contains(msg, "kubectl") {
			return nil, fmt.Errorf("kubectl not found on %s (set hosts.<name>.kube.kubectl)", where(m))
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

func where(m *openssh.Master) string {
	if m.IsLocal() {
		return "this machine"
	}
	return m.Dest()
}

// Contexts lists kubectl's contexts on m and the current one.
func Contexts(ctx context.Context, m *openssh.Master, opts Options) (all []string, current string, err error) {
	out, err := run(ctx, m, opts, "", "", "config", "get-contexts", "-o", "name")
	if err != nil {
		return nil, "", err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			all = append(all, l)
		}
	}
	cur, err := run(ctx, m, opts, "", "", "config", "current-context")
	if err == nil {
		current = strings.TrimSpace(string(cur))
	}
	return all, current, nil
}

// CurrentContext is kubectl's current context on m.
func CurrentContext(ctx context.Context, m *openssh.Master, opts Options) (string, error) {
	out, err := run(ctx, m, opts, "", "", "config", "current-context")
	if err != nil {
		return "", err
	}
	c := strings.TrimSpace(string(out))
	if c == "" {
		return "", errors.New("kubectl has no current context")
	}
	return c, nil
}

// labels on helper pods.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelOwner     = "tether.dev/owner"
	labelInstance  = "tether.dev/instance"
	managedBy      = "tether"
)

// Owner identifies this user on this machine in helper pod labels, so each
// daemon only cleans up its own pods.
var Owner = func() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return labelValue(name + "." + host)
}()

// instance tells this daemon's pods from those a previous one left behind.
var instance = randomSuffix(8)

var labelBad = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// labelValue makes s a valid label value: at most 63 characters of
// [A-Za-z0-9._-], starting and ending with an alphanumeric.
func labelValue(s string) string {
	s = labelBad.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Trim(s, "._-")
}

func randomSuffix(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// ClientVersion is kubectl's version on m, checking that it runs.
func ClientVersion(ctx context.Context, m *openssh.Master, opts Options) (string, error) {
	out, err := run(ctx, m, opts, "", "", "version", "--client")
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

// helperPermissions is what running a helper pod takes, as verb and
// resource for `kubectl auth can-i`.
var helperPermissions = [][2]string{
	{"create", "pods"}, {"get", "pods"}, {"delete", "pods"},
	{"create", "pods/exec"},
}

// MissingPermissions lists what the user can't do in namespace ns (the
// context's default if empty) that running a helper pod needs.
func MissingPermissions(ctx context.Context, m *openssh.Master, opts Options, kctx, ns string) ([]string, error) {
	var missing []string
	for _, p := range helperPermissions {
		args := []string{"auth", "can-i", p[0], p[1]}
		if ns != "" {
			args = append(args, "-n", ns)
		}
		// can-i exits 1 for "no"; tell that apart from failing to ask.
		out, err := m.Run(ctx, "", kubectl(opts, kctx, args...)+" || true")
		if err != nil {
			return nil, err
		}
		switch answer := strings.TrimSpace(string(out)); {
		case strings.HasPrefix(answer, "yes"):
		case strings.HasPrefix(answer, "no"):
			missing = append(missing, p[0]+" "+p[1])
		default:
			return nil, fmt.Errorf("kubectl auth can-i: unexpected answer %q", answer)
		}
	}
	return missing, nil
}

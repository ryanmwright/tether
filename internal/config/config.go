// Package config loads and validates tether's TOML configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/kube"
	"github.com/ryanmwright/tether/internal/mount"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/usbip"
)

type Config struct {
	Defaults Defaults           `toml:"defaults"`
	Hosts    map[string]Host    `toml:"hosts"`
	Profiles map[string]Profile `toml:"profiles"`
}

type Defaults struct {
	ReconnectBackoff Backoff `toml:"reconnect_backoff"`
	// LocalHost offers the built-in "local" host (PVC mounts with kubectl on
	// this machine) when kubectl is installed. On by default.
	LocalHost bool `toml:"local_host"`
	// CheckTargets checks that forwards' targets answer, every 30 seconds.
	// On by default; off for targets that log every connection.
	CheckTargets bool `toml:"check_targets"`
}

type Host struct {
	// SSH is the destination passed to ssh, usually an alias from
	// ~/.ssh/config. Defaults to the host's name.
	SSH         string `toml:"ssh"`
	Autoconnect bool   `toml:"autoconnect"`
	// Local makes this host stand for this machine, with no SSH: only PVC
	// mounts, with kubectl run here.
	Local bool `toml:"local"`
	Kube  Kube `toml:"kube"`
}

// LocalHost is the name of the built-in local host, present when kubectl is
// installed here and no configured host has that name.
const LocalHost = "local"

// Dest is where the host's session connects: its ssh destination, or
// openssh.LocalDest for a local host.
func (h Host) Dest() string {
	if h.Local {
		return openssh.LocalDest
	}
	return h.SSH
}

// Kube says how to run kubectl on a host and what helper pods look like;
// see kube.Options.
type Kube struct {
	Kubectl      string            `toml:"kubectl"`
	Kubeconfig   string            `toml:"kubeconfig"`
	Image        string            `toml:"image"`
	SFTPServer   string            `toml:"sftp_server"`
	RunAsUser    *int64            `toml:"run_as_user"`
	RunAsGroup   *int64            `toml:"run_as_group"`
	FSGroup      *int64            `toml:"fs_group"`
	MountRoot    string            `toml:"mount_root"`
	StartTimeout Duration          `toml:"start_timeout"`
	Env          map[string]string `toml:"env"`
}

func (k Kube) Options() kube.Options {
	return kube.Options{
		Kubectl:      k.Kubectl,
		Kubeconfig:   k.Kubeconfig,
		Image:        k.Image,
		SFTPServer:   k.SFTPServer,
		RunAsUser:    k.RunAsUser,
		RunAsGroup:   k.RunAsGroup,
		FSGroup:      k.FSGroup,
		MountRoot:    k.MountRoot,
		StartTimeout: time.Duration(k.StartTimeout),
		Env:          k.Env,
	}.WithDefaults()
}

// Duration is a time.Duration written as "2m".
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil || v <= 0 {
		return fmt.Errorf("invalid duration %q", text)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Profile is a named bundle of forwards, gpg, mounts and USB devices on one
// host.
type Profile struct {
	Host        string   `toml:"host"`
	Autoconnect bool     `toml:"autoconnect"`
	GPG         bool     `toml:"gpg"`     // forward gpg-agent
	GPGSSH      bool     `toml:"gpg_ssh"` // forward gpg-agent's SSH socket
	Forwards    []string `toml:"forwards"`
	Mounts      []Mount  `toml:"mounts"`
	USB         []string `toml:"usb"` // devices to share: bus IDs or vendor:product
}

type Direction = mount.Direction

const (
	RemoteToLocal = mount.RemoteToLocal
	LocalToRemote = mount.LocalToRemote
)

type Mount struct {
	Direction Direction `toml:"direction"`
	Remote    string    `toml:"remote"`
	Local     string    `toml:"local"`
	Options   []string  `toml:"options"`
	// PVC mounts a Kubernetes claim here instead: "[context/]namespace/claim".
	// Direction and Remote are then unused.
	PVC      string `toml:"pvc"`
	SubPath  string `toml:"sub_path"`
	ReadOnly bool   `toml:"read_only"`
}

// Spec is the mount as the mount package describes it, not yet normalized.
// A PVC mount gets the host's kube options, and its default mount point if
// Local is empty.
func (m Mount) Spec(h Host) (mount.Spec, error) {
	if m.PVC == "" {
		return mount.Spec{Direction: m.Direction, Remote: m.Remote, Local: m.Local, Options: m.Options}, nil
	}
	k, err := kube.ParseRef(m.PVC)
	if err != nil {
		return mount.Spec{}, err
	}
	k.SubPath, k.ReadOnly, k.Opts = m.SubPath, m.ReadOnly, h.Kube.Options()
	local := m.Local
	if local == "" {
		local = mount.DefaultPVCLocal(k.Opts.MountRoot, k)
	}
	return mount.Spec{Direction: mount.PVCToLocal, Local: local, Options: m.Options, Kube: &k}, nil
}

// Backoff is a reconnect delay range, written as "1s..60s" or a single
// duration for a fixed delay.
type Backoff struct {
	Min, Max time.Duration
}

func (b *Backoff) UnmarshalText(text []byte) error {
	lo, hi, isRange := strings.Cut(string(text), "..")
	if !isRange {
		hi = lo
	}
	var err error
	if b.Min, err = time.ParseDuration(strings.TrimSpace(lo)); err != nil {
		return fmt.Errorf("invalid backoff %q: %w", text, err)
	}
	if b.Max, err = time.ParseDuration(strings.TrimSpace(hi)); err != nil {
		return fmt.Errorf("invalid backoff %q: %w", text, err)
	}
	if b.Min <= 0 || b.Max < b.Min {
		return fmt.Errorf("invalid backoff %q: need 0 < min <= max", text)
	}
	return nil
}

func (b Backoff) MarshalText() ([]byte, error) {
	return []byte(b.Min.String() + ".." + b.Max.String()), nil
}

func Default() *Config {
	return &Config{
		Defaults: Defaults{ReconnectBackoff: Backoff{Min: time.Second, Max: time.Minute}, LocalHost: true, CheckTargets: true},
		Hosts:    map[string]Host{},
		Profiles: map[string]Profile{},
	}
}

// Load reads the config at path. A missing file yields the default (empty)
// config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func Parse(data []byte) (*Config, error) {
	c := Default()
	md, err := toml.Decode(string(data), c)
	if err != nil {
		return nil, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	for name, h := range c.Hosts {
		if h.SSH == "" && !h.Local {
			h.SSH = name
			c.Hosts[name] = h
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateHost checks one host's name and ssh destination.
func ValidateHost(name string, h Host) error {
	var errs []error
	if !namePattern.MatchString(name) {
		errs = append(errs, fmt.Errorf("hosts.%s: name must match %s", name, namePattern))
	}
	if err := h.Kube.Options().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("hosts.%s.kube: %w", name, err))
	}
	if h.Local {
		return errors.Join(errs...)
	}
	// The destination is passed to ssh as an argument, so it must not be
	// mistaken for an option.
	if h.SSH == "" || strings.HasPrefix(h.SSH, "-") || strings.ContainsFunc(h.SSH, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		errs = append(errs, fmt.Errorf("hosts.%s.ssh: invalid destination %q", name, h.SSH))
	}
	return errors.Join(errs...)
}

func validateMount(m Mount, h Host, home string) error {
	switch {
	case m.PVC != "" && (m.Remote != "" || m.Direction != ""):
		return errors.New("pvc can't be combined with remote or direction")
	case m.PVC == "" && (m.SubPath != "" || m.ReadOnly):
		return errors.New("sub_path and read_only are only for pvc mounts")
	case m.PVC == "" && h.Local:
		return errors.New("the host is local, so it can only have pvc mounts")
	case m.PVC == "" && (m.Remote == "" || m.Local == ""):
		return errors.New("remote and local are required")
	}
	spec, err := m.Spec(h)
	if err != nil {
		return err
	}
	_, err = mount.Normalize(spec, home)
	return err
}

// Validate reports every problem found, not just the first.
func (c *Config) Validate() error {
	home, _ := os.UserHomeDir()
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(c.Hosts)) {
		if err := ValidateHost(name, c.Hosts[name]); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Profiles)) {
		p := c.Profiles[name]
		if !namePattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("profiles.%s: name must match %s", name, namePattern))
		}
		host, known := c.Hosts[p.Host]
		if p.Host == LocalHost && !known && c.Defaults.LocalHost {
			host, known = Host{Local: true}, true // the built-in local host
		}
		if p.Host == "" {
			errs = append(errs, fmt.Errorf("profiles.%s.host: required", name))
		} else if !known {
			errs = append(errs, fmt.Errorf("profiles.%s.host: unknown host %q", name, p.Host))
		}
		if host.Local && (p.GPG || p.GPGSSH || len(p.USB) > 0) {
			errs = append(errs, fmt.Errorf("profiles.%s: host %q is local, so it can only have PVC mounts and Kubernetes forwards", name, p.Host))
		}
		for i, f := range p.Forwards {
			_, spec, err := forward.ParseInput(f)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("profiles.%s.forwards[%d]: %w", name, i, err))
			case host.Local && spec.Kind != forward.Kube:
				errs = append(errs, fmt.Errorf("profiles.%s.forwards[%d]: host %q is local, so it can only have Kubernetes forwards (K:…)", name, i, p.Host))
			}
		}
		for i, m := range p.Mounts {
			if err := validateMount(m, host, home); err != nil {
				errs = append(errs, fmt.Errorf("profiles.%s.mounts[%d]: %w", name, i, err))
			}
		}
		for i, u := range p.USB {
			if _, err := usbip.ParseSpec(u); err != nil {
				errs = append(errs, fmt.Errorf("profiles.%s.usb[%d]: %w", name, i, err))
			}
		}
	}
	return errors.Join(errs...)
}

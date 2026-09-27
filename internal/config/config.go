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
	"github.com/ryanmwright/tether/internal/mount"
)

type Config struct {
	Defaults Defaults           `toml:"defaults"`
	Hosts    map[string]Host    `toml:"hosts"`
	Profiles map[string]Profile `toml:"profiles"`
}

type Defaults struct {
	ReconnectBackoff Backoff `toml:"reconnect_backoff"`
}

type Host struct {
	// SSH is the destination passed to ssh, usually an alias from
	// ~/.ssh/config. Defaults to the host's name.
	SSH         string `toml:"ssh"`
	Autoconnect bool   `toml:"autoconnect"`
}

// Profile is a named bundle of forwards, gpg and mounts on one host.
type Profile struct {
	Host        string   `toml:"host"`
	Autoconnect bool     `toml:"autoconnect"`
	GPG         bool     `toml:"gpg"`     // forward gpg-agent
	GPGSSH      bool     `toml:"gpg_ssh"` // forward gpg-agent's SSH socket
	Forwards    []string `toml:"forwards"`
	Mounts      []Mount  `toml:"mounts"`
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
}

// Spec is the mount as the mount package describes it, not yet normalized.
func (m Mount) Spec() mount.Spec {
	return mount.Spec{Direction: m.Direction, Remote: m.Remote, Local: m.Local, Options: m.Options}
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
		Defaults: Defaults{ReconnectBackoff: Backoff{Min: time.Second, Max: time.Minute}},
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
		if h.SSH == "" {
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
	// The destination is passed to ssh as an argument, so it must not be
	// mistaken for an option.
	if h.SSH == "" || strings.HasPrefix(h.SSH, "-") || strings.ContainsFunc(h.SSH, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		errs = append(errs, fmt.Errorf("hosts.%s.ssh: invalid destination %q", name, h.SSH))
	}
	return errors.Join(errs...)
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
		if p.Host == "" {
			errs = append(errs, fmt.Errorf("profiles.%s.host: required", name))
		} else if _, ok := c.Hosts[p.Host]; !ok {
			errs = append(errs, fmt.Errorf("profiles.%s.host: unknown host %q", name, p.Host))
		}
		for i, f := range p.Forwards {
			if _, err := forward.Parse(f); err != nil {
				errs = append(errs, fmt.Errorf("profiles.%s.forwards[%d]: %w", name, i, err))
			}
		}
		for i, m := range p.Mounts {
			if m.Remote == "" || m.Local == "" {
				errs = append(errs, fmt.Errorf("profiles.%s.mounts[%d]: remote and local are required", name, i))
			} else if _, err := mount.Normalize(m.Spec(), home); err != nil {
				errs = append(errs, fmt.Errorf("profiles.%s.mounts[%d]: %w", name, i, err))
			}
		}
	}
	return errors.Join(errs...)
}

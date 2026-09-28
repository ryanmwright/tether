package usbip

import (
	"bytes"
	"cmp"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Files that `tether usbip-helper install` writes, relative to the root.
const (
	UnitPath      = "/etc/systemd/system/tether-usbip.service"
	ModulesPath   = "/etc/modules-load.d/tether-usbip.conf"
	InstallDir    = "/usr/local/lib/tether"
	InstalledPath = InstallDir + "/tether"
	serviceName   = "tether-usbip.service"
)

// installMarker heads every file install writes; uninstall removes only
// files that carry it.
const installMarker = "# Written by `tether usbip-helper install`; remove with `tether usbip-helper uninstall`."

// Installer sets the helper up as a systemd system service on distributions
// where no package or NixOS module does it.
type Installer struct {
	Root   string   // filesystem root; "" means /
	Binary string   // tether binary to install
	Users  []string // --allow-user values
	Listen string
	Out    io.Writer

	// Run runs a system command (systemctl, modprobe, restorecon); nil
	// means exec it.
	Run func(name string, args ...string) error
	// WaitReady waits for the started helper to answer; nil means dial its
	// socket.
	WaitReady    func(ctx context.Context) error
	ReadyTimeout time.Duration // default 10s
}

func (in *Installer) path(p string) string { return filepath.Join(cmp.Or(in.Root, "/"), p) }

func (in *Installer) run(name string, args ...string) error {
	if in.Run != nil {
		return in.Run(name, args...)
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (in *Installer) say(format string, args ...any) { fmt.Fprintf(in.Out, format+"\n", args...) }

// checkSystem refuses to touch NixOS (the module owns the service there) and
// systems without systemd.
func (in *Installer) checkSystem() error {
	if _, err := os.Stat(in.path("/etc/NIXOS")); err == nil {
		return errors.New("this is NixOS: use the module instead (imports = [ tether.nixosModules.usbip-helper ]; services.tether-usbip.enable = true)")
	}
	if _, err := os.Stat(in.path("/run/systemd/system")); err != nil {
		return errors.New("systemd isn't running here; start `tether usbip-helper` as root some other way")
	}
	return nil
}

// Unit is the service file.
func (in *Installer) Unit() string {
	args := []string{InstalledPath, "usbip-helper"}
	for _, u := range in.Users {
		args = append(args, "--allow-user", u)
	}
	if in.Listen != "" && in.Listen != DefaultListen {
		args = append(args, "--listen", in.Listen)
	}
	return installMarker + `
[Unit]
Description=tether USB/IP helper
After=systemd-modules-load.service

[Service]
ExecStart=` + strings.Join(args, " ") + `
RuntimeDirectory=tether-usbip
Restart=on-failure

[Install]
WantedBy=multi-user.target
`
}

// Install copies the binary, writes the unit and module config, and
// (re)starts the service. Running it again updates everything, e.g. after
// upgrading tether.
func (in *Installer) Install(ctx context.Context) error {
	if err := in.checkSystem(); err != nil {
		return err
	}
	if len(in.Users) == 0 {
		return errors.New("name the users who may share devices with --allow-user")
	}
	if err := in.checkStatic(); err != nil {
		return err
	}

	if err := in.copyBinary(); err != nil {
		return err
	}
	// With SELinux, files under /usr/local/lib get the label for binaries,
	// which systemd may run (unlike files in a home directory).
	if _, err := os.Stat(in.path("/sys/fs/selinux")); err == nil {
		if err := in.run("restorecon", "-R", in.path(InstallDir)); err != nil {
			in.say("warning: %v", err)
		}
	}

	if replaced, err := in.writeFile(UnitPath, in.Unit()); err != nil {
		return err
	} else if replaced {
		in.say("replaced %s, which tether didn't write", UnitPath)
	}
	if _, err := in.writeFile(ModulesPath, installMarker+"\nusbip-host\n"); err != nil {
		return err
	}
	if err := in.run("modprobe", "usbip-host"); err != nil {
		in.say("warning: couldn't load the usbip-host kernel module (%v); on Fedora, install kernel-modules-extra", err)
	}

	for _, args := range [][]string{{"daemon-reload"}, {"enable", serviceName}, {"restart", serviceName}} {
		if err := in.run("systemctl", args...); err != nil {
			return err
		}
	}
	wait := in.WaitReady
	if wait == nil {
		wait = func(ctx context.Context) error {
			_, err := (&Client{Socket: DefaultHelperSocket}).Port(ctx)
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(in.ReadyTimeout, 10*time.Second))
	defer cancel()
	var err error
	for {
		if err = wait(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the service started but the helper doesn't answer: %w; see `journalctl -u tether-usbip`", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	in.say("installed and started %s for %s", serviceName, strings.Join(in.Users, ", "))
	in.say("run this again after upgrading tether, so the service uses the new version")
	return nil
}

// checkStatic refuses a binary linked against libraries in the Nix store:
// the copy would stop working once those are garbage-collected.
func (in *Installer) checkStatic() error {
	f, err := elf.Open(in.Binary)
	if err != nil {
		return fmt.Errorf("reading %s: %w", in.Binary, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		interp, _ := io.ReadAll(p.Open())
		if bytes.HasPrefix(interp, []byte("/nix/store/")) {
			return fmt.Errorf("%s is linked against the Nix store, so a copy would break after garbage collection; use a statically linked tether (the flake's package is one, or build with CGO_ENABLED=0)", in.Binary)
		}
	}
	return nil
}

// copyBinary installs the binary atomically, so a running helper keeps its
// old copy until restarted.
func (in *Installer) copyBinary() error {
	src, err := os.Open(in.Binary)
	if err != nil {
		return err
	}
	defer src.Close()
	dst := in.path(InstalledPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tether-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// writeFile writes a config file and reports whether it replaced one that
// install hadn't written.
func (in *Installer) writeFile(p, content string) (replaced bool, err error) {
	full := in.path(p)
	if old, err := os.ReadFile(full); err == nil && !strings.HasPrefix(string(old), installMarker) {
		replaced = true
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}
	return replaced, os.WriteFile(full, []byte(content), 0o644)
}

// Uninstall stops and removes the service, which gives every shared device
// back to this machine. Files install didn't write are left alone.
func (in *Installer) Uninstall() error {
	if err := in.checkSystem(); err != nil {
		return err
	}
	unit, err := os.ReadFile(in.path(UnitPath))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		in.say("%s isn't installed", serviceName)
	case err != nil:
		return err
	case !strings.HasPrefix(string(unit), installMarker):
		return fmt.Errorf("%s wasn't written by `tether usbip-helper install`; remove it by hand", UnitPath)
	default:
		if err := in.run("systemctl", "disable", "--now", serviceName); err != nil {
			return err
		}
		if err := os.Remove(in.path(UnitPath)); err != nil {
			return err
		}
		if err := in.run("systemctl", "daemon-reload"); err != nil {
			return err
		}
	}

	if b, err := os.ReadFile(in.path(ModulesPath)); err == nil && strings.HasPrefix(string(b), installMarker) {
		os.Remove(in.path(ModulesPath))
	}
	if err := os.Remove(in.path(InstalledPath)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	os.Remove(in.path(InstallDir)) // only if empty
	in.say("removed %s; shared devices are back on this machine", serviceName)
	return nil
}

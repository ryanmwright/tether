package usbip

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeELF writes a minimal ELF executable, dynamically linked against interp
// unless it's empty.
func fakeELF(t *testing.T, interp string) string {
	t.Helper()
	var b bytes.Buffer
	h := elf.Header64{
		Type: uint16(elf.ET_EXEC), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
		Ehsize: 64, Phentsize: 56, Shentsize: 64,
	}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS], h.Ident[elf.EI_DATA], h.Ident[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	if interp != "" {
		h.Phoff, h.Phnum = 64, 1
	}
	binary.Write(&b, binary.LittleEndian, h)
	if interp != "" {
		n := uint64(len(interp) + 1)
		binary.Write(&b, binary.LittleEndian, elf.Prog64{Type: uint32(elf.PT_INTERP), Flags: uint32(elf.PF_R), Off: 64 + 56, Filesz: n, Memsz: n, Align: 1})
		b.WriteString(interp + "\x00")
	}
	path := filepath.Join(t.TempDir(), "tether")
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakeSystem struct {
	root string
	cmds []string
	out  bytes.Buffer
}

func newFakeSystem(t *testing.T, dirs ...string) *fakeSystem {
	t.Helper()
	fs := &fakeSystem{root: t.TempDir()}
	for _, d := range append([]string{"/run/systemd/system"}, dirs...) {
		os.MkdirAll(filepath.Join(fs.root, d), 0o755)
	}
	return fs
}

func (fs *fakeSystem) installer(binary string) *Installer {
	return &Installer{
		Root: fs.root, Binary: binary, Users: []string{"1000"}, Listen: DefaultListen, Out: &fs.out,
		Run: func(name string, args ...string) error {
			fs.cmds = append(fs.cmds, strings.Join(append([]string{name}, args...), " "))
			return nil
		},
		WaitReady: func(context.Context) error { return nil },
	}
}

func (fs *fakeSystem) read(p string) string {
	b, _ := os.ReadFile(filepath.Join(fs.root, p))
	return string(b)
}

func (fs *fakeSystem) exists(p string) bool {
	_, err := os.Stat(filepath.Join(fs.root, p))
	return err == nil
}

func TestInstall(t *testing.T) {
	fs := newFakeSystem(t)
	bin := fakeELF(t, "")
	if err := fs.installer(bin).Install(context.Background()); err != nil {
		t.Fatal(err)
	}

	unit := fs.read(UnitPath)
	if !strings.HasPrefix(unit, installMarker) || !strings.Contains(unit, "ExecStart=/usr/local/lib/tether/tether usbip-helper --allow-user 1000\n") ||
		!strings.Contains(unit, "RuntimeDirectory=tether-usbip") || strings.Contains(unit, "--listen") {
		t.Errorf("unit:\n%s", unit)
	}
	if got := fs.read(ModulesPath); !strings.HasSuffix(got, "\nusbip-host\n") {
		t.Errorf("modules-load: %q", got)
	}
	want, _ := os.ReadFile(bin)
	if fi, err := os.Stat(filepath.Join(fs.root, InstalledPath)); err != nil || fi.Mode().Perm() != 0o755 || fs.read(InstalledPath) != string(want) {
		t.Errorf("installed binary: %v %v", fi, err)
	}
	if want := []string{"modprobe usbip-host", "systemctl daemon-reload", "systemctl enable tether-usbip.service", "systemctl restart tether-usbip.service"}; !slices.Equal(fs.cmds, want) {
		t.Errorf("commands = %q, want %q", fs.cmds, want)
	}
	if strings.Contains(fs.out.String(), "replaced") {
		t.Errorf("output: %s", fs.out.String())
	}

	// Again, e.g. after an upgrade: same result, with a non-default port.
	in := fs.installer(bin)
	in.Listen = "127.0.0.1:3241"
	if err := in.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs.read(UnitPath), "--allow-user 1000 --listen 127.0.0.1:3241\n") || strings.Contains(fs.out.String(), "replaced") {
		t.Errorf("reinstall: unit %s, output %s", fs.read(UnitPath), fs.out.String())
	}
}

func TestInstallSELinux(t *testing.T) {
	fs := newFakeSystem(t, "/sys/fs/selinux")
	if err := fs.installer(fakeELF(t, "")).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "restorecon -R " + filepath.Join(fs.root, InstallDir); !slices.Contains(fs.cmds, want) {
		t.Errorf("commands %q lack %q", fs.cmds, want)
	}
}

func TestInstallReplacesHandWrittenUnit(t *testing.T) {
	fs := newFakeSystem(t, "/etc/systemd/system")
	os.WriteFile(filepath.Join(fs.root, UnitPath), []byte("[Service]\nExecStart=/home/me/.nix-profile/bin/tether usbip-helper\n"), 0o644)
	if err := fs.installer(fakeELF(t, "")).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs.out.String(), "replaced /etc/systemd/system/tether-usbip.service") || !strings.HasPrefix(fs.read(UnitPath), installMarker) {
		t.Errorf("output %q, unit %q", fs.out.String(), fs.read(UnitPath))
	}
}

func TestInstallRefuses(t *testing.T) {
	static := fakeELF(t, "")
	for _, tt := range []struct {
		name  string
		setup func(fs *fakeSystem, in *Installer)
		want  string
	}{
		{"nixos", func(fs *fakeSystem, _ *Installer) { os.WriteFile(filepath.Join(fs.root, "/etc/NIXOS"), nil, 0o644) }, "this is NixOS"},
		{"no systemd", func(fs *fakeSystem, _ *Installer) { os.RemoveAll(filepath.Join(fs.root, "/run/systemd")) }, "systemd isn't running"},
		{"no users", func(_ *fakeSystem, in *Installer) { in.Users = nil }, "--allow-user"},
		{"nix-linked binary", func(_ *fakeSystem, in *Installer) {
			in.Binary = fakeELF(t, "/nix/store/abc-glibc-2.42/lib/ld-linux-x86-64.so.2")
		}, "linked against the Nix store"},
		{"helper doesn't answer", func(_ *fakeSystem, in *Installer) {
			in.WaitReady = func(context.Context) error { return errors.New("connection refused") }
			in.ReadyTimeout = 50 * time.Millisecond
		}, "journalctl -u tether-usbip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeSystem(t, "/etc")
			in := fs.installer(static)
			tt.setup(fs, in)
			err := in.Install(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	// A system-linked binary is fine.
	fs := newFakeSystem(t)
	if err := fs.installer(fakeELF(t, "/lib64/ld-linux-x86-64.so.2")).Install(context.Background()); err != nil {
		t.Errorf("system-linked binary: %v", err)
	}
}

func TestUninstall(t *testing.T) {
	fs := newFakeSystem(t)
	if err := fs.installer(fakeELF(t, "")).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs.cmds = nil
	if err := fs.installer("").Uninstall(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{UnitPath, ModulesPath, InstalledPath, InstallDir} {
		if fs.exists(p) {
			t.Errorf("%s still exists", p)
		}
	}
	if want := []string{"systemctl disable --now tether-usbip.service", "systemctl daemon-reload"}; !slices.Equal(fs.cmds, want) {
		t.Errorf("commands = %q, want %q", fs.cmds, want)
	}

	// Nothing installed: says so, runs nothing.
	fs.cmds, fs.out = nil, bytes.Buffer{}
	if err := fs.installer("").Uninstall(); err != nil || len(fs.cmds) != 0 || !strings.Contains(fs.out.String(), "isn't installed") {
		t.Errorf("err %v, commands %q, output %q", err, fs.cmds, fs.out.String())
	}

	// A unit written by hand is left alone.
	os.MkdirAll(filepath.Dir(filepath.Join(fs.root, UnitPath)), 0o755)
	os.WriteFile(filepath.Join(fs.root, UnitPath), []byte("[Service]\n"), 0o644)
	if err := fs.installer("").Uninstall(); err == nil || !fs.exists(UnitPath) {
		t.Errorf("hand-written unit: err %v, exists %v", err, fs.exists(UnitPath))
	}
}

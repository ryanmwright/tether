package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/sshtest"
)

// startWithMounts is startWithSSH where the "remote" sessions can run the
// test's sshfs: they get this test's PATH.
func startWithMounts(t *testing.T, extraTOML string) *sshHarness {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("sshfs"); err != nil {
		t.Skip("sshfs not found")
	}
	path := strings.ReplaceAll(os.Getenv("PATH"), "'", "")
	srv := sshtest.StartWithConfig(t, fmt.Sprintf(`ForceCommand env PATH='%s' /bin/sh -c "$SSH_ORIGINAL_COMMAND"`, path))
	cfg := fmt.Sprintf("[defaults]\nreconnect_backoff = \"100ms..500ms\"\n\n[hosts.dev]\nssh = %q\n%s", sshtest.HostAlias, extraTOML)
	h := start(t, cfg, openssh.Options{ConfigFile: srv.ConfigFile})
	return &sshHarness{harness: h, srv: srv, c: h.client(t)}
}

// mountDirs makes short-lived directories, lazily unmounting anything left
// on them if the test fails midway.
func mountDirs(t *testing.T, names ...string) []string {
	t.Helper()
	base := mkdirTemp(t, "tether-dm-")
	var dirs []string
	for _, n := range names {
		d := filepath.Join(base, n)
		os.Mkdir(d, 0o755)
		dirs = append(dirs, d)
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			exec.Command("fusermount3", "-uz", d).Run()
		}
	})
	return dirs
}

func mountOf(st api.Status, hostName, key string) api.MountStatus {
	for _, m := range host(st, hostName).Mounts {
		if m.Key == key {
			return m
		}
	}
	return api.MountStatus{}
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestProfileMounts(t *testing.T) {
	dirs := mountDirs(t, "remote-src", "local-mp", "shared", "remote-mp")
	remoteSrc, localMP, shared, remoteMP := dirs[0], dirs[1], dirs[2], dirs[3]
	os.WriteFile(filepath.Join(remoteSrc, "r.txt"), []byte("remote file"), 0o644)
	os.WriteFile(filepath.Join(shared, "l.txt"), []byte("local file"), 0o644)

	h := startWithMounts(t, fmt.Sprintf(`
[profiles.work]
host = "dev"
[[profiles.work.mounts]]
direction = "remote-to-local"
remote = %q
local = %q
[[profiles.work.mounts]]
direction = "local-to-remote"
remote = %q
local = %q
`, remoteSrc, localMP, remoteMP, shared))
	downKey := "remote:" + remoteSrc + " -> " + localMP
	upKey := shared + " -> remote:" + remoteMP

	var res api.DoctorResult
	h.call(t, api.MethodDoctor, api.DoctorParams{Host: "dev"}, &res)
	for _, section := range []string{"local", "remote"} {
		found := false
		for _, c := range res.Checks {
			if c.Section == section && c.Name == "mounts" {
				found = true
				if c.Status != api.CheckOK {
					t.Errorf("doctor %s mounts = %+v", section, c)
				}
			}
		}
		if !found {
			t.Errorf("doctor has no %s mounts check", section)
		}
	}

	h.call(t, api.MethodUp, api.TargetParams{Name: "work"}, nil)
	st := h.waitFor(t, "profile up", func(st api.Status) bool { return profile(st, "work").State == api.StateUp })
	if m := mountOf(st, "dev", downKey); m.State != api.StateUp || m.Direction != "remote-to-local" || m.Profiles[0] != "work" {
		t.Errorf("remote-to-local mount = %+v", m)
	}
	if m := mountOf(st, "dev", upKey); m.State != api.StateUp || m.Direction != "local-to-remote" {
		t.Errorf("local-to-remote mount = %+v", m)
	}
	if readFile(filepath.Join(localMP, "r.txt")) != "remote file" || readFile(filepath.Join(remoteMP, "l.txt")) != "local file" {
		t.Fatal("files not visible through the mounts")
	}

	// A mount unmounted by hand is noticed and restored.
	if out, err := exec.Command("fusermount3", "-u", localMP).CombinedOutput(); err != nil {
		t.Fatalf("manual unmount: %v %s", err, out)
	}
	h.waitFor(t, "mount marked lost", func(st api.Status) bool { return mountOf(st, "dev", downKey).State == api.StateError })
	h.call(t, api.MethodUp, api.TargetParams{Name: "work"}, nil) // retry now rather than in 15s
	h.waitFor(t, "mount restored", func(st api.Status) bool { return mountOf(st, "dev", downKey).State == api.StateUp })
	if readFile(filepath.Join(localMP, "r.txt")) != "remote file" {
		t.Error("restored mount not working")
	}

	// Mounts come back after the server restarts.
	h.srv.Stop()
	h.waitFor(t, "connection lost", func(st api.Status) bool { return host(st, "dev").State == api.StateError })
	h.srv.Restart(t)
	h.waitFor(t, "profile back up", func(st api.Status) bool { return profile(st, "work").State == api.StateUp })
	if readFile(filepath.Join(localMP, "r.txt")) != "remote file" || readFile(filepath.Join(remoteMP, "l.txt")) != "local file" {
		t.Fatal("mounts not restored after reconnect")
	}

	h.call(t, api.MethodDown, api.TargetParams{Name: "work"}, nil)
	h.waitFor(t, "host down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })
	if readFile(filepath.Join(localMP, "r.txt")) != "" || readFile(filepath.Join(remoteMP, "l.txt")) != "" {
		t.Error("still mounted after down")
	}
}

func TestAdHocMount(t *testing.T) {
	dirs := mountDirs(t, "remote-src", "local-mp")
	os.WriteFile(filepath.Join(dirs[0], "r.txt"), []byte("remote file"), 0o644)
	h := startWithMounts(t, "")

	var res api.MountResult
	h.call(t, api.MethodMountAdd, api.MountParams{Host: "dev", Direction: "remote-to-local", Remote: dirs[0], Local: dirs[1]}, &res)
	h.waitFor(t, "mount up", func(st api.Status) bool {
		m := mountOf(st, "dev", res.Key)
		return m.State == api.StateUp && m.AdHoc
	})
	if readFile(filepath.Join(dirs[1], "r.txt")) != "remote file" {
		t.Fatal("ad-hoc mount not working")
	}

	h.call(t, api.MethodMountRemove, api.MountParams{Host: "dev", Direction: "remote-to-local", Remote: dirs[0], Local: dirs[1]}, nil)
	h.waitFor(t, "host down", func(st api.Status) bool { return host(st, "dev").State == api.StateDown })
	if readFile(filepath.Join(dirs[1], "r.txt")) != "" {
		t.Error("still mounted after removal")
	}
}

func TestMountFailureDegrades(t *testing.T) {
	dirs := mountDirs(t, "local-mp")
	h := startWithMounts(t, fmt.Sprintf(`
[profiles.work]
host = "dev"
[[profiles.work.mounts]]
direction = "remote-to-local"
remote = "/nonexistent-tether-dir"
local = %q
`, dirs[0]))
	h.call(t, api.MethodUp, api.TargetParams{Name: "work"}, nil)
	st := h.waitFor(t, "degraded", func(st api.Status) bool { return profile(st, "work").State == api.StateDegraded })
	if p := profile(st, "work"); !strings.Contains(p.Error, "No such file") {
		t.Errorf("profile error = %q", p.Error)
	}
}

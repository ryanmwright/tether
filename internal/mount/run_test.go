package mount

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/sshtest"
)

// startSSH runs a test sshd whose sessions use this test's PATH, so the
// "remote" finds the same sshfs and fusermount as the local side.
func startSSH(t *testing.T) (*sshtest.Server, *openssh.Master) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("sshfs"); err != nil {
		t.Skip("sshfs not found")
	}
	if len(fusermountCandidates()) == 0 {
		t.Skip("fusermount not found")
	}
	srv := sshtest.StartWithConfig(t, fmt.Sprintf(`ForceCommand env PATH=%s /bin/sh -c "$SSH_ORIGINAL_COMMAND"`, shellQuote(os.Getenv("PATH"))))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := openssh.Start(ctx, openssh.Options{ConfigFile: srv.ConfigFile}, sshtest.HostAlias, filepath.Join(srv.Dir, "ctl"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return srv, m
}

// tempDir is a short-lived directory whose mounts, if a test fails midway,
// are lazily unmounted before it's removed.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tether-mount-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if mounted(dir) {
			unmountLocal(dir, true)
		}
		os.RemoveAll(dir)
	})
	return dir
}

func start(t *testing.T, m *openssh.Master, s Spec) *Running {
	t.Helper()
	s, err := Normalize(s, "/nonexistent-home")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Start(ctx, m, s, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func stop(t *testing.T, r *Running) {
	t.Helper()
	if err := r.Stop(context.Background()); err != nil {
		t.Errorf("stop: %v", err)
	}
}

func TestRemoteToLocal(t *testing.T) {
	_, m := startSSH(t)
	remote, mp := tempDir(t), tempDir(t)
	os.WriteFile(filepath.Join(remote, "hello.txt"), []byte("from remote"), 0o644)

	r := start(t, m, Spec{Direction: RemoteToLocal, Remote: remote, Local: mp})
	if got, err := os.ReadFile(filepath.Join(mp, "hello.txt")); err != nil || string(got) != "from remote" {
		t.Fatalf("read through mount: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(mp, "new.txt"), []byte("from local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(remote, "new.txt")); string(got) != "from local" {
		t.Errorf("write didn't reach the remote: %q", got)
	}

	stop(t, r)
	if mounted(mp) {
		t.Error("still mounted after Stop")
	}
	if _, err := os.Stat(filepath.Join(mp, "hello.txt")); err == nil {
		t.Error("remote files still visible after Stop")
	}
}

func TestLocalToRemote(t *testing.T) {
	_, m := startSSH(t)
	base := tempDir(t)
	shared, remoteMP := filepath.Join(base, "shared"), filepath.Join(base, "remote-mp")
	os.Mkdir(shared, 0o755)
	os.WriteFile(filepath.Join(shared, "hello.txt"), []byte("from local"), 0o644)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("do not share"), 0o600)
	os.Symlink("../secret.txt", filepath.Join(shared, "escape"))

	r := start(t, m, Spec{Direction: LocalToRemote, Remote: remoteMP, Local: shared})
	// "Remote" and local are the same machine here, so read the remote
	// mount point directly.
	if got, err := os.ReadFile(filepath.Join(remoteMP, "hello.txt")); err != nil || string(got) != "from local" {
		t.Fatalf("read through mount: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(remoteMP, "new.txt"), []byte("from remote"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(shared, "new.txt")); string(got) != "from remote" {
		t.Errorf("write didn't reach the local directory: %q", got)
	}
	// The remote only reaches the shared directory, even via a symlink.
	if got, err := os.ReadFile(filepath.Join(remoteMP, "escape")); err == nil {
		t.Errorf("read outside the shared directory through a symlink: %q", got)
	}

	stop(t, r)
	if mounted(remoteMP) {
		t.Error("remote still mounted after Stop")
	}
}

func TestMountEndsWithConnection(t *testing.T) {
	srv, m := startSSH(t)
	remote, mp := tempDir(t), tempDir(t)
	r := start(t, m, Spec{Direction: RemoteToLocal, Remote: remote, Local: mp})

	srv.Stop()
	select {
	case <-r.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("mount didn't notice the connection going away")
	}
	if mounted(mp) {
		t.Error("stale mount left behind")
	}
}

func TestStartErrors(t *testing.T) {
	_, m := startSSH(t)
	ctx := context.Background()
	s, _ := Normalize(Spec{Direction: LocalToRemote, Remote: "/tmp/x", Local: "/nonexistent-tether-dir"}, "/")
	if _, err := Start(ctx, m, s, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("sharing a missing local directory succeeded")
	}
	mp := tempDir(t)
	s, _ = Normalize(Spec{Direction: RemoteToLocal, Remote: "/nonexistent-tether-dir", Local: mp}, "/")
	if _, err := Start(ctx, m, s, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("mounting a missing remote directory succeeded")
	} else {
		t.Logf("missing remote dir: %v", err)
	}
	if mounted(mp) {
		t.Error("failed mount left behind")
	}
}

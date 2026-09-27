package sftpjail

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pkg/sftp"
)

// setup serves a jail rooted at <tmp>/jail, next to a secret file outside
// it, and returns a client.
func setup(t *testing.T) (*sftp.Client, string, string) {
	t.Helper()
	base := t.TempDir()
	jail := filepath.Join(base, "jail")
	os.Mkdir(jail, 0o755)
	secret := filepath.Join(base, "secret.txt")
	os.WriteFile(secret, []byte("do not share"), 0o600)

	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(jail, server) }()
	c, err := sftp.NewClientPipe(client, client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return c, jail, secret
}

func TestFileOperations(t *testing.T) {
	c, jail, _ := setup(t)

	f, err := c.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello world"))
	f.Close()
	if got, _ := os.ReadFile(filepath.Join(jail, "hello.txt")); string(got) != "hello world" {
		t.Fatalf("written file = %q", got)
	}

	f, _ = c.Open("hello.txt") // relative paths resolve from the root
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "hello world" {
		t.Errorf("read = %q", got)
	}

	if err := c.Truncate("/hello.txt", 5); err != nil {
		t.Fatal(err)
	}
	if err := c.Chmod("/hello.txt", 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := c.Stat("/hello.txt")
	if err != nil || fi.Size() != 5 || fi.Mode().Perm() != 0o600 {
		t.Errorf("stat = %v %v", fi, err)
	}

	if err := c.MkdirAll("/a/b"); err != nil {
		t.Fatal(err)
	}
	if err := c.PosixRename("/hello.txt", "/a/b/moved.txt"); err != nil {
		t.Fatal(err)
	}
	entries, err := c.ReadDir("/a/b")
	if err != nil || len(entries) != 1 || entries[0].Name() != "moved.txt" {
		t.Errorf("readdir = %v %v", entries, err)
	}
	if err := c.Symlink("moved.txt", "/a/b/link"); err != nil {
		t.Fatal(err)
	}
	if target, err := c.ReadLink("/a/b/link"); err != nil || target != "moved.txt" {
		t.Errorf("readlink = %q %v", target, err)
	}
	if _, err := c.StatVFS("/"); err != nil {
		t.Errorf("statvfs: %v", err)
	}
	for _, p := range []string{"/a/b/link", "/a/b/moved.txt"} {
		if err := c.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.RemoveDirectory("/a/b"); err != nil {
		t.Fatal(err)
	}
}

// The remote side of a mount may be hostile; nothing outside the shared
// directory may be reachable.
func TestConfinement(t *testing.T) {
	c, jail, secret := setup(t)

	for _, p := range []string{"/../secret.txt", "../secret.txt", "/../../etc/passwd"} {
		if f, err := c.Open(p); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if string(b) == "do not share" {
				t.Errorf("read the secret through %q", p)
			}
		}
	}

	// A symlink planted inside (absolute, or climbing out) mustn't be
	// followed out of the root.
	os.Symlink(secret, filepath.Join(jail, "abs"))
	os.Symlink("../secret.txt", filepath.Join(jail, "rel"))
	for _, p := range []string{"/abs", "/rel"} {
		if _, err := c.Open(p); err == nil {
			t.Errorf("opened %s, which points outside the root", p)
		}
		if _, err := c.Stat(p); err == nil {
			t.Errorf("stat %s followed a link outside the root", p)
		}
		if f, err := c.OpenFile(p, os.O_WRONLY|os.O_TRUNC); err == nil {
			f.Close()
			t.Errorf("opened %s for writing", p)
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "do not share" {
		t.Errorf("secret modified: %q", b)
	}

	// Symlinks can be created (they're only text), but not used to escape.
	if err := c.Symlink("/etc/passwd", "/planted"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open("/planted"); err == nil {
		t.Error("followed a planted absolute symlink")
	}
	// ".." can't climb above the root: these land inside it.
	if err := c.Rename("/planted", "/../escaped"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mkdir("/../outside-dir"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"escaped", "outside-dir"} {
		if _, err := os.Lstat(filepath.Join(jail, name)); err != nil {
			t.Errorf("%s not created inside the root: %v", name, err)
		}
	}

	entries, _ := os.ReadDir(filepath.Dir(jail))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"jail", "secret.txt"}) {
		t.Errorf("files outside the root changed: %v", names)
	}
}

func TestRenameDoesNotReplace(t *testing.T) {
	c, jail, _ := setup(t)
	os.WriteFile(filepath.Join(jail, "a"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(jail, "b"), []byte("b"), 0o644)
	if err := c.Rename("/a", "/b"); err == nil {
		t.Error("v3 rename replaced an existing file")
	}
	if err := c.PosixRename("/a", "/b"); err != nil {
		t.Errorf("posix rename: %v", err)
	}
}

package gpg

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestDesktopPinentry(t *testing.T) {
	if _, err := exec.LookPath("gpg-agent"); err != nil {
		t.Skip("gpg-agent not found")
	}
	home, err := os.MkdirTemp("", "tether-gpg") // short: the agent's socket path is limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd := exec.Command("gpgconf", "--homedir", home, "--kill", "gpg-agent")
		cmd.Run()
		os.RemoveAll(home)
	})
	if out, err := exec.Command("gpgconf", "--homedir", home, "--launch", "gpg-agent").CombinedOutput(); err != nil {
		t.Fatalf("starting gpg-agent: %v: %s", err, out)
	}
	ctx := context.Background()
	base := []string{"GNUPGHOME=" + home, "PATH=" + os.Getenv("PATH")}
	startup := func() map[string]string {
		t.Helper()
		out, err := agentCommand(ctx, base, "GETINFO std_startup_env")
		if err != nil {
			t.Fatal(err)
		}
		return parseEnvData(out)
	}

	// As a shell's startup hook without a display leaves it.
	if _, err := agentCommand(ctx, append(base, "GPG_TTY=/dev/pts/9", "TERM=xterm"), "UPDATESTARTUPTTY"); err != nil {
		t.Fatal(err)
	}
	desktop := append(base, "WAYLAND_DISPLAY=wayland-1", "DISPLAY=:1", "GPG_TTY=/dev/pts/1")
	if changed, err := DesktopPinentry(ctx, desktop); err != nil || !changed {
		t.Fatalf("DesktopPinentry = %t, %v", changed, err)
	}
	if env := startup(); env["WAYLAND_DISPLAY"] != "wayland-1" || env["DISPLAY"] != ":1" || env["GPG_TTY"] != "/dev/pts/9" || env["TERM"] != "xterm" {
		t.Errorf("startup env = %v, want the display added and the terminal kept", env)
	}
	if changed, err := DesktopPinentry(ctx, desktop); err != nil || changed {
		t.Errorf("again: DesktopPinentry = %t, %v", changed, err)
	}
	// Outside a desktop session it does nothing.
	if changed, err := DesktopPinentry(ctx, base); err != nil || changed {
		t.Errorf("without a display: DesktopPinentry = %t, %v", changed, err)
	}
}

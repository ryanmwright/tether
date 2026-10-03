package gpg

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DesktopPinentry makes sure the local gpg-agent's default environment
// names a display, so a PIN prompt for a remote's request can pop up on the
// desktop. Requests through the extra socket (a remote's) may not set their
// own environment, so Pinentry gets the default; without a display, a
// graphical Pinentry falls back to a terminal it doesn't have and fails
// ("Inappropriate ioctl for device"). The default is whatever last ran
// `gpg-connect-agent updatestartuptty`, often a shell's startup hook, so a
// shell without a display can take it away again.
//
// env is the desktop session's environment. The terminal already in the
// default (GPG_TTY, TERM) is kept. It reports whether it changed anything.
func DesktopPinentry(ctx context.Context, env []string) (bool, error) {
	if lookupEnv(env, "DISPLAY") == "" && lookupEnv(env, "WAYLAND_DISPLAY") == "" {
		return false, nil // not a desktop session
	}
	startup, err := startupEnv(ctx, env)
	if err != nil {
		return false, err
	}
	if PromptDisplay(startup) != "" {
		return false, nil
	}
	var e []string
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != "GPG_TTY" && k != "TERM" {
			e = append(e, kv)
		}
	}
	for _, k := range []string{"GPG_TTY", "TERM"} {
		if v := startup[k]; v != "" {
			e = append(e, k+"="+v)
		}
	}
	if _, err := agentCommand(ctx, e, "UPDATESTARTUPTTY"); err != nil {
		return false, err
	}
	return true, nil
}

// StartupEnv is the local gpg-agent's default environment, which Pinentry
// gets for requests that can't set their own, such as a remote's.
func StartupEnv(ctx context.Context) (map[string]string, error) {
	return startupEnv(ctx, os.Environ())
}

func startupEnv(ctx context.Context, env []string) (map[string]string, error) {
	out, err := agentCommand(ctx, env, "GETINFO std_startup_env")
	if err != nil {
		return nil, err
	}
	return parseEnvData(out), nil
}

// PromptDisplay is the display named in env that a graphical Pinentry
// would use, or "".
func PromptDisplay(env map[string]string) string {
	if d := env["WAYLAND_DISPLAY"]; d != "" {
		return d
	}
	return env["DISPLAY"]
}

// agentCommand runs an Assuan command on the local gpg-agent, which must
// be running, with env as the client's environment.
func agentCommand(ctx context.Context, env []string, command string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gpg-connect-agent", "--no-autostart", "--decode", command, "/bye")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		for line := range bytes.Lines(out) {
			if bytes.HasPrefix(line, []byte("ERR ")) {
				err = fmt.Errorf("%s", bytes.TrimSpace(line))
				break
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gpg-connect-agent %s: %v: %s", command, err, bytes.TrimSpace(out))
	}
	return out, nil
}

// parseEnvData reads NAME=VALUE pairs, each ended by a NUL, from GETINFO's
// decoded data lines.
func parseEnvData(out []byte) map[string]string {
	vars := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "D ")
		if !ok {
			continue
		}
		for pair := range strings.SplitSeq(line, "\x00") {
			pair = strings.TrimPrefix(pair, "D ") // decoded lines run together
			if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
				vars[k] = v
			}
		}
	}
	return vars
}

func lookupEnv(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

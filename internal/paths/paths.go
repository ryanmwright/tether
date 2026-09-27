// Package paths resolves tether's default file locations following the XDG
// base directory spec.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const app = "tether"

// ConfigFile is $XDG_CONFIG_HOME/tether/config.toml.
func ConfigFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, app, "config.toml")
}

// RuntimeDir is $XDG_RUNTIME_DIR/tether, falling back to a per-user
// directory under the system temp dir.
func RuntimeDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, app)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", app, os.Getuid()))
}

func SocketPath() string {
	return filepath.Join(RuntimeDir(), app+".sock")
}

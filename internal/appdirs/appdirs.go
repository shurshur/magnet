// Package appdirs contains default locations used by the command-line tools.
package appdirs

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultDataDir returns the default state directory for magnet.
func DefaultDataDir() string {
	if v := strings.TrimSpace(os.Getenv("MAGNET_DATA_DIR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); v != "" {
		return filepath.Join(v, "magnet")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "magnet")
	}
	return ".magnet"
}

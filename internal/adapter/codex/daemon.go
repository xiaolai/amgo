package codex

import (
	"fmt"
	"os"
	"path/filepath"

	"amgo/internal/platform"
)

// SocketPath is the standard managed-daemon control socket.
func SocketPath(codexHome string) string {
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
}

// ValidateDaemon resolves and validates the user-owned control socket. The
// published path is normally a symlink to a private socket.
func ValidateDaemon(path string) (string, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("codex daemon socket not found: %w", err)
	}
	if li.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("codex daemon symlink cannot be resolved: %w", err)
		}
		if err := platform.CheckSocket(resolved, platform.UID()); err != nil {
			return "", fmt.Errorf("codex daemon socket rejected: %w", err)
		}
		return resolved, nil
	}
	if err := platform.CheckSocket(path, platform.UID()); err != nil {
		return "", fmt.Errorf("codex daemon socket rejected: %w", err)
	}
	return path, nil
}

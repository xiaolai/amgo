// Package config resolves the amgo home and native agent homes. v0.1 has no
// project config file: an untrusted repository cannot select executables,
// endpoints or permissions.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const appName = "amgo"

// Home resolves the private state root. Explicit --home wins, then AMGO_HOME,
// then a platform default. It never falls back to a repository-relative path:
// the working directory must not select state.
func Home(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("AMGO_HOME"); v != "" {
		return v, nil
	}
	hd, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot resolve the user home directory; pass --home")
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(hd, "Library", "Application Support", appName), nil
	case "linux":
		if v := os.Getenv("XDG_STATE_HOME"); v != "" {
			return filepath.Join(v, appName), nil
		}
		return filepath.Join(hd, ".local", "state", appName), nil
	default:
		return filepath.Join(hd, ".amgo"), nil
	}
}

func DBPath(home string) string       { return filepath.Join(home, "state.db") }
func OutcomesLog(home string) string  { return filepath.Join(home, "outcomes.log") }
func ArtifactsDir(home string) string { return filepath.Join(home, "artifacts") }

// EnsureHome creates the private state root with owner-only permissions.
func EnsureHome(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return os.Chmod(home, 0o700)
}

// ClaudeHomes resolves the Claude config homes to scan. Explicit flags win,
// then CLAUDE_CONFIG_DIR, then ~/.claude.
func ClaudeHomes(flags []string) []string {
	if len(flags) > 0 {
		return dedupe(flags)
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return []string{v}
	}
	hd, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(hd, ".claude")}
}

// CodexHome resolves the Codex config home.
func CodexHome(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	hd, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(hd, ".codex")
}

func HostID() string { return runtime.GOOS + "-" + runtime.GOARCH }

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

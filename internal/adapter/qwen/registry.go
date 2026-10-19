package qwen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"amgo/internal/config"
	"amgo/internal/model"
	"amgo/internal/platform"
)

const maxRecordBytes = 64 << 10

// record is one $QWEN_HOME/sessions/<pid>.json entry. ipcToken is read only
// just before a send and is never copied into a Target, candidate, log or
// export.
type record struct {
	SchemaVersion int             `json:"schemaVersion"`
	PID           int             `json:"pid"`
	ProcStart     *string         `json:"procStart"`
	PIDNS         json.RawMessage `json:"pidNs"`
	SessionID     string          `json:"sessionId"`
	Cwd           string          `json:"cwd"`
	Name          string          `json:"name"`
	StartedAt     int64           `json:"startedAt"`
	QwenVersion   string          `json:"qwenVersion"`
	Kind          string          `json:"kind"`
	IPCPath       string          `json:"ipcPath"`
	IPCToken      string          `json:"ipcToken"`
}

// filenamePID validates the documented record filename and returns its PID.
func filenamePID(path string) (int, bool) {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	if base == filepath.Base(path) {
		return 0, false
	}
	pidPart := base
	if i := strings.IndexByte(base, '-'); i > 0 {
		if len(base)-i-1 != 8 {
			return 0, false
		}
		for _, r := range base[i+1:] {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return 0, false
			}
		}
		pidPart = base[:i]
	}
	pid, err := strconv.Atoi(pidPart)
	if err != nil || pid <= 0 || strconv.Itoa(pid) != pidPart {
		return 0, false
	}
	return pid, true
}

func readRecord(path string) (record, error) {
	var rec record
	raw, err := platform.ReadRegularFile(path, maxRecordBytes, platform.UID())
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, err
	}
	pid, ok := filenamePID(path)
	if !ok || rec.PID != pid {
		return rec, fmt.Errorf("record PID does not match its filename")
	}
	if rec.SchemaVersion != 1 {
		return rec, fmt.Errorf("unsupported schemaVersion %d", rec.SchemaVersion)
	}
	if rec.SessionID == "" {
		return rec, fmt.Errorf("record has no sessionId")
	}
	if rec.IPCToken != "" && !validToken(rec.IPCToken) {
		return rec, fmt.Errorf("record ipcToken is not 64 hex characters")
	}
	return rec, nil
}

func validToken(t string) bool {
	if len(t) != 64 {
		return false
	}
	for _, r := range t {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func procStartOf(r record) string {
	if r.ProcStart == nil {
		return ""
	}
	return *r.ProcStart
}

// live checks the documented registry liveness rules that can be evaluated on
// this platform. On Linux it also binds pidNs and boot-id/start-ticks; on other
// platforms the official record leaves procStart and pidNs null, so PID reuse
// is not distinguishable.
func live(r record) (bool, string) {
	if !platform.PIDAlive(r.PID) {
		return false, "process is not alive"
	}
	if ok, reason := linuxIdentityMatches(r); !ok {
		return false, reason
	}
	return true, ""
}

// checkInbox validates the socket and its containing directory. The official
// protocol requires an owner-only directory (0700) and socket (0600).
func checkInbox(path string) error {
	if path == "" {
		return fmt.Errorf("record has no ipcPath")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("ipcPath is not absolute")
	}
	if err := platform.CheckSocket(path, platform.UID()); err != nil {
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("ipcPath mode is %o; want owner-only", fi.Mode().Perm())
	}
	return nil
}

func kindOf(r record) string {
	if r.Kind == "" {
		return "tui"
	}
	return r.Kind
}

func displayName(r record) string {
	if r.Name != "" {
		return r.Name
	}
	return fmt.Sprintf("qwen-%d", r.PID)
}

// candidates reads every registry record in home. Unreadable, stale or
// higher-schema records are skipped; they are never deleted.
func candidates(home string) []model.Candidate {
	if home == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(home, "sessions", "*.json"))
	out := make([]model.Candidate, 0, len(paths))
	for _, path := range paths {
		rec, err := readRecord(path)
		if err != nil {
			continue
		}
		reason := ""
		ok, why := live(rec)
		if !ok {
			reason = why
		}
		if ok {
			if err := checkInbox(rec.IPCPath); err != nil {
				ok = false
				reason = err.Error()
			}
		}
		target := model.Target{
			Adapter: model.AdapterQwen, HostID: config.HostID(), ConfigHome: home,
			NativeSessionID: rec.SessionID, Endpoint: rec.IPCPath, PID: rec.PID,
			ProcStart: procStartOf(rec), PIDDomain: runtime.GOOS, Cwd: rec.Cwd,
			Version: rec.QwenVersion, Kind: kindOf(rec),
		}
		out = append(out, model.Candidate{
			Target: target, Name: displayName(rec), Live: ok,
			EvidenceLevel: "documented", Reason: reason,
		})
	}
	return out
}

// recordForTarget re-reads the registry immediately before a send because
// /clear and /resume replace sessionId under the same PID.
func recordForTarget(target model.Target) (record, error) {
	if target.ConfigHome == "" || target.PID <= 0 {
		return record{}, model.NewError(model.ErrTargetUnavailable, "qwen target has no registry identity")
	}
	paths, _ := filepath.Glob(filepath.Join(target.ConfigHome, "sessions", fmt.Sprintf("%d*.json", target.PID)))
	for _, path := range paths {
		rec, err := readRecord(path)
		if err != nil || rec.PID != target.PID {
			continue
		}
		if rec.SessionID != target.NativeSessionID {
			continue
		}
		if rec.IPCPath != target.Endpoint {
			return record{}, model.NewError(model.ErrTargetUnavailable,
				"qwen inbox path changed; re-read discovery and join again")
		}
		if ok, why := live(rec); !ok {
			return record{}, model.NewError(model.ErrTargetNotLive, why)
		}
		if err := checkInbox(rec.IPCPath); err != nil {
			return record{}, model.WrapError(model.ErrTargetUnavailable, "qwen inbox is not usable", err)
		}
		return rec, nil
	}
	return record{}, model.NewError(model.ErrTargetUnavailable,
		"qwen registry record with that session ID is gone; re-read discovery and join again")
}

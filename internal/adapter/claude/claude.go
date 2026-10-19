// Package claude implements the native Claude Code session adapter.
package claude

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/model"
	"amgo/internal/platform"
)

const (
	MinVersion    = "2.1.248"
	TestedVersion = "2.1.303"
	maxBodyBytes  = 64 << 10
	maxLineBytes  = 1048576
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return model.AdapterClaude }

func (a *Adapter) Probe(ctx context.Context) (model.Capabilities, error) {
	caps := model.Capabilities{
		Adapter:         model.AdapterClaude,
		Maturity:        model.MaturityFixtureTested,
		MinKnownVersion: MinVersion,
		TestedVersions:  []string{TestedVersion},
		LiveMessaging:   false,
		NativeReceipts:  false,
		Constraints: []string{
			"an external sender receives no native delivery receipts",
			"receiver holds/refusals are unobservable",
			"session_id is an ingress guard only",
		},
	}
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		caps.Constraints = append(caps.Constraints, "claude binary unavailable")
		return caps, nil
	}
	v := strings.Fields(strings.TrimSpace(string(out)))
	if len(v) == 0 {
		return caps, nil
	}
	caps.LiveMessaging = versionAtLeast(v[0], MinVersion)
	if !caps.LiveMessaging {
		caps.Constraints = append(caps.Constraints, "installed version is below the known messaging floor")
	}
	return caps, nil
}

func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	var out []model.Candidate
	for _, home := range req.Homes {
		if home == "" {
			continue
		}
		abs, err := filepath.Abs(home)
		if err != nil {
			continue
		}
		home = abs
		entries, err := os.ReadDir(sessionsDir(home))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			default:
			}
			rec, err := readRecord(filepath.Join(sessionsDir(home), e.Name()))
			if err != nil {
				continue
			}
			if c, ok := candidate(home, rec); ok {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

func candidate(home string, rec SessionRecord) (model.Candidate, bool) {
	if rec.Spare || rec.ParkedJobID != "" || rec.SessionID == "" || rec.MessagingSocketPath == "" || rec.PeerProtocol < 1 {
		return model.Candidate{}, false
	}
	c := model.Candidate{
		Target: model.Target{
			Adapter:         model.AdapterClaude,
			HostID:          config.HostID(),
			ConfigHome:      home,
			NativeSessionID: rec.SessionID,
			SocketPath:      rec.MessagingSocketPath,
			PID:             rec.PID,
			ProcStart:       normalizeProcStart(rec.ProcStart),
			PIDDomain:       rec.PIDDomain,
			Version:         rec.Version,
			PeerProtocol:    rec.PeerProtocol,
			PeerFeatures:    rec.PeerFeatures,
			Status:          rec.Status,
			Kind:            rec.Kind,
		},
		Name:          rec.Name,
		EvidenceLevel: "observed",
		Live:          true,
	}
	if c.Name == "" {
		c.Name = "claude-" + strconv.Itoa(rec.PID)
	}
	if rec.PIDDomain != "" && rec.PIDDomain != localDomain() {
		c.Live = false
		c.Reason = "pid domain mismatch"
		return c, true
	}
	if !platform.PIDAlive(rec.PID) {
		return c, false
	}
	if rec.ProcStart != "" {
		live, err := platform.ProcStartUTC(rec.PID)
		if err != nil || normalizeProcStart(live) != c.Target.ProcStart {
			return c, false
		}
	}
	if err := platform.CheckSocket(rec.MessagingSocketPath, platform.UID()); err != nil {
		c.Live = false
		c.Reason = "socket check failed: " + err.Error()
		return c, true
	}
	if rec.Version != "" && !versionAtLeast(rec.Version, MinVersion) {
		c.Live = false
		c.Reason = "version is below " + MinVersion
		return c, true
	}
	kp := KeyPath(home, rec.PID, rec.MessagingSocketPath)
	if _, err := os.Lstat(kp); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, true
		}
		c.Live = false
		c.Reason = "key file is unreadable"
		return c, true
	}
	k, err := ReadKey(kp)
	if err != nil {
		c.Live = false
		c.Reason = "key file rejected: " + err.Error()
		return c, true
	}
	if k.PIDDomain != "" && k.PIDDomain != localDomain() {
		c.Live = false
		c.Reason = "key pid domain mismatch"
		return c, true
	}
	if k.ProcStart != "" && normalizeProcStart(k.ProcStart) != c.Target.ProcStart {
		c.Live = false
		c.Reason = "key process start mismatch"
		return c, true
	}
	return c, true
}

func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	if len(req.Body) == 0 {
		return adapter.SendResult{}, model.NewError(model.ErrUsage, "empty message body")
	}
	if len(req.Body) > maxBodyBytes {
		return adapter.SendResult{}, model.NewError(model.ErrUsage, "message body exceeds 64 KiB")
	}
	if req.Target.Adapter != model.AdapterClaude {
		return adapter.SendResult{}, model.NewError(model.ErrInternal, "wrong adapter target")
	}
	if req.Target.NativeSessionID == "" {
		return adapter.SendResult{}, model.NewError(model.ErrTargetUnavailable, "target has no native session ID; the delivery guard would be omitted")
	}
	if req.Target.ConfigHome == "" {
		return adapter.SendResult{}, model.NewError(model.ErrTargetUnavailable, "target has no config home; the peer key cannot be located")
	}
	if req.Target.PIDDomain != "" && req.Target.PIDDomain != localDomain() {
		return adapter.SendResult{}, model.NewError(model.ErrTargetUnavailable, "pid domain mismatch")
	}
	if !platform.PIDAlive(req.Target.PID) {
		return adapter.SendResult{}, model.NewError(model.ErrTargetUnavailable, "session process is not alive")
	}
	if req.Target.ProcStart != "" {
		live, err := platform.ProcStartUTC(req.Target.PID)
		if err != nil || normalizeProcStart(live) != normalizeProcStart(req.Target.ProcStart) {
			return adapter.SendResult{}, model.NewError(model.ErrTargetUnavailable, "session process start changed")
		}
	}
	if err := platform.CheckSocket(req.Target.SocketPath, platform.UID()); err != nil {
		return adapter.SendResult{}, model.WrapError(model.ErrTargetUnavailable, "session socket rejected", err)
	}

	token := ""
	configHome, err := filepath.Abs(req.Target.ConfigHome)
	if err != nil {
		return adapter.SendResult{}, model.WrapError(model.ErrTargetUnavailable, "config home cannot be resolved", err)
	}
	kp := KeyPath(configHome, req.Target.PID, req.Target.SocketPath)
	if _, err := os.Lstat(kp); err == nil {
		k, err := ReadKey(kp)
		if err != nil {
			return adapter.SendResult{}, model.WrapError(model.ErrTargetUnavailable, "session key rejected", err)
		}
		if k.PIDDomain != "" && k.PIDDomain != localDomain() {
			return adapter.SendResult{}, model.NewError(model.ErrTargetUnavailable, "session key pid domain mismatch")
		}
		if k.ProcStart != "" && normalizeProcStart(k.ProcStart) != normalizeProcStart(req.Target.ProcStart) {
			return adapter.SendResult{}, model.NewError(model.ErrTargetUnavailable, "session key process start mismatch")
		}
		token = k.PeerToken
	} else if !errors.Is(err, os.ErrNotExist) {
		return adapter.SendResult{}, model.WrapError(model.ErrTargetUnavailable, "session key is unreadable", err)
	}

	frame, err := buildFrame(req, token)
	if err != nil {
		return adapter.SendResult{}, err
	}
	return writeFrame(ctx, req.Target.SocketPath, frame, req.Timeout)
}

func versionAtLeast(v, min string) bool {
	a := parseVersion(v)
	b := parseVersion(min)
	for i := 0; i < 3; i++ {
		if a[i] > b[i] {
			return true
		}
		if a[i] < b[i] {
			return false
		}
	}
	return true
}

func parseVersion(v string) [3]int {
	var out [3]int
	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n, _ := strconv.Atoi(strings.TrimFunc(parts[i], func(r rune) bool { return r < '0' || r > '9' }))
		out[i] = n
	}
	return out
}

var _ adapter.Adapter = (*Adapter)(nil)

// compile-time guard for the timeout helper

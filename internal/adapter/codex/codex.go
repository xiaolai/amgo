// Package codex implements the Codex app-server adapter. It speaks WebSocket
// over the shared daemon's Unix socket and uses the experimental queue API.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/ids"
	"amgo/internal/model"
)

const (
	TestedVersion = "0.163.0"
	maxPages      = 10
	pageSize      = 100
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return model.AdapterCodex }

func (a *Adapter) Probe(ctx context.Context) (model.Capabilities, error) {
	caps := model.Capabilities{
		Adapter:         model.AdapterCodex,
		Maturity:        model.MaturityFixtureTested,
		MinKnownVersion: TestedVersion,
		TestedVersions:  []string{TestedVersion},
		LiveMessaging:   false,
		NativeReceipts:  true,
		Constraints: []string{
			"app-server WebSocket transport and thread/queue/add are experimental and undocumented",
			"the loaded target guard is best-effort; an ABA reload is possible",
			"a send to an idle thread starts a turn and may spend tokens",
		},
	}
	out, err := exec.CommandContext(ctx, "codex", "--version").Output()
	if err != nil {
		caps.Constraints = append(caps.Constraints, "codex binary unavailable")
		return caps, nil
	}
	v := versionToken(string(out))
	if v == "" {
		return caps, nil
	}
	if sameMinor(v, TestedVersion) {
		caps.LiveMessaging = true
	} else {
		caps.Constraints = append(caps.Constraints, "version is outside the tested minor release")
	}
	home := config.CodexHome("")
	if home == "" {
		caps.Constraints = append(caps.Constraints, "codex config home cannot be resolved")
		return caps, nil
	}
	sock := SocketPath(home)
	resolved, err := ValidateDaemon(sock)
	if err != nil {
		caps.LiveMessaging = false
		caps.Constraints = append(caps.Constraints, err.Error())
		return caps, nil
	}
	c, err := dial(ctx, resolved, 2*time.Second)
	if err != nil {
		caps.LiveMessaging = false
		caps.Constraints = append(caps.Constraints, "daemon handshake failed")
		return caps, nil
	}
	defer c.close()
	if err := handshake(ctx, c); err != nil {
		caps.LiveMessaging = false
		caps.Constraints = append(caps.Constraints, "daemon initialize failed")
	}
	return caps, nil
}

func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	home := req.CodexHome
	if home == "" {
		home = config.CodexHome("")
	}
	if home == "" {
		return nil, model.NewError(model.ErrTargetUnavailable, "codex config home cannot be resolved; pass --codex-home")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "codex config home cannot be resolved", err)
	}
	home = abs
	sock := SocketPath(home)
	resolved, err := ValidateDaemon(sock)
	if err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "codex daemon unavailable", err)
	}
	c, err := dial(ctx, resolved, 2*time.Second)
	if err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "codex daemon unavailable", err)
	}
	defer c.close()
	if err := handshake(ctx, c); err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "codex daemon handshake failed", err)
	}

	var loaded loadedListResponse
	if err := c.call(ctx, "thread/loaded/list", loadedListParams{}, &loaded); err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "cannot list loaded Codex threads", err)
	}
	loadedSet := map[string]bool{}
	for _, id := range loaded.Data {
		loadedSet[id] = true
	}

	seen := map[string]bool{}
	var threads []thread
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var res threadListResponse
		p := threadListParams{Limit: pageSize, Cursor: cursor, UseStateDBOnly: true}
		if err := c.call(ctx, "thread/list", p, &res); err != nil {
			return nil, model.WrapError(model.ErrTargetUnavailable, "cannot list Codex threads", err)
		}
		for _, t := range res.Data {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			threads = append(threads, t)
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			break
		}
		cursor = res.NextCursor
	}

	known := make(map[string]bool, len(threads))
	for _, t := range threads {
		known[t.ID] = true
	}
	var missing []string
	for id := range loadedSet {
		if !known[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		t := thread{ID: id}
		var read threadReadResponse
		if err := c.call(ctx, "thread/read", threadReadParams{ThreadID: id}, &read); err == nil && read.Thread.ID != "" {
			t = read.Thread
		}
		if t.Ephemeral {
			continue
		}
		threads = append(threads, t)
	}

	out := make([]model.Candidate, 0, len(threads))
	for _, t := range threads {
		if t.ID == "" || t.Ephemeral {
			continue
		}
		live := loadedSet[t.ID] || (t.CanAcceptDirectInput != nil && *t.CanAcceptDirectInput)
		cand := model.Candidate{
			Target: model.Target{
				Adapter:         model.AdapterCodex,
				HostID:          config.HostID(),
				ConfigHome:      home,
				NativeSessionID: t.ID,
				Endpoint:        resolved,
				SocketPath:      resolved,
				PID:             0,
				Cwd:             t.Cwd,
				Version:         t.CLIVersion,
				Status:          t.Status.Type,
				Kind:            t.Originator,
			},
			Name:          "codex-" + shortID(t.ID),
			EvidenceLevel: "observed",
			Live:          live,
		}
		if !live {
			cand.Reason = "thread is not loaded in the shared daemon"
		}
		out = append(out, cand)
	}
	return out, nil
}

func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	if req.Target.Adapter != model.AdapterCodex {
		return adapter.SendResult{}, model.NewError(model.ErrInternal, "wrong adapter target")
	}
	if len(req.Body) == 0 {
		return adapter.SendResult{}, model.NewError(model.ErrUsage, "empty message body")
	}
	sock := req.Target.SocketPath
	if sock == "" {
		if req.Target.ConfigHome == "" {
			return preSend("target_unavailable", "target has no codex config home"), nil
		}
		home, err := filepath.Abs(req.Target.ConfigHome)
		if err != nil {
			return preSend("target_unavailable", "codex config home cannot be resolved"), nil
		}
		sock = SocketPath(home)
	}
	resolved, err := ValidateDaemon(sock)
	if err != nil {
		return preSend("target_unavailable", err.Error()), nil
	}
	c, err := dial(ctx, resolved, 2*time.Second)
	if err != nil {
		return preSend("target_unavailable", "codex daemon unavailable"), nil
	}
	defer c.close()
	if err := handshake(ctx, c); err != nil {
		return preSend("target_unavailable", "codex daemon handshake failed"), nil
	}

	var read threadReadResponse
	if err := c.call(ctx, "thread/read", threadReadParams{ThreadID: req.Target.NativeSessionID}, &read); err != nil {
		code := "target_unavailable"
		var rpc *rpcError
		if errors.As(err, &rpc) && (strings.Contains(rpc.Message, "no rollout found") || strings.Contains(rpc.Message, "not loaded")) {
			code = "target_not_live"
		}
		return preSend(code, "thread read failed: "+err.Error()), nil
	}
	t := read.Thread
	if t.Ephemeral {
		return preSend("target_not_live", "ephemeral threads cannot receive amgo messages"), nil
	}
	if t.CanAcceptDirectInput != nil && !*t.CanAcceptDirectInput {
		return preSend("target_not_live", "the daemon does not accept direct input for this thread"), nil
	}
	var loaded loadedListResponse
	if err := c.call(ctx, "thread/loaded/list", loadedListParams{}, &loaded); err != nil {
		if t.CanAcceptDirectInput == nil || !*t.CanAcceptDirectInput {
			return preSend("target_unavailable", "cannot verify that the thread is loaded: "+err.Error()), nil
		}
	} else {
		isLoaded := false
		for _, id := range loaded.Data {
			if id == req.Target.NativeSessionID {
				isLoaded = true
				break
			}
		}
		if !isLoaded && (t.CanAcceptDirectInput == nil || !*t.CanAcceptDirectInput) {
			return preSend("target_not_live", "thread is stored but not loaded in the shared daemon"), nil
		}
	}

	uuid, err := ids.UUIDPart(req.MessageID)
	if err != nil {
		uuid = ids.NewUUID()
	}
	body := adapter.FrameBody(req)
	params := queueAddParams{
		ThreadID:            req.Target.NativeSessionID,
		ClientUserMessageID: uuid,
		Input:               []userInput{{Type: "text", Text: body}},
	}
	var res queueAddResponse
	err = c.call(ctx, "thread/queue/add", params, &res)
	if err != nil {
		if errors.Is(err, ErrNotSent) {
			return preSend("target_unavailable", "queue request was not sent"), nil
		}
		var rpc *rpcError
		if errors.As(err, &rpc) {
			code := "target_unavailable"
			if strings.Contains(rpc.Message, "no rollout found") || strings.Contains(rpc.Message, "not loaded") {
				code = "target_not_live"
			}
			return preSend(code, rpc.Message), nil
		}
		// The request was written but the response was lost. A matching item
		// is positive evidence even in this case.
		if started, _ := matchingItem(c.takeNotifications(), req.Target.NativeSessionID, uuid); started {
			return adapter.SendResult{
				Outcome:           "submitted",
				NativeDisposition: model.DispositionStarted,
				Evidence:          "queue response was lost but the target thread emitted a matching user item",
			}, nil
		}
		return adapter.SendResult{
			Outcome:           "unknown",
			NativeDisposition: model.DispositionUnknown,
			Evidence:          "queue request may have been delivered; response was lost",
		}, nil
	}

	if res.QueuedSubmission.ID == "" {
		return adapter.SendResult{
			Outcome:           "unknown",
			NativeDisposition: model.DispositionUnknown,
			Evidence:          "thread/queue/add returned no submission ID; delivery is ambiguous",
		}, nil
	}
	if res.QueuedSubmission.ClientUserMessageID != uuid {
		return adapter.SendResult{
			Outcome:           "unknown",
			NativeDisposition: model.DispositionUnknown,
			Evidence:          "thread/queue/add returned a submission for a different client message ID",
		}, nil
	}
	disposition := model.DispositionQueued
	if started, _ := matchingItem(c.takeNotifications(), req.Target.NativeSessionID, uuid); started {
		disposition = model.DispositionStarted
	}
	return adapter.SendResult{
		Outcome:            "submitted",
		NativeSubmissionID: res.QueuedSubmission.ID,
		NativeDisposition:  disposition,
		Evidence:           "thread/queue/add returned a queued submission",
	}, nil
}

func preSend(code, detail string) adapter.SendResult {
	return adapter.SendResult{
		Outcome:           "failed_before_submit",
		NativeDisposition: model.DispositionUnknown,
		ErrorCode:         code,
		ErrorDetail:       detail,
		Evidence:          "no payload was written",
	}
}

func handshake(ctx context.Context, c *client) error {
	var res initializeResponse
	if err := c.call(ctx, "initialize", initializeParams{
		ClientInfo:   clientInfo{Name: "amgo", Title: "amgo", Version: "0.1.0"},
		Capabilities: capabilities{ExperimentalAPI: true},
	}, &res); err != nil {
		return err
	}
	v := versionFromUserAgent(res.UserAgent)
	if v == "" {
		return fmt.Errorf("initialize response has no parseable app-server version")
	}
	if !sameMinor(v, TestedVersion) {
		return fmt.Errorf("untested app-server version %s", v)
	}
	return c.notify(ctx, "initialized", map[string]any{})
}

func matchingItem(notes []rpcNotification, threadID, clientID string) (started, completed bool) {
	for _, n := range notes {
		switch n.Method {
		case "item/started", "item/completed":
			var p itemStartedParams
			if err := json.Unmarshal(n.Params, &p); err != nil {
				continue
			}
			if p.ThreadID != threadID || p.Item.Type != "userMessage" || p.Item.ClientID != clientID {
				continue
			}
			if n.Method == "item/started" {
				started = true
			} else {
				completed = true
				started = true
			}
		}
	}
	return started, completed
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func versionToken(s string) string {
	for _, f := range strings.Fields(s) {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && strings.Contains(f, ".") {
			return strings.TrimFunc(f, func(r rune) bool { return !(r >= '0' && r <= '9' || r == '.') })
		}
	}
	return ""
}

func versionFromUserAgent(s string) string {
	// Example: codex-tui/0.163.0 (Mac OS ...; arm64) ...
	if i := strings.IndexByte(s, '/'); i >= 0 {
		rest := s[i+1:]
		if j := strings.IndexByte(rest, ' '); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	return ""
}

func sameMinor(v, want string) bool {
	a, ok := parseVersion(v)
	if !ok {
		return false
	}
	b, ok := parseVersion(want)
	if !ok {
		return false
	}
	return len(a) >= 2 && len(b) >= 2 && a[0] == b[0] && a[1] == b[1]
}

func parseVersion(v string) ([]int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return nil, false
	}
	out := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

var _ adapter.Adapter = (*Adapter)(nil)

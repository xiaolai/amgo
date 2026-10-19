package opencode

import (
	"context"
	"fmt"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/model"
)

// Discover lists sessions on an explicitly supplied loopback server. There is
// no documented way to find a TUI's random port, so no endpoint means no
// candidates rather than a guess.
func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	if req.Endpoint == "" {
		return nil, nil
	}
	api, err := newAPI(req.Endpoint)
	if err != nil {
		return nil, model.WrapError(model.ErrUsage, "invalid OpenCode endpoint", err)
	}
	h, err := api.health(ctx)
	if err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "OpenCode server is not reachable", err)
	}
	if !h.Healthy {
		return nil, model.NewError(model.ErrTargetUnavailable, "OpenCode server reports unhealthy")
	}
	sessions, err := api.sessions(ctx)
	if err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "cannot list OpenCode sessions", err)
	}
	out := make([]model.Candidate, 0, len(sessions))
	for _, s := range sessions {
		if s.ID == "" || s.ParentID != "" {
			continue
		}
		name := s.Title
		if name == "" {
			name = s.ID
		}
		out = append(out, model.Candidate{
			Target: model.Target{
				Adapter: model.AdapterOpenCode, HostID: config.HostID(),
				Endpoint: api.base, NativeSessionID: s.ID, Cwd: s.Directory,
				Version: h.Version, Kind: "serve",
			},
			Name: name, Live: true, EvidenceLevel: "documented",
		})
	}
	return out, nil
}

// Send posts one prompt to an existing session. HTTP 204 is acceptance only.
func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	api, err := newAPI(req.Target.Endpoint)
	if err != nil {
		return adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrUsage, ErrorDetail: err.Error()},
			model.WrapError(model.ErrUsage, "invalid OpenCode endpoint", err)
	}
	if req.Target.NativeSessionID == "" {
		return adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable, ErrorDetail: "session ID is missing"},
			model.NewError(model.ErrTargetUnavailable, "OpenCode target has no session ID")
	}
	h, err := api.health(ctx)
	if err != nil {
		return adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable, ErrorDetail: "server health check failed"},
			model.WrapError(model.ErrTargetUnavailable, "OpenCode server is not reachable", err)
	}
	if req.Target.Version != "" && h.Version != req.Target.Version {
		return adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable, ErrorDetail: "OpenCode server version changed"},
			model.NewError(model.ErrTargetUnavailable, fmt.Sprintf("OpenCode server changed from %s to %s; re-join", req.Target.Version, h.Version))
	}
	if _, err := api.session(ctx, req.Target.NativeSessionID); err != nil {
		return adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable, ErrorDetail: "session is not available"},
			model.WrapError(model.ErrTargetUnavailable, "OpenCode session is not available", err)
	}

	connected := false
	status, err := api.promptAsync(ctx, req.Target.NativeSessionID, adapter.FrameBody(req), &connected)
	switch {
	case err != nil && !connected:
		return adapter.SendResult{
			Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable,
			ErrorDetail: "no connection to the OpenCode server",
		}, model.WrapError(model.ErrTargetUnavailable, "cannot reach the OpenCode server", err)
	case err != nil:
		if status == 400 || status == 401 || status == 403 || status == 404 {
			return adapter.SendResult{
				Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable,
				ErrorDetail: fmt.Sprintf("OpenCode rejected the prompt with HTTP %d", status),
			}, nil
		}
		return adapter.SendResult{
			Outcome: "unknown", ErrorCode: model.ErrUnknownSubmission,
			NativeDisposition: model.DispositionUnknown,
			ErrorDetail:       "the prompt request may have reached the OpenCode server",
		}, nil
	case status != 204:
		return adapter.SendResult{
			Outcome: "unknown", ErrorCode: model.ErrUnknownSubmission,
			NativeDisposition: model.DispositionUnknown,
			ErrorDetail:       fmt.Sprintf("unexpected OpenCode status HTTP %d", status),
		}, nil
	}
	return adapter.SendResult{
		Outcome: "submitted", NativeDisposition: model.DispositionUnknown,
		Evidence: "OpenCode HTTP 204 accepted the prompt; no native delivery receipt exists",
	}, nil
}

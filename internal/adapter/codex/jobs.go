package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"amgo/internal/adapter"
	"amgo/internal/model"
	"amgo/internal/runner"
)

// RunSpec builds `codex exec --json`. The prompt is written to stdin; no bypass
// or approval flag is ever added. Review maps to the provider's read-only
// sandbox. A resume cannot change the sandbox, so review continuation is
// refused rather than silently downgraded.
func (a *Adapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	sandbox := "workspace-write"
	switch req.Policy {
	case "", "default":
	case "review":
		sandbox = "read-only"
	default:
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported, fmt.Sprintf("unknown execution policy %q", req.Policy))
	}
	if req.NativeSessionID != "" && req.Policy == "review" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			"codex resume cannot set the read-only sandbox; review continuation is unsupported")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return adapter.RunPlan{}, model.WrapError(model.ErrUnsupported, "codex binary is unavailable", err)
	}

	var args []string
	var provenance string
	if req.NativeSessionID != "" {
		args = []string{"exec", "resume", "--json", "--skip-git-repo-check"}
		if req.Model != "" {
			args = append(args, "-m", req.Model)
		}
		if req.Effort != "" {
			args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
		}
		args = append(args, req.NativeSessionID, "-")
		provenance = "codex exec resume; sandbox inherited from the stopped native session; approvals native"
	} else {
		args = []string{"exec", "--json", "--skip-git-repo-check", "-C", req.Workspace, "-s", sandbox}
		if req.Model != "" {
			args = append(args, "-m", req.Model)
		}
		if req.Effort != "" {
			args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
		}
		args = append(args, "-")
		provenance = fmt.Sprintf("codex exec sandbox=%s; approvals native", sandbox)
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: path, Args: args, Dir: req.Workspace,
			Prompt: []byte(req.Prompt), Provenance: provenance,
		},
		Parser: NewJobParser(),
	}, nil
}

// JobParser consumes codex exec --json events.
type JobParser struct {
	sessionID   string
	lastMessage string
	usage       *model.Usage
	done        bool
	failed      bool
	needsAction bool
	detail      string
}

func NewJobParser() *JobParser { return &JobParser{} }

func (p *JobParser) Line(line []byte) ([]runner.Event, error) {
	var raw struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Message  string `json:"message"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
		Item *struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Status string `json:"status"`
		} `json:"item"`
		Usage *struct {
			InputTokens           int64 `json:"input_tokens"`
			CachedInputTokens     int64 `json:"cached_input_tokens"`
			CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
			OutputTokens          int64 `json:"output_tokens"`
			ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	switch raw.Type {
	case "thread.started":
		p.sessionID = raw.ThreadID
		return []runner.Event{{Kind: "native_session", Payload: map[string]any{"session_id": raw.ThreadID}}}, nil
	case "turn.started":
		return []runner.Event{{Kind: "progress", Payload: map[string]any{"event": "turn_started"}}}, nil
	case "item.started", "item.updated", "item.completed":
		if raw.Item == nil {
			return nil, nil
		}
		switch raw.Item.Type {
		case "agent_message":
			if raw.Item.Text != "" {
				p.lastMessage = raw.Item.Text
			}
		case "command_execution":
			if raw.Item.Status == "declined" || raw.Item.Status == "denied" {
				p.needsAction = true
				p.detail = "provider denied a required command"
			}
		case "error":
			p.failed = true
			p.detail = raw.Item.Text
		}
		return []runner.Event{{Kind: "progress", Payload: map[string]any{
			"event": raw.Type, "item_type": raw.Item.Type, "status": raw.Item.Status,
		}}}, nil
	case "turn.completed":
		p.done = true
		if raw.Usage != nil {
			in, cached, out := raw.Usage.InputTokens, raw.Usage.CachedInputTokens, raw.Usage.OutputTokens
			cw := raw.Usage.CacheWriteInputTokens
			p.usage = &model.Usage{InputTokens: &in, OutputTokens: &out, CacheReadTokens: &cached, CacheWriteTokens: &cw}
			return []runner.Event{{Kind: "usage", Payload: map[string]any{
				"input_tokens": in, "output_tokens": out,
				"cache_read_tokens": cached, "cache_write_tokens": cw,
			}}}, nil
		}
		return nil, nil
	case "turn.failed":
		p.failed = true
		p.done = true
		if raw.Error != nil && raw.Error.Message != "" {
			p.detail = raw.Error.Message
		} else if raw.Message != "" {
			p.detail = raw.Message
		} else {
			p.detail = "codex reported a failed turn"
		}
		return []runner.Event{{Kind: "error", Payload: map[string]any{"detail": p.detail}}}, nil
	case "error":
		p.failed = true
		if raw.Error != nil && raw.Error.Message != "" {
			p.detail = raw.Error.Message
		} else if raw.Message != "" {
			p.detail = raw.Message
		} else {
			p.detail = "codex reported an error"
		}
		return []runner.Event{{Kind: "error", Payload: map[string]any{"detail": p.detail}}}, nil
	}
	return nil, nil
}

func (p *JobParser) Result() runner.Result {
	res := runner.Result{
		NativeSessionID: p.sessionID,
		Evidence:        p.lastMessage,
		Usage:           p.usage,
	}
	switch {
	case p.failed:
		res.Outcome = model.RunFailed
		res.ErrorCode = model.ErrProviderFailed
		res.ErrorDetail = p.detail
	case p.needsAction:
		res.Outcome = model.RunNeedsAction
		res.ErrorCode = model.ErrProviderNeedsAction
		res.ErrorDetail = p.detail
	case p.done:
		res.Outcome = model.RunCompleted
	default:
		return res
	}
	return res
}

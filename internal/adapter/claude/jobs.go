package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"amgo/internal/adapter"
	"amgo/internal/model"
	"amgo/internal/runner"
)

// RunSpec builds `claude -p --output-format stream-json`. The prompt is written
// to stdin. No bypass flag is added; the operator's native permission mode and
// settings apply. Review is refused because plan mode is a workflow mode, not
// an OS sandbox this adapter can guarantee.
func (a *Adapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	if req.Policy != "" && req.Policy != "default" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			fmt.Sprintf("claude jobs support only the default policy, not %q; plan mode is not an OS sandbox", req.Policy))
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return adapter.RunPlan{}, model.WrapError(model.ErrUnsupported, "claude binary is unavailable", err)
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	nativeID := ""
	if req.NativeSessionID != "" {
		args = append(args, "--resume", req.NativeSessionID)
		nativeID = req.NativeSessionID
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: path, Args: args, Dir: req.Workspace,
			Prompt:     []byte(req.Prompt),
			Provenance: "claude -p stream-json; native permission mode and local settings apply; no bypass flag",
		},
		Parser:          NewJobParser(),
		NativeSessionID: nativeID,
	}, nil
}

// JobParser consumes Claude Code stream-json events.
type JobParser struct {
	sessionID   string
	model       string
	lastText    string
	resultText  string
	usage       *model.Usage
	done        bool
	failed      bool
	partial     bool
	needsAction bool
	detail      string
}

func NewJobParser() *JobParser { return &JobParser{} }

func (p *JobParser) Line(line []byte) ([]runner.Event, error) {
	var raw struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		Model     string `json:"model"`
		Message   *struct {
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		PermissionDenials []json.RawMessage `json:"permission_denials"`
		Usage             *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		TotalCostUSD *float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	if raw.SessionID != "" && raw.SessionID != p.sessionID {
		p.sessionID = raw.SessionID
	}
	switch raw.Type {
	case "system":
		if raw.Subtype == "init" {
			if raw.Model != "" {
				p.model = raw.Model
			}
			return []runner.Event{{Kind: "native_session", Payload: map[string]any{"session_id": p.sessionID}}}, nil
		}
	case "assistant":
		if raw.Message != nil {
			if raw.Message.Model != "" {
				p.model = raw.Message.Model
			}
			text := ""
			for _, part := range raw.Message.Content {
				if part.Type == "text" {
					text += part.Text
				}
			}
			if text != "" {
				p.lastText = text
			}
		}
		return []runner.Event{{Kind: "progress", Payload: map[string]any{"event": "assistant"}}}, nil
	case "result":
		p.done = true
		p.resultText = raw.Result
		if len(raw.PermissionDenials) > 0 {
			p.needsAction = true
			p.detail = "provider denied a required tool or permission"
		} else if raw.IsError || (raw.Subtype != "" && raw.Subtype != "success") {
			if raw.Subtype == "error_max_turns" {
				p.partial = true
				p.detail = "provider reached its turn limit"
			} else {
				p.failed = true
				p.detail = firstText(raw.Result, "provider reported an error")
			}
		}
		if raw.Usage != nil {
			in, out := raw.Usage.InputTokens, raw.Usage.OutputTokens
			cr, cw := raw.Usage.CacheReadInputTokens, raw.Usage.CacheCreationInputTokens
			p.usage = &model.Usage{
				InputTokens: &in, OutputTokens: &out,
				CacheReadTokens: &cr, CacheWriteTokens: &cw,
				CostUSD: raw.TotalCostUSD, Model: p.model,
			}
		} else if raw.TotalCostUSD != nil {
			p.usage = &model.Usage{CostUSD: raw.TotalCostUSD, Model: p.model}
		}
		events := []runner.Event{{Kind: "progress", Payload: map[string]any{"event": "result", "subtype": raw.Subtype}}}
		if p.usage != nil {
			payload := map[string]any{"model": p.usage.Model}
			if p.usage.InputTokens != nil {
				payload["input_tokens"] = *p.usage.InputTokens
			}
			if p.usage.OutputTokens != nil {
				payload["output_tokens"] = *p.usage.OutputTokens
			}
			if p.usage.CacheReadTokens != nil {
				payload["cache_read_tokens"] = *p.usage.CacheReadTokens
			}
			if p.usage.CacheWriteTokens != nil {
				payload["cache_write_tokens"] = *p.usage.CacheWriteTokens
			}
			if p.usage.CostUSD != nil {
				payload["cost_usd"] = *p.usage.CostUSD
			}
			events = append(events, runner.Event{Kind: "usage", Payload: payload})
		}
		if p.failed || p.needsAction {
			events = append(events, runner.Event{Kind: "error", Payload: map[string]any{"detail": p.detail}})
		}
		return events, nil
	}
	return nil, nil
}

func (p *JobParser) Result() runner.Result {
	res := runner.Result{
		NativeSessionID: p.sessionID,
		Evidence:        firstText(p.resultText, p.lastText),
		Usage:           p.usage,
	}
	switch {
	case p.needsAction:
		res.Outcome = model.RunNeedsAction
		res.ErrorCode = model.ErrProviderNeedsAction
		res.ErrorDetail = p.detail
	case p.partial:
		res.Outcome = model.RunPartial
		res.ErrorCode = model.ErrProviderFailed
		res.ErrorDetail = p.detail
	case p.failed:
		res.Outcome = model.RunFailed
		res.ErrorCode = model.ErrProviderFailed
		res.ErrorDetail = p.detail
	case p.done:
		res.Outcome = model.RunCompleted
	default:
		return res
	}
	return res
}

func firstText(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

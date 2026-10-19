package qwen

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"amgo/internal/adapter"
	"amgo/internal/model"
	"amgo/internal/runner"
)

// RunSpec builds the documented headless stream-json command. The prompt is
// read from stdin; no --yolo or approval bypass is ever added.
func (a *Adapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	if !req.AdmitExperimental {
		return adapter.RunPlan{}, model.NewError(model.ErrPolicyRefused,
			"qwen jobs are documented-only; pass --admit-experimental after reviewing the constraints")
	}
	if req.Policy != "" && req.Policy != "default" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			fmt.Sprintf("qwen jobs support only the default policy, not %q", req.Policy))
	}
	if req.Effort != "" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			"qwen documents no reasoning-effort flag")
	}
	path, err := exec.LookPath("qwen")
	if err != nil {
		return adapter.RunPlan{}, model.WrapError(model.ErrUnsupported, "qwen binary is unavailable", err)
	}
	args := []string{"--output-format", "stream-json"}
	if req.NativeSessionID != "" {
		args = append(args, "--resume", req.NativeSessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: path, Args: args, Dir: req.Workspace,
			Prompt:     []byte(req.Prompt),
			Provenance: "qwen headless stream-json; native approval mode; no --yolo; no bypass flag",
		},
		Parser:          NewJobParser(),
		NativeSessionID: req.NativeSessionID,
	}, nil
}

// JobParser consumes Qwen Code stream-json events.
type JobParser struct {
	sessionID  string
	model      string
	lastText   string
	resultText string
	usage      *model.Usage
	done       bool
	failed     bool
	detail     string
}

func NewJobParser() *JobParser { return &JobParser{} }

func (p *JobParser) Line(line []byte) ([]runner.Event, error) {
	var raw struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		Model     string `json:"model"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		Message   *struct {
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			TotalTokens              int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	if raw.SessionID != "" {
		p.sessionID = raw.SessionID
	}
	if raw.Model != "" {
		p.model = raw.Model
	}
	switch raw.Type {
	case "system":
		if raw.Subtype == "session_start" {
			return []runner.Event{{Kind: "native_session", Payload: map[string]any{
				"session_id": p.sessionID, "model": p.model,
			}}}, nil
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
		if raw.IsError || (raw.Subtype != "" && raw.Subtype != "success") {
			p.failed = true
			p.detail = "qwen reported " + raw.Subtype
			if raw.Result != "" {
				p.detail = raw.Result
			}
		}
		if raw.Usage != nil {
			in, out := raw.Usage.InputTokens, raw.Usage.OutputTokens
			cr, cw := raw.Usage.CacheReadInputTokens, raw.Usage.CacheCreationInputTokens
			p.usage = &model.Usage{
				InputTokens: &in, OutputTokens: &out,
				CacheReadTokens: &cr, CacheWriteTokens: &cw, Model: p.model,
			}
			return []runner.Event{
				{Kind: "progress", Payload: map[string]any{"event": "result", "subtype": raw.Subtype}},
				{Kind: "usage", Payload: map[string]any{
					"input_tokens": in, "output_tokens": out,
					"cache_read_tokens": cr, "cache_write_tokens": cw, "model": p.model,
				}},
			}, nil
		}
		return []runner.Event{{Kind: "progress", Payload: map[string]any{"event": "result", "subtype": raw.Subtype}}}, nil
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

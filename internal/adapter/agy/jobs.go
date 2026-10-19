package agy

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
// sent as one NDJSON user event on stdin; agy ignores a -p prompt in this mode.
// No permission bypass flag is added.
func (a *Adapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	if !req.AdmitExperimental {
		return adapter.RunPlan{}, model.NewError(model.ErrPolicyRefused,
			"agy jobs are documented-only; pass --admit-experimental after reviewing the constraints")
	}
	if req.Policy != "" && req.Policy != "default" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			fmt.Sprintf("agy jobs support only the default policy, not %q", req.Policy))
	}
	path, err := exec.LookPath("agy")
	if err != nil {
		return adapter.RunPlan{}, model.WrapError(model.ErrUnsupported, "agy binary is unavailable", err)
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json"}
	if req.NativeSessionID != "" {
		args = append(args, "--conversation", req.NativeSessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	prompt, err := json.Marshal(map[string]any{
		"event":   "user",
		"message": map[string]any{"content": req.Prompt},
	})
	if err != nil {
		return adapter.RunPlan{}, err
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: path, Args: args, Dir: req.Workspace,
			Prompt:     append(prompt, '\n'),
			Provenance: "agy headless stream-json; native approval mode; no bypass flag",
		},
		Parser:          NewJobParser(),
		NativeSessionID: req.NativeSessionID,
	}, nil
}

// JobParser consumes agy stream-json events.
type JobParser struct {
	sessionID   string
	model       string
	response    string
	usage       *model.Usage
	done        bool
	failed      bool
	needsAction bool
	detail      string
}

func NewJobParser() *JobParser { return &JobParser{} }

func (p *JobParser) Line(line []byte) ([]runner.Event, error) {
	var raw struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		Init           *struct {
			Model string `json:"model"`
		} `json:"init"`
		StepUpdate *struct {
			State     string `json:"state"`
			StepType  string `json:"step_type"`
			TextDelta string `json:"text_delta"`
		} `json:"step_update"`
		Result *struct {
			ConversationID string `json:"conversation_id"`
			Status         string `json:"status"`
			Response       string `json:"response"`
			Error          string `json:"error"`
			Usage          *struct {
				InputTokens     int64 `json:"input_tokens"`
				OutputTokens    int64 `json:"output_tokens"`
				ThinkingTokens  int64 `json:"thinking_tokens"`
				CacheReadTokens int64 `json:"cache_read_tokens"`
				TotalTokens     int64 `json:"total_tokens"`
			} `json:"usage"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	switch raw.Event {
	case "init":
		p.sessionID = raw.ConversationID
		if raw.Init != nil && raw.Init.Model != "" {
			p.model = raw.Init.Model
		}
		return []runner.Event{{Kind: "native_session", Payload: map[string]any{
			"session_id": p.sessionID, "model": p.model,
		}}}, nil
	case "step_update":
		payload := map[string]any{"event": "step_update"}
		if raw.StepUpdate != nil {
			payload["state"] = raw.StepUpdate.State
			payload["step_type"] = raw.StepUpdate.StepType
		}
		return []runner.Event{{Kind: "progress", Payload: payload}}, nil
	case "result":
		if raw.Result == nil {
			return nil, fmt.Errorf("agy result event has no result payload")
		}
		if raw.Result.ConversationID != "" {
			p.sessionID = raw.Result.ConversationID
		}
		p.done = true
		p.response = raw.Result.Response
		if raw.Result.Usage != nil {
			in, out := raw.Result.Usage.InputTokens, raw.Result.Usage.OutputTokens
			cr := raw.Result.Usage.CacheReadTokens
			p.usage = &model.Usage{InputTokens: &in, OutputTokens: &out, CacheReadTokens: &cr, Model: p.model}
		}
		switch raw.Result.Status {
		case "SUCCESS":
		case "WAITING":
			p.needsAction = true
			p.detail = firstText(raw.Result.Error, "agy is waiting for an action")
		case "CANCELED", "INTERRUPTED":
			p.failed = true
			p.detail = firstText(raw.Result.Error, "agy reported "+raw.Result.Status)
		case "":
			p.failed = true
			p.detail = "agy result has no status"
		default:
			p.failed = true
			p.detail = firstText(raw.Result.Error, "agy reported "+raw.Result.Status)
		}
		events := []runner.Event{{Kind: "progress", Payload: map[string]any{"event": "result", "status": raw.Result.Status}}}
		if p.usage != nil {
			events = append(events, runner.Event{Kind: "usage", Payload: map[string]any{
				"input_tokens": *p.usage.InputTokens, "output_tokens": *p.usage.OutputTokens,
				"cache_read_tokens": *p.usage.CacheReadTokens, "model": p.model,
			}})
		}
		return events, nil
	}
	return nil, nil
}

func (p *JobParser) Result() runner.Result {
	res := runner.Result{NativeSessionID: p.sessionID, Evidence: p.response, Usage: p.usage}
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

func firstText(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

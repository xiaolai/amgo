// Package opencode implements bounded, non-interactive OpenCode jobs. Live
// session attachment is not part of this adapter yet.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"amgo/internal/adapter"
	"amgo/internal/model"
	"amgo/internal/runner"
)

const (
	TestedVersion = "1.19.3"
	MinVersion    = "1.19.3"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return model.AdapterOpenCode }

func (a *Adapter) Probe(ctx context.Context) (model.Capabilities, error) {
	caps := model.Capabilities{
		Adapter:         model.AdapterOpenCode,
		Maturity:        model.MaturityFixtureTested,
		MinKnownVersion: MinVersion,
		TestedVersions:  []string{TestedVersion},
		Jobs:            false,
		Constraints: []string{
			"jobs use a locally owned opencode run process; external servers are never attached or disposed",
			"prompt is read from stdin, not argv",
			"no auto-approve flag is added",
		},
	}
	out, err := exec.CommandContext(ctx, "opencode", "--version").Output()
	if err != nil {
		caps.Constraints = append(caps.Constraints, "opencode binary unavailable")
		return caps, nil
	}
	v := strings.TrimSpace(string(out))
	caps.Jobs = v != ""
	caps.Resume = caps.Jobs
	if caps.Jobs {
		caps.Constraints = append(caps.Constraints, "jobs native-verified at 1.19.3 on macOS arm64")
	} else {
		caps.Constraints = append(caps.Constraints, "opencode version could not be read")
	}
	return caps, nil
}

func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}

func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	return adapter.SendResult{}, model.NewError(model.ErrUnsupported,
		"OpenCode existing-session messaging is not implemented; only owned jobs are supported")
}

// RunSpec builds `opencode run --format json` with the prompt on stdin.
func (a *Adapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	if req.Policy != "" && req.Policy != "default" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			fmt.Sprintf("opencode jobs support only the default policy, not %q", req.Policy))
	}
	path, err := exec.LookPath("opencode")
	if err != nil {
		return adapter.RunPlan{}, model.WrapError(model.ErrUnsupported, "opencode binary is unavailable", err)
	}
	args := []string{"run", "--format", "json", "--dir", req.Workspace}
	if req.NativeSessionID != "" {
		args = append(args, "--session", req.NativeSessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--variant", req.Effort)
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: path, Args: args, Dir: req.Workspace,
			Prompt:     []byte(req.Prompt),
			Provenance: "opencode run --format json; native permissions; no auto-approve; no fork",
		},
		Parser:          NewJobParser(),
		NativeSessionID: req.NativeSessionID,
	}, nil
}

// JobParser consumes opencode run --format json events.
type JobParser struct {
	sessionID   string
	text        strings.Builder
	in, out     int64
	read, write int64
	cost        float64
	usageSeen   bool
	done        bool
	failed      bool
	needsAction bool
	detail      string
}

func NewJobParser() *JobParser { return &JobParser{} }

func (p *JobParser) Line(line []byte) ([]runner.Event, error) {
	var raw struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionID"`
		Message   string `json:"message"`
		Error     *struct {
			Message string `json:"message"`
		} `json:"error"`
		Part *struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Reason string `json:"reason"`
			Tokens *struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
			Cost float64 `json:"cost"`
		} `json:"part"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	if raw.SessionID != "" && raw.SessionID != p.sessionID {
		p.sessionID = raw.SessionID
	}
	switch raw.Type {
	case "step_start":
		return []runner.Event{{Kind: "native_session", Payload: map[string]any{"session_id": p.sessionID}}}, nil
	case "text":
		if raw.Part != nil && raw.Part.Text != "" {
			if p.text.Len() > 0 {
				p.text.WriteString("\n")
			}
			p.text.WriteString(raw.Part.Text)
		}
		return nil, nil
	case "step_finish":
		if raw.Part == nil {
			return nil, nil
		}
		if raw.Part.Tokens != nil {
			p.usageSeen = true
			p.in += raw.Part.Tokens.Input
			p.out += raw.Part.Tokens.Output
			p.read += raw.Part.Tokens.Cache.Read
			p.write += raw.Part.Tokens.Cache.Write
			p.cost += raw.Part.Cost
		}
		if raw.Part.Reason == "stop" {
			p.done = true
		}
		return []runner.Event{{Kind: "progress", Payload: map[string]any{
			"event": "step_finish", "reason": raw.Part.Reason,
		}}}, nil
	case "permission", "permission_denied":
		p.needsAction = true
		p.detail = "opencode requested or was denied a permission"
		return []runner.Event{{Kind: "error", Payload: map[string]any{"detail": p.detail}}}, nil
	case "error":
		p.failed = true
		switch {
		case raw.Error != nil && raw.Error.Message != "":
			p.detail = raw.Error.Message
		case raw.Message != "":
			p.detail = raw.Message
		default:
			p.detail = "opencode reported an error"
		}
		return []runner.Event{{Kind: "error", Payload: map[string]any{"detail": p.detail}}}, nil
	}
	return nil, nil
}

func (p *JobParser) Result() runner.Result {
	res := runner.Result{NativeSessionID: p.sessionID, Evidence: p.text.String()}
	if p.usageSeen {
		in, out, read, write, cost := p.in, p.out, p.read, p.write, p.cost
		res.Usage = &model.Usage{
			InputTokens: &in, OutputTokens: &out,
			CacheReadTokens: &read, CacheWriteTokens: &write, CostUSD: &cost,
		}
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

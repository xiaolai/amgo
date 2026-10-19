// Package acp implements a documented-only Agent Client Protocol v1 client.
// The agent command is explicit operator input; no ACP agent is installed or
// certified here. The adapter speaks JSON-RPC over newline-delimited stdio and
// rejects every permission request rather than approving one.
package acp

import (
	"context"
	"fmt"
	"os/exec"

	"amgo/internal/adapter"
	"amgo/internal/model"
	"amgo/internal/runner"
	"amgo/internal/version"
)

const ProtocolVersion = 1

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return model.AdapterACP }

func (a *Adapter) Probe(ctx context.Context) (model.Capabilities, error) {
	return model.Capabilities{
		Adapter:  model.AdapterACP,
		Maturity: model.MaturityDocumented,
		Jobs:     true,
		Resume:   true,
		Constraints: []string{
			"ACP v1 over JSON-RPC/NDJSON stdio; no ACP agent is installed or native-certified",
			"the agent command is explicit operator input; there is no discovery or config-file selection",
			"client filesystem and terminal capabilities are disabled",
			"every permission request is rejected; a rejected request makes the job needs_action",
			"review policy is not offered: ACP has no enforceable amgo sandbox",
			"requires --admit-experimental",
		},
	}, nil
}

func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}

func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	return adapter.SendResult{}, model.NewError(model.ErrUnsupported,
		"ACP is a job protocol; use run with an explicit agent command")
}

// RunSpec launches the operator-supplied ACP agent and drives one prompt.
func (a *Adapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	if !req.AdmitExperimental {
		return adapter.RunPlan{}, model.NewError(model.ErrPolicyRefused,
			"ACP is documented-only; pass --admit-experimental after reviewing the constraints")
	}
	if req.Policy != "" && req.Policy != "default" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			fmt.Sprintf("ACP jobs support only the default policy, not %q", req.Policy))
	}
	if req.ACPCommand == "" {
		return adapter.RunPlan{}, model.NewError(model.ErrUsage,
			"ACP requires --acp-command <agent-executable> (and optional --acp-arg)")
	}
	if req.Effort != "" {
		return adapter.RunPlan{}, model.NewError(model.ErrUnsupported,
			"ACP does not define a reasoning-effort field; pass it to the agent command instead")
	}
	path := req.ACPCommand
	if !hasPathSeparator(path) {
		found, err := exec.LookPath(path)
		if err != nil {
			return adapter.RunPlan{}, model.WrapError(model.ErrUnsupported, "ACP agent executable is unavailable", err)
		}
		path = found
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: path, Args: append([]string(nil), req.ACPArgs...), Dir: req.Workspace,
			Provenance: "acp v1 stdio; client fs/terminal disabled; every permission request rejected",
			Driver: &Driver{
				Workspace: req.Workspace, Prompt: req.Prompt,
				SessionID: req.NativeSessionID, Version: version.Version,
			},
		},
		NativeSessionID: req.NativeSessionID,
	}, nil
}

func hasPathSeparator(s string) bool {
	for _, r := range s {
		if r == '/' || r == '\\' {
			return true
		}
	}
	return false
}

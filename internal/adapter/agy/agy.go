// Package agy reports Antigravity capability. The installed 1.4.2 structured
// stream-json surface exists, but native job evidence requires an eligible
// account; until that is verified the job lane is refused rather than guessed.
package agy

import (
	"context"
	"os/exec"
	"strings"

	"amgo/internal/adapter"
	"amgo/internal/model"
)

const TestedVersion = "1.4.2"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return model.AdapterAgy }

func (a *Adapter) Probe(ctx context.Context) (model.Capabilities, error) {
	caps := model.Capabilities{
		Adapter:         model.AdapterAgy,
		Maturity:        model.MaturityDocumented,
		MinKnownVersion: TestedVersion,
		TestedVersions:  []string{TestedVersion},
		Constraints: []string{
			"structured stream-json exists in 1.4.2, but the current account is not eligible in this location",
			"the job lane is refused until a native structured result is verified",
			"legacy text-only output is diagnostic and is not parsed as completion",
			"headless stream-json and --conversation are implemented from the official CLI docs",
			"the job lane is documented-only and requires --admit-experimental",
		},
	}
	out, err := exec.CommandContext(ctx, "agy", "--version").Output()
	if err != nil {
		caps.Constraints = append(caps.Constraints, "agy binary unavailable")
		return caps, nil
	}
	if v := strings.TrimSpace(string(out)); v != "" {
		caps.TestedVersions = []string{v}
	}
	caps.Jobs = true
	caps.Resume = true
	return caps, nil
}

func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}

func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	return adapter.SendResult{}, model.NewError(model.ErrUnsupported, "agy live messaging is not implemented")
}

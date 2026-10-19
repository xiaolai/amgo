// Package qwen implements the documented Qwen Code cross-session protocol and
// headless jobs. The protocol code is fixture-tested; no native gate is
// possible without the qwen binary, so mutation paths require the operator's
// explicit experimental admission.
package qwen

import (
	"context"
	"os/exec"

	"amgo/internal/adapter"
	"amgo/internal/model"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return model.AdapterQwen }

func (a *Adapter) Probe(ctx context.Context) (model.Capabilities, error) {
	caps := model.Capabilities{
		Adapter:  model.AdapterQwen,
		Maturity: model.MaturityDocumented,
		Constraints: []string{
			"cross-session schema 1 / msgV 1 is implemented from the official protocol; no native gate exists",
			"the sender presents only the target registry ipcToken; it never claims own-process or controller authority",
			"fromMode is omitted, so a policy-matching receiver holds the message for review by default",
			"receipts are best-effort and observed only for a short bounded window",
			"outside Linux the registry leaves procStart/pidNs null, so PID reuse is not distinguishable",
			"mutation paths require --admit-experimental",
		},
	}
	caps.LiveMessaging = true
	if _, err := exec.LookPath("qwen"); err != nil {
		caps.Constraints = append(caps.Constraints,
			"qwen binary is unavailable; messaging is documented and fixture-tested, jobs are disabled")
		return caps, nil
	}
	caps.Jobs = true
	caps.Resume = true
	caps.Constraints = append(caps.Constraints, "qwen binary found; version and native behavior are not yet certified")
	return caps, nil
}

func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	return candidates(req.QwenHome), nil
}

func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	return send(ctx, req)
}

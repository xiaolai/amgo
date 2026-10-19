// Package qwen reports Qwen capability. The binary is absent locally, so the
// adapter stays unavailable: documentation alone never enables a live lane.
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
			"qwen binary is unavailable; install and certify a pinned version before enabling jobs or peer messaging",
			"documented flags and protocol schemas are not native evidence",
		},
	}
	if _, err := exec.LookPath("qwen"); err != nil {
		return caps, nil
	}
	caps.Constraints = []string{"qwen binary found but no pinned fixture or native result gate exists; the lane stays disabled"}
	return caps, nil
}

func (a *Adapter) Discover(ctx context.Context, req adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}

func (a *Adapter) Send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	return adapter.SendResult{}, model.NewError(model.ErrUnsupported, "qwen peer messaging is not implemented")
}

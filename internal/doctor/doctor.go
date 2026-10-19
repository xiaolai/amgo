// Package doctor reports installed, supported and ready capabilities without
// inferring success from a binary's presence.
package doctor

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/message"
	"amgo/internal/model"
	"amgo/internal/store"
)

type StateReport struct {
	Initialized bool              `json:"initialized"`
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Violations  []store.Violation `json:"violations,omitempty"`
}

type Report struct {
	Version  string               `json:"version"`
	Go       string               `json:"go"`
	Host     string               `json:"host"`
	Home     string               `json:"home"`
	State    StateReport          `json:"state"`
	Adapters []model.Capabilities `json:"adapters"`
}

func Run(ctx context.Context, home, version string, adapters map[string]adapter.Adapter) (Report, error) {
	r := Report{
		Version: version,
		Go:      runtime.Version(),
		Host:    config.HostID(),
		Home:    home,
	}
	r.State = checkState(home)
	r.Adapters = AdapterCapabilities(ctx, adapters)
	return r, nil
}

func AdapterCapabilities(ctx context.Context, adapters map[string]adapter.Adapter) []model.Capabilities {
	var out []model.Capabilities
	for _, name := range []string{
		model.AdapterClaude, model.AdapterCodex, model.AdapterOpenCode,
		model.AdapterAgy, model.AdapterQwen, model.AdapterACP,
	} {
		if a, ok := adapters[name]; ok {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			caps, err := a.Probe(pctx)
			cancel()
			if err != nil {
				caps.LiveMessaging = false
				caps.Constraints = append(caps.Constraints, err.Error())
			}
			if caps.Adapter == "" {
				caps.Adapter = name
			}
			out = append(out, caps)
			continue
		}
		out = append(out, unavailable(name))
	}
	return out
}

func checkState(home string) StateReport {
	if _, err := os.Stat(config.DBPath(home)); err != nil {
		if os.IsNotExist(err) {
			return StateReport{Initialized: false, OK: true}
		}
		return StateReport{Initialized: true, OK: false, Error: err.Error()}
	}
	st, err := store.OpenReadOnly(config.DBPath(home), 2*time.Second)
	if err != nil {
		return StateReport{Initialized: true, OK: false, Error: err.Error()}
	}
	defer st.Close()
	v, err := st.CheckInvariants(time.Now().UTC())
	if err != nil {
		return StateReport{Initialized: true, OK: false, Error: err.Error()}
	}
	return StateReport{Initialized: true, OK: len(v) == 0, Violations: v}
}

func unavailable(name string) model.Capabilities {
	caps := model.Capabilities{Adapter: name, Maturity: model.MaturityDocumented}
	switch name {
	case model.AdapterOpenCode:
		if _, err := exec.LookPath("opencode"); err == nil {
			caps.Constraints = []string{"v0.1 has no OpenCode adapter; live messaging is M3"}
		} else {
			caps.Constraints = []string{"opencode not found"}
		}
	case model.AdapterAgy:
		if _, err := exec.LookPath("agy"); err == nil {
			caps.Constraints = []string{"v0.1 has no agy adapter; structured jobs are M2"}
		} else {
			caps.Constraints = []string{"agy not found"}
		}
	case model.AdapterQwen:
		if _, err := exec.LookPath("qwen"); err != nil {
			caps.Constraints = []string{"qwen not found; do not auto-install"}
		} else {
			caps.Constraints = []string{"qwen present but uncertified; live messaging is M3"}
		}
	}
	return caps
}

// ProbeSession sends a nonce and waits for an acknowledgement or reply whose
// provenance is the bound recipient. An operator ack is deliberately not
// enough. It costs one model turn and is never run by default.
func ProbeSession(ctx context.Context, svc *message.Service, alias string, timeout time.Duration) (message.WaitResult, error) {
	nonce := "probe-" + time.Now().UTC().Format("20060102T150405.000000000")
	out, err := svc.Send(ctx, message.SendInput{
		RecipientAlias:        alias,
		Body:                  "amgo delivery probe " + nonce + "\nAcknowledge this message with `amgo ack <message-id> --as " + alias + "` or reply to it.",
		ExpectReply:           true,
		AcceptUnknownDelivery: true,
	})
	if err != nil {
		return message.WaitResult{}, err
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		rec, err := svc.Read(out.Message.ID)
		if err != nil {
			return message.WaitResult{}, err
		}
		for _, r := range rec.Receipts {
			if r.Provenance == model.ProvenanceRecipient && (r.Kind == "ack" || r.Kind == "reply") {
				return message.WaitResult{Message: rec.Message, Receipts: rec.Receipts}, nil
			}
		}
		select {
		case <-ctx.Done():
			return message.WaitResult{Message: rec.Message, Receipts: rec.Receipts}, model.WrapError(model.ErrInterrupted, "probe interrupted", ctx.Err())
		case <-deadline.C:
			return message.WaitResult{Message: rec.Message, Receipts: rec.Receipts}, &model.CodedError{
				Code:    model.ErrDeadlineExceeded,
				Message: "probe timed out without a recipient acknowledgement",
				Details: map[string]any{"message_id": out.Message.ID, "state": string(rec.Message.State)},
			}
		case <-tick.C:
		}
	}
}

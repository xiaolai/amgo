package contract

import (
	"bytes"
	"encoding/json"
	"testing"

	"amgo/internal/model"
)

func TestExitCodeMapping(t *testing.T) {
	cases := map[string]int{
		model.ErrUsage:             2,
		model.ErrUnsupported:       3,
		model.ErrAmbiguousTarget:   4,
		model.ErrBudgetExhausted:   4,
		model.ErrTargetUnavailable: 5,
		model.ErrNativeHold:        6,
		model.ErrPolicyRefused:     6,
		model.ErrUnknownSubmission: 7,
		model.ErrDeadlineExceeded:  9,
		model.ErrStoreBusy:         10,
		model.ErrStateIncompatible: 11,
		model.ErrInterrupted:       130,
	}
	for code, want := range cases {
		if got := ExitCode(model.NewError(code, "x")); got != want {
			t.Errorf("%s: got %d want %d", code, got, want)
		}
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil should be 0")
	}
}

func TestEnvelopeJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, OK("send", map[string]any{"message_id": "m_1"})); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["schema_version"] != float64(1) || got["operation"] != "send" || got["outcome"] != "ok" {
		t.Fatalf("envelope=%v", got)
	}
}

func TestFailEnvelopeKeepsCode(t *testing.T) {
	e := Fail("send", model.NewError(model.ErrIdemConflict, "conflict"), nil)
	if e.Outcome != "error" || e.Error == nil || e.Error.Code != model.ErrIdemConflict {
		t.Fatalf("envelope=%+v", e)
	}
}

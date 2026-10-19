package opencode

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"amgo/internal/adapter"
	"amgo/internal/model"
)

func feed(t *testing.T, p *JobParser, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if _, err := p.Line([]byte(line)); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
	}
}

func TestJobParserSuccess(t *testing.T) {
	p := NewJobParser()
	feed(t, p,
		`{"type":"step_start","sessionID":"ses_1","part":{"type":"step-start"}}`,
		`{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"ORCHID"}}`,
		`{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","tokens":{"total":17323,"input":17273,"output":50,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`,
	)
	res := p.Result()
	if res.Outcome != model.RunCompleted || res.NativeSessionID != "ses_1" || res.Evidence != "ORCHID" {
		t.Fatalf("result=%+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens == nil || *res.Usage.InputTokens != 17273 ||
		res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 50 {
		t.Fatalf("usage=%+v", res.Usage)
	}
}

func TestJobParserSumsStepsAndIgnoresNonStopFinish(t *testing.T) {
	p := NewJobParser()
	feed(t, p,
		`{"type":"step_start","sessionID":"ses_2"}`,
		`{"type":"step_finish","sessionID":"ses_2","part":{"reason":"tool-calls","tokens":{"input":10,"output":2},"cost":0.1}}`,
		`{"type":"step_start","sessionID":"ses_2"}`,
		`{"type":"text","sessionID":"ses_2","part":{"type":"text","text":"done"}}`,
		`{"type":"step_finish","sessionID":"ses_2","part":{"reason":"stop","tokens":{"input":5,"output":3},"cost":0.2}}`,
	)
	res := p.Result()
	if res.Outcome != model.RunCompleted {
		t.Fatalf("result=%+v", res)
	}
	if *res.Usage.InputTokens != 15 || *res.Usage.OutputTokens != 5 {
		t.Fatalf("usage=%+v", res.Usage)
	}
	if res.Usage.CostUSD == nil || *res.Usage.CostUSD < 0.299 || *res.Usage.CostUSD > 0.301 {
		t.Fatalf("cost=%v", res.Usage.CostUSD)
	}
}

func TestJobParserErrorAndPermission(t *testing.T) {
	p := NewJobParser()
	feed(t, p, `{"type":"error","sessionID":"s","error":{"message":"boom"}}`)
	if res := p.Result(); res.Outcome != model.RunFailed || res.ErrorDetail != "boom" {
		t.Fatalf("result=%+v", res)
	}
	p = NewJobParser()
	feed(t, p, `{"type":"permission","sessionID":"s"}`)
	if res := p.Result(); res.Outcome != model.RunNeedsAction {
		t.Fatalf("result=%+v", res)
	}
}

func TestRunSpecPolicyGuardAndArgs(t *testing.T) {
	a := New()
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Policy: "review"}); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("review err=%v", err)
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary is unavailable")
	}
	plan, err := a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "hello", NativeSessionID: "ses_1", Model: "p/m", Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Spec.Args, " ")
	if !strings.Contains(joined, "run --format json --dir") ||
		!strings.Contains(joined, "--session ses_1") ||
		!strings.Contains(joined, "--model p/m") ||
		!strings.Contains(joined, "--variant high") {
		t.Fatalf("args=%v", plan.Spec.Args)
	}
	for _, bad := range []string{"--auto", "--fork", "--continue"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("unexpected flag %s: %v", bad, plan.Spec.Args)
		}
	}
	if len(plan.Spec.Prompt) != 5 {
		t.Fatalf("prompt is not on stdin: %q", plan.Spec.Prompt)
	}
}

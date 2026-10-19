package codex

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
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ORCHID"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":14461,"cached_input_tokens":12288,"cache_write_input_tokens":0,"output_tokens":7,"reasoning_output_tokens":0}}`,
	)
	res := p.Result()
	if res.Outcome != model.RunCompleted || res.NativeSessionID != "t1" || res.Evidence != "ORCHID" {
		t.Fatalf("result=%+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens == nil || *res.Usage.InputTokens != 14461 ||
		res.Usage.CacheReadTokens == nil || *res.Usage.CacheReadTokens != 12288 ||
		res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 7 {
		t.Fatalf("usage=%+v", res.Usage)
	}
	if res.Usage.CostUSD != nil {
		t.Fatalf("codex cost must stay unknown: %+v", res.Usage)
	}
}

func TestJobParserFailedTurn(t *testing.T) {
	p := NewJobParser()
	feed(t, p, `{"type":"turn.failed","error":{"message":"boom"}}`)
	res := p.Result()
	if res.Outcome != model.RunFailed || res.ErrorCode != model.ErrProviderFailed || res.ErrorDetail != "boom" {
		t.Fatalf("result=%+v", res)
	}
}

func TestJobParserDeniedCommandNeedsAction(t *testing.T) {
	p := NewJobParser()
	feed(t, p,
		`{"type":"item.completed","item":{"type":"command_execution","status":"declined"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	)
	res := p.Result()
	if res.Outcome != model.RunNeedsAction || res.ErrorCode != model.ErrProviderNeedsAction {
		t.Fatalf("result=%+v", res)
	}
}

func TestJobParserRejectsMalformedEvent(t *testing.T) {
	p := NewJobParser()
	if _, err := p.Line([]byte("not json")); err == nil {
		t.Fatal("malformed event accepted")
	}
}

func TestJobParserIgnoresUnknownEvents(t *testing.T) {
	p := NewJobParser()
	feed(t, p, `{"type":"future.event","payload":{"x":1}}`)
	if res := p.Result(); res.Outcome != "" {
		t.Fatalf("unknown event produced a result: %+v", res)
	}
}

func TestRunSpecPolicyAndResumeGuards(t *testing.T) {
	a := New()
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Policy: "bogus"}); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("unknown policy err=%v", err)
	}
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Policy: "review", NativeSessionID: "t1"}); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("review resume err=%v", err)
	}
}

func TestRunSpecArgs(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary is unavailable")
	}
	a := New()
	plan, err := a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "hello", Policy: "default", Model: "m", Effort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Spec.Args, " ")
	if !strings.Contains(joined, "exec --json") || !strings.Contains(joined, "-s workspace-write") ||
		!strings.Contains(joined, "-C ") || !strings.Contains(joined, `model_reasoning_effort="high"`) {
		t.Fatalf("args=%v", plan.Spec.Args)
	}
	if plan.Spec.Args[len(plan.Spec.Args)-1] != "-" || len(plan.Spec.Prompt) != 5 {
		t.Fatalf("prompt is not on stdin: args=%v", plan.Spec.Args)
	}
	if strings.Contains(joined, "dangerously") {
		t.Fatalf("bypass flag present: %v", plan.Spec.Args)
	}

	plan, err = a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "hello", Policy: "review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Spec.Args, " "), "-s read-only") {
		t.Fatalf("review args=%v", plan.Spec.Args)
	}

	plan, err = a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "next", NativeSessionID: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Spec.Args[0] != "exec" || plan.Spec.Args[1] != "resume" ||
		plan.Spec.Args[len(plan.Spec.Args)-2] != "t1" || plan.Spec.Args[len(plan.Spec.Args)-1] != "-" {
		t.Fatalf("resume args=%v", plan.Spec.Args)
	}
	if strings.Contains(strings.Join(plan.Spec.Args, " "), "--last") {
		t.Fatalf("resume must not guess the newest session: %v", plan.Spec.Args)
	}
}

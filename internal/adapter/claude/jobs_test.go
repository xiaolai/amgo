package claude

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

const claudeSuccess = `{"type":"result","subtype":"success","is_error":false,"duration_api_ms":1801,"session_id":"5ad244a2-bdbe-4efe-83a1-8b0f3725e06b","total_cost_usd":0.0908,"usage":{"input_tokens":18040,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":24},"modelUsage":{"deepseek-flash[1m]":{"costUSD":0.0908}},"permission_denials":[],"result":"ORCHID","type":"result"}`

func TestClaudeJobParserSuccess(t *testing.T) {
	p := NewJobParser()
	feed(t, p,
		`{"type":"system","subtype":"init","session_id":"5ad244a2-bdbe-4efe-83a1-8b0f3725e06b","model":"deepseek-flash[1m]"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":1,"session_id":"5ad244a2-bdbe-4efe-83a1-8b0f3725e06b"}`,
		`{"type":"assistant","message":{"model":"deepseek-flash","content":[{"type":"thinking","thinking":"consider"},{"type":"text","text":"ORCHID"}]},"session_id":"5ad244a2-bdbe-4efe-83a1-8b0f3725e06b"}`,
		claudeSuccess,
	)
	res := p.Result()
	if res.Outcome != model.RunCompleted || res.NativeSessionID != "5ad244a2-bdbe-4efe-83a1-8b0f3725e06b" || res.Evidence != "ORCHID" {
		t.Fatalf("result=%+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens == nil || *res.Usage.InputTokens != 18040 ||
		res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 24 ||
		res.Usage.CostUSD == nil || *res.Usage.CostUSD != 0.0908 {
		t.Fatalf("usage=%+v", res.Usage)
	}
}

func TestClaudeJobParserPermissionDenial(t *testing.T) {
	p := NewJobParser()
	feed(t, p, `{"type":"result","subtype":"success","is_error":false,"session_id":"s","permission_denials":[{"tool":"Bash"}],"result":"blocked"}`)
	res := p.Result()
	if res.Outcome != model.RunNeedsAction || res.ErrorCode != model.ErrProviderNeedsAction {
		t.Fatalf("result=%+v", res)
	}
}

func TestClaudeJobParserError(t *testing.T) {
	p := NewJobParser()
	feed(t, p, `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s","result":"boom"}`)
	res := p.Result()
	if res.Outcome != model.RunFailed || res.ErrorDetail != "boom" {
		t.Fatalf("result=%+v", res)
	}
}

func TestClaudeJobParserMaxTurnsIsPartial(t *testing.T) {
	p := NewJobParser()
	feed(t, p, `{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s","result":""}`)
	res := p.Result()
	if res.Outcome != model.RunPartial {
		t.Fatalf("result=%+v", res)
	}
}

func TestClaudeJobParserRejectsMalformed(t *testing.T) {
	p := NewJobParser()
	if _, err := p.Line([]byte("{")); err == nil {
		t.Fatal("malformed event accepted")
	}
}

func TestClaudeRunSpec(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary is unavailable")
	}
	a := New()
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Policy: "review"}); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("review err=%v", err)
	}
	plan, err := a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "hello", Policy: "default", Model: "m", Effort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Spec.Args, " ")
	if !strings.Contains(joined, "-p --output-format stream-json --verbose") ||
		!strings.Contains(joined, "--effort low") {
		t.Fatalf("args=%v", plan.Spec.Args)
	}
	if strings.Contains(joined, "--session-id") || plan.NativeSessionID != "" {
		t.Fatalf("new run must let the provider assign its session: args=%v", plan.Spec.Args)
	}
	if strings.Contains(joined, "dangerously") || strings.Contains(joined, "permission-mode") {
		t.Fatalf("unexpected policy flag: %v", plan.Spec.Args)
	}
	if len(plan.Spec.Prompt) != 5 {
		t.Fatalf("prompt is not on stdin: %q", plan.Spec.Prompt)
	}

	plan, err = a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "next", NativeSessionID: "native-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(plan.Spec.Args, " ")
	if !strings.Contains(joined, "--resume native-1") || strings.Contains(joined, "--session-id") {
		t.Fatalf("resume args=%v", plan.Spec.Args)
	}
}

package qwen

import (
	"context"
	"os"
	"path/filepath"
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
		`{"type":"system","subtype":"session_start","session_id":"q1","model":"qwen3-coder"}`,
		`{"type":"assistant","session_id":"q1","message":{"model":"qwen3-coder","content":[{"type":"text","text":"ORCHID"}]}}`,
		`{"type":"result","subtype":"success","session_id":"q1","is_error":false,"result":"ORCHID","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":1}}`,
	)
	res := p.Result()
	if res.Outcome != model.RunCompleted || res.NativeSessionID != "q1" || res.Evidence != "ORCHID" {
		t.Fatalf("result=%+v", res)
	}
	if res.Usage == nil || *res.Usage.InputTokens != 10 || *res.Usage.OutputTokens != 2 || *res.Usage.CacheReadTokens != 3 {
		t.Fatalf("usage=%+v", res.Usage)
	}
}

func TestJobParserError(t *testing.T) {
	p := NewJobParser()
	feed(t, p, `{"type":"result","subtype":"error_during_execution","session_id":"q1","is_error":true,"result":"boom"}`)
	res := p.Result()
	if res.Outcome != model.RunFailed || res.ErrorDetail != "boom" {
		t.Fatalf("result=%+v", res)
	}
}

func fakeQwen(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "qwen")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunSpecDocumentedArgs(t *testing.T) {
	fakeQwen(t)
	a := New()
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Prompt: "hello"}); model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("admission err=%v", err)
	}
	plan, err := a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "secret prompt", Model: "qwen3-coder", AdmitExperimental: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Spec.Args, " ")
	if !strings.Contains(joined, "--output-format stream-json") {
		t.Fatalf("args=%v", plan.Spec.Args)
	}
	if strings.Contains(joined, "secret prompt") || strings.Contains(joined, "--yolo") {
		t.Fatalf("prompt or bypass flag in argv: %v", plan.Spec.Args)
	}
	if !strings.Contains(string(plan.Spec.Prompt), "secret prompt") {
		t.Fatalf("prompt is not on stdin")
	}
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Prompt: "x", Policy: "review", AdmitExperimental: true}); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("review err=%v", err)
	}
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Prompt: "x", Effort: "high", AdmitExperimental: true}); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("effort err=%v", err)
	}
	plan, err = a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "next", NativeSessionID: "q1", AdmitExperimental: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Spec.Args, " "), "--resume q1") {
		t.Fatalf("resume args=%v", plan.Spec.Args)
	}
}

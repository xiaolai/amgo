package agy

import (
	"context"
	"encoding/json"
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
		`{"event":"init","conversation_id":"conv-1","init":{"model":"agy-model","permission_mode":"request-review"}}`,
		`{"event":"step_update","conversation_id":"conv-1","step_update":{"state":"DONE","step_type":"agent_response"}}`,
		`{"event":"result","result":{"conversation_id":"conv-1","status":"SUCCESS","response":"ORCHID","usage":{"input_tokens":7,"output_tokens":3,"thinking_tokens":0,"cache_read_tokens":2,"total_tokens":10}}}`,
	)
	res := p.Result()
	if res.Outcome != model.RunCompleted || res.NativeSessionID != "conv-1" || res.Evidence != "ORCHID" {
		t.Fatalf("result=%+v", res)
	}
	if res.Usage == nil || *res.Usage.InputTokens != 7 || *res.Usage.OutputTokens != 3 || *res.Usage.CacheReadTokens != 2 {
		t.Fatalf("usage=%+v", res.Usage)
	}
}

func TestJobParserStatuses(t *testing.T) {
	cases := map[string]string{
		"ERROR": model.RunFailed, "INVALID": model.RunFailed, "CANCELED": model.RunFailed,
		"INTERRUPTED": model.RunFailed, "WAITING": model.RunNeedsAction,
	}
	for status, want := range cases {
		p := NewJobParser()
		line, _ := json.Marshal(map[string]any{"event": "result", "result": map[string]any{
			"conversation_id": "c", "status": status, "response": "", "error": status + " detail",
		}})
		feed(t, p, string(line))
		if got := p.Result().Outcome; got != want {
			t.Fatalf("%s: outcome=%s want=%s", status, got, want)
		}
	}
}

func fakeAgy(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "agy")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunSpecDocumentedStdin(t *testing.T) {
	fakeAgy(t)
	a := New()
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Prompt: "hello"}); model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("admission err=%v", err)
	}
	plan, err := a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "secret prompt", Model: "m", Effort: "high", AdmitExperimental: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Spec.Args, " ")
	if !strings.Contains(joined, "--input-format stream-json --output-format stream-json") ||
		!strings.Contains(joined, "--effort high") || !strings.Contains(joined, "--model m") {
		t.Fatalf("args=%v", plan.Spec.Args)
	}
	if strings.Contains(joined, "secret prompt") || strings.Contains(joined, "dangerously") {
		t.Fatalf("prompt or bypass flag in argv: %v", plan.Spec.Args)
	}
	var ev struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(plan.Spec.Prompt, &ev); err != nil {
		t.Fatalf("stdin is not one JSON event: %v", err)
	}
	if ev.Event != "user" || ev.Message.Content != "secret prompt" {
		t.Fatalf("stdin event=%+v", ev)
	}
	plan, err = a.RunSpec(context.Background(), adapter.RunRequest{
		Workspace: t.TempDir(), Prompt: "next", NativeSessionID: "conv-1", AdmitExperimental: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Spec.Args, " "), "--conversation conv-1") {
		t.Fatalf("resume args=%v", plan.Spec.Args)
	}
	if _, err := a.RunSpec(context.Background(), adapter.RunRequest{Prompt: "x", Policy: "review", AdmitExperimental: true}); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("review err=%v", err)
	}
}

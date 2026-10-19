package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/model"
	"amgo/internal/runner"
)

// TestFakeACPAgent is a fixture process. It implements enough of ACP v1 to
// exercise initialize, session/new, session/load, session/prompt, permission
// requests, cancellation and protocol errors.
func TestFakeACPAgent(t *testing.T) {
	if os.Getenv("AMGO_ACP_FAKE") != "1" {
		return
	}
	mode := os.Getenv("AMGO_ACP_FAKE_MODE")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	write := func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = os.Stdout.Write(append(raw, '\n'))
	}
	result := func(id json.RawMessage, v any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
	}
	rpcErr := func(id json.RawMessage, code int, msg string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	update := func(session, text string) {
		write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": session,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		}})
	}
	sessionID := "sess-1"
	var promptID json.RawMessage
	for sc.Scan() {
		var m struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Method == "" {
			if promptID != nil && string(m.ID) == "900" {
				var r struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				_ = json.Unmarshal(m.Result, &r)
				fmt.Fprintf(os.Stderr, "permission-%s:%s\n", r.Outcome.Outcome, r.Outcome.OptionID)
				result(promptID, map[string]any{"stopReason": "end_turn"})
				promptID = nil
			}
			continue
		}
		switch m.Method {
		case "initialize":
			if mode == "garbage" {
				fmt.Fprintln(os.Stdout, "this is not json")
				continue
			}
			methods := []any{}
			if mode == "auth" {
				methods = []any{map[string]any{"id": "login", "name": "Login", "type": "terminal"}}
			}
			result(m.ID, map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{
					"loadSession": mode != "no-load",
				},
				"authMethods": methods,
			})
		case "session/new":
			if mode == "auth" {
				rpcErr(m.ID, -32000, "Authentication required")
				continue
			}
			result(m.ID, map[string]any{"sessionId": sessionID})
		case "session/load":
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			sessionID = p.SessionID
			fmt.Fprintf(os.Stderr, "session-load:%s\n", sessionID)
			result(m.ID, map[string]any{})
		case "session/prompt":
			switch mode {
			case "success", "resume":
				update(sessionID, "hello ")
				update(sessionID, "world")
				result(m.ID, map[string]any{"stopReason": "end_turn"})
			case "permission":
				promptID = m.ID
				write(map[string]any{
					"jsonrpc": "2.0", "id": 900, "method": "session/request_permission",
					"params": map[string]any{
						"sessionId": sessionID,
						"toolCall":  map[string]any{"toolCallId": "t1", "title": "Write a file", "kind": "edit"},
						"options": []any{
							map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
							map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
						},
					},
				})
			case "refusal":
				result(m.ID, map[string]any{"stopReason": "refusal"})
			case "max":
				result(m.ID, map[string]any{"stopReason": "max_tokens"})
			case "hang":
				promptID = m.ID
			}
		case "session/cancel":
			fmt.Fprintln(os.Stderr, "cancel-seen")
			if promptID != nil {
				result(promptID, map[string]any{"stopReason": "cancelled"})
				promptID = nil
			}
		default:
			if len(m.ID) > 0 {
				rpcErr(m.ID, -32601, "method not found")
			}
		}
	}
	os.Exit(0)
}

func runFake(t *testing.T, mode, sessionID string, opts runner.Options) (runner.Outcome, string) {
	t.Helper()
	if opts.LogPath == "" {
		opts.LogPath = filepath.Join(t.TempDir(), "agent.log")
	}
	ws := t.TempDir()
	out, err := runner.Execute(context.Background(), runner.Spec{
		Path: os.Args[0], Args: []string{"-test.run=TestFakeACPAgent", "--"},
		Dir: ws, Env: []string{"AMGO_ACP_FAKE=1", "AMGO_ACP_FAKE_MODE=" + mode},
		Driver: &Driver{Workspace: ws, Prompt: "hello", SessionID: sessionID, Version: "test"},
	}, nil, opts, runner.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(opts.LogPath)
	return out, string(raw)
}

func TestDriverEndTurn(t *testing.T) {
	out, log := runFake(t, "success", "", runner.Options{})
	if out.Result.Outcome != model.RunCompleted || out.Result.Evidence != "hello world" ||
		out.Result.NativeSessionID != "sess-1" {
		t.Fatalf("out=%+v log=%s", out, log)
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		t.Fatalf("exit=%v", out.ExitCode)
	}
}

func TestDriverRejectsPermission(t *testing.T) {
	out, log := runFake(t, "permission", "", runner.Options{})
	if out.Result.Outcome != model.RunNeedsAction || out.Result.ErrorCode != model.ErrProviderNeedsAction {
		t.Fatalf("out=%+v", out.Result)
	}
	if !strings.Contains(log, "permission-selected:reject") {
		t.Fatalf("client did not select the reject option: %s", log)
	}
}

func TestDriverStopReasons(t *testing.T) {
	out, _ := runFake(t, "refusal", "", runner.Options{})
	if out.Result.Outcome != model.RunNeedsAction {
		t.Fatalf("refusal=%+v", out.Result)
	}
	out, _ = runFake(t, "max", "", runner.Options{})
	if out.Result.Outcome != model.RunPartial {
		t.Fatalf("max=%+v", out.Result)
	}
}

func TestDriverResumeRequiresLoadSession(t *testing.T) {
	out, log := runFake(t, "resume", "sess-old", runner.Options{})
	if out.Result.Outcome != model.RunCompleted || out.Result.NativeSessionID != "sess-old" {
		t.Fatalf("out=%+v log=%s", out, log)
	}
	if !strings.Contains(log, "session-load:sess-old") {
		t.Fatalf("session/load was not used: %s", log)
	}
	out, _ = runFake(t, "no-load", "sess-old", runner.Options{})
	if out.Result.Outcome != model.RunFailed || out.Result.ErrorCode != model.ErrUnsupported {
		t.Fatalf("no-load=%+v", out.Result)
	}
}

func TestDriverAuthRequired(t *testing.T) {
	out, _ := runFake(t, "auth", "", runner.Options{})
	if out.Result.Outcome != model.RunNeedsAction ||
		!strings.Contains(out.Result.ErrorDetail, "authentication") {
		t.Fatalf("out=%+v", out.Result)
	}
}

func TestDriverCancellationSendsCancel(t *testing.T) {
	cancel := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(cancel)
	}()
	out, log := runFake(t, "hang", "", runner.Options{Cancel: cancel})
	if !out.Cancelled {
		t.Fatalf("out=%+v", out)
	}
	if !strings.Contains(log, "cancel-seen") {
		t.Fatalf("session/cancel was not sent: %s", log)
	}
}

func TestDriverRejectsInvalidJSON(t *testing.T) {
	out, _ := runFake(t, "garbage", "", runner.Options{})
	if out.Result.Outcome != model.RunFailed || out.Result.ErrorCode != model.ErrEventParse {
		t.Fatalf("out=%+v", out.Result)
	}
}

func adapterRunRequest(t *testing.T) adapter.RunRequest {
	t.Helper()
	return adapter.RunRequest{Workspace: t.TempDir(), Prompt: "hello"}
}

func TestRunSpecGuards(t *testing.T) {
	a := New()
	if _, err := a.RunSpec(context.Background(), adapterRunRequest(t)); model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("admission err=%v", err)
	}
	req := adapterRunRequest(t)
	req.AdmitExperimental = true
	if _, err := a.RunSpec(context.Background(), req); model.Code(err) != model.ErrUsage {
		t.Fatalf("missing command err=%v", err)
	}
	req.ACPCommand = os.Args[0]
	req.ACPArgs = []string{"-test.run=TestFakeACPAgent"}
	plan, err := a.RunSpec(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Spec.Driver == nil || plan.Spec.Path != os.Args[0] {
		t.Fatalf("plan=%+v", plan)
	}
	req.Policy = "review"
	if _, err := a.RunSpec(context.Background(), req); model.Code(err) != model.ErrUnsupported {
		t.Fatalf("review err=%v", err)
	}
}

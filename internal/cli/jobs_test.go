package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"amgo/internal/adapter"
	"amgo/internal/model"
	"amgo/internal/runner"
)

func TestCLIJobHelper(t *testing.T) {
	if os.Getenv("AMGO_CLI_JOB_HELPER") != "1" {
		return
	}
	sess := os.Getenv("AMGO_CLI_JOB_SESSION")
	if sess == "" {
		sess = "s1"
	}
	fmt.Printf("{\"event\":\"native\",\"session\":%q}\n", sess)
	switch os.Getenv("AMGO_CLI_JOB_MODE") {
	case "ok":
		fmt.Println(`{"event":"text","text":"RESULT"}`)
		fmt.Println(`{"event":"done"}`)
	case "fail":
		fmt.Println(`{"event":"failed"}`)
	}
	os.Exit(0)
}

type cliJobParser struct {
	session, text string
	done, failed  bool
}

func (p *cliJobParser) Line(line []byte) ([]runner.Event, error) {
	var ev struct {
		Event   string `json:"event"`
		Session string `json:"session"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "native":
		p.session = ev.Session
		return []runner.Event{{Kind: "native_session", Payload: map[string]any{"session_id": ev.Session}}}, nil
	case "text":
		p.text = ev.Text
	case "done":
		p.done = true
	case "failed":
		p.failed = true
	}
	return nil, nil
}

func (p *cliJobParser) Result() runner.Result {
	switch {
	case p.failed:
		return runner.Result{Outcome: model.RunFailed, ErrorCode: model.ErrProviderFailed, ErrorDetail: "fake failure"}
	case p.done:
		return runner.Result{Outcome: model.RunCompleted, NativeSessionID: p.session, Evidence: p.text}
	}
	return runner.Result{NativeSessionID: p.session}
}

type cliFakeAdapter struct{}

func (f *cliFakeAdapter) Name() string { return "fake" }
func (f *cliFakeAdapter) Probe(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Adapter: "fake", Jobs: true}, nil
}
func (f *cliFakeAdapter) Discover(context.Context, adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}
func (f *cliFakeAdapter) Send(context.Context, adapter.SendRequest) (adapter.SendResult, error) {
	return adapter.SendResult{}, model.NewError(model.ErrUnsupported, "fake send")
}
func (f *cliFakeAdapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	env := []string{"AMGO_CLI_JOB_HELPER=1", "AMGO_CLI_JOB_MODE=" + req.Prompt}
	if req.NativeSessionID != "" {
		env = append(env, "AMGO_CLI_JOB_SESSION="+req.NativeSessionID)
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: os.Args[0], Args: []string{"-test.run=TestCLIJobHelper", "--"},
			Dir: req.Workspace, Env: env, Prompt: []byte("ignored"), Provenance: "fake",
		},
		Parser: &cliJobParser{}, NativeSessionID: req.NativeSessionID,
	}, nil
}

type cliEnvelope struct {
	SchemaVersion int            `json:"schema_version"`
	Operation     string         `json:"operation"`
	Outcome       string         `json:"outcome"`
	Data          map[string]any `json:"data"`
	Error         map[string]any `json:"error"`
}

func runJobCLI(t *testing.T, stdin string, args ...string) (int, cliEnvelope, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runWith(args, strings.NewReader(stdin), &out, &errOut,
		map[string]adapter.Adapter{"fake": &cliFakeAdapter{}})
	var env cliEnvelope
	if strings.Contains(strings.Join(args, " "), "--json") {
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("invalid JSON %q: %v (stderr %s)", out.String(), err, errOut.String())
		}
	}
	return code, env, errOut.String()
}

func TestRunStatusResultJobsFollow(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	code, env, stderr := runJobCLI(t, "", "--home", home, "--json", "run", "fake", "--cwd", ws, "--text", "ok")
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, stderr)
	}
	if env.Outcome != "ok" || env.Data["state"] != string(model.JobCompleted) || env.Data["result"] != "RESULT" {
		t.Fatalf("run envelope=%+v", env)
	}
	id, _ := env.Data["job_id"].(string)
	if id == "" || env.Data["native_session_id"] != "s1" {
		t.Fatalf("run data=%+v", env.Data)
	}

	code, env, stderr = runJobCLI(t, "", "--home", home, "--json", "status", id)
	if code != 0 || env.Data["state"] != string(model.JobCompleted) {
		t.Fatalf("status exit=%d stderr=%s data=%+v", code, stderr, env.Data)
	}
	if evs, ok := env.Data["events"].([]any); !ok || len(evs) < 3 {
		t.Fatalf("status events=%+v", env.Data["events"])
	}

	code, env, stderr = runJobCLI(t, "", "--home", home, "--json", "result", id)
	if code != 0 || env.Data["result"] != "RESULT" {
		t.Fatalf("result exit=%d stderr=%s data=%+v", code, stderr, env.Data)
	}
	if arts, ok := env.Data["artifacts"].([]any); !ok || len(arts) != 1 {
		t.Fatalf("artifacts=%+v", env.Data["artifacts"])
	}

	code, env, stderr = runJobCLI(t, "", "--home", home, "--json", "jobs")
	if code != 0 || len(env.Data["jobs"].([]any)) != 1 {
		t.Fatalf("jobs exit=%d stderr=%s data=%+v", code, stderr, env.Data)
	}

	code, env, stderr = runJobCLI(t, "", "--home", home, "--json", "follow", id, "--after", "0", "--timeout", "1s")
	if code != 0 || env.Data["state"] != string(model.JobCompleted) {
		t.Fatalf("follow exit=%d stderr=%s data=%+v", code, stderr, env.Data)
	}

	code, env, stderr = runJobCLI(t, "", "--home", home, "--json", "continue", id, "--text", "ok")
	if code != 0 || env.Data["resume_of"] != id {
		t.Fatalf("continue exit=%d stderr=%s data=%+v", code, stderr, env.Data)
	}
}

func TestRunFailedJobExitsEight(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	code, env, stderr := runJobCLI(t, "", "--home", home, "--json", "run", "fake", "--cwd", ws, "--text", "fail")
	if code != 8 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if env.Outcome != "ok" || env.Data["state"] != string(model.JobFailed) {
		t.Fatalf("envelope=%+v", env)
	}
}

func TestRunRequiresOnePromptSource(t *testing.T) {
	code, _, stderr := runJobCLI(t, "", "--home", t.TempDir(), "run", "fake", "--cwd", t.TempDir())
	if code != 2 || !strings.Contains(stderr, "--prompt-file") {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
}

func TestRunUnknownAdapterUnsupported(t *testing.T) {
	code, _, stderr := runJobCLI(t, "", "--home", t.TempDir(), "run", "nope", "--cwd", t.TempDir(), "--text", "ok")
	if code != 3 || !strings.Contains(stderr, "not registered") {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
}

func TestRunRefusesUnenforceablePolicyBeforeInvocation(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	code, _, stderr := runJobCLI(t, "", "--home", home, "run", "claude", "--cwd", ws, "--policy", "review", "--text", "ok")
	if code != 3 || !strings.Contains(stderr, "not an OS sandbox") {
		t.Fatalf("claude review exit=%d stderr=%s", code, stderr)
	}
	code, _, stderr = runJobCLI(t, "", "--home", home, "run", "codex", "--cwd", ws, "--policy", "bogus", "--text", "ok")
	if code != 3 || !strings.Contains(stderr, "unknown execution policy") {
		t.Fatalf("codex policy exit=%d stderr=%s", code, stderr)
	}
}

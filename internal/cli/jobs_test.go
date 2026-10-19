package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/model"
	"amgo/internal/runner"
	"amgo/internal/session"
	"amgo/internal/store"
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

func TestCancelAcceptUnconfirmedFlagOrders(t *testing.T) {
	for _, args := range [][]string{
		{"cancel", "--accept-unconfirmed-termination", "j_missing"},
		{"cancel", "j_missing", "--accept-unconfirmed-termination"},
		{"cancel", "j_missing", "--accept-unconfirmed-termination", "--grace", "1s"},
		{"cancel", "--grace", "1s", "j_missing", "--accept-unconfirmed-termination"},
	} {
		home := t.TempDir()
		full := append([]string{"--home", home, "--json"}, args...)
		code, _, stderr := runJobCLI(t, "", full...)
		if code != 5 {
			t.Fatalf("%v exit=%d stderr=%s", args, code, stderr)
		}
	}
}

func TestRecoverReconcilesJobs(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	st, err := store.Open(config.DBPath(home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	checkout, _ := session.CheckoutIDs(ws)
	job := model.Job{
		ID: "j_lost", Adapter: "fake", Workspace: ws, CheckoutID: checkout,
		Prompt: "x", PromptSHA256: "h", Policy: "default", State: model.JobRunning,
		CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
		Deadline: now.Add(time.Hour), OwnerPID: 999999, OwnerProcStart: "gone",
		ProviderStartRecorded: true,
	}
	if _, err := st.CreateJob(job, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLease(model.Lease{
		CheckoutID: checkout, JobID: job.ID, ExpiresAt: now.Add(time.Hour),
	}, now, nil); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	code, env, stderr := runJobCLI(t, "", "--home", home, "--json", "recover")
	if code != 0 {
		t.Fatalf("recover exit=%d stderr=%s", code, stderr)
	}
	if got, _ := env.Data["jobs_recovered"].(float64); got != 1 {
		t.Fatalf("jobs_recovered=%v data=%+v", env.Data["jobs_recovered"], env.Data)
	}
	st, err = store.Open(config.DBPath(home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.JobInterrupted {
		t.Fatalf("state=%s", got.State)
	}
	if _, found, err := st.GetLease(checkout); err != nil || found {
		t.Fatalf("lease found=%v err=%v", found, err)
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

// TestCLIACPAgent is a minimal ACP fixture process for the CLI test binary.
func TestCLIACPAgent(t *testing.T) {
	if os.Getenv("AMGO_CLI_ACP_FAKE") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	write := func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = os.Stdout.Write(append(raw, '\n'))
	}
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		result := func(v any) { write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v}) }
		switch m.Method {
		case "initialize":
			result(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}})
		case "session/new":
			result(map[string]any{"sessionId": "sess-1"})
		case "session/prompt":
			for _, text := range []string{"hello ", "world"} {
				write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "sess-1",
					"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}},
				}})
			}
			result(map[string]any{"stopReason": "end_turn"})
		default:
			if len(m.ID) > 0 {
				write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			}
		}
	}
	os.Exit(0)
}

func TestRunACPDocumentedOnly(t *testing.T) {
	t.Setenv("AMGO_CLI_ACP_FAKE", "1")
	agent, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	ws := t.TempDir()
	base := []string{"run", "acp", "--cwd", ws, "--text", "hello", "--acp-command", agent,
		"--acp-arg=-test.run=TestCLIACPAgent", "--acp-arg=--"}
	code, _, stderr := runJobCLI(t, "", append([]string{"--home", home}, base...)...)
	if code != 6 || !strings.Contains(stderr, "documented-only") {
		t.Fatalf("without admission exit=%d stderr=%s", code, stderr)
	}
	args := append([]string{"--home", home, "--json"}, base...)
	args = append(args, "--admit-experimental")
	code, env, stderr := runJobCLI(t, "", args...)
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s env=%+v", code, stderr, env)
	}
	if env.Data["state"] != string(model.JobCompleted) || env.Data["result"] != "hello world" ||
		env.Data["native_session_id"] != "sess-1" {
		t.Fatalf("data=%+v", env.Data)
	}
}

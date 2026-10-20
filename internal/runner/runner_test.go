package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"amgo/internal/model"
	"amgo/internal/platform"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("AMGO_HELPER") != "1" {
		return
	}
	switch os.Getenv("AMGO_HELPER_MODE") {
	case "ok":
		in, _ := io.ReadAll(os.Stdin)
		fmt.Println(`{"event":"start","session":"s1"}`)
		fmt.Printf("{\"event\":\"text\",\"text\":%q}\n", "hello "+string(in))
		fmt.Fprintln(os.Stderr, "helper says hi")
		fmt.Println(`{"event":"done","status":"success"}`)
	case "slow":
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Printf("{\"event\":\"start\",\"session\":\"s2\",\"grandchild\":%d}\n", child.Process.Pid)
		time.Sleep(60 * time.Second)
	case "malformed":
		fmt.Print("not json\n")
		time.Sleep(30 * time.Second)
	case "exit1":
		fmt.Println(`{"event":"start","session":"s3"}`)
		os.Exit(1)
	case "backlog":
		line := strings.Repeat("x", 1024)
		for i := 0; i < 50; i++ {
			fmt.Printf("{\"event\":\"text\",\"text\":%q}\n", line)
		}
	case "secret":
		token := os.Getenv("ANTHROPIC_AUTH_TOKEN")
		fmt.Printf("{\"event\":\"text\",\"text\":%q}\n", token)
		fmt.Fprintln(os.Stderr, "stderr token "+token)
		fmt.Println(`{"event":"done","status":"success"}`)
	case "stray":
		child := exec.Command("sleep", "60")
		child.Stdout = nil
		child.Stderr = nil
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if p := os.Getenv("AMGO_HELPER_PIDFILE"); p != "" {
			_ = os.WriteFile(p, []byte(fmt.Sprint(child.Process.Pid)), 0o600)
		}
		fmt.Println(`{"event":"start","session":"s4"}`)
		fmt.Println(`{"event":"done","status":"success"}`)
	case "noresult":
		os.Exit(0)
	}
	os.Exit(0)
}

type testParser struct {
	session  string
	grandkid int
	done     bool
	evidence string
}

func (p *testParser) Line(line []byte) ([]Event, error) {
	var ev struct {
		Event      string
		Session    string
		Text       string
		Status     string
		Grandchild int
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "start":
		p.session = ev.Session
		p.grandkid = ev.Grandchild
		return []Event{{Kind: "native_id", Payload: map[string]any{"session_id": ev.Session}}}, nil
	case "done":
		p.done = true
		p.evidence = ev.Status
		return []Event{{Kind: "result", Payload: map[string]any{"status": ev.Status}}}, nil
	}
	return nil, nil
}

func (p *testParser) Result() Result {
	if !p.done {
		return Result{}
	}
	return Result{Outcome: model.RunCompleted, NativeSessionID: p.session, Evidence: p.evidence}
}

func helperSpec(t *testing.T, mode string) Spec {
	t.Helper()
	return Spec{
		Path:   os.Args[0],
		Args:   []string{"-test.run=TestHelperProcess", "--"},
		Dir:    t.TempDir(),
		Env:    []string{"AMGO_HELPER=1", "AMGO_HELPER_MODE=" + mode},
		Prompt: []byte("PROMPT"),
	}
}

func TestExecuteSuccessAndLog(t *testing.T) {
	logPath := t.TempDir() + "/agent.log"
	var started bool
	var events []string
	out, err := Execute(context.Background(), helperSpec(t, "ok"), &testParser{}, Options{LogPath: logPath}, Hooks{
		OnStart: func(pid int, procStart string) error {
			started = pid > 0 && procStart != ""
			return nil
		},
		OnEvent: func(ev Event) error {
			events = append(events, ev.Kind)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("OnStart did not receive a live process identity")
	}
	if out.Result.Outcome != model.RunCompleted || out.Result.NativeSessionID != "s1" {
		t.Fatalf("result=%+v", out.Result)
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		t.Fatalf("exit=%v", out.ExitCode)
	}
	if out.LogBytes == 0 || len(out.LogSHA256) != 64 || out.LogTruncated {
		t.Fatalf("log=%+v", out)
	}
	if strings.Join(events, ",") != "native_id,result" {
		t.Fatalf("events=%v", events)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"event":"start"`) || !strings.Contains(string(raw), "stderr: helper says hi") {
		t.Fatalf("log=%s", raw)
	}
}

func TestExecuteCancelKillsGroup(t *testing.T) {
	cancel := make(chan struct{})
	started := make(chan struct{})
	p := &testParser{}
	go func() {
		<-started
		time.Sleep(100 * time.Millisecond)
		close(cancel)
	}()
	out, err := Execute(context.Background(), helperSpec(t, "slow"), p, Options{Cancel: cancel, LogPath: t.TempDir() + "/agent.log"}, Hooks{
		OnStart: func(int, string) error { close(started); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Cancelled || out.Result.ErrorCode != model.ErrInterrupted {
		t.Fatalf("out=%+v", out)
	}
	if p.grandkid <= 0 {
		t.Fatal("helper did not report a grandchild")
	}
	deadline := time.Now().Add(3 * time.Second)
	for platform.PIDAlive(p.grandkid) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if platform.PIDAlive(p.grandkid) {
		t.Fatalf("grandchild %d survived cancellation", p.grandkid)
	}
}

func TestExecuteTimeout(t *testing.T) {
	out, err := Execute(context.Background(), helperSpec(t, "slow"), &testParser{}, Options{
		Timeout: 300 * time.Millisecond, LogPath: t.TempDir() + "/agent.log",
	}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut || out.Result.ErrorCode != model.ErrDeadlineExceeded {
		t.Fatalf("out=%+v", out)
	}
}

func TestExecuteRejectsMalformedEvent(t *testing.T) {
	out, err := Execute(context.Background(), helperSpec(t, "malformed"), &testParser{}, Options{LogPath: t.TempDir() + "/agent.log"}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Outcome != model.RunFailed || out.Result.ErrorCode != model.ErrEventParse {
		t.Fatalf("out=%+v", out.Result)
	}
}

func TestExecuteExitWithoutTerminalEvidence(t *testing.T) {
	out, err := Execute(context.Background(), helperSpec(t, "exit1"), &testParser{}, Options{LogPath: t.TempDir() + "/agent.log"}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Outcome != model.RunFailed || !out.Result.ResultUnverified {
		t.Fatalf("out=%+v", out.Result)
	}
	if out.ExitCode == nil || *out.ExitCode != 1 {
		t.Fatalf("exit=%v", out.ExitCode)
	}

	out, err = Execute(context.Background(), helperSpec(t, "noresult"), &testParser{}, Options{LogPath: t.TempDir() + "/agent.log"}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Outcome != model.RunPartial || !out.Result.ResultUnverified {
		t.Fatalf("clean exit without evidence must be partial: %+v", out.Result)
	}
}

func TestExecuteBoundsLog(t *testing.T) {
	logPath := t.TempDir() + "/agent.log"
	out, err := Execute(context.Background(), helperSpec(t, "backlog"), &testParser{}, Options{
		LogPath: logPath, MaxLogBytes: 2048,
	}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.LogTruncated {
		t.Fatal("log cap was not recorded")
	}
	if out.LogBytes > 2048 {
		t.Fatalf("log bytes=%d", out.LogBytes)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "log truncated at the per-job limit") {
		t.Fatalf("truncation marker missing: %q", raw)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != out.LogSHA256 {
		t.Fatalf("log hash=%s, file hash=%s", out.LogSHA256, got)
	}
	if int64(len(raw)) != out.LogBytes {
		t.Fatalf("log bytes=%d, file bytes=%d", out.LogBytes, len(raw))
	}
}

type captureParser struct {
	lines []string
}

func (p *captureParser) Line(line []byte) ([]Event, error) {
	p.lines = append(p.lines, string(line))
	return nil, nil
}

func (p *captureParser) Result() Result { return Result{Outcome: model.RunCompleted} }

func TestExecuteRedactsCredentialValues(t *testing.T) {
	secret := strings.Repeat("z", 32)
	logPath := filepath.Join(t.TempDir(), "agent.log")
	spec := helperSpec(t, "secret")
	spec.Env = append(spec.Env, "ANTHROPIC_AUTH_TOKEN="+secret)
	p := &captureParser{}
	out, err := Execute(context.Background(), spec, p, Options{LogPath: logPath}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("log leaked the credential: %q", raw)
	}
	if !bytes.Contains(raw, []byte("[redacted]")) {
		t.Fatalf("log has no redaction marker: %q", raw)
	}
	for _, line := range p.lines {
		if strings.Contains(line, secret) {
			t.Fatalf("parser saw the credential: %q", line)
		}
	}
	if out.Result.Outcome != model.RunCompleted {
		t.Fatalf("outcome=%s", out.Result.Outcome)
	}
}

func TestExecuteTerminatesStrayGroupMembers(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	spec := helperSpec(t, "stray")
	spec.Env = append(spec.Env, "AMGO_HELPER_PIDFILE="+pidFile)
	out, err := Execute(context.Background(), spec, &testParser{}, Options{
		LogPath: t.TempDir() + "/agent.log",
	}, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.StrayGroup {
		t.Fatalf("stray group member was not recorded: %+v", out)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for platform.PIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if platform.PIDAlive(pid) {
		t.Fatalf("same-group writer %d survived Execute", pid)
	}
}

func TestExecutePersistenceErrorAborts(t *testing.T) {
	out, err := Execute(context.Background(), helperSpec(t, "ok"), &testParser{}, Options{LogPath: t.TempDir() + "/agent.log"}, Hooks{
		OnEvent: func(Event) error { return model.NewError(model.ErrStoreBusy, "store is busy") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Outcome != model.RunFailed || out.Result.ErrorCode != model.ErrStoreBusy {
		t.Fatalf("out=%+v", out.Result)
	}
}

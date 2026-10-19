package job

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/model"
	"amgo/internal/platform"
	"amgo/internal/runner"
	"amgo/internal/session"
	"amgo/internal/store"
)

func TestJobHelper(t *testing.T) {
	if os.Getenv("AMGO_JOB_HELPER") != "1" {
		return
	}
	sess := os.Getenv("AMGO_JOB_HELPER_SESSION")
	if sess == "" {
		sess = "s1"
	}
	fmt.Printf("{\"event\":\"native\",\"session\":%q}\n", sess)
	switch os.Getenv("AMGO_JOB_HELPER_MODE") {
	case "ok":
		fmt.Println(`{"event":"text","text":"RESULT"}`)
		fmt.Println(`{"event":"done"}`)
	case "exit0":
	case "fail":
		fmt.Println(`{"event":"failed"}`)
	case "slow":
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

type fakeParser struct {
	session, text string
	done, failed  bool
}

func (p *fakeParser) Line(line []byte) ([]runner.Event, error) {
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
		return nil, nil
	case "done":
		p.done = true
	case "failed":
		p.failed = true
	}
	return nil, nil
}

func (p *fakeParser) Result() runner.Result {
	switch {
	case p.failed:
		return runner.Result{Outcome: model.RunFailed, ErrorCode: model.ErrProviderFailed, ErrorDetail: "fake failure", NativeSessionID: p.session}
	case p.done:
		return runner.Result{Outcome: model.RunCompleted, NativeSessionID: p.session, Evidence: p.text}
	}
	return runner.Result{NativeSessionID: p.session}
}

type fakeAdapter struct{}

func (f *fakeAdapter) Name() string { return "fake" }
func (f *fakeAdapter) Probe(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Adapter: "fake", Jobs: true}, nil
}
func (f *fakeAdapter) Discover(context.Context, adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}
func (f *fakeAdapter) Send(context.Context, adapter.SendRequest) (adapter.SendResult, error) {
	return adapter.SendResult{}, model.NewError(model.ErrUnsupported, "fake send")
}
func (f *fakeAdapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	env := []string{"AMGO_JOB_HELPER=1", "AMGO_JOB_HELPER_MODE=" + req.Prompt}
	if req.NativeSessionID != "" {
		env = append(env, "AMGO_JOB_HELPER_SESSION="+req.NativeSessionID)
	}
	return adapter.RunPlan{
		Spec: runner.Spec{
			Path: os.Args[0], Args: []string{"-test.run=TestJobHelper", "--"},
			Dir: req.Workspace, Env: env, Prompt: []byte("ignored"), Provenance: "fake",
		},
		Parser: &fakeParser{}, NativeSessionID: req.NativeSessionID,
	}, nil
}

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	home := t.TempDir()
	ws := t.TempDir()
	return New(home, map[string]adapter.Adapter{"fake": &fakeAdapter{}}), ws
}

func TestStartCompletesAndPersists(t *testing.T) {
	svc, ws := newTestService(t)
	job, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != model.JobCompleted || job.NativeSessionID != "s1" || job.ResultText != "RESULT" {
		t.Fatalf("job=%+v", job)
	}
	if job.LogPath == "" || job.LogBytes == 0 {
		t.Fatalf("log=%+v", job)
	}
	if _, err := os.Stat(filepath.Join(svc.Home, job.LogPath)); err != nil {
		t.Fatalf("log file missing: %v", err)
	}
	status, err := svc.Status(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Job.State != model.JobCompleted || len(status.Events) < 3 || len(status.Artifacts) != 1 {
		t.Fatalf("status=%+v", status)
	}
	if len(status.Artifacts[0].SHA256) != 64 {
		t.Fatalf("artifact=%+v", status.Artifacts[0])
	}
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, found, err := st.GetLease(job.CheckoutID); err != nil || found {
		t.Fatalf("lease found=%v err=%v", found, err)
	}
}

func TestStartWithoutTerminalEvidenceIsPartial(t *testing.T) {
	svc, ws := newTestService(t)
	job, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "exit0"})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != model.JobPartial || !job.ResultUnverified {
		t.Fatalf("job=%+v", job)
	}
}

func TestStartProviderFailure(t *testing.T) {
	svc, ws := newTestService(t)
	job, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "fail"})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != model.JobFailed || job.ErrorCode != model.ErrProviderFailed {
		t.Fatalf("job=%+v", job)
	}
}

func TestWorkspaceBusyAndCancel(t *testing.T) {
	svc, ws := newTestService(t)
	done := make(chan model.Job, 1)
	go func() {
		j, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "slow"})
		if err != nil {
			t.Errorf("slow start: %v", err)
		}
		done <- j
	}()
	var slow model.Job
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := svc.List(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 1 && jobs[0].State == model.JobRunning {
			slow = jobs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if slow.ID == "" {
		t.Fatal("slow job never reached running")
	}
	_, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "ok"})
	if model.Code(err) != model.ErrWorkspaceBusy {
		t.Fatalf("second start err=%v", err)
	}
	cancelled, err := svc.Cancel(context.Background(), slow.ID, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != model.JobCancelled {
		t.Fatalf("cancel state=%s", cancelled.State)
	}
	select {
	case j := <-done:
		if j.State != model.JobCancelled {
			t.Fatalf("supervisor state=%s", j.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow supervisor did not finish")
	}
}

func TestContinueCreatesLinkedJob(t *testing.T) {
	svc, ws := newTestService(t)
	first, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Continue(context.Background(), ContinueInput{JobID: first.ID, Prompt: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != model.JobCompleted || second.ResumeOf != first.ID || second.NativeSessionID != first.NativeSessionID {
		t.Fatalf("second=%+v", second)
	}
	orig, err := svc.Status(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig.Job.ResumedBy) != 1 || orig.Job.ResumedBy[0] != second.ID {
		t.Fatalf("resumed_by=%v", orig.Job.ResumedBy)
	}
}

func TestContinueRefusesLiveRegisteredSession(t *testing.T) {
	svc, ws := newTestService(t)
	first, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	start, err := platform.ProcStartUTC(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSession(model.Session{
		RecordVersion: 1, ID: "s_live", Alias: "live",
		Target: model.Target{Adapter: "fake", NativeSessionID: first.NativeSessionID, PID: os.Getpid(), ProcStart: start},
	}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	_ = st.Close()
	if _, err := svc.Continue(context.Background(), ContinueInput{JobID: first.ID, Prompt: "ok"}); model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("continue err=%v", err)
	}
}

func TestRecoverReapsLostSupervisor(t *testing.T) {
	svc, ws := newTestService(t)
	now := time.Now().UTC()
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	checkout, _ := session.CheckoutIDs(ws)
	job := model.Job{
		ID: "j_lost", Adapter: "fake", Workspace: ws, CheckoutID: checkout,
		Prompt: "ok", PromptSHA256: "h", Policy: "default",
		State: model.JobRunning, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
		Deadline: now.Add(time.Hour), OwnerPID: 999999, OwnerProcStart: "gone",
	}
	if _, err := st.CreateJob(job, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireLease(model.Lease{CheckoutID: checkout, JobID: job.ID, ExpiresAt: now.Add(time.Hour)}, now, nil); err != nil {
		t.Fatal(err)
	}
	created := model.Job{
		ID: "j_orphan", Adapter: "fake", Workspace: ws, CheckoutID: checkout + "x",
		Prompt: "ok", PromptSHA256: "h", Policy: "default",
		State: model.JobCreated, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
		Deadline: now.Add(time.Hour),
	}
	if _, err := st.CreateJob(created, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	n, err := svc.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("recovered=%d", n)
	}
	got, err := svc.Status(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Job.State != model.JobUnknown || !got.Job.ResultUnverified {
		t.Fatalf("job=%+v", got.Job)
	}
	got, err = svc.Status(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Job.State != model.JobInterrupted {
		t.Fatalf("orphan=%+v", got.Job)
	}
}

func TestFollowReturnsTerminalAndEvents(t *testing.T) {
	svc, ws := newTestService(t)
	job, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	got, events, err := svc.Follow(context.Background(), job.ID, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.State.Terminal() || len(events) < 3 {
		t.Fatalf("job=%+v events=%+v", got, events)
	}
}

func TestResultRequiresTerminal(t *testing.T) {
	svc, ws := newTestService(t)
	now := time.Now().UTC()
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(model.Job{
		ID: "j_running", Adapter: "fake", Workspace: ws, Prompt: "p", PromptSHA256: "h",
		Policy: "default", State: model.JobRunning, CreatedAt: now, UpdatedAt: now, Deadline: now.Add(time.Hour),
	}, now); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	_ = st.Close()
	if _, _, err := svc.Result("j_running"); model.Code(err) != model.ErrUsage {
		t.Fatalf("result err=%v", err)
	}
}

type admitAdapter struct{ fakeAdapter }

func (a *admitAdapter) RunSpec(ctx context.Context, req adapter.RunRequest) (adapter.RunPlan, error) {
	if !req.AdmitExperimental {
		return adapter.RunPlan{}, model.NewError(model.ErrPolicyRefused, "documented-only")
	}
	return a.fakeAdapter.RunSpec(ctx, req)
}

func TestExperimentalAdmissionSurvivesSupervision(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	svc := New(home, map[string]adapter.Adapter{"fake": &admitAdapter{}})
	if _, err := svc.Start(context.Background(), StartInput{Adapter: "fake", Workspace: ws, Prompt: "ok"}); model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("without admission err=%v", err)
	}
	job, err := svc.Start(context.Background(), StartInput{
		Adapter: "fake", Workspace: ws, Prompt: "ok", AdmitExperimental: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != model.JobCompleted || !job.AdmitExperimental {
		t.Fatalf("job=%+v", job)
	}
}

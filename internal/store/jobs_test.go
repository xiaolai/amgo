package store

import (
	"testing"
	"time"

	"amgo/internal/model"
)

func testJob(now time.Time, id string) model.Job {
	return model.Job{
		ID: id, Adapter: "fake", Workspace: "/work", CheckoutID: "co_1",
		Prompt: "do work", PromptSHA256: "h", PromptBytes: 7, Policy: "default",
		State: model.JobCreated, CreatedAt: now, Deadline: now.Add(time.Hour),
	}
}

func TestJobLifecycleAndEvents(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	if _, err := st.CreateJob(testJob(now, "j1"), now); err != nil {
		t.Fatal(err)
	}
	job, err := st.UpdateJob("j1", now.Add(time.Second), func(j *model.Job) error {
		j.State = model.JobRunning
		j.ChildPID = 42
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != model.JobRunning || job.ChildPID != 42 {
		t.Fatalf("job=%+v", job)
	}
	if _, err := st.AppendJobEvent("j1", "progress", now.Add(2*time.Second), map[string]any{"note": "halfway"}); err != nil {
		t.Fatal(err)
	}
	evs, err := st.ListJobEvents("j1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].Kind != "created" || evs[1].Kind != "state" || evs[2].Kind != "progress" {
		t.Fatalf("events=%+v", evs)
	}
	after, err := st.ListJobEvents("j1", evs[1].Seq, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Kind != "progress" {
		t.Fatalf("after=%+v", after)
	}
	js, err := st.ListJobs(10)
	if err != nil || len(js) != 1 || js[0].ID != "j1" {
		t.Fatalf("jobs=%+v err=%v", js, err)
	}
	active, err := st.ListActiveJobs()
	if err != nil || len(active) != 1 {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	if _, err := st.UpdateJob("j1", now.Add(3*time.Second), func(j *model.Job) error {
		j.State = model.JobCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	active, err = st.ListActiveJobs()
	if err != nil || len(active) != 0 {
		t.Fatalf("terminal job is active: %+v err=%v", active, err)
	}
}

func TestLeaseBusyStaleAndReleaseOwnership(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	if _, err := st.CreateJob(testJob(now, "j1"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(testJob(now, "j2"), now); err != nil {
		t.Fatal(err)
	}
	live := func(pid int, start string) bool { return pid == 100 && start == "live" }
	lease := model.Lease{CheckoutID: "co_1", JobID: "j1", OwnerPID: 100, OwnerProcStart: "live", ExpiresAt: now.Add(time.Hour)}
	if _, err := st.AcquireLease(lease, now, live); err != nil {
		t.Fatal(err)
	}
	_, err := st.AcquireLease(model.Lease{CheckoutID: "co_1", JobID: "j2", OwnerPID: 200, OwnerProcStart: "other", ExpiresAt: now.Add(time.Hour)}, now, live)
	if model.Code(err) != model.ErrWorkspaceBusy {
		t.Fatalf("err=%v", err)
	}
	// The same job may re-acquire idempotently.
	if _, err := st.AcquireLease(lease, now, live); err != nil {
		t.Fatalf("idempotent acquire: %v", err)
	}
	// A dead owner is replaced even if the wall-clock expiry is in the future.
	dead := func(int, string) bool { return false }
	if _, err := st.AcquireLease(model.Lease{CheckoutID: "co_1", JobID: "j2", OwnerPID: 200, OwnerProcStart: "other", ExpiresAt: now.Add(time.Hour)}, now, dead); err != nil {
		t.Fatalf("stale acquire: %v", err)
	}
	// j1 cannot release j2's lease.
	if err := st.ReleaseLease("co_1", "j1", now); err != nil {
		t.Fatal(err)
	}
	l, found, err := st.GetLease("co_1")
	if err != nil || !found || l.JobID != "j2" {
		t.Fatalf("lease=%+v found=%v err=%v", l, found, err)
	}
	if err := st.ReleaseLease("co_1", "j2", now); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.GetLease("co_1"); err != nil || found {
		t.Fatalf("released lease found=%v err=%v", found, err)
	}
}

func TestArtifactsAreScopedToJob(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	for _, id := range []string{"j1", "j2"} {
		if _, err := st.CreateJob(testJob(now, id), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordArtifact(model.Artifact{ID: "a1", JobID: "j1", Kind: "log", Path: "artifacts/j1/agent.log", Bytes: 10}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordArtifact(model.Artifact{ID: "a2", JobID: "j2", Kind: "log", Path: "artifacts/j2/agent.log", Bytes: 20}, now); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListArtifacts("j1")
	if err != nil || len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("artifacts=%+v err=%v", got, err)
	}
}

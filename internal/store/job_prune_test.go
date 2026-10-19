package store

import (
	"fmt"
	"testing"
	"time"

	"amgo/internal/model"
)

func oldJob(id string, state model.JobState, finished *time.Time, now time.Time) model.Job {
	return model.Job{
		ID: id, Adapter: "fake", Workspace: "/work", CheckoutID: "co_" + id,
		Prompt: "p", PromptSHA256: "h", Policy: "default",
		State: state, CreatedAt: now.Add(-40 * 24 * time.Hour), UpdatedAt: now.Add(-40 * 24 * time.Hour),
		Deadline: now.Add(-39 * 24 * time.Hour), FinishedAt: finished,
	}
}

func TestPruneJobsKeepsUnknownAndActive(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	old := now.Add(-31 * 24 * time.Hour)
	finished := now.Add(-31 * 24 * time.Hour)
	for _, j := range []model.Job{
		oldJob("j_done", model.JobCompleted, &finished, now),
		oldJob("j_unknown", model.JobUnknown, &finished, now),
		oldJob("j_active", model.JobRunning, nil, now),
	} {
		if _, err := st.CreateJob(j, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AppendJobEvent("j_done", "progress", old, map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordArtifact(model.Artifact{
		ID: "a1", JobID: "j_done", Kind: "log", Path: "artifacts/j_done/agent.log", Bytes: 10,
	}, old); err != nil {
		t.Fatal(err)
	}

	dry, err := st.PruneJobs(now, now.Add(-30*24*time.Hour), true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Jobs != 1 || dry.Events != 3 || dry.Artifacts != 1 || dry.ArtifactBytes != 10 || dry.JobIDs[0] != "j_done" {
		t.Fatalf("dry=%+v", dry)
	}
	if _, err := st.GetJob("j_done"); err != nil {
		t.Fatal("dry run removed a job")
	}

	rep, err := st.PruneJobs(now, now.Add(-30*24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Jobs != 1 {
		t.Fatalf("rep=%+v", rep)
	}
	if _, err := st.GetJob("j_done"); err == nil {
		t.Fatal("eligible job survived")
	}
	if evs, err := st.ListJobEvents("j_done", 0, 0); err != nil || len(evs) != 0 {
		t.Fatalf("events=%+v err=%v", evs, err)
	}
	if arts, err := st.ListArtifacts("j_done"); err != nil || len(arts) != 0 {
		t.Fatalf("artifacts=%+v err=%v", arts, err)
	}
	for _, id := range []string{"j_unknown", "j_active"} {
		if _, err := st.GetJob(id); err != nil {
			t.Fatalf("%s was pruned: %v", id, err)
		}
	}
}

func TestInvariantsReportJobProblems(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	running := oldJob("j_run", model.JobRunning, nil, now)
	running.Deadline = now.Add(time.Hour)
	if _, err := st.CreateJob(running, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	terminal := oldJob("j_term", model.JobCompleted, nil, now)
	if _, err := st.CreateJob(terminal, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordArtifact(model.Artifact{
		ID: "a_bad", JobID: "j_term", Kind: "log", Path: "../outside.log",
	}, now); err != nil {
		t.Fatal(err)
	}
	violations, err := st.CheckInvariants(now)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, v := range violations {
		found[v.Code] = true
	}
	for _, code := range []string{"job_active_without_lease", "job_terminal_without_finish", "artifact_outside_artifacts"} {
		if !found[code] {
			t.Fatalf("missing %s in %s", code, fmt.Sprint(violations))
		}
	}
}

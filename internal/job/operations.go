package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/process"
)

// Status is one job with its bounded event history and artifacts.
type Status struct {
	Job       model.Job        `json:"job"`
	Events    []model.JobEvent `json:"events"`
	Artifacts []model.Artifact `json:"artifacts,omitempty"`
}

func (s *Service) Status(id string) (Status, error) {
	st, err := s.open()
	if err != nil {
		return Status{}, err
	}
	defer st.Close()
	job, err := st.GetJob(id)
	if err != nil {
		return Status{}, err
	}
	events, err := st.ListJobEvents(id, 0, 1000)
	if err != nil {
		return Status{}, err
	}
	artifacts, err := st.ListArtifacts(id)
	if err != nil {
		return Status{}, err
	}
	return Status{Job: job, Events: events, Artifacts: artifacts}, nil
}

func (s *Service) List(limit int) ([]model.Job, error) {
	st, err := s.open()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.ListJobs(limit)
}

// Follow polls local state until the job is terminal or timeout elapses. It
// holds no database handle between polls and wakes no model.
func (s *Service) Follow(ctx context.Context, id string, after uint64, timeout time.Duration) (model.Job, []model.JobEvent, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var all []model.JobEvent
	for {
		st, err := s.open()
		if err != nil {
			return model.Job{}, all, err
		}
		job, jerr := st.GetJob(id)
		events, eerr := st.ListJobEvents(id, after, 1000)
		_ = st.Close()
		if jerr != nil {
			return model.Job{}, all, jerr
		}
		if eerr != nil {
			return job, all, eerr
		}
		all = append(all, events...)
		if len(events) > 0 {
			after = events[len(events)-1].Seq
		}
		if job.State.Terminal() {
			return job, all, nil
		}
		if time.Now().After(deadline) {
			return job, all, model.NewError(model.ErrDeadlineExceeded, "follow deadline exceeded")
		}
		select {
		case <-ctx.Done():
			return job, all, model.NewError(model.ErrInterrupted, "follow interrupted")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Result returns a terminal job's evidence and owned artifacts.
func (s *Service) Result(id string) (model.Job, []model.Artifact, error) {
	st, err := s.open()
	if err != nil {
		return model.Job{}, nil, err
	}
	defer st.Close()
	job, err := st.GetJob(id)
	if err != nil {
		return model.Job{}, nil, err
	}
	if !job.State.Terminal() {
		return job, nil, model.NewError(model.ErrUsage, "job is not terminal; use status or follow")
	}
	artifacts, err := st.ListArtifacts(id)
	return job, artifacts, err
}

// Cancel requests cancellation and waits for the supervisor to record a
// terminal state. It signals only the verifiably owned child process group.
func (s *Service) Cancel(ctx context.Context, id string, grace time.Duration) (model.Job, error) {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	now := s.now()
	st, err := s.open()
	if err != nil {
		return model.Job{}, err
	}
	job, err := st.GetJob(id)
	if err != nil {
		_ = st.Close()
		return model.Job{}, err
	}
	if job.State.Terminal() {
		_ = st.Close()
		return job, nil
	}
	job, err = st.UpdateJob(id, now, func(j *model.Job) error {
		if j.State.Terminal() {
			return nil
		}
		if j.CancelRequestedAt == nil {
			t := now
			j.CancelRequestedAt = &t
		}
		if j.OwnerPID == 0 && j.ChildPID == 0 && j.State == model.JobCreated {
			j.State = model.JobCancelled
			j.TerminalEvidence = "cancelled before the supervisor started"
			finished := now
			j.FinishedAt = &finished
		}
		return nil
	})
	_ = st.Close()
	if err != nil {
		return job, err
	}
	if job.State == model.JobCancelled {
		s.releaseLease(job, now)
		return job, nil
	}
	// A live supervisor owns the child and watches CancelRequestedAt; let it
	// terminate the group so the provider result is classified as cancelled.
	// Only a lost supervisor leaves the child for this process to signal.
	if job.OwnerPID == 0 || !process.Alive(job.OwnerPID, job.OwnerProcStart) {
		if job.ChildPID > 0 && process.Alive(job.ChildPID, job.ChildProcStart) {
			if err := process.TerminateGroup(job.ChildPID, grace); err != nil {
				return job, model.WrapError(model.ErrProcessLost, "cancellation could not confirm child termination", err)
			}
		}
	}
	deadline := time.Now().Add(grace + 5*time.Second)
	for time.Now().Before(deadline) {
		st, err := s.open()
		if err != nil {
			return job, err
		}
		latest, jerr := st.GetJob(id)
		_ = st.Close()
		if jerr != nil {
			return job, jerr
		}
		job = latest
		if job.State.Terminal() {
			return job, nil
		}
		if job.OwnerPID > 0 && !process.Alive(job.OwnerPID, job.OwnerProcStart) {
			if err := s.recoverJob(job); err != nil {
				return job, err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return job, model.NewError(model.ErrInterrupted, "cancel interrupted")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return job, model.NewError(model.ErrDeadlineExceeded, "cancellation was requested but the job has not finished")
}

// ContinueInput starts a new job in a stopped job's native session.
type ContinueInput struct {
	JobID             string
	Prompt            string
	Timeout           time.Duration
	Background        bool
	AdmitExperimental bool
}

func (s *Service) Continue(ctx context.Context, in ContinueInput) (model.Job, error) {
	if err := validatePrompt(in.Prompt); err != nil {
		return model.Job{}, err
	}
	now := s.now()
	st, err := s.open()
	if err != nil {
		return model.Job{}, err
	}
	orig, err := st.GetJob(in.JobID)
	if err != nil {
		_ = st.Close()
		return model.Job{}, err
	}
	if !orig.State.Terminal() {
		_ = st.Close()
		return model.Job{}, model.NewError(model.ErrUsage, "cannot continue a job that is not terminal")
	}
	if orig.NativeSessionID == "" {
		_ = st.Close()
		return model.Job{}, model.NewError(model.ErrUnsupported, "job has no native session ID to continue")
	}
	sessions, err := st.ListSessions(false)
	if err != nil {
		_ = st.Close()
		return model.Job{}, err
	}
	for _, sess := range sessions {
		if sess.Target.NativeSessionID != orig.NativeSessionID {
			continue
		}
		if sess.Target.PID > 0 && process.Alive(sess.Target.PID, sess.Target.ProcStart) {
			_ = st.Close()
			return model.Job{}, model.NewError(model.ErrPolicyRefused,
				"native session is registered as live; use send instead of continue")
		}
	}
	active, err := st.ListActiveJobs()
	if err != nil {
		_ = st.Close()
		return model.Job{}, err
	}
	for _, j := range active {
		if j.NativeSessionID == orig.NativeSessionID {
			_ = st.Close()
			return model.Job{}, model.NewError(model.ErrPolicyRefused,
				"another active job owns this native session")
		}
	}
	_ = st.Close()

	timeout := in.Timeout
	if timeout <= 0 {
		timeout = time.Until(orig.Deadline)
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
	}
	ja, err := s.jobAdapter(orig.Adapter)
	if err != nil {
		return model.Job{}, err
	}
	if _, err := ja.RunSpec(ctx, adapter.RunRequest{
		JobID: "validate", Workspace: orig.Workspace, Prompt: in.Prompt,
		Model: orig.Model, Effort: orig.Effort, Policy: orig.Policy,
		NativeSessionID: orig.NativeSessionID, Timeout: timeout,
		AdmitExperimental: in.AdmitExperimental,
	}); err != nil {
		return model.Job{}, err
	}
	job := model.Job{
		RecordVersion: 1, ID: newJobID(), Adapter: orig.Adapter,
		Workspace: orig.Workspace, CheckoutID: orig.CheckoutID, RepoID: orig.RepoID,
		Prompt: in.Prompt, PromptSHA256: hashPrompt(in.Prompt), PromptBytes: len(in.Prompt),
		Model: orig.Model, Effort: orig.Effort, Policy: orig.Policy,
		AdmitExperimental: in.AdmitExperimental,
		State:             model.JobCreated, CreatedAt: now, UpdatedAt: now,
		Deadline: now.Add(timeout), NativeSessionID: orig.NativeSessionID,
		ResumeOf: orig.ID,
	}
	job, err = s.createAndLease(job)
	if err != nil {
		return job, err
	}
	st, err = s.open()
	if err != nil {
		s.failUnstarted(job.ID, model.Code(err), err.Error())
		return job, err
	}
	_, uerr := st.UpdateJob(orig.ID, now, func(j *model.Job) error {
		j.ResumedBy = append(j.ResumedBy, job.ID)
		return nil
	})
	_ = st.Close()
	if uerr != nil {
		s.failUnstarted(job.ID, model.Code(uerr), uerr.Error())
		return job, uerr
	}
	return s.launch(ctx, job, in.Background)
}

// Recover reconciles jobs whose supervisor is gone. It never guesses success:
// a lost supervisor yields interrupted or unknown. A verification callback
// checks process identity before any signal is sent.
func (s *Service) Recover(ctx context.Context) (int, error) {
	st, err := s.open()
	if err != nil {
		return 0, err
	}
	jobs, err := st.ListActiveJobs()
	_ = st.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.State == model.JobCreated && j.OwnerPID == 0 {
			if s.now().Sub(j.UpdatedAt) > supervisorGrace {
				if err := s.recoverJob(j); err != nil {
					return n, err
				}
				n++
			}
			continue
		}
		if j.OwnerPID == 0 || process.Alive(j.OwnerPID, j.OwnerProcStart) {
			continue
		}
		if err := s.recoverJob(j); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Service) recoverJob(j model.Job) error {
	now := s.now()
	detail := "supervisor exited without terminal evidence"
	if j.ChildPID > 0 && process.Alive(j.ChildPID, j.ChildProcStart) {
		if err := process.TerminateGroup(j.ChildPID, 5*time.Second); err != nil {
			detail = "supervisor lost; owned child could not be confirmed terminated"
		} else {
			detail = "supervisor lost; owned child terminated"
		}
	}
	state := model.JobInterrupted
	if j.State == model.JobRunning && j.ChildPID == 0 {
		state = model.JobUnknown
	}
	st, err := s.open()
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = st.UpdateJob(j.ID, now, func(x *model.Job) error {
		if x.State.Terminal() {
			return nil
		}
		x.State = state
		x.TerminalEvidence = detail
		x.ErrorCode = model.ErrProcessLost
		x.ErrorDetail = detail
		x.ResultUnverified = true
		finished := now
		x.FinishedAt = &finished
		return nil
	})
	if err != nil {
		return err
	}
	return st.ReleaseLease(j.CheckoutID, j.ID, now)
}

func newJobID() string { return ids.New("j") }

func hashPrompt(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

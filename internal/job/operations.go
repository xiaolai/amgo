package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/process"
	"amgo/internal/store"
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
	events, err := st.ListJobEvents(id, 0, 0)
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
			// Do not lose a tail page when the event count is a multiple of the
			// page size. A terminal job cannot grow new events, so drain until a
			// short page.
			for len(events) == 1000 {
				st, err := s.open()
				if err != nil {
					return job, all, err
				}
				events, err = st.ListJobEvents(id, after, 1000)
				_ = st.Close()
				if err != nil {
					return job, all, err
				}
				all = append(all, events...)
				if len(events) > 0 {
					after = events[len(events)-1].Seq
				}
			}
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

// terminateGroup is a test seam; production always signals through the
// process package.
var terminateGroup = process.TerminateGroup

// liveRegisteredSession reports whether a registered target makes continuation
// unsafe. A PID-zero registration (Codex loaded thread, OpenCode server) cannot
// prove the session inactive; only a positive PID that is provably dead allows
// continuation past a registration.
func liveRegisteredSession(adapterName, nativeID string, sessions []model.Session) bool {
	for _, sess := range sessions {
		if sess.Target.Adapter != adapterName || sess.Target.NativeSessionID != nativeID {
			continue
		}
		if sess.Target.PID <= 0 || process.Alive(sess.Target.PID, sess.Target.ProcStart) {
			return true
		}
	}
	return false
}

// CancelInput is one explicit operator cancellation request.
type CancelInput struct {
	JobID string
	Grace time.Duration
	// AcceptUnconfirmed releases a workspace whose owned execution could not be
	// confirmed stopped. Without it, the lease stays held.
	AcceptUnconfirmed bool
}

// Cancel requests cancellation and waits for the supervisor to record a
// terminal state. It signals only the verifiably owned child process group.
func (s *Service) Cancel(ctx context.Context, in CancelInput) (model.Job, error) {
	grace := in.Grace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	now := s.now()
	st, err := s.open()
	if err != nil {
		return model.Job{}, err
	}
	job, err := st.GetJob(in.JobID)
	if err != nil {
		_ = st.Close()
		return model.Job{}, err
	}
	if job.State.Terminal() {
		_ = st.Close()
		return s.terminalCancel(job, in)
	}
	job, err = st.UpdateJob(in.JobID, now, func(j *model.Job) error {
		if j.State.Terminal() {
			return nil
		}
		if j.CancelRequestedAt == nil {
			t := now
			j.CancelRequestedAt = &t
		}
		if j.OwnerPID == 0 && j.ChildPID == 0 && j.State == model.JobCreated && !j.ProviderStartAttempted {
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
		if err := s.releaseLease(job, now); err != nil {
			return job, err
		}
		return job, nil
	}
	if job.State.Terminal() {
		return s.terminalCancel(job, in)
	}
	// A live supervisor owns the child and watches CancelRequestedAt; let it
	// terminate the group so the provider result is classified as cancelled.
	// Only a lost supervisor leaves the child for this process to signal.
	if job.OwnerPID == 0 || !process.Alive(job.OwnerPID, job.OwnerProcStart) {
		switch process.Check(job.ChildPID, job.ChildProcStart) {
		case process.Live:
			if err := terminateGroup(job.ChildPID, grace); err != nil {
				return s.unresolvedCancel(job, in,
					"cancellation could not confirm child termination: "+err.Error())
			}
			if process.GroupAlive(job.ChildPID) {
				return s.unresolvedCancel(job, in, "cancellation could not confirm child termination")
			}
		case process.Unknown:
			return s.unresolvedCancel(job, in,
				"child process identity cannot be verified; refusing to signal by PID alone")
		}
	}
	deadline := time.Now().Add(grace + 5*time.Second)
	for time.Now().Before(deadline) {
		st, err := s.open()
		if err != nil {
			return job, err
		}
		latest, jerr := st.GetJob(in.JobID)
		_ = st.Close()
		if jerr != nil {
			return job, jerr
		}
		job = latest
		if job.State.Terminal() {
			return s.terminalCancel(job, in)
		}
		if job.OwnerPID == 0 || !process.Alive(job.OwnerPID, job.OwnerProcStart) {
			reconciled, rerr := s.recoverJob(job)
			if rerr != nil {
				return job, rerr
			}
			if !reconciled {
				return s.unresolvedCancel(job, in,
					"supervisor is gone and owned execution could not be confirmed stopped")
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

// terminalCancel handles a job that already reached a terminal state. A held
// lease requires an explicit operator risk acceptance.
func (s *Service) terminalCancel(job model.Job, in CancelInput) (model.Job, error) {
	if !job.LeaseHeld {
		return job, nil
	}
	if !in.AcceptUnconfirmed {
		return job, model.NewError(model.ErrPolicyRefused,
			"workspace lease is held because owned execution could not be confirmed stopped; "+
				"inspect the process, then pass --accept-unconfirmed-termination")
	}
	return s.acceptUnconfirmed(job)
}

func (s *Service) unresolvedCancel(job model.Job, in CancelInput, reason string) (model.Job, error) {
	if in.AcceptUnconfirmed {
		return s.acceptUnconfirmed(job)
	}
	held, err := s.holdUnresolved(job, reason)
	if err != nil {
		return held, err
	}
	return held, model.NewError(model.ErrProcessLost, reason+"; the workspace lease is held")
}

// acceptUnconfirmed records the operator's explicit acceptance, makes the job
// terminal when it was not, and releases the held lease.
func (s *Service) acceptUnconfirmed(job model.Job) (model.Job, error) {
	now := s.now()
	st, err := s.open()
	if err != nil {
		return job, err
	}
	defer st.Close()
	job, err = st.UpdateJob(job.ID, now, func(j *model.Job) error {
		if !j.State.Terminal() {
			j.State = model.JobUnknown
			finished := now
			j.FinishedAt = &finished
		}
		j.LeaseHeld = false
		j.LeaseHoldReason = ""
		j.ResultUnverified = true
		j.ErrorCode = model.ErrProcessLost
		j.ErrorDetail = "operator accepted unconfirmed termination"
		j.TerminalEvidence = "operator released the workspace lease without confirmed process exit"
		return nil
	})
	if err != nil {
		return job, err
	}
	return st.ReleaseJobLease(job.ID, job.CheckoutID, now)
}

// holdUnresolved retains the workspace lease and records why. It never changes
// a terminal job back into a running one.
func (s *Service) holdUnresolved(job model.Job, reason string) (model.Job, error) {
	st, err := s.open()
	if err != nil {
		return job, err
	}
	defer st.Close()
	out, held, err := st.HoldJobLease(job.ID, job.CheckoutID, reason, model.ErrProcessLost, nil, s.now())
	if err != nil {
		return out, err
	}
	if !held {
		// The job finished between the snapshot and the hold; a terminal job
		// owns no unresolved lease.
		return st.GetJob(job.ID)
	}
	return out, nil
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
	if liveRegisteredSession(orig.Adapter, orig.NativeSessionID, sessions) {
		_ = st.Close()
		return model.Job{}, model.NewError(model.ErrPolicyRefused,
			"native session is registered as live; use send or leave the registration before continuing")
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
		ACPCommand: orig.ACPCommand, ACPArgs: orig.ACPArgs,
		AdmitExperimental: in.AdmitExperimental,
	}); err != nil {
		return model.Job{}, err
	}
	job := model.Job{
		RecordVersion: 1, ID: newJobID(), Adapter: orig.Adapter,
		Workspace: orig.Workspace, CheckoutID: orig.CheckoutID, RepoID: orig.RepoID,
		Prompt: in.Prompt, PromptSHA256: hashPrompt(in.Prompt), PromptBytes: len(in.Prompt),
		Model: orig.Model, Effort: orig.Effort, Policy: orig.Policy,
		ACPCommand: orig.ACPCommand, ACPArgs: append([]string(nil), orig.ACPArgs...),
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
// a lost supervisor yields interrupted, or a held lease when owned execution
// cannot be proven stopped. A verification callback checks process identity
// before any signal is sent.
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
			if s.now().Sub(j.UpdatedAt) <= supervisorGrace {
				continue
			}
		} else {
			switch process.Check(j.OwnerPID, j.OwnerProcStart) {
			case process.Live, process.Unknown:
				// A live owner is still working. An unverifiable owner is not
				// proof of death, so it owns the job until a later lookup works.
				continue
			case process.Dead:
			}
		}
		reconciled, err := s.recoverJob(j)
		if err != nil {
			return n, err
		}
		if reconciled {
			n++
		}
	}
	return n, nil
}

var errRecoverySnapshotStale = errors.New("recovery snapshot changed")

func (s *Service) recoverJob(j model.Job) (bool, error) {
	now := s.now()
	expect := store.JobHoldExpect{
		State: j.State, OwnerPID: j.OwnerPID, OwnerProcStart: j.OwnerProcStart,
		ChildPID: j.ChildPID, ChildProcStart: j.ChildProcStart,
		ProviderStartRecorded: j.ProviderStartRecorded,
		ProviderStartAttempted: j.ProviderStartAttempted, StartedAt: j.StartedAt,
	}
	if j.ChildPID == 0 {
		if j.State == model.JobCreated && j.OwnerPID == 0 {
			return s.finalizeRecovered(j, expect, "supervisor never claimed the job", now)
		}
		switch {
		case j.ProviderStartAttempted:
			return s.holdRecovered(j, expect,
				"supervisor exited after spawning but before persisting the child identity; the provider may still be running", now)
		case !j.ProviderStartRecorded:
			return s.holdRecovered(j, expect,
				"supervisor exited and no verifiable child identity exists; ownership cannot be proven", now)
		default:
			return s.finalizeRecovered(j, expect, "supervisor exited before the provider started", now)
		}
	}
	switch process.Check(j.ChildPID, j.ChildProcStart) {
	case process.Unknown:
		return s.holdRecovered(j, expect,
			"owned child identity cannot be verified; refusing to signal by PID alone", now)
	case process.Live:
		if err := terminateGroup(j.ChildPID, 5*time.Second); err != nil {
			return s.holdRecovered(j, expect,
				"owned child could not be confirmed terminated: "+err.Error(), now)
		}
		if process.GroupAlive(j.ChildPID) {
			return s.holdRecovered(j, expect,
				"owned process group is still alive after termination", now)
		}
		return s.finalizeRecovered(j, expect, "supervisor lost; owned child terminated", now)
	case process.Dead:
		return s.finalizeRecovered(j, expect, "supervisor lost; owned child is gone", now)
	}
	return false, nil
}

func (s *Service) finalizeRecovered(j model.Job, expect store.JobHoldExpect, detail string, now time.Time) (bool, error) {
	st, err := s.open()
	if err != nil {
		return false, err
	}
	defer st.Close()
	_, err = st.UpdateJob(j.ID, now, func(x *model.Job) error {
		if x.State.Terminal() || !store.JobMatchesExpect(*x, expect) {
			return errRecoverySnapshotStale
		}
		x.State = model.JobInterrupted
		x.TerminalEvidence = detail
		x.ErrorCode = model.ErrProcessLost
		x.ErrorDetail = detail
		x.ResultUnverified = true
		finished := now
		x.FinishedAt = &finished
		return nil
	})
	if errors.Is(err, errRecoverySnapshotStale) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := st.ReleaseJobLease(j.ID, j.CheckoutID, now); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) holdRecovered(j model.Job, expect store.JobHoldExpect, reason string, now time.Time) (bool, error) {
	st, err := s.open()
	if err != nil {
		return false, err
	}
	defer st.Close()
	_, held, err := st.HoldJobLease(j.ID, j.CheckoutID, reason, model.ErrProcessLost, &expect, now)
	if err != nil {
		return false, err
	}
	// A false result means the snapshot was stale or the job is terminal; the
	// next Recover pass re-reads it.
	_ = held
	return false, nil
}

func newJobID() string { return ids.New("j") }

func hashPrompt(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

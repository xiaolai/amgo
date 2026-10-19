package job

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/platform"
	"amgo/internal/runner"
)

// Supervise claims a created job, runs the provider, and persists terminal
// evidence. It is the only writer of a running job's state.
func (s *Service) Supervise(ctx context.Context, jobID string) (model.Job, error) {
	now := s.now()
	st, err := s.open()
	if err != nil {
		return model.Job{}, err
	}
	job, err := st.UpdateJob(jobID, now, func(j *model.Job) error {
		if j.State.Terminal() {
			return model.NewError(model.ErrPolicyRefused, "job is already terminal")
		}
		if j.OwnerPID != 0 {
			return model.NewError(model.ErrWorkspaceBusy, "job already has a supervisor")
		}
		j.StartedAt = &now
		if j.CancelRequestedAt != nil {
			j.State = model.JobCancelled
			j.TerminalEvidence = "cancelled before the provider started"
			finished := now
			j.FinishedAt = &finished
			return nil
		}
		j.State = model.JobStarting
		j.OwnerPID = os.Getpid()
		start, _ := platform.ProcStartUTC(os.Getpid())
		j.OwnerProcStart = start
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

	ja, err := s.jobAdapter(job.Adapter)
	if err != nil {
		return s.finalizeError(job, err)
	}
	timeout := time.Until(job.Deadline)
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	plan, err := ja.RunSpec(ctx, adapter.RunRequest{
		JobID: job.ID, Workspace: job.Workspace, Prompt: job.Prompt,
		Model: job.Model, Effort: job.Effort, Policy: job.Policy,
		NativeSessionID: job.NativeSessionID, Timeout: timeout,
		AdmitExperimental: job.AdmitExperimental,
	})
	if err != nil {
		return s.finalizeError(job, err)
	}
	if job.NativeSessionID != "" && plan.NativeSessionID != "" && job.NativeSessionID != plan.NativeSessionID {
		return s.finalizeError(job, model.NewError(model.ErrPolicyRefused,
			"adapter changed the continued native session ID"))
	}

	st, err = s.open()
	if err != nil {
		return s.finalizeError(job, err)
	}
	job, err = st.UpdateJob(jobID, s.now(), func(j *model.Job) error {
		if j.State.Terminal() {
			return model.NewError(model.ErrPolicyRefused, "job is already terminal")
		}
		j.State = model.JobRunning
		j.PolicyProvenance = plan.Spec.Provenance
		if plan.NativeSessionID != "" {
			j.NativeSessionID = plan.NativeSessionID
		}
		return nil
	})
	_ = st.Close()
	if err != nil {
		return job, err
	}

	dir := filepath.Join(config.ArtifactsDir(s.Home), job.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return s.finalizeError(job, model.WrapError(model.ErrInternal, "cannot create the job artifact directory", err))
	}
	logPath := filepath.Join(dir, "agent.log")

	cancel := make(chan struct{})
	var cancelOnce sync.Once
	stopPoll := make(chan struct{})
	go s.pollCancel(job.ID, cancel, &cancelOnce, stopPoll)
	defer close(stopPoll)

	out, execErr := runner.Execute(ctx, plan.Spec, plan.Parser, runner.Options{
		Timeout: timeout, Cancel: cancel, LogPath: logPath,
	}, runner.Hooks{
		OnStart: func(pid int, procStart string) error {
			st, err := s.open()
			if err != nil {
				return err
			}
			defer st.Close()
			_, err = st.UpdateJob(job.ID, s.now(), func(j *model.Job) error {
				j.ChildPID = pid
				j.ChildProcStart = procStart
				return nil
			})
			return err
		},
		OnEvent: func(ev runner.Event) error {
			return s.persistEvent(job.ID, ev)
		},
	})
	if execErr != nil {
		out.Result = runner.Result{Outcome: model.RunFailed, ErrorCode: model.Code(execErr), ErrorDetail: execErr.Error()}
		if out.ErrorCode == "" {
			out.ErrorCode = model.Code(execErr)
			out.ErrorDetail = execErr.Error()
		}
	}
	return s.finalize(job, out, logPath)
}

// persistEvent appends a bounded event and mirrors native identity and usage
// onto the live job record.
func (s *Service) persistEvent(jobID string, ev runner.Event) error {
	st, err := s.open()
	if err != nil {
		return err
	}
	defer st.Close()
	now := s.now()
	if _, err := st.AppendJobEvent(jobID, ev.Kind, now, ev.Payload); err != nil {
		return err
	}
	switch ev.Kind {
	case "native_session":
		_, err = st.UpdateJob(jobID, now, func(j *model.Job) error {
			if v, ok := ev.Payload["session_id"].(string); ok && v != "" {
				j.NativeSessionID = v
			}
			if v, ok := ev.Payload["turn_id"].(string); ok && v != "" {
				j.NativeTurnID = v
			}
			return nil
		})
	case "usage":
		_, err = st.UpdateJob(jobID, now, func(j *model.Job) error {
			j.Usage = usageFromPayload(ev.Payload)
			return nil
		})
	}
	return err
}

func usageFromPayload(p map[string]any) *model.Usage {
	u := &model.Usage{}
	set := false
	if v, ok := int64Value(p["input_tokens"]); ok {
		u.InputTokens = &v
		set = true
	}
	if v, ok := int64Value(p["output_tokens"]); ok {
		u.OutputTokens = &v
		set = true
	}
	if v, ok := int64Value(p["cache_read_tokens"]); ok {
		u.CacheReadTokens = &v
		set = true
	}
	if v, ok := int64Value(p["cache_write_tokens"]); ok {
		u.CacheWriteTokens = &v
		set = true
	}
	if v, ok := float64Value(p["cost_usd"]); ok {
		u.CostUSD = &v
		set = true
	}
	if v, ok := p["model"].(string); ok {
		u.Model = v
	}
	if !set && u.Model == "" {
		return nil
	}
	return u
}

func int64Value(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func float64Value(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func (s *Service) pollCancel(jobID string, cancel chan struct{}, once *sync.Once, stop <-chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			st, err := s.open()
			if err != nil {
				continue
			}
			j, err := st.GetJob(jobID)
			_ = st.Close()
			if err == nil && j.CancelRequestedAt != nil {
				once.Do(func() { close(cancel) })
				return
			}
		}
	}
}

func (s *Service) finalizeError(job model.Job, cause error) (model.Job, error) {
	out := runner.Outcome{Result: runner.Result{
		Outcome: model.RunFailed, ErrorCode: model.Code(cause), ErrorDetail: cause.Error(),
	}}
	j, ferr := s.finalize(job, out, "")
	if ferr != nil {
		return j, ferr
	}
	return j, cause
}

func (s *Service) finalize(job model.Job, out runner.Outcome, logPath string) (model.Job, error) {
	now := s.now()
	state, evidence := classify(out)
	st, err := s.open()
	if err != nil {
		return job, err
	}
	defer st.Close()

	if logPath != "" {
		if fi, statErr := os.Stat(logPath); statErr == nil {
			rel, relErr := filepath.Rel(s.Home, logPath)
			if relErr != nil {
				rel = logPath
			}
			if _, err := st.RecordArtifact(model.Artifact{
				ID: ids.New("art"), JobID: job.ID, Kind: "log", Path: rel,
				Bytes: fi.Size(), SHA256: out.LogSHA256, Truncated: out.LogTruncated,
			}, now); err != nil {
				return job, err
			}
		}
	}

	resultText, resultBytes, truncated := truncateResult(out.Result.Evidence)
	job, err = st.UpdateJob(job.ID, now, func(j *model.Job) error {
		if j.State.Terminal() {
			return nil
		}
		j.State = state
		j.TerminalEvidence = evidence
		j.ResultText = resultText
		j.ResultBytes = resultBytes
		j.ResultTruncated = truncated
		j.ResultUnverified = out.Result.ResultUnverified
		if out.Result.NativeSessionID != "" {
			j.NativeSessionID = out.Result.NativeSessionID
		}
		if out.Result.NativeTurnID != "" {
			j.NativeTurnID = out.Result.NativeTurnID
		}
		if out.Result.Usage != nil {
			j.Usage = out.Result.Usage
		}
		j.ExitCode = out.ExitCode
		j.LogPath = relOrEmpty(s.Home, logPath)
		j.LogBytes = out.LogBytes
		j.LogTruncated = out.LogTruncated
		j.ErrorCode = firstNonEmpty(out.Result.ErrorCode, out.ErrorCode)
		j.ErrorDetail = firstNonEmpty(out.Result.ErrorDetail, out.ErrorDetail)
		finished := now
		j.FinishedAt = &finished
		return nil
	})
	if err != nil {
		return job, err
	}
	if err := st.ReleaseLease(job.CheckoutID, job.ID, now); err != nil {
		return job, model.WrapError(model.ErrStoreBusy, "job finished but the workspace lease could not be released", err)
	}
	return job, nil
}

func relOrEmpty(home, path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return path
	}
	return rel
}

func (s *Service) releaseLease(job model.Job, now time.Time) {
	st, err := s.open()
	if err != nil {
		return
	}
	defer st.Close()
	_ = st.ReleaseLease(job.CheckoutID, job.ID, now)
}

// classify maps provider evidence and supervisor observations to a job state.
// Terminal provider evidence outranks a late cancellation or timeout.
func classify(out runner.Outcome) (model.JobState, string) {
	if out.Result.Outcome == model.RunCompleted {
		return model.JobCompleted, "provider reported completion"
	}
	if out.Cancelled {
		return model.JobCancelled, "operator cancelled the job"
	}
	if out.TimedOut || out.Result.ErrorCode == model.ErrDeadlineExceeded {
		return model.JobTimedOut, "job deadline exceeded"
	}
	switch out.Result.Outcome {
	case model.RunPartial:
		return model.JobPartial, firstNonEmpty(out.Result.ErrorDetail, "provider exited without verifiable completion evidence")
	case model.RunNeedsAction:
		return model.JobNeedsAction, firstNonEmpty(out.Result.ErrorDetail, "provider reported a required action or denied work")
	case model.RunFailed:
		return model.JobFailed, firstNonEmpty(out.Result.ErrorDetail, "provider run failed")
	default:
		return model.JobUnknown, "no terminal provider evidence"
	}
}

func truncateResult(s string) (string, int, bool) {
	full := len(s)
	if full <= MaxResultBytes {
		return s, full, false
	}
	cut := MaxResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], full, true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

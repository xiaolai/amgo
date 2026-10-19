// Package job owns provider processes: creation, supervision, cancellation,
// continuation, bounded logs and recovery. It never lets peer data authorize a
// run; callers pass explicit operator input.
package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/process"
	"amgo/internal/session"
	"amgo/internal/store"
)

// execCommand is a test seam; production always uses os/exec.
var execCommand = exec.Command

const (
	DefaultTimeout = 15 * time.Minute
	MaxPromptBytes = 64 << 10
	MaxResultBytes = 1 << 20
	leaseGrace     = 10 * time.Minute
	// supervisorGrace is how long a created job with no owner may wait before
	// recovery declares that its supervisor never started.
	supervisorGrace = 30 * time.Second
	storeTimeout    = 2 * time.Second
)

// StartInput is one explicit operator request to run a provider.
type StartInput struct {
	Adapter           string
	Workspace         string
	Prompt            string
	Model             string
	Effort            string
	Policy            string
	Timeout           time.Duration
	Background        bool
	AdmitExperimental bool
}

// Service owns the job lifecycle. It opens the store per operation and never
// holds it across provider I/O.
type Service struct {
	Home     string
	Adapters map[string]adapter.Adapter
	Now      func() time.Time
}

func New(home string, adapters map[string]adapter.Adapter) *Service {
	return &Service{Home: home, Adapters: adapters, Now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) open() (*store.Store, error) {
	return store.Open(config.DBPath(s.Home), storeTimeout)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func validatePrompt(p string) error {
	if strings.TrimSpace(p) == "" {
		return model.NewError(model.ErrUsage, "empty prompt")
	}
	if len(p) > MaxPromptBytes {
		return model.NewError(model.ErrUsage, "prompt exceeds 64 KiB")
	}
	if !utf8.ValidString(p) {
		return model.NewError(model.ErrUsage, "prompt is not valid UTF-8")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return model.NewError(model.ErrUsage, "prompt contains NUL")
	}
	return nil
}

func resolveWorkspace(ws string) (string, error) {
	if ws == "" {
		var err error
		ws, err = os.Getwd()
		if err != nil {
			return "", model.WrapError(model.ErrUsage, "cannot resolve the working directory", err)
		}
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", model.WrapError(model.ErrUsage, "invalid workspace", err)
	}
	if canon, err := filepath.EvalSymlinks(abs); err == nil {
		abs = canon
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", model.WrapError(model.ErrUsage, "workspace is not accessible", err)
	}
	if !fi.IsDir() {
		return "", model.NewError(model.ErrUsage, "workspace is not a directory")
	}
	return abs, nil
}

func (s *Service) jobAdapter(name string) (adapter.JobAdapter, error) {
	a, ok := s.Adapters[name]
	if !ok {
		return nil, model.NewError(model.ErrUnsupported, fmt.Sprintf("adapter %q is not registered", name))
	}
	ja, ok := a.(adapter.JobAdapter)
	if !ok {
		return nil, model.NewError(model.ErrUnsupported, fmt.Sprintf("adapter %q cannot run jobs", name))
	}
	return ja, nil
}

// Start validates the run, persists the job and its lease, and then either
// supervises it in this process or spawns one detached supervisor.
func (s *Service) Start(ctx context.Context, in StartInput) (model.Job, error) {
	if err := validatePrompt(in.Prompt); err != nil {
		return model.Job{}, err
	}
	ja, err := s.jobAdapter(in.Adapter)
	if err != nil {
		return model.Job{}, err
	}
	workspace, err := resolveWorkspace(in.Workspace)
	if err != nil {
		return model.Job{}, err
	}
	policy := in.Policy
	if policy == "" {
		policy = "default"
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// Build the provider command now so an unavailable binary or an
	// unsupported policy fails before any job or lease record is written.
	plan, err := ja.RunSpec(ctx, adapter.RunRequest{
		JobID: ids.New("j"), Workspace: workspace, Prompt: in.Prompt,
		Model: in.Model, Effort: in.Effort, Policy: policy, Timeout: timeout,
		AdmitExperimental: in.AdmitExperimental,
	})
	if err != nil {
		return model.Job{}, err
	}

	now := s.now()
	checkoutID, repoID := session.CheckoutIDs(workspace)
	sum := sha256.Sum256([]byte(in.Prompt))
	job := model.Job{
		RecordVersion: 1, ID: ids.New("j"), Adapter: in.Adapter,
		Workspace: workspace, CheckoutID: checkoutID, RepoID: repoID,
		Prompt: in.Prompt, PromptSHA256: hex.EncodeToString(sum[:]), PromptBytes: len(in.Prompt),
		Model: in.Model, Effort: in.Effort, Policy: policy,
		AdmitExperimental: in.AdmitExperimental,
		State:             model.JobCreated, CreatedAt: now, UpdatedAt: now,
		Deadline: now.Add(timeout),
	}
	if plan.NativeSessionID != "" {
		job.NativeSessionID = plan.NativeSessionID
	}

	job, lerr := s.createAndLease(job)
	if lerr != nil {
		return job, lerr
	}
	return s.launch(ctx, job, in.Background)
}

// createAndLease persists a new job and its workspace lease. On lease failure
// the job record is retained as failed evidence of the attempt.
func (s *Service) createAndLease(job model.Job) (model.Job, error) {
	now := job.CreatedAt
	st, err := s.open()
	if err != nil {
		return model.Job{}, err
	}
	defer st.Close()
	job, err = st.CreateJob(job, now)
	if err != nil {
		return model.Job{}, err
	}
	_, lerr := st.AcquireLease(model.Lease{
		CheckoutID: job.CheckoutID, JobID: job.ID, OwnerPID: 0,
		ExpiresAt: job.Deadline.Add(leaseGrace),
	}, now, process.Alive)
	if lerr != nil {
		if updated, uerr := st.UpdateJob(job.ID, now, func(j *model.Job) error {
			j.State = model.JobFailed
			j.ErrorCode = model.Code(lerr)
			j.ErrorDetail = lerr.Error()
			finished := now
			j.FinishedAt = &finished
			return nil
		}); uerr == nil {
			job = updated
		}
		return job, lerr
	}
	return job, nil
}

// launch runs the job in this process or detaches a supervisor.
func (s *Service) launch(ctx context.Context, job model.Job, background bool) (model.Job, error) {
	if background {
		if err := s.spawnSupervisor(job.ID); err != nil {
			s.failUnstarted(job.ID, model.ErrProcessLost, err.Error())
			return job, err
		}
		return job, nil
	}
	return s.Supervise(ctx, job.ID)
}

// failUnstarted marks a job that never reached a supervisor. It releases the
// lease so the workspace is not stranded.
func (s *Service) failUnstarted(jobID, code, detail string) {
	st, err := s.open()
	if err != nil {
		return
	}
	defer st.Close()
	now := s.now()
	var job model.Job
	job, err = st.UpdateJob(jobID, now, func(j *model.Job) error {
		if j.State.Terminal() {
			return nil
		}
		j.State = model.JobFailed
		j.ErrorCode = code
		j.ErrorDetail = detail
		finished := now
		j.FinishedAt = &finished
		return nil
	})
	if err == nil {
		_ = st.ReleaseLease(job.CheckoutID, job.ID, now)
	}
}

// spawnSupervisor starts `amgo __supervise <id>` in a new session and returns
// immediately. The supervisor's own output goes to its artifact directory.
func (s *Service) spawnSupervisor(jobID string) error {
	exe, err := os.Executable()
	if err != nil {
		return model.WrapError(model.ErrProcessLost, "cannot resolve the amgo executable", err)
	}
	dir := filepath.Join(config.ArtifactsDir(s.Home), jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return model.WrapError(model.ErrInternal, "cannot create the job artifact directory", err)
	}
	logf, err := os.OpenFile(filepath.Join(dir, "supervisor.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return model.WrapError(model.ErrInternal, "cannot create the supervisor log", err)
	}
	defer logf.Close()
	cmd := execCommand(exe, "--home", s.Home, "__supervise", jobID)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	process.ConfigureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return model.WrapError(model.ErrProcessLost, "cannot start the job supervisor", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

package model

import "time"

// JobState is the amgo-observed lifecycle of an owned provider process. It is
// never inferred from a supervisor's exit alone: a dead supervisor leaves the
// job interrupted or unknown until its child is reconciled.
type JobState string

const (
	JobCreated     JobState = "created"
	JobStarting    JobState = "starting"
	JobRunning     JobState = "running"
	JobCompleted   JobState = "completed"
	JobPartial     JobState = "partial"
	JobNeedsAction JobState = "needs_action"
	JobFailed      JobState = "failed"
	JobTimedOut    JobState = "timed_out"
	JobCancelled   JobState = "cancelled"
	JobInterrupted JobState = "interrupted"
	JobUnknown     JobState = "unknown"
)

// Terminal reports whether no further provider action belongs to this job.
func (s JobState) Terminal() bool {
	switch s {
	case JobCompleted, JobPartial, JobNeedsAction, JobFailed, JobTimedOut,
		JobCancelled, JobInterrupted, JobUnknown:
		return true
	}
	return false
}

// Job is one owned provider process. Continuation creates a new job linked by
// ResumeOf; a terminal job record is never mutated back into a running one.
type Job struct {
	RecordVersion int    `json:"record_version"`
	ID            string `json:"id"`
	Adapter       string `json:"adapter"`
	Workspace     string `json:"workspace"`
	CheckoutID    string `json:"checkout_id,omitempty"`
	RepoID        string `json:"repo_id,omitempty"`

	Prompt            string   `json:"prompt"`
	PromptSHA256      string   `json:"prompt_sha256"`
	PromptBytes       int      `json:"prompt_bytes"`
	Model             string   `json:"model,omitempty"`
	Effort            string   `json:"effort,omitempty"`
	Policy            string   `json:"policy"`
	ACPCommand        string   `json:"acp_command,omitempty"`
	ACPArgs           []string `json:"acp_args,omitempty"`
	AdmitExperimental bool     `json:"admit_experimental,omitempty"`
	// PolicyProvenance records the exact native restrictions requested, never
	// a claim that they are an OS sandbox the provider does not supply.
	PolicyProvenance string `json:"policy_provenance,omitempty"`

	State            JobState `json:"state"`
	TerminalEvidence string   `json:"terminal_evidence,omitempty"`
	ResultText       string   `json:"result_text,omitempty"`
	ResultBytes      int      `json:"result_bytes,omitempty"`
	ResultTruncated  bool     `json:"result_truncated,omitempty"`
	ResultUnverified bool     `json:"result_unverified,omitempty"`

	NativeSessionID string   `json:"native_session_id,omitempty"`
	NativeTurnID    string   `json:"native_turn_id,omitempty"`
	ResumeOf        string   `json:"resume_of,omitempty"`
	ResumedBy       []string `json:"resumed_by,omitempty"`

	OwnerPID       int    `json:"owner_pid,omitempty"`
	OwnerProcStart string `json:"owner_proc_start,omitempty"`
	ChildPID       int    `json:"child_pid,omitempty"`
	ChildProcStart string `json:"child_proc_start,omitempty"`
	// ProviderStartRecorded marks a supervisor that claimed this job under the
	// current recovery protocol. Without it, a legacy starting/running record
	// with no child identity cannot be assumed never to have spawned.
	ProviderStartRecorded bool `json:"provider_start_recorded,omitempty"`
	// ProviderStartAttempted is persisted immediately before cmd.Start. It
	// distinguishes a supervisor that died before spawning from one that may
	// have spawned an untracked provider.
	ProviderStartAttempted bool   `json:"provider_start_attempted,omitempty"`
	ExitCode               *int   `json:"exit_code,omitempty"`
	ErrorCode              string `json:"error_code,omitempty"`
	ErrorDetail            string `json:"error_detail,omitempty"`
	// LeaseHeld records that the workspace lease was deliberately retained
	// because owned execution could not be confirmed stopped.
	LeaseHeld       bool   `json:"lease_held,omitempty"`
	LeaseHoldReason string `json:"lease_hold_reason,omitempty"`

	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Deadline          time.Time  `json:"deadline"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`

	LogPath      string `json:"log_path,omitempty"`
	LogBytes     int64  `json:"log_bytes"`
	LogTruncated bool   `json:"log_truncated,omitempty"`

	Usage *Usage `json:"usage,omitempty"`
}

// Usage normalizes provider accounting. A nil field means the provider did not
// report that value; zero is a real reported zero.
type Usage struct {
	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	CacheReadTokens  *int64   `json:"cache_read_tokens"`
	CacheWriteTokens *int64   `json:"cache_write_tokens"`
	CostUSD          *float64 `json:"cost_usd"`
	Model            string   `json:"model,omitempty"`
}

// JobEvent is an append-only progress record. Raw provider output lives in the
// bounded job log; events carry state transitions, native IDs, usage and
// bounded progress summaries.
type JobEvent struct {
	Seq     uint64         `json:"seq"`
	JobID   string         `json:"job_id"`
	Kind    string         `json:"kind"`
	At      time.Time      `json:"at"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Lease coordinates amgo writers for one checkout. It cannot exclude tools
// outside amgo.
type Lease struct {
	RecordVersion  int       `json:"record_version"`
	CheckoutID     string    `json:"checkout_id"`
	JobID          string    `json:"job_id"`
	OwnerPID       int       `json:"owner_pid"`
	OwnerProcStart string    `json:"owner_proc_start"`
	AcquiredAt     time.Time `json:"acquired_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	// Held marks an unresolved lease that cannot expire into another writer's
	// hands. Only the owning job, or an operator accepting the risk, releases it.
	Held       bool   `json:"held,omitempty"`
	HoldReason string `json:"hold_reason,omitempty"`
}

// Artifact is an owned, bounded file under the state root.
type Artifact struct {
	RecordVersion int       `json:"record_version"`
	ID            string    `json:"id"`
	JobID         string    `json:"job_id"`
	Kind          string    `json:"kind"`
	Path          string    `json:"path"`
	Bytes         int64     `json:"bytes"`
	SHA256        string    `json:"sha256,omitempty"`
	Truncated     bool      `json:"truncated,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// JobRun states used by adapters when reporting terminal evidence.
const (
	RunCompleted   = "completed"
	RunPartial     = "partial"
	RunNeedsAction = "needs_action"
	RunFailed      = "failed"
)

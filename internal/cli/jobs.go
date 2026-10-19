package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"amgo/internal/config"
	"amgo/internal/contract"
	"amgo/internal/job"
	"amgo/internal/model"
	"amgo/internal/render"
)

func parseDeadline(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, model.NewError(model.ErrUsage, "deadline must be a positive duration such as 15m")
	}
	return d, nil
}

func (a *App) cmdRun(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	cwd := fs.String("cwd", "", "workspace directory")
	promptFile := fs.String("prompt-file", "", "read the prompt from a file")
	text := fs.String("text", "", "inline prompt")
	useStdin := fs.Bool("stdin", false, "read the prompt from stdin")
	modelName := fs.String("model", "", "provider model")
	effort := fs.String("effort", "", "provider reasoning effort")
	policy := fs.String("policy", "default", "execution policy: default or review")
	deadline := fs.String("deadline", "", "job deadline (default 15m)")
	background := fs.Bool("background", false, "start a detached supervisor and return")
	pos, err := parseCommand(fs, args, map[string]bool{
		"cwd": true, "prompt-file": true, "text": true, "model": true,
		"effort": true, "policy": true, "deadline": true,
	})
	if err != nil {
		return a.fail("run", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("run", model.NewError(model.ErrUsage,
			"usage: amgo run <adapter> --cwd <dir> [--prompt-file <file>|--stdin|--text <text>] [--background]"), nil)
	}
	src := ""
	if *useStdin {
		src = "stdin"
	}
	prompt, err := a.readBodyLabeled(ctx, src, *promptFile, *text, "--prompt-file")
	if err != nil {
		return a.fail("run", err, nil)
	}
	timeout, derr := parseDeadline(*deadline, a.jobDeadline())
	if derr != nil {
		return a.fail("run", derr, nil)
	}
	if err := a.recoverJobs(ctx); err != nil {
		return a.fail("run", err, nil)
	}
	j, err := a.JobSvc.Start(ctx, job.StartInput{
		Adapter: pos[0], Workspace: *cwd, Prompt: prompt,
		Model: *modelName, Effort: *effort, Policy: *policy,
		Timeout: timeout, Background: *background,
	})
	if err != nil {
		return a.failJob("run", err, j)
	}
	return a.finishJob("run", j, jobData(j), func() {
		fmt.Fprintf(a.Out, "job %s %s adapter=%s", j.ID, j.State, j.Adapter)
		if j.NativeSessionID != "" {
			fmt.Fprintf(a.Out, " native=%s", render.Safe(j.NativeSessionID))
		}
		if j.ExitCode != nil {
			fmt.Fprintf(a.Out, " exit=%d", *j.ExitCode)
		}
		fmt.Fprintln(a.Out)
	})
}

func (a *App) cmdJobs(args []string) int {
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	limit := fs.Int("limit", 20, "maximum jobs")
	pos, err := parseCommand(fs, args, map[string]bool{"limit": true})
	if err != nil {
		return a.fail("jobs", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 0 {
		return a.fail("jobs", model.NewError(model.ErrUsage, "usage: amgo jobs [--limit N]"), nil)
	}
	if *limit <= 0 || *limit > 1000 {
		return a.fail("jobs", model.NewError(model.ErrUsage, "limit must be between 1 and 1000"), nil)
	}
	jobs, err := a.JobSvc.List(*limit)
	if err != nil {
		return a.fail("jobs", err, nil)
	}
	views := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, jobData(j))
	}
	return a.ok("jobs", map[string]any{"jobs": views}, func() {
		for _, j := range jobs {
			fmt.Fprintf(a.Out, "%s %-12s %-9s %s\n", j.ID, j.State, j.Adapter, render.Safe(j.Workspace))
		}
	})
}

func (a *App) cmdStatus(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	pos, err := parseCommand(fs, args, nil)
	if err != nil {
		return a.fail("status", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("status", model.NewError(model.ErrUsage, "usage: amgo status <job-id>"), nil)
	}
	if err := a.recoverJobs(ctx); err != nil {
		return a.fail("status", err, nil)
	}
	st, err := a.JobSvc.Status(pos[0])
	if err != nil {
		return a.fail("status", err, nil)
	}
	data := jobData(st.Job)
	data["events"] = st.Events
	data["artifacts"] = st.Artifacts
	return a.ok("status", data, func() {
		a.printJob(st.Job)
		for _, ev := range st.Events {
			fmt.Fprintln(a.Out, eventLine(ev))
		}
	})
}

func (a *App) cmdFollow(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	after := fs.Uint64("after", 0, "last event sequence already seen")
	pos, err := parseCommand(fs, args, map[string]bool{"after": true})
	if err != nil {
		return a.fail("follow", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("follow", model.NewError(model.ErrUsage, "usage: amgo follow <job-id> [--after N] [--timeout 60s]"), nil)
	}
	if err := a.recoverJobs(ctx); err != nil {
		return a.fail("follow", err, nil)
	}
	j, events, err := a.JobSvc.Follow(ctx, pos[0], *after, a.Timeout)
	if err != nil {
		return a.fail("follow", err, jobData(j))
	}
	data := jobData(j)
	data["events"] = events
	return a.ok("follow", data, func() {
		for _, ev := range events {
			fmt.Fprintln(a.Out, eventLine(ev))
		}
	})
}

func (a *App) cmdResult(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("result", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	pos, err := parseCommand(fs, args, nil)
	if err != nil {
		return a.fail("result", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("result", model.NewError(model.ErrUsage, "usage: amgo result <job-id>"), nil)
	}
	if err := a.recoverJobs(ctx); err != nil {
		return a.fail("result", err, nil)
	}
	j, artifacts, err := a.JobSvc.Result(pos[0])
	if err != nil {
		return a.failJob("result", err, j)
	}
	data := jobData(j)
	data["artifacts"] = artifacts
	return a.finishJob("result", j, data, func() {
		a.printJob(j)
		if j.ResultText != "" {
			fmt.Fprintln(a.Out, render.Safe(j.ResultText))
		}
	})
}

func (a *App) cmdCancel(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	grace := fs.Duration("grace", 5*time.Second, "time to wait for confirmed termination")
	pos, err := parseCommand(fs, args, map[string]bool{"grace": true})
	if err != nil {
		return a.fail("cancel", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("cancel", model.NewError(model.ErrUsage, "usage: amgo cancel <job-id>"), nil)
	}
	if err := a.recoverJobs(ctx); err != nil {
		return a.fail("cancel", err, nil)
	}
	j, err := a.JobSvc.Cancel(ctx, pos[0], *grace)
	if err != nil {
		return a.failJob("cancel", err, j)
	}
	return a.ok("cancel", jobData(j), func() {
		fmt.Fprintf(a.Out, "job %s %s\n", j.ID, j.State)
	})
}

func (a *App) cmdContinue(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("continue", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	promptFile := fs.String("prompt-file", "", "read the follow-up prompt from a file")
	text := fs.String("text", "", "inline follow-up prompt")
	useStdin := fs.Bool("stdin", false, "read the follow-up prompt from stdin")
	deadline := fs.String("deadline", "", "job deadline (default: the original deadline or 15m)")
	background := fs.Bool("background", false, "start a detached supervisor and return")
	pos, err := parseCommand(fs, args, map[string]bool{
		"prompt-file": true, "text": true, "deadline": true,
	})
	if err != nil {
		return a.fail("continue", model.WrapError(model.ErrUsage, "invalid flags", err), nil)
	}
	if len(pos) != 1 {
		return a.fail("continue", model.NewError(model.ErrUsage,
			"usage: amgo continue <job-id> [--prompt-file <file>|--stdin|--text <text>] [--background]"), nil)
	}
	if err := a.recoverJobs(ctx); err != nil {
		return a.fail("continue", err, nil)
	}
	src := ""
	if *useStdin {
		src = "stdin"
	}
	prompt, err := a.readBodyLabeled(ctx, src, *promptFile, *text, "--prompt-file")
	if err != nil {
		return a.fail("continue", err, nil)
	}
	var timeout time.Duration
	if *deadline != "" {
		timeout, err = parseDeadline(*deadline, a.jobDeadline())
		if err != nil {
			return a.fail("continue", err, nil)
		}
	} else if a.TimeoutSet {
		timeout = a.Timeout
	}
	j, err := a.JobSvc.Continue(ctx, job.ContinueInput{
		JobID: pos[0], Prompt: prompt, Timeout: timeout, Background: *background,
	})
	if err != nil {
		return a.failJob("continue", err, j)
	}
	return a.finishJob("continue", j, jobData(j), func() {
		fmt.Fprintf(a.Out, "job %s %s resume_of=%s\n", j.ID, j.State, j.ResumeOf)
	})
}

func (a *App) cmdSupervise(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("__supervise", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	pos, err := parseCommand(fs, args, nil)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(a.Err, "supervisor: one job id is required")
		return 2
	}
	j, err := a.JobSvc.Supervise(ctx, pos[0])
	if err != nil {
		fmt.Fprintln(a.Err, "supervisor:", err)
		return 1
	}
	fmt.Fprintf(a.Out, "job %s %s\n", j.ID, j.State)
	return 0
}

// jobDeadline uses an explicitly set global --timeout as the job deadline;
// otherwise jobs keep their own longer default.
func (a *App) jobDeadline() time.Duration {
	if a.TimeoutSet && a.Timeout > 0 {
		return a.Timeout
	}
	return job.DefaultTimeout
}

func (a *App) recoverJobs(ctx context.Context) error {
	_, err := a.JobSvc.Recover(ctx)
	return err
}

func (a *App) failJob(op string, err error, j model.Job) int {
	var data any
	if j.ID != "" {
		data = jobData(j)
	}
	return a.fail(op, err, data)
}

func (a *App) finishJob(op string, j model.Job, data any, human func()) int {
	if a.JSON {
		if err := contract.WriteJSON(a.Out, contract.OK(op, data)); err != nil {
			fmt.Fprintln(a.Err, "error: writing JSON output:", err)
			return 1
		}
		return jobExit(j.State)
	}
	if human != nil {
		human()
	}
	return jobExit(j.State)
}

func jobExit(s model.JobState) int {
	switch s {
	case model.JobCompleted:
		return 0
	case model.JobTimedOut:
		return 9
	case model.JobCancelled, model.JobInterrupted:
		return 130
	case model.JobPartial, model.JobNeedsAction, model.JobFailed, model.JobUnknown:
		return 8
	default:
		return 0
	}
}

func jobData(j model.Job) map[string]any {
	data := map[string]any{
		"job_id":            j.ID,
		"adapter":           j.Adapter,
		"state":             string(j.State),
		"workspace":         j.Workspace,
		"policy":            j.Policy,
		"policy_provenance": j.PolicyProvenance,
		"native_session_id": j.NativeSessionID,
		"native_turn_id":    j.NativeTurnID,
		"resume_of":         j.ResumeOf,
		"resumed_by":        j.ResumedBy,
		"exit_code":         j.ExitCode,
		"error_code":        j.ErrorCode,
		"error_detail":      j.ErrorDetail,
		"result":            j.ResultText,
		"result_truncated":  j.ResultTruncated,
		"result_unverified": j.ResultUnverified,
		"evidence":          j.TerminalEvidence,
		"usage":             j.Usage,
		"log_path":          j.LogPath,
		"log_bytes":         j.LogBytes,
		"log_truncated":     j.LogTruncated,
		"created_at":        j.CreatedAt,
		"deadline":          j.Deadline,
		"started_at":        j.StartedAt,
		"finished_at":       j.FinishedAt,
	}
	return data
}

func (a *App) printJob(j model.Job) {
	fmt.Fprintf(a.Out, "job %s %s adapter=%s", j.ID, j.State, j.Adapter)
	if j.ExitCode != nil {
		fmt.Fprintf(a.Out, " exit=%d", *j.ExitCode)
	}
	if j.NativeSessionID != "" {
		fmt.Fprintf(a.Out, " native=%s", render.Safe(j.NativeSessionID))
	}
	fmt.Fprintln(a.Out)
	if j.ErrorCode != "" {
		fmt.Fprintf(a.Out, "error: %s: %s\n", j.ErrorCode, render.Safe(j.ErrorDetail))
	}
	if j.TerminalEvidence != "" {
		fmt.Fprintf(a.Out, "evidence: %s\n", render.Safe(j.TerminalEvidence))
	}
	if j.LogPath != "" {
		fmt.Fprintf(a.Out, "log: %s (%d bytes", render.Safe(j.LogPath), j.LogBytes)
		if j.LogTruncated {
			fmt.Fprint(a.Out, ", truncated")
		}
		fmt.Fprintln(a.Out, ")")
	}
}

func eventLine(ev model.JobEvent) string {
	parts := []string{strconv.FormatUint(ev.Seq, 10), ev.Kind}
	keys := make([]string, 0, len(ev.Payload))
	for k := range ev.Payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+render.Safe(fmt.Sprint(ev.Payload[k])))
	}
	return strings.Join(parts, " ")
}

// removeJobArtifactDirs deletes only directories named by amgo job IDs under
// the owned artifacts root.
func removeJobArtifactDirs(home string, jobIDs []string) (int, error) {
	root := filepath.Clean(config.ArtifactsDir(home))
	n := 0
	for _, id := range jobIDs {
		if !strings.HasPrefix(id, "j_") || strings.ContainsAny(id, `/\`) {
			return n, model.NewError(model.ErrInternal, "refusing to remove an artifact path for an invalid job id")
		}
		dir := filepath.Join(root, id)
		if filepath.Dir(dir) != root {
			return n, model.NewError(model.ErrInternal, "refusing to remove an artifact path outside the owned root")
		}
		if err := os.RemoveAll(dir); err != nil {
			return n, model.WrapError(model.ErrInternal, "cannot remove the job artifact directory", err)
		}
		n++
	}
	return n, nil
}

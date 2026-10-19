// Package runner executes one owned provider process with bounded output and
// a provider-specific JSONL parser. It never interprets provider output as
// operator authority; it records evidence.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"amgo/internal/model"
	"amgo/internal/platform"
	"amgo/internal/process"
)

// MaxLineBytes bounds one provider event. An adapter cannot raise this without
// a reviewed fixture.
const MaxLineBytes = 4 << 20

// DefaultMaxLogBytes is the per-job hard ceiling. Reaching it truncates the log
// with explicit metadata; it never truncates the parser's view.
const DefaultMaxLogBytes = 100 << 20

// Spec is a provider command. Prompt is written to stdin so it never appears in
// the process list.
type Spec struct {
	Path       string
	Args       []string
	Dir        string
	Env        []string
	Prompt     []byte
	Provenance string
}

// Event is a bounded progress record from the parser.
type Event struct {
	Kind    string
	Payload map[string]any
}

// Parser consumes provider JSONL. Line must tolerate unknown object types and
// return an error only for malformed input or a contract violation.
type Parser interface {
	Line(line []byte) ([]Event, error)
	Result() Result
}

// Result is the provider's terminal evidence, before supervisor classification.
type Result struct {
	Outcome          string // completed | partial | needs_action | failed
	NativeSessionID  string
	NativeTurnID     string
	Evidence         string
	ResultUnverified bool
	Usage            *model.Usage
	ErrorCode        string
	ErrorDetail      string
}

// Hooks persist ownership and progress. An OnStart error aborts the run; an
// OnEvent error does too, because silently losing progress is worse than a
// loudly interrupted job.
type Hooks struct {
	OnStart func(pid int, procStart string) error
	OnEvent func(Event) error
}

// Options bound one run.
type Options struct {
	Timeout     time.Duration
	Cancel      <-chan struct{}
	LogPath     string
	MaxLogBytes int64
}

// Outcome combines parser evidence with supervisor observations.
type Outcome struct {
	Result       Result
	LogBytes     int64
	LogSHA256    string
	LogTruncated bool
	Cancelled    bool
	TimedOut     bool
	ExitCode     *int
	ErrorCode    string
	ErrorDetail  string
}

// Execute starts spec in its own process group, parses stdout as JSONL, writes
// bounded raw output to LogPath, and confirms termination of the whole group.
func Execute(ctx context.Context, spec Spec, parser Parser, opts Options, hooks Hooks) (Outcome, error) {
	if spec.Path == "" {
		return Outcome{Result: Result{Outcome: model.RunFailed, ErrorCode: model.ErrUnsupported, ErrorDetail: "provider executable is unavailable"}},
			model.NewError(model.ErrUnsupported, "provider executable is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return Outcome{Result: Result{Outcome: model.RunFailed, ErrorCode: model.ErrInterrupted, ErrorDetail: "run cancelled before start"}},
			model.WrapError(model.ErrInterrupted, "run cancelled before start", err)
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = DefaultMaxLogBytes
	}
	lw, err := newLogWriter(opts.LogPath, opts.MaxLogBytes)
	if err != nil {
		return Outcome{}, model.WrapError(model.ErrInternal, "cannot create job log", err)
	}
	defer lw.Close()

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdin = bytes.NewReader(spec.Prompt)
	process.Configure(cmd)
	outR, outW, err := os.Pipe()
	if err != nil {
		return Outcome{}, model.WrapError(model.ErrInternal, "cannot capture provider stdout", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return Outcome{}, model.WrapError(model.ErrInternal, "cannot capture provider stderr", err)
	}
	cmd.Stdout = outW
	cmd.Stderr = errW
	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return Outcome{Result: Result{Outcome: model.RunFailed, ErrorCode: model.ErrTargetUnavailable, ErrorDetail: err.Error()}},
			model.WrapError(model.ErrTargetUnavailable, "cannot start provider", err)
	}
	_ = outW.Close()
	_ = errW.Close()
	pid := cmd.Process.Pid
	procStart, _ := platform.ProcStartUTC(pid)
	if hooks.OnStart != nil {
		if err := hooks.OnStart(pid, procStart); err != nil {
			_ = process.TerminateGroup(pid, 5*time.Second)
			_ = cmd.Wait()
			_ = outR.Close()
			_ = errR.Close()
			return Outcome{}, err
		}
	}

	var parseErr error
	var parseCode string
	var parseMu sync.Mutex
	setParseErr := func(code string, err error) {
		parseMu.Lock()
		if parseErr == nil {
			parseCode = code
			parseErr = err
		}
		parseMu.Unlock()
	}
	var hookErr error
	var hookMu sync.Mutex
	eventErr := make(chan error, 1)
	emit := func(events []Event) bool {
		if hooks.OnEvent == nil {
			return true
		}
		for _, ev := range events {
			if err := hooks.OnEvent(ev); err != nil {
				select {
				case eventErr <- err:
				default:
				}
				return false
			}
		}
		return true
	}

	var scan sync.WaitGroup
	scan.Add(2)
	go func() {
		defer scan.Done()
		defer outR.Close()
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			lw.Out(line)
			events, err := parser.Line(line)
			if err != nil {
				setParseErr(model.ErrEventParse, model.WrapError(model.ErrEventParse, "provider event rejected", err))
				go process.TerminateGroup(pid, 2*time.Second)
				return
			}
			if !emit(events) {
				go process.TerminateGroup(pid, 2*time.Second)
				return
			}
		}
		if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
			setParseErr(model.ErrEventTooLarge, model.WrapError(model.ErrEventTooLarge, "provider event stream failed", err))
			go process.TerminateGroup(pid, 2*time.Second)
		}
	}()
	go func() {
		defer scan.Done()
		defer errR.Close()
		sc := bufio.NewScanner(errR)
		sc.Buffer(make([]byte, 0, 16<<10), MaxLineBytes)
		for sc.Scan() {
			lw.Err(sc.Bytes())
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var timedOut, cancelled bool
	var timer <-chan time.Time
	if opts.Timeout > 0 {
		t := time.NewTimer(opts.Timeout)
		defer t.Stop()
		timer = t.C
	}
	var wait error
loop:
	for {
		select {
		case wait = <-waitErr:
			break loop
		case <-timer:
			timedOut = true
			_ = process.TerminateGroup(pid, 5*time.Second)
			wait = <-waitErr
			break loop
		case <-opts.Cancel:
			cancelled = true
			_ = process.TerminateGroup(pid, 5*time.Second)
			wait = <-waitErr
			break loop
		case err := <-eventErr:
			hookMu.Lock()
			if hookErr == nil {
				hookErr = err
			}
			hookMu.Unlock()
			_ = process.TerminateGroup(pid, 5*time.Second)
			wait = <-waitErr
			break loop
		case <-ctx.Done():
			cancelled = true
			_ = process.TerminateGroup(pid, 5*time.Second)
			wait = <-waitErr
			break loop
		}
	}
	scanDone := make(chan struct{})
	go func() { scan.Wait(); close(scanDone) }()
	select {
	case <-scanDone:
	case <-time.After(3 * time.Second):
		// A detached grandchild can hold the pipe open after the leader exits.
		// The process group was terminated above; close the readers to unblock
		// the scanners rather than waiting forever.
		_ = outR.Close()
		_ = errR.Close()
		<-scanDone
	}
	select {
	case err := <-eventErr:
		hookMu.Lock()
		if hookErr == nil {
			hookErr = err
		}
		hookMu.Unlock()
	default:
	}
	lw.Close()

	out := Outcome{
		Result:       parser.Result(),
		LogBytes:     lw.Bytes(),
		LogSHA256:    lw.SHA256(),
		LogTruncated: lw.Truncated(),
		Cancelled:    cancelled,
		TimedOut:     timedOut,
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		out.ExitCode = &code
	}

	parseMu.Lock()
	pe, pc := parseErr, parseCode
	parseMu.Unlock()
	hookMu.Lock()
	he := hookErr
	hookMu.Unlock()
	switch {
	case he != nil:
		code := model.Code(he)
		out.Result.Outcome = model.RunFailed
		out.Result.ErrorCode = code
		out.Result.ErrorDetail = he.Error()
		out.ErrorCode = code
		out.ErrorDetail = he.Error()
	case pe != nil:
		out.Result.Outcome = model.RunFailed
		out.Result.ErrorCode = pc
		out.Result.ErrorDetail = pe.Error()
		out.ErrorCode = pc
		out.ErrorDetail = pe.Error()
	case cancelled:
		out.Result.Outcome = model.RunFailed
		out.Result.ErrorCode = model.ErrInterrupted
		out.Result.ErrorDetail = "cancelled by the operator"
		out.ErrorCode = model.ErrInterrupted
		out.ErrorDetail = "cancelled by the operator"
	case timedOut:
		out.Result.Outcome = model.RunFailed
		out.Result.ErrorCode = model.ErrDeadlineExceeded
		out.Result.ErrorDetail = "job deadline exceeded"
		out.ErrorCode = model.ErrDeadlineExceeded
		out.ErrorDetail = "job deadline exceeded"
	case out.Result.Outcome == "":
		code := -1
		if out.ExitCode != nil {
			code = *out.ExitCode
		}
		detail := "provider exited without a terminal result"
		if wait != nil {
			detail = wait.Error()
		}
		out.Result.Outcome = model.RunFailed
		out.Result.ErrorCode = model.ErrProcessLost
		out.Result.ErrorDetail = detail
		out.Result.ResultUnverified = true
		out.ErrorCode = model.ErrProcessLost
		out.ErrorDetail = detail
		if code == 0 {
			out.Result.Outcome = model.RunPartial
		}
	}
	if wait != nil && out.Result.Outcome == model.RunCompleted {
		// A terminal provider event is stronger evidence than the exit code,
		// but a nonzero exit is recorded and surfaced rather than hidden.
		out.ErrorDetail = wait.Error()
	}
	return out, nil
}

// logWriter caps total bytes and keeps a streaming SHA-256. It is safe for the
// two scanner goroutines.
type logWriter struct {
	mu        sync.Mutex
	f         *os.File
	remaining int64
	bytes     int64
	truncated bool
	marker    bool
	hash      hash.Hash
}

func newLogWriter(path string, max int64) (*logWriter, error) {
	w := &logWriter{remaining: max, hash: sha256.New()}
	if path == "" {
		return w, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	w.f = f
	return w, nil
}

func (w *logWriter) write(prefix string, line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.remaining <= 0 {
		w.truncated = true
		if !w.marker && w.f != nil {
			_, _ = w.f.WriteString("[amgo: log truncated at the per-job limit]\n")
			w.marker = true
		}
		return
	}
	payload := make([]byte, 0, len(prefix)+len(line)+1)
	payload = append(payload, prefix...)
	payload = append(payload, line...)
	payload = append(payload, '\n')
	if int64(len(payload)) > w.remaining {
		payload = payload[:w.remaining]
		w.truncated = true
	}
	if w.f != nil {
		if _, err := w.f.Write(payload); err != nil {
			w.truncated = true
		}
	}
	_, _ = w.hash.Write(payload)
	w.bytes += int64(len(payload))
	w.remaining -= int64(len(payload))
	if w.remaining <= 0 && !w.marker && w.f != nil {
		_, _ = w.f.WriteString("[amgo: log truncated at the per-job limit]\n")
		w.marker = true
	}
}

func (w *logWriter) Out(line []byte) { w.write("", line) }
func (w *logWriter) Err(line []byte) { w.write("stderr: ", line) }

func (w *logWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		return err
	}
	return nil
}

func (w *logWriter) Bytes() int64    { w.mu.Lock(); defer w.mu.Unlock(); return w.bytes }
func (w *logWriter) Truncated() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.truncated }
func (w *logWriter) SHA256() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return hex.EncodeToString(w.hash.Sum(nil))
}

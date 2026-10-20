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
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	Driver     Driver
	Provenance string
}

// Driver runs a protocol that needs a bidirectional conversation instead of a
// one-shot prompt. The runner still owns the process, log, deadline and
// cancellation. Next returns one bounded stdout line, already written to the
// raw log; Emit persists a job event.
type Driver interface {
	Run(ctx context.Context, io *DriverIO) (Result, error)
}

type DriverIO struct {
	In   io.Writer
	Next func() ([]byte, error)
	Emit func(Event) error
}

// lineReader logs each bounded stdout line and hands it to a driver.
type lineReader struct {
	sc *bufio.Scanner
	lw *logWriter
}

func newLineReader(r io.Reader, lw *logWriter) *lineReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	return &lineReader{sc: sc, lw: lw}
}

func (l *lineReader) Next() ([]byte, error) {
	if l.sc.Scan() {
		line := append([]byte(nil), l.sc.Bytes()...)
		line = l.lw.redact(line)
		l.lw.Out(line)
		return line, nil
	}
	if err := l.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
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

// MaxEvidenceBytes bounds the result text a parser or driver retains in memory
// and hands to the supervisor. EvidenceBytes keeps the original size visible.
const MaxEvidenceBytes = 1 << 20

// exitedProcessIdentity records a child that cmd.Wait already reaped before its
// start time could be read. It cannot match any ps start string, so the process
// is provably not the original child even if its PID is reused.
const exitedProcessIdentity = "amgo:confirmed-exit"

// Result is the provider's terminal evidence, before supervisor classification.
type Result struct {
	Outcome           string // completed | partial | needs_action | failed
	NativeSessionID   string
	NativeTurnID      string
	Evidence          string
	EvidenceBytes     int
	EvidenceTruncated bool
	ResultUnverified  bool
	Usage             *model.Usage
	ErrorCode         string
	ErrorDetail       string
}

// Hooks persist ownership and progress. An OnStart error aborts the run; an
// OnEvent error does too, because silently losing progress is worse than a
// loudly interrupted job.
type Hooks struct {
	// OnSpawn runs immediately before cmd.Start, so recovery can distinguish
	// "never spawned" from "spawned but the identity was never persisted".
	OnSpawn func() error
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
	// StrayGroup means the leader exited but group members were still live and
	// had to be terminated. The provider's own terminal evidence is not enough
	// to call the work complete.
	StrayGroup bool
	// TerminationUnconfirmed means the process group could not be proven gone.
	// Callers must not release the workspace lease in that case.
	TerminationUnconfirmed bool
	TerminationDetail      string
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
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	lw, err := newLogWriter(opts.LogPath, opts.MaxLogBytes)
	if err != nil {
		return Outcome{}, model.WrapError(model.ErrInternal, "cannot create job log", err)
	}
	lw.addSecrets(spec.Env)
	defer lw.Close()

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	var stdin io.WriteCloser
	if spec.Driver != nil {
		var serr error
		stdin, serr = cmd.StdinPipe()
		if serr != nil {
			return Outcome{}, model.WrapError(model.ErrInternal, "cannot capture provider stdin", serr)
		}
	} else {
		cmd.Stdin = bytes.NewReader(spec.Prompt)
	}
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
	if hooks.OnSpawn != nil {
		if err := hooks.OnSpawn(); err != nil {
			if stdin != nil {
				_ = stdin.Close()
			}
			_ = outR.Close()
			_ = outW.Close()
			_ = errR.Close()
			_ = errW.Close()
			return Outcome{}, err
		}
	}
	if err := cmd.Start(); err != nil {
		if stdin != nil {
			_ = stdin.Close()
		}
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
	waitErr := make(chan error, 1)
	exited := make(chan struct{})
	go func() { waitErr <- cmd.Wait(); close(exited) }()
	procStart, psErr := platform.ProcStartUTC(pid)
	for attempt := 0; psErr != nil && attempt < 5; attempt++ {
		select {
		case <-exited:
		case <-time.After(20 * time.Millisecond):
		}
		if channelClosed(exited) {
			break
		}
		procStart, psErr = platform.ProcStartUTC(pid)
	}
	if psErr != nil && channelClosed(exited) {
		procStart = exitedProcessIdentity
		psErr = nil
	}
	if psErr != nil {
		// Without a start identity, recovery cannot distinguish a live child
		// from a recycled PID. Never leave an untrackable provider running.
		termErr := process.TerminateGroup(pid, 5*time.Second)
		waitBounded(waitErr, 10*time.Second)
		if stdin != nil {
			_ = stdin.Close()
		}
		_ = outR.Close()
		_ = errR.Close()
		out := Outcome{
			Result: Result{
				Outcome: model.RunFailed, ErrorCode: model.ErrProcessLost,
				ErrorDetail: "cannot verify the provider's process identity",
			},
			ErrorCode: model.ErrProcessLost, ErrorDetail: "cannot verify the provider's process identity",
		}
		if termErr != nil {
			out.TerminationUnconfirmed = true
			out.TerminationDetail = termErr.Error()
		}
		return out, model.WrapError(model.ErrProcessLost, "cannot verify the provider's process identity", psErr)
	}
	if hooks.OnStart != nil {
		if err := hooks.OnStart(pid, procStart); err != nil {
			termErr := process.TerminateGroup(pid, 5*time.Second)
			waitBounded(waitErr, 10*time.Second)
			if stdin != nil {
				_ = stdin.Close()
			}
			_ = outR.Close()
			_ = errR.Close()
			out := Outcome{}
			if termErr != nil {
				out.TerminationUnconfirmed = true
				out.TerminationDetail = termErr.Error()
			}
			return out, err
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
	scan.Add(1)
	if spec.Driver == nil {
		scan.Add(1)
		go func() {
			defer scan.Done()
			defer outR.Close()
			sc := bufio.NewScanner(outR)
			sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
			for sc.Scan() {
				line := append([]byte(nil), sc.Bytes()...)
				line = lw.redact(line)
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
	}
	go func() {
		defer scan.Done()
		defer errR.Close()
		sc := bufio.NewScanner(errR)
		sc.Buffer(make([]byte, 0, 16<<10), MaxLineBytes)
		for sc.Scan() {
			lw.Err(sc.Bytes())
		}
	}()

	type driverResult struct {
		result Result
		err    error
	}
	var driverDone chan driverResult
	if spec.Driver != nil {
		driverDone = make(chan driverResult, 1)
		reader := newLineReader(outR, lw)
		dio := &DriverIO{
			In:   stdin,
			Next: reader.Next,
			Emit: func(ev Event) error {
				if hooks.OnEvent == nil {
					return nil
				}
				return hooks.OnEvent(ev)
			},
		}
		go func() {
			res, derr := spec.Driver.Run(runCtx, dio)
			_ = stdin.Close()
			driverDone <- driverResult{result: res, err: derr}
		}()
	}

	var timedOut, cancelled bool
	var termErr error
	terminate := func(grace time.Duration) {
		if err := process.TerminateGroup(pid, grace); err != nil && termErr == nil {
			termErr = err
		}
	}
	var timer <-chan time.Time
	if opts.Timeout > 0 {
		t := time.NewTimer(opts.Timeout)
		defer t.Stop()
		timer = t.C
	}
	var wait error
	var early *driverResult
loop:
	for {
		select {
		case wait = <-waitErr:
			break loop
		case dr := <-driverDone:
			// The driver finished before the process did. Give the agent a
			// short grace to exit after stdin closed, then terminate it.
			early = &dr
			select {
			case wait = <-waitErr:
			case <-time.After(5 * time.Second):
				terminate(5 * time.Second)
				wait = waitBounded(waitErr, 10*time.Second)
			}
			break loop
		case <-timer:
			timedOut = true
			runCancel()
			time.Sleep(100 * time.Millisecond)
			terminate(5 * time.Second)
			wait = waitBounded(waitErr, 10*time.Second)
			break loop
		case <-opts.Cancel:
			cancelled = true
			runCancel()
			time.Sleep(100 * time.Millisecond)
			terminate(5 * time.Second)
			wait = waitBounded(waitErr, 10*time.Second)
			break loop
		case err := <-eventErr:
			hookMu.Lock()
			if hookErr == nil {
				hookErr = err
			}
			hookMu.Unlock()
			terminate(5 * time.Second)
			wait = waitBounded(waitErr, 10*time.Second)
			break loop
		case <-ctx.Done():
			cancelled = true
			runCancel()
			time.Sleep(100 * time.Millisecond)
			terminate(5 * time.Second)
			wait = waitBounded(waitErr, 10*time.Second)
			break loop
		}
	}
	// A provider can exit while leaving same-group writers behind. They are
	// owned by this job: terminate them before the lease can be released.
	strayGroup := process.GroupAlive(pid)
	if strayGroup {
		terminate(5 * time.Second)
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
	var dres driverResult
	if spec.Driver != nil {
		if early != nil {
			dres = *early
		} else {
			select {
			case dres = <-driverDone:
			case <-time.After(3 * time.Second):
				_ = outR.Close()
				select {
				case dres = <-driverDone:
				case <-time.After(time.Second):
					dres.err = model.NewError(model.ErrProcessLost, "protocol driver did not stop")
				}
			}
		}
	}
	lw.Close()

	res := Result{}
	if spec.Driver == nil {
		res = parser.Result()
	} else {
		res = dres.result
	}
	out := Outcome{
		Result:       res,
		LogBytes:     lw.Bytes(),
		LogSHA256:    lw.SHA256(),
		LogTruncated: lw.Truncated(),
		Cancelled:    cancelled,
		TimedOut:     timedOut,
		StrayGroup:   strayGroup,
	}
	if termErr != nil {
		out.TerminationUnconfirmed = true
		out.TerminationDetail = termErr.Error()
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
	case termErr != nil:
		detail := "provider process group could not be confirmed terminated: " + termErr.Error()
		out.Result.Outcome = model.RunFailed
		out.Result.ErrorCode = model.ErrProcessLost
		out.Result.ErrorDetail = detail
		out.ErrorCode = model.ErrProcessLost
		out.ErrorDetail = detail
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
	case dres.err != nil:
		code := model.Code(dres.err)
		if code == model.ErrInternal {
			code = model.ErrProviderFailed
		}
		out.Result.Outcome = model.RunFailed
		out.Result.ErrorCode = code
		out.Result.ErrorDetail = dres.err.Error()
		out.ErrorCode = code
		out.ErrorDetail = dres.err.Error()
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

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// waitBounded waits for cmd.Wait, but returns an error instead of blocking
// forever when a process cannot be terminated.
func waitBounded(ch <-chan error, d time.Duration) error {
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		return fmt.Errorf("provider did not exit within %s", d)
	}
}

// logWriter caps total bytes and keeps a streaming SHA-256. The hash, byte
// count and stored file describe exactly the same bytes: the truncation marker
// is reserved inside the cap and passes through the same accounting. It is safe
// for the two scanner goroutines.
type logWriter struct {
	mu        sync.Mutex
	f         *os.File
	remaining int64 // payload budget, excluding the reserved marker
	marker    []byte
	bytes     int64
	truncated bool
	markerOut bool
	failed    bool
	hash      hash.Hash
	secrets   [][]byte
}

var logTruncationMarker = []byte("\n[amgo: log truncated at the per-job limit]\n")

// credentialEnvNames are the environment values redacted from provider output.
// The list is intentionally explicit: redacting every *_TOKEN or *_KEY would
// mangle ordinary diagnostics.
var credentialEnvNames = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CODEX_API_KEY",
	"CODEX_ACCESS_TOKEN",
	"DEEPSEEK_API_KEY",
	"OPENAI_API_KEY",
	"OPENCODE_SERVER_PASSWORD",
	"GITHUB_TOKEN",
	"GH_TOKEN",
}

func newLogWriter(path string, max int64) (*logWriter, error) {
	marker := append([]byte(nil), logTruncationMarker...)
	if int64(len(marker)) > max {
		marker = marker[:max]
	}
	w := &logWriter{remaining: max - int64(len(marker)), marker: marker, hash: sha256.New()}
	w.addSecrets(os.Environ())
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

// addSecrets records credential environment values for redaction. Call it
// before the scanner goroutines start; the resulting slice is read-only.
func (w *logWriter) addSecrets(env []string) {
	known := make(map[string]bool, len(credentialEnvNames))
	for _, name := range credentialEnvNames {
		known[name] = true
	}
	seen := make(map[string]bool)
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !known[name] {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) < 8 || seen[value] {
			continue
		}
		seen[value] = true
		w.secrets = append(w.secrets, []byte(value))
	}
	// Replace longer values first so a contained value cannot split them.
	sort.Slice(w.secrets, func(i, j int) bool { return len(w.secrets[i]) > len(w.secrets[j]) })
}

func (w *logWriter) redact(line []byte) []byte {
	for _, secret := range w.secrets {
		if bytes.Contains(line, secret) {
			line = bytes.ReplaceAll(line, secret, []byte("[redacted]"))
		}
	}
	return line
}

func (w *logWriter) write(prefix string, line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	line = w.redact(line)
	if w.failed || w.markerOut {
		w.truncated = true
		return
	}
	payload := make([]byte, 0, len(prefix)+len(line)+1)
	payload = append(payload, prefix...)
	payload = append(payload, line...)
	payload = append(payload, '\n')
	if int64(len(payload)) > w.remaining {
		payload = payload[:w.remaining]
		w.truncated = true
		w.account(payload)
		w.markTruncated()
		return
	}
	w.account(payload)
}

// account stores the bytes and updates the hash and count with exactly the
// bytes that were written. A short write or error stops further writes.
func (w *logWriter) account(payload []byte) {
	if len(payload) == 0 {
		return
	}
	stored := payload
	if w.f != nil {
		n, err := w.f.Write(payload)
		if n < 0 {
			n = 0
		}
		if n > len(payload) {
			n = len(payload)
		}
		stored = payload[:n]
		if err != nil || n != len(payload) {
			w.truncated = true
			w.failed = true
		}
	}
	_, _ = w.hash.Write(stored)
	w.bytes += int64(len(stored))
	w.remaining -= int64(len(stored))
	if w.remaining < 0 {
		w.remaining = 0
	}
}

func (w *logWriter) markTruncated() {
	if w.markerOut {
		return
	}
	w.markerOut = true
	w.truncated = true
	w.account(w.marker)
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

//go:build native

// Package native_test contains opt-in native delivery gates.
//
// They launch disposable Claude Code and Codex sessions in private config
// homes and are never run by the default test suite. Set AMGO_NATIVE=1:
//
//	AMGO_NATIVE=1 go test -tags native -timeout 20m ./tests/native -v
//
// No personal session is messaged. Every session, config home, and repository
// is created under t.TempDir and removed when the test ends.
package native_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"amgo/internal/adapter"
	"amgo/internal/adapter/claude"
	"amgo/internal/config"
	"amgo/internal/ids"
	"amgo/internal/message"
	"amgo/internal/model"
	"amgo/internal/store"
)

var (
	amgoBin  string
	repoRoot string
)

func TestMain(m *testing.M) {
	if os.Getenv("AMGO_NATIVE") != "1" {
		fmt.Fprintln(os.Stderr, "native gates skipped: set AMGO_NATIVE=1 to run them")
		os.Exit(0)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "cannot locate the test source")
		os.Exit(1)
	}
	repoRoot = filepath.Dir(filepath.Dir(filepath.Dir(file)))
	dir, err := os.MkdirTemp("", "amgo-native-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	amgoBin = filepath.Join(dir, "amgo")
	build := exec.Command("go", "build", "-trimpath", "-o", amgoBin, "./cmd/amgo")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build amgo: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	if out, err := exec.Command("pgrep", "-f", amgoBin).Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
		fmt.Fprintf(os.Stderr, "amgo process left running: %s\n", out)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// ---- CLI helpers ----

type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Operation     string          `json:"operation"`
	Outcome       string          `json:"outcome"`
	Data          json.RawMessage `json:"data"`
	Error         *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func runAmgo(t *testing.T, dir, home string, args ...string) (envelope, int) {
	t.Helper()
	full := append([]string{"--home", home, "--json"}, args...)
	cmd := exec.Command(amgoBin, full...)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run amgo %v: %v (%s)", args, err, errOut.String())
		}
		code = exit.ExitCode()
	}
	line := lastJSONLine(out.Bytes())
	if line == nil {
		t.Fatalf("amgo %v produced no JSON envelope: stdout=%q stderr=%q", args, out.String(), errOut.String())
	}
	var e envelope
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatalf("amgo %v: invalid envelope: %v: %s", args, err, line)
	}
	return e, code
}

func lastJSONLine(raw []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) > 0 && line[0] == '{' {
			return line
		}
	}
	return nil
}

func data(t *testing.T, e envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if len(e.Data) == 0 {
		return m
	}
	if err := json.Unmarshal(e.Data, &m); err != nil {
		t.Fatalf("decode data: %v (%s)", err, e.Data)
	}
	return m
}

func mustOK(t *testing.T, e envelope, code int, what string) map[string]any {
	t.Helper()
	if code != 0 || e.Outcome != "ok" {
		t.Fatalf("%s failed: exit=%d outcome=%s error=%+v", what, code, e.Outcome, e.Error)
	}
	return data(t, e)
}

func mustCode(t *testing.T, e envelope, code int, wantCode string, what string) {
	t.Helper()
	if code == 0 || e.Error == nil || e.Error.Code != wantCode {
		t.Fatalf("%s: exit=%d error=%+v want %s", what, code, e.Error, wantCode)
	}
}

func randomLabel(prefix string) string {
	return prefix + "-" + strings.ReplaceAll(ids.NewUUID(), "-", "")
}

func discover(t *testing.T, dir, home, claudeConfig, codexHome string) []map[string]any {
	t.Helper()
	args := []string{"sessions", "--discover"}
	if claudeConfig != "" {
		args = append(args, "--config-home", claudeConfig)
	}
	if codexHome != "" {
		args = append(args, "--codex-home", codexHome)
	}
	e, code := runAmgo(t, dir, home, args...)
	m := mustOK(t, e, code, "sessions --discover")
	raw, _ := m["sessions"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func pickCandidate(t *testing.T, rows []map[string]any, adapterName, nativeID string) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row["adapter"] != adapterName {
			continue
		}
		if nativeID != "" && row["native_session_id"] != nativeID {
			continue
		}
		if row["reachable"] != true {
			continue
		}
		return row
	}
	t.Fatalf("no reachable %s candidate %q in %+v", adapterName, nativeID, rows)
	return nil
}

func joinClaude(t *testing.T, dir, home, configHome, sessionID, alias string) {
	t.Helper()
	e, code := runAmgo(t, dir, home, "join", "claude", "--name", alias, "--config-home", configHome, "--session", sessionID, "--cwd", dir)
	mustOK(t, e, code, "join claude")
}

func joinCodex(t *testing.T, dir, home, codexHome, threadID, alias string) {
	t.Helper()
	e, code := runAmgo(t, dir, home, "join", "codex", "--name", alias, "--codex-home", codexHome, "--thread", threadID, "--cwd", dir)
	mustOK(t, e, code, "join codex")
}

func sendText(t *testing.T, dir, home, alias, body string, extra ...string) (envelope, int) {
	t.Helper()
	args := append([]string{"send", alias, "--text", body}, extra...)
	return runAmgo(t, dir, home, args...)
}

func waitReadAck(t *testing.T, dir, home, messageID string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		e, code := runAmgo(t, dir, home, "read", messageID)
		if code == 0 {
			m := data(t, e)
			receipts, _ := m["receipts"].([]any)
			for _, item := range receipts {
				r, _ := item.(map[string]any)
				if r["kind"] == "ack" && r["provenance"] == string(model.ProvenanceRecipient) {
					return true
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false
}

// ---- Claude disposable session ----

type claudeGate struct {
	base      string
	config    string
	work      string
	session   string
	name      string
	pid       int
	nativeID  string
	t         *testing.T
	onboarded bool
}

func startClaude(t *testing.T, policy string) *claudeGate {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary is unavailable")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	g := &claudeGate{
		base:    base,
		config:  filepath.Join(base, "config"),
		work:    filepath.Join(base, "work"),
		session: fmt.Sprintf("amgo-native-claude-%d-%d", os.Getpid(), time.Now().UnixNano()),
		t:       t,
	}
	if err := os.MkdirAll(g.config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(g.work, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", g.work, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	settings := fmt.Sprintf(`{"crossSessionInbound":%q}`, policy)
	if err := os.WriteFile(filepath.Join(g.config, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("env CLAUDE_CONFIG_DIR=%s HOME=%s claude --dangerously-skip-permissions",
		shellQuote(g.config), shellQuote(os.Getenv("HOME")))
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", g.session, "-x", "140", "-y", "45", line).CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", g.session).Run()
		deadline := time.Now().Add(10 * time.Second)
		for g.pid > 0 && time.Now().Before(deadline) && pidAlive(g.pid) {
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
	})
	g.onboard()
	g.waitRecord()
	return g
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (g *claudeGate) pane(raw bool) string {
	args := []string{"capture-pane", "-p", "-t", g.session}
	if raw {
		args = []string{"capture-pane", "-p", "-e", "-t", g.session}
	}
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (g *claudeGate) keys(keys ...string) {
	for _, key := range keys {
		_ = exec.Command("tmux", "send-keys", "-t", g.session, key).Run()
	}
}

func (g *claudeGate) onboard() {
	deadline := time.Now().Add(90 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		pane := g.pane(false)
		switch {
		case strings.Contains(pane, "bypass permissions on"):
			g.onboarded = true
			return
		case strings.Contains(pane, "Dark mode"):
			g.keys("Enter")
		case strings.Contains(pane, "Press Enter to continue"):
			g.keys("Enter")
		case strings.Contains(pane, "Yes, I trust this folder"):
			g.keys("Down", "Enter")
		case strings.Contains(pane, "Yes, I accept"):
			g.keys("Down", "Enter")
		}
		if pane == last {
			time.Sleep(300 * time.Millisecond)
		} else {
			last = pane
			time.Sleep(500 * time.Millisecond)
		}
	}
	g.t.Fatalf("disposable Claude session did not reach the composer: %s", g.pane(true))
}

func (g *claudeGate) waitRecord() {
	g.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(g.config, "sessions", "*.json"))
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			var rec struct {
				PID                 int    `json:"pid"`
				SessionID           string `json:"sessionId"`
				MessagingSocketPath string `json:"messagingSocketPath"`
				Status              string `json:"status"`
			}
			if json.Unmarshal(raw, &rec) != nil || rec.SessionID == "" || rec.MessagingSocketPath == "" {
				continue
			}
			g.pid = rec.PID
			g.nativeID = rec.SessionID
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	g.t.Fatalf("disposable Claude session did not publish a registry record")
}

func (g *claudeGate) transcriptHas(needle string) bool {
	found := false
	_ = filepath.Walk(g.config, func(path string, info os.FileInfo, err error) error {
		if err != nil || found || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err == nil && bytes.Contains(raw, []byte(needle)) {
			found = true
		}
		return nil
	})
	return found
}

func (g *claudeGate) waitTranscript(needle string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if g.transcriptHas(needle) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func (g *claudeGate) paneHas(needle string) bool {
	return strings.Contains(g.pane(true), needle)
}

func (g *claudeGate) kill() {
	_ = exec.Command("tmux", "kill-session", "-t", g.session).Run()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(g.pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func TestClaudeAcceptNonceAndRecipientAck(t *testing.T) {
	g := startClaude(t, "accept")
	home := t.TempDir()
	rows := discover(t, g.work, home, g.config, "")
	pickCandidate(t, rows, model.AdapterClaude, g.nativeID)
	joinClaude(t, g.work, home, g.config, g.nativeID, "claude-gate")

	nonce := randomLabel("claude-accept")
	e, code := sendText(t, g.work, home, "claude-gate",
		fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce),
		"--accept-unknown-delivery")
	m := mustOK(t, e, code, "send")
	messageID, _ := m["message_id"].(string)
	if messageID == "" {
		t.Fatalf("send returned no message_id: %+v", m)
	}
	if !g.waitTranscript("ACK "+nonce, 90*time.Second) {
		t.Fatalf("nonce %s was not observed in the disposable session transcript", nonce)
	}

	ackBody := fmt.Sprintf("Run exactly this shell command, then reply DONE: %s --home %s ack %s --as claude-gate",
		shellQuote(amgoBin), shellQuote(home), shellQuote(messageID))
	e, code = sendText(t, g.work, home, "claude-gate", ackBody, "--accept-unknown-delivery")
	mustOK(t, e, code, "ack instruction")
	if !waitReadAck(t, g.work, home, messageID, 90*time.Second) {
		t.Fatal("recipient-provenance acknowledgement was not observed")
	}
}

func TestClaudeHoldDoesNotDeliver(t *testing.T) {
	g := startClaude(t, "hold")
	home := t.TempDir()
	rows := discover(t, g.work, home, g.config, "")
	pickCandidate(t, rows, model.AdapterClaude, g.nativeID)
	joinClaude(t, g.work, home, g.config, g.nativeID, "claude-hold")

	nonce := randomLabel("claude-hold")
	e, code := sendText(t, g.work, home, "claude-hold",
		fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce),
		"--accept-unknown-delivery")
	m := mustOK(t, e, code, "send")
	time.Sleep(8 * time.Second)
	if g.transcriptHas(nonce) {
		t.Fatalf("held message reached the recipient transcript")
	}
	messageID, _ := m["message_id"].(string)
	e, code = runAmgo(t, g.work, home, "read", messageID)
	rec := mustOK(t, e, code, "read")
	msg, _ := rec["message"].(map[string]any)
	if msg["state"] != string(model.MessageSubmitted) {
		t.Fatalf("amgo claimed state %v for a held message", msg["state"])
	}
	if !g.paneHas("Held peer message") {
		t.Log("hold notice was not visible in the captured pane; delivery evidence remains negative")
	}
}

func TestClaudeRefuseDoesNotDeliver(t *testing.T) {
	g := startClaude(t, "refuse")
	home := t.TempDir()
	rows := discover(t, g.work, home, g.config, "")
	pickCandidate(t, rows, model.AdapterClaude, g.nativeID)
	joinClaude(t, g.work, home, g.config, g.nativeID, "claude-refuse")

	nonce := randomLabel("claude-refuse")
	e, code := sendText(t, g.work, home, "claude-refuse",
		fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce),
		"--accept-unknown-delivery")
	m := mustOK(t, e, code, "send")
	time.Sleep(8 * time.Second)
	if g.transcriptHas(nonce) {
		t.Fatalf("refused message reached the recipient transcript")
	}
	messageID, _ := m["message_id"].(string)
	e, code = runAmgo(t, g.work, home, "read", messageID)
	rec := mustOK(t, e, code, "read")
	msg, _ := rec["message"].(map[string]any)
	if msg["state"] != string(model.MessageSubmitted) {
		t.Fatalf("amgo claimed state %v for a refused message", msg["state"])
	}
}

func TestClaudeDeadTargetReplayAndNewSend(t *testing.T) {
	g := startClaude(t, "accept")
	home := t.TempDir()
	rows := discover(t, g.work, home, g.config, "")
	pickCandidate(t, rows, model.AdapterClaude, g.nativeID)
	joinClaude(t, g.work, home, g.config, g.nativeID, "claude-dead")

	nonce := randomLabel("claude-dead")
	body := fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce)
	e, code := sendText(t, g.work, home, "claude-dead", body, "--accept-unknown-delivery")
	m := mustOK(t, e, code, "send")
	key, _ := m["idempotency_key"].(string)
	if key == "" {
		t.Fatal("send returned no idempotency key")
	}
	if !g.waitTranscript("ACK "+nonce, 90*time.Second) {
		t.Fatal("initial delivery did not complete")
	}
	g.kill()

	// An identical replay is served from the durable membership, not the
	// dead native target.
	e, code = sendText(t, g.work, home, "claude-dead", body, "--accept-unknown-delivery", "--idempotency-key", key)
	dup := mustOK(t, e, code, "replay after death")
	if dup["duplicate"] != true {
		t.Fatalf("replay after death was not a duplicate: %+v", dup)
	}

	newNonce := randomLabel("claude-after-death")
	e, code = sendText(t, g.work, home, "claude-dead",
		fmt.Sprintf("amgo native delivery probe %s", newNonce), "--accept-unknown-delivery")
	mustCode(t, e, code, model.ErrTargetUnavailable, "new send to a dead target")
	if g.transcriptHas(newNonce) {
		t.Fatal("a message addressed to a dead target appeared in the transcript")
	}
}

func TestClaudeTargetSnapshotMismatch(t *testing.T) {
	g := startClaude(t, "accept")
	home := t.TempDir()
	rows := discover(t, g.work, home, g.config, "")
	pickCandidate(t, rows, model.AdapterClaude, g.nativeID)
	joinClaude(t, g.work, home, g.config, g.nativeID, "claude-snapshot")

	st, err := store.Open(config.DBPath(home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := st.ListSessions(false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions=%+v err=%v", sessions, err)
	}
	sess := sessions[0]
	sess.Target.ProcStart = "Thu Jan  1 00:00:00 1970"
	if err := st.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	nonce := randomLabel("claude-snapshot")
	e, code := sendText(t, g.work, home, "claude-snapshot",
		fmt.Sprintf("amgo native delivery probe %s", nonce), "--accept-unknown-delivery")
	mustCode(t, e, code, model.ErrTargetUnavailable, "send after process-start change")
	if g.transcriptHas(nonce) {
		t.Fatal("a retargeted message reached the transcript")
	}
}

// ---- Codex disposable TUI ----

type codexGate struct {
	home string
	work string
	ptmx *os.File
	cmd  *exec.Cmd
	wmu  sync.Mutex
	mu   sync.Mutex
	log  bytes.Buffer

	trusted bool
	closed  bool
}

func startCodex(t *testing.T) *codexGate {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary is unavailable")
	}
	auth := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")
	rawAuth, err := os.ReadFile(auth)
	if err != nil {
		t.Skipf("no disposable Codex credentials available: %v", err)
	}
	base := t.TempDir()
	home := filepath.Join(base, "codex")
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", work, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), rawAuth, 0o600); err != nil {
		t.Fatal(err)
	}
	modelName := os.Getenv("AMGO_CODEX_MODEL")
	if modelName == "" {
		modelName = "gpt-6.1-sol"
	}
	cfg := fmt.Sprintf("model = %q\nmodel_provider = \"openai\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\n", modelName)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	g := &codexGate{home: home, work: work}
	cmd := exec.Command("codex", "--no-alt-screen")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home, "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 45, Cols: 140})
	if err != nil {
		t.Fatalf("start Codex pty: %v", err)
	}
	g.ptmx = ptmx
	g.cmd = cmd
	go g.pump()
	t.Cleanup(func() {
		g.close()
		// A managed daemon can outlive `daemon stop` once its CODEX_HOME is
		// removed. Kill every daemon bound to this disposable home.
		out, _ := exec.Command("pgrep", "-f", g.home+"/packages/app-server-daemon").Output()
		for _, field := range strings.Fields(string(out)) {
			if pid, err := strconv.Atoi(field); err == nil {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		time.Sleep(500 * time.Millisecond)
	})
	if !g.waitLog(func(s string) bool {
		return strings.Contains(s, "Ask Codex to do anything")
	}, 90*time.Second) {
		t.Fatalf("Codex TUI did not reach the composer: %s", tailText(g.logText(), 1500))
	}
	return g
}

func (g *codexGate) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := g.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			g.mu.Lock()
			g.log.Write(chunk)
			tail := g.log.String()
			if len(tail) > 8192 {
				tail = tail[len(tail)-8192:]
			}
			g.mu.Unlock()
			if bytes.Contains(chunk, []byte("\x1b[6n")) {
				g.write([]byte("\x1b[1;1R"))
			}
			if bytes.Contains(chunk, []byte("]10;?")) {
				g.write([]byte("\x1b]10;rgb:ffff/ffff/ffff\x07"))
			}
			if bytes.Contains(chunk, []byte("]11;?")) {
				g.write([]byte("\x1b]11;rgb:0000/0000/0000\x07"))
			}
			if strings.Contains(tail, "Trust this folder?") && !g.trusted {
				g.trusted = true
				time.Sleep(300 * time.Millisecond)
				g.write([]byte("\r"))
			}
		}
		if err != nil {
			return
		}
	}
}

func (g *codexGate) write(data []byte) {
	g.wmu.Lock()
	defer g.wmu.Unlock()
	_, _ = g.ptmx.Write(data)
}

func (g *codexGate) sendKeys(s string) {
	g.write([]byte(s))
}

func (g *codexGate) logText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.log.String()
}

func (g *codexGate) waitLog(pred func(string) bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(g.logText()) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func (g *codexGate) close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	g.mu.Unlock()
	_ = g.ptmx.Close()
	if g.cmd.Process != nil {
		_ = g.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_ = g.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = g.cmd.Process.Kill()
			<-done
		}
	}
}

func (g *codexGate) discoverThread(t *testing.T, dir, home string) string {
	t.Helper()
	empty := t.TempDir()
	deadline := time.Now().Add(45 * time.Second)
	var rows []map[string]any
	for time.Now().Before(deadline) {
		rows = discover(t, dir, home, empty, g.home)
		for _, row := range rows {
			if row["adapter"] == model.AdapterCodex && row["reachable"] == true {
				if id, _ := row["native_session_id"].(string); id != "" {
					return id
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no loaded Codex thread discovered in the disposable TUI: %+v log=%s", rows, tailText(g.logText(), 1500))
	return ""
}

func TestCodexQueueDeliveryAndIdempotentReplay(t *testing.T) {
	g := startCodex(t)
	home := t.TempDir()
	thread := g.discoverThread(t, g.work, home)
	joinCodex(t, g.work, home, g.home, thread, "codex-gate")

	nonce := randomLabel("codex")
	body := fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce)
	e, code := sendText(t, g.work, home, "codex-gate", body)
	m := mustOK(t, e, code, "codex send")
	key, _ := m["idempotency_key"].(string)
	messageID, _ := m["message_id"].(string)
	if key == "" || messageID == "" {
		t.Fatalf("codex send lost identifiers: %+v", m)
	}
	if !g.waitLog(func(s string) bool { return strings.Contains(s, "amgo native delivery probe "+nonce) }, 60*time.Second) {
		t.Fatal("queued message did not appear in the TUI")
	}
	if !g.waitLog(func(s string) bool { return strings.Contains(s, "ACK "+nonce) }, 120*time.Second) {
		t.Fatal("Codex did not acknowledge the queued nonce")
	}

	e, code = sendText(t, g.work, home, "codex-gate", body, "--idempotency-key", key)
	dup := mustOK(t, e, code, "codex replay")
	if dup["duplicate"] != true || dup["message_id"] != messageID {
		t.Fatalf("replay=%+v", dup)
	}
	if got := strings.Count(g.logText(), "amgo native delivery probe "+nonce); got != 1 {
		t.Fatalf("native message appeared %d times; want exactly one", got)
	}
}

func TestCodexBusyQueue(t *testing.T) {
	g := startCodex(t)
	home := t.TempDir()
	thread := g.discoverThread(t, g.work, home)
	joinCodex(t, g.work, home, g.home, thread, "codex-busy")

	g.sendKeys("Use the shell tool to run exactly: sleep 20. Then reply with exactly BUSY-DONE\r")
	time.Sleep(3 * time.Second)
	nonce := randomLabel("codex-busy")
	e, code := sendText(t, g.work, home, "codex-busy",
		fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce))
	mustOK(t, e, code, "busy send")
	if !g.waitLog(func(s string) bool { return strings.Contains(s, "ACK "+nonce) }, 150*time.Second) {
		t.Fatalf("busy queue did not deliver the nonce; tail=%s", tailText(g.logText(), 1200))
	}
}

func TestCodexDeadTarget(t *testing.T) {
	g := startCodex(t)
	home := t.TempDir()
	thread := g.discoverThread(t, g.work, home)
	joinCodex(t, g.work, home, g.home, thread, "codex-dead")

	nonce := randomLabel("codex-dead")
	body := fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce)
	e, code := sendText(t, g.work, home, "codex-dead", body)
	m := mustOK(t, e, code, "send")
	key, _ := m["idempotency_key"].(string)
	if !g.waitLog(func(s string) bool { return strings.Contains(s, "ACK "+nonce) }, 120*time.Second) {
		t.Fatal("initial Codex delivery did not complete")
	}
	g.close()
	stop := exec.Command("codex", "app-server", "daemon", "stop")
	stop.Env = append(os.Environ(), "CODEX_HOME="+g.home)
	_ = stop.Run()
	time.Sleep(2 * time.Second)

	e, code = sendText(t, g.work, home, "codex-dead", body, "--idempotency-key", key)
	dup := mustOK(t, e, code, "replay after Codex death")
	if dup["duplicate"] != true {
		t.Fatalf("replay after death was not a duplicate: %+v", dup)
	}
	e, code = sendText(t, g.work, home, "codex-dead", "amgo native delivery probe "+randomLabel("codex-after-death"))
	mustCode(t, e, code, model.ErrTargetUnavailable, "new send to a dead Codex target")
}

// ---- Native transport boundaries ----

func TestClaudeInterruptDuringWriteIsUnknown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "amgo-native-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "blocked.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f, err := ln.File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_ = unix.SetsockoptInt(int(f.Fd()), unix.SOL_SOCKET, unix.SO_RCVBUF, 1024)

	target := model.Target{
		Adapter: model.AdapterClaude, HostID: config.HostID(), ConfigHome: t.TempDir(),
		NativeSessionID: "interrupt-gate", SocketPath: sock, PID: os.Getpid(), PIDDomain: runtime.GOOS,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	res, err := claude.New().Send(ctx, adapter.SendRequest{
		Target: target, MessageID: ids.New("m"), Body: strings.Repeat("x", 64<<10), Timeout: 2 * time.Second,
	})
	cancel()
	if err != nil {
		t.Fatalf("send error: %v", err)
	}
	if res.Outcome == "submitted" {
		t.Skip("the socket accepted the complete frame before cancellation; mid-write cancellation was not exercised")
	}
	if res.Outcome != "unknown" || res.ErrorCode != model.ErrInterrupted {
		t.Fatalf("result=%+v", res)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestStoreDiskFullReportsOutcomeUnpersisted(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("hdiutil gate is macOS-only")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil is unavailable")
	}
	base := t.TempDir()
	image := filepath.Join(base, "gate.dmg")
	mnt := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("hdiutil", "create", "-size", "2m", "-fs", "APFS", "-volname", "AmgoGate", "-ov", image).CombinedOutput(); err != nil {
		t.Skipf("cannot create the gate disk image: %v: %s", err, out)
	}
	if out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-mountpoint", mnt, image).CombinedOutput(); err != nil {
		t.Skipf("cannot mount the gate disk image: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("chflags", "nouchg", config.DBPath(mnt)).Run()
		_ = exec.Command("hdiutil", "detach", "-force", mnt).Run()
		_ = os.Remove(image)
	})

	home := mnt
	db := config.DBPath(home)
	svc := message.New(home, map[string]adapter.Adapter{
		"fake": &fillAdapter{dir: mnt, db: db, result: adapter.SendResult{Outcome: "submitted", NativeDisposition: model.DispositionQueued}},
	})
	st, err := store.Open(db, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSession(model.Session{
		RecordVersion: 1, ID: "s_fill", Alias: "fill",
		Target:       model.Target{Adapter: "fake", HostID: config.HostID(), NativeSessionID: "fill"},
		Capabilities: model.Capabilities{Adapter: "fake", LiveMessaging: true},
		RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	out, err := svc.Send(context.Background(), message.SendInput{RecipientAlias: "fill", Body: "fill the disk", IdempotencyKey: "k-fill"})
	if model.Code(err) != model.ErrOutcomeUnpersisted {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if out.Message.ID == "" || out.Reservation.ID == "" {
		t.Fatalf("identifiers lost: %+v", out)
	}
}

type fillAdapter struct {
	dir    string
	db     string
	result adapter.SendResult
}

func (f *fillAdapter) Name() string { return "fake" }
func (f *fillAdapter) Probe(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Adapter: "fake", LiveMessaging: true}, nil
}
func (f *fillAdapter) Discover(context.Context, adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}
func (f *fillAdapter) Send(context.Context, adapter.SendRequest) (adapter.SendResult, error) {
	// Fill the volume first, then make the database immutable. APFS can
	// satisfy a small finalization write from free pages even after ENOSPC,
	// so the immutable bit supplies the deterministic post-acceptance failure.
	buf := bytes.Repeat([]byte{'x'}, 32<<10)
	for i := 0; i < 4096; i++ {
		file, err := os.OpenFile(filepath.Join(f.dir, fmt.Sprintf("filler-%d", i)), os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			break
		}
		_, werr := file.Write(buf)
		_ = file.Close()
		if werr != nil {
			break
		}
	}
	_ = exec.Command("chflags", "uchg", f.db).Run()
	return f.result, nil
}

func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestClaudeBudgetReplayCloseExpiry(t *testing.T) {
	g := startClaude(t, "accept")
	home := t.TempDir()
	rows := discover(t, g.work, home, g.config, "")
	pickCandidate(t, rows, model.AdapterClaude, g.nativeID)
	joinClaude(t, g.work, home, g.config, g.nativeID, "claude-budget")

	e, code := runAmgo(t, g.work, home, "conversation", "create", "gate", "--max-messages", "1", "--expires", "1h")
	mustOK(t, e, code, "create conversation")

	nonce := randomLabel("claude-budget")
	body := fmt.Sprintf("amgo native delivery probe %s. Reply with exactly: ACK %s", nonce, nonce)
	e, code = sendText(t, g.work, home, "claude-budget", body,
		"--accept-unknown-delivery", "--conversation", "gate", "--idempotency-key", "k-budget-1")
	m := mustOK(t, e, code, "first send")
	if !g.waitTranscript("ACK "+nonce, 90*time.Second) {
		t.Fatal("first conversation message did not arrive")
	}

	e, code = sendText(t, g.work, home, "claude-budget", body,
		"--accept-unknown-delivery", "--conversation", "gate", "--idempotency-key", "k-budget-1")
	dup := mustOK(t, e, code, "replay after budget exhaustion")
	if dup["duplicate"] != true || dup["message_id"] != m["message_id"] {
		t.Fatalf("replay=%+v first=%+v", dup, m)
	}

	e, code = sendText(t, g.work, home, "claude-budget", "second message",
		"--accept-unknown-delivery", "--conversation", "gate", "--idempotency-key", "k-budget-2")
	mustCode(t, e, code, model.ErrBudgetExhausted, "second send over budget")

	e, code = runAmgo(t, g.work, home, "conversation", "close", "gate")
	mustOK(t, e, code, "close conversation")
	e, code = sendText(t, g.work, home, "claude-budget", "third message",
		"--accept-unknown-delivery", "--conversation", "gate", "--idempotency-key", "k-budget-3")
	mustCode(t, e, code, model.ErrConversationClosed, "send to a closed conversation")

	e, code = runAmgo(t, g.work, home, "conversation", "create", "short", "--max-messages", "2", "--expires", "1ms")
	mustOK(t, e, code, "create short conversation")
	time.Sleep(20 * time.Millisecond)
	e, code = sendText(t, g.work, home, "claude-budget", "expired message",
		"--accept-unknown-delivery", "--conversation", "short", "--idempotency-key", "k-budget-4")
	mustCode(t, e, code, model.ErrConversationClosed, "send to an expired conversation")
}

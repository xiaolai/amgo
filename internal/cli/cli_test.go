package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"amgo/internal/config"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/platform"
	"amgo/internal/store"
)

func TestVersionUsageAndInputValidation(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"version"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("version exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "amgo "+Version) {
		t.Fatalf("out=%q", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--json", "version"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("json version exit=%d err=%s", code, errOut.String())
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, out.String())
	}
	if env["schema_version"] != float64(1) || env["operation"] != "version" {
		t.Fatalf("envelope=%v", env)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"no-such-command"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("unknown command exit=%d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--home", t.TempDir(), "send", "worker"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("missing body exit=%d", code)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--home", t.TempDir(), "read", "m_missing"}, strings.NewReader(""), &out, &errOut); code != 5 {
		t.Fatalf("missing message exit=%d err=%s", code, errOut.String())
	}
}

func TestClaudeEndToEnd(t *testing.T) {
	claudeHome := t.TempDir()
	amgoHome := t.TempDir()
	sock, frames, stop := startFakeInbox(t)
	defer stop()
	sid := writeClaudeSession(t, claudeHome, sock)
	var out, errOut bytes.Buffer

	code := Run([]string{"--home", amgoHome, "join", "claude", "--name", "worker", "--session", sid, "--config-home", claudeHome}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("join exit=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"--home", amgoHome, "send", "worker", "--stdin", "--accept-unknown-delivery", "--expect-reply"}, strings.NewReader("hello from the cli test"), &out, &errOut)
	if code != 0 {
		t.Fatalf("send exit=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "submitted") {
		t.Fatalf("send out=%q", out.String())
	}
	select {
	case lines := <-frames:
		if len(lines) != 2 {
			t.Fatalf("frames=%v", lines)
		}
		if !strings.Contains(lines[1], "hello from the cli test") || !strings.Contains(lines[1], "amgo reply") {
			t.Fatalf("frame=%s", lines[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake inbox received no frame")
	}

	st, err := store.Open(config.DBPath(amgoHome), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ListAllMessages(10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages=%+v err=%v", msgs, err)
	}
	id := msgs[0].ID
	_ = st.Close()

	out.Reset()
	errOut.Reset()
	code = Run([]string{"--home", amgoHome, "--json", "read", id}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("read exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"state":"submitted"`) || !strings.Contains(out.String(), `"native_disposition":"unknown"`) {
		t.Fatalf("read out=%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"--home", amgoHome, "ack", id, "--as", "worker"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("ack exit=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"--home", amgoHome, "wait", id, "--until", "acknowledged", "--timeout", "1s"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("wait exit=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"--home", amgoHome, "inbox", "--session", "worker", "--json"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("inbox exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) {
		t.Fatalf("inbox out=%s", out.String())
	}
}

func startFakeInbox(t *testing.T) (string, chan []string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "amgo-cli-")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "inbox.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	frames := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(frames)
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		frames <- lines
		close(frames)
	}()
	stop := func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}
	return sock, frames, stop
}

func writeClaudeSession(t *testing.T, home, sock string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	start, err := platform.ProcStartUTC(pid)
	if err != nil {
		t.Fatal(err)
	}
	sid := ids.NewUUID()
	rec := map[string]any{
		"pid": pid, "sessionId": sid, "procStart": start, "version": "2.1.303",
		"peerProtocol": 1, "peerFeatures": []string{"notify_idle"}, "kind": "interactive",
		"entrypoint": "cli", "pidDomain": runtime.GOOS, "messagingSocketPath": sock,
		"name": "cli-fake", "status": "idle",
	}
	raw, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(home, "sessions", strconv.Itoa(pid)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(sock))
	keyPath := filepath.Join(home, "sessions", strconv.Itoa(pid)+"."+hex.EncodeToString(h[:])+".key")
	key := map[string]any{
		"peerToken": strings.Repeat("c", 32), "procStart": start, "pidDomain": runtime.GOOS,
	}
	raw, _ = json.Marshal(key)
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return sid
}

func TestHumanOutputSanitizesPeerText(t *testing.T) {
	home := t.TempDir()
	st, err := store.Open(config.DBPath(home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateConversation(model.Conversation{
		RecordVersion: 1, ID: "c_esc", Participants: []string{"operator:1", "s_r"},
		MaxMessages: 4, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	body := "\x1b[31mred\x1b[0m"
	_, err = st.PrepareSend(store.PrepareRequest{
		CallerID: "operator:1", IdemKey: "esc", Fingerprint: "fp",
		Message: model.Message{
			RecordVersion: 1, ID: "m_esc", ConversationID: "c_esc", SenderLabel: "operator",
			RecipientSessionID: "s_r", Body: body, BodySHA256: "h", BodyBytes: len(body),
			State: model.MessageDispatching, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		Attempt: model.Attempt{
			RecordVersion: 1, ID: "a_esc", MessageID: "m_esc", ReservationID: "r_esc",
			State: model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(time.Minute),
		},
		Reservation: model.Reservation{
			RecordVersion: 1, ID: "r_esc", MessageID: "m_esc", ConversationID: "c_esc",
			State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(time.Minute),
		},
		ConversationID: "c_esc", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	var out, errOut bytes.Buffer
	if code := Run([]string{"--home", home, "read", "m_esc"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("read exit=%d err=%s", code, errOut.String())
	}
	if strings.ContainsRune(out.String(), 0x1b) {
		t.Fatalf("raw escape reached the terminal: %q", out.String())
	}
	if !strings.Contains(out.String(), `\x1b[31m`) {
		t.Fatalf("sanitized body missing: %q", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"--home", home, "--json", "read", "m_esc"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("json read exit=%d err=%s", code, errOut.String())
	}
	var env struct {
		Data struct {
			Message struct {
				Body string `json:"body"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Message.Body != body {
		t.Fatalf("JSON body is not lossless: %q", env.Data.Message.Body)
	}
}

func TestSessionsDoesNotCreateState(t *testing.T) {
	home := t.TempDir()
	var out, errOut bytes.Buffer
	if code := Run([]string{"--home", home, "--json", "sessions"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("sessions exit=%d err=%s", code, errOut.String())
	}
	if _, err := os.Stat(config.DBPath(home)); !os.IsNotExist(err) {
		t.Fatalf("sessions created state.db: %v", err)
	}
}

func TestParseGlobalsHonorsCommandValuesAndTerminator(t *testing.T) {
	g, rest, err := parseGlobals([]string{"send", "worker", "--text", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if g.json {
		t.Fatal("command value was parsed as a global flag")
	}
	if strings.Join(rest, "|") != "send|worker|--text|--json" {
		t.Fatalf("rest=%q", rest)
	}
	g, rest, err = parseGlobals([]string{"send", "worker", "--", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if g.json {
		t.Fatal("global flag after -- was consumed")
	}
	if strings.Join(rest, "|") != "send|worker|--|--json" {
		t.Fatalf("rest=%q", rest)
	}
	g, rest, err = parseGlobals([]string{"--", "version"})
	if err != nil || g.json || strings.Join(rest, "|") != "version" {
		t.Fatalf("terminator before command: g=%+v rest=%q err=%v", g, rest, err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"--", "version"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("-- version exit=%d err=%s", code, errOut.String())
	}
}

func TestUnknownCommandEmitsJSONEnvelope(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"--home", t.TempDir(), "--json", "bogus"}, strings.NewReader(""), &out, &errOut)
	if code != 2 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	var env struct {
		Outcome string `json:"outcome"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, out.String())
	}
	if env.Outcome != "error" || env.Error.Code != model.ErrUsage {
		t.Fatalf("envelope=%+v", env)
	}
}

func TestJSONWriteFailureIsNotSuccess(t *testing.T) {
	var errOut bytes.Buffer
	code := Run([]string{"--json", "version"}, strings.NewReader(""), failWriter{}, &errOut)
	if code == 0 {
		t.Fatal("a failed JSON write exited 0")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestReadBodyHonorsCancellation(t *testing.T) {
	app := &App{In: blockingReader{}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := app.readBody(ctx, "stdin", "", "")
	if model.Code(err) != model.ErrInterrupted {
		t.Fatalf("err=%v", err)
	}
}

type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	select {}
}

func TestFirstLineTruncatesAtRuneBoundary(t *testing.T) {
	got := firstLine(strings.Repeat("界", 100))
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("not truncated: %q", got)
	}
	if len([]rune(got)) != 81 {
		t.Fatalf("runes=%d", len([]rune(got)))
	}
}

func TestRetryRequiresDuplicateRiskFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"--home", t.TempDir(), "retry", "m_missing"}, strings.NewReader(""), &out, &errOut)
	if code != 6 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "accept-duplicate-risk") {
		t.Fatalf("message does not name the flag: %s", errOut.String())
	}
}

func TestRetryUnknownMessageIsTargetUnavailable(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"--home", t.TempDir(), "retry", "m_missing", "--accept-duplicate-risk"}, strings.NewReader(""), &out, &errOut)
	if code != 5 {
		t.Fatalf("exit=%d err=%s", code, errOut.String())
	}
}

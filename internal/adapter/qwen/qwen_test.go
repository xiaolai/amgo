package qwen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/model"
)

// A low-entropy 64-hex value that satisfies the registry format without
// looking like a real credential to secret scanners.
var testToken = strings.Repeat("0a", 32)

func writeRecord(t *testing.T, home string, schema int, sessionID, sock, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	start, ns := testIdentity()
	rec := map[string]any{
		"schemaVersion": schema, "pid": os.Getpid(), "procStart": start, "pidNs": ns,
		"sessionId": sessionID, "cwd": "/work", "name": "qwen-test", "startedAt": 1,
		"qwenVersion": "0.0.0-test", "kind": "tui", "ipcPath": sock,
	}
	if token != "" {
		rec["ipcToken"] = token
	}
	raw, _ := json.Marshal(rec)
	path := filepath.Join(home, "sessions", fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

type receivedFrame struct {
	Auth bool
	User map[string]any
}

// startInbox runs a fake Qwen inbox that authenticates, records the user frame
// and returns the receipt statuses supplied by respond.
func startInbox(t *testing.T, home, token, sessionID string, respond func(map[string]any) []string) (string, chan receivedFrame) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "amgo-qw-inbox-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	writeRecord(t, home, 1, sessionID, sock, token)

	frames := make(chan receivedFrame, 8)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		authed := token == ""
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				return
			}
			switch m["type"] {
			case "auth":
				if m["token"] != token {
					return
				}
				authed = true
				frames <- receivedFrame{Auth: true}
			case "user":
				if !authed {
					return
				}
				frames <- receivedFrame{User: m}
				for _, status := range respond(m) {
					sendReceipt(m, status)
				}
				return
			}
		}
	}()
	return sock, frames
}

func sendReceipt(user map[string]any, status string) {
	from, _ := user["from"].(string)
	reply, _ := user["replyToken"].(string)
	orig, _ := user["msgId"].(string)
	conn, err := net.DialTimeout("unix", from, time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	_ = enc.Encode(map[string]any{"msgV": 1, "type": "auth", "token": reply})
	_ = enc.Encode(map[string]any{
		"msgV": 1, "msgId": "rcpt-" + orig, "type": "control",
		"action": "delivery_status", "status": status, "origMsgId": orig,
		"from": "/fake/receiver",
	})
}

func discovered(t *testing.T, home string) model.Candidate {
	t.Helper()
	a := New()
	cands, err := a.Discover(context.Background(), adapter.DiscoverRequest{QwenHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || !cands[0].Live {
		t.Fatalf("candidates=%+v", cands)
	}
	return cands[0]
}

func TestDiscoverAndSendDelivered(t *testing.T) {
	home := t.TempDir()
	_, frames := startInbox(t, home, testToken, "sess-1", func(map[string]any) []string { return []string{"delivered"} })
	cand := discovered(t, home)
	if cand.Target.NativeSessionID != "sess-1" || cand.Target.Endpoint == "" {
		t.Fatalf("candidate=%+v", cand)
	}
	if strings.Contains(fmt.Sprint(cand), testToken) {
		t.Fatal("candidate leaks the registry token")
	}
	a := New()
	res, err := a.Send(context.Background(), adapter.SendRequest{
		Target: cand.Target, MessageID: "m_1", ConversationID: "c_1",
		Body: "hello qwen", AdmitExperimental: true, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" || res.NativeDisposition != model.DispositionQueued ||
		!strings.Contains(res.Evidence, "delivered") {
		t.Fatalf("result=%+v", res)
	}
	got := <-frames
	if got.Auth {
		got = <-frames
	}
	if got.User["toSessionId"] != "sess-1" || got.User["type"] != "user" {
		t.Fatalf("frame=%+v", got.User)
	}
	if !strings.Contains(fmt.Sprint(got.User["message"]), "hello qwen") {
		t.Fatalf("frame message=%v", got.User["message"])
	}
}

func TestSendWithoutAdmissionIsRefused(t *testing.T) {
	home := t.TempDir()
	startInbox(t, home, "", "sess-2", func(map[string]any) []string { return nil })
	cand := discovered(t, home)
	_, err := New().Send(context.Background(), adapter.SendRequest{Target: cand.Target, MessageID: "m_2", ConversationID: "c_2", Body: "x"})
	if model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("err=%v", err)
	}
}

func TestSendNoTokenOmitsAuthAndMapsReceipts(t *testing.T) {
	cases := map[string]string{
		"held": model.DispositionHeld, "refused": model.DispositionRefused,
		"denied": model.DispositionDenied, "misaddressed": model.DispositionMisaddressed,
		"dropped": model.DispositionDropped, "expired": model.DispositionExpired,
	}
	for status, want := range cases {
		t.Run(status, func(t *testing.T) {
			home := t.TempDir()
			_, frames := startInbox(t, home, "", "sess-"+status, func(map[string]any) []string { return []string{status} })
			cand := discovered(t, home)
			res, err := New().Send(context.Background(), adapter.SendRequest{
				Target: cand.Target, MessageID: "m_" + status, ConversationID: "c", Body: "x",
				AdmitExperimental: true, Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.NativeDisposition != want {
				t.Fatalf("disposition=%s want=%s", res.NativeDisposition, want)
			}
			got := <-frames
			if got.Auth {
				t.Logf("first frame: %+v", got)
				select {
				case second := <-frames:
					t.Logf("second frame: %+v", second)
				default:
				}
				t.Fatal("auth frame sent to a token-free inbox")
			}
		})
	}
}

func TestReceiptTransitionsIgnoreReorderingAndDuplicates(t *testing.T) {
	home := t.TempDir()
	startInbox(t, home, "", "sess-r", func(map[string]any) []string {
		return []string{"delivered", "held", "delivered"}
	})
	cand := discovered(t, home)
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: cand.Target, MessageID: "m_r", ConversationID: "c", Body: "x",
		AdmitExperimental: true, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeDisposition != model.DispositionQueued {
		t.Fatalf("result=%+v", res)
	}

	home = t.TempDir()
	startInbox(t, home, "", "sess-h", func(map[string]any) []string {
		return []string{"held", "delivered"}
	})
	cand = discovered(t, home)
	res, err = New().Send(context.Background(), adapter.SendRequest{
		Target: cand.Target, MessageID: "m_h", ConversationID: "c", Body: "x",
		AdmitExperimental: true, Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeDisposition != model.DispositionQueued {
		t.Fatalf("held then delivered result=%+v", res)
	}
}

func TestSessionChangeAndInvalidRecords(t *testing.T) {
	home := t.TempDir()
	startInbox(t, home, "", "sess-old", func(map[string]any) []string { return []string{"delivered"} })
	cand := discovered(t, home)
	writeRecord(t, home, 1, "sess-new", cand.Target.Endpoint, "")
	_, err := New().Send(context.Background(), adapter.SendRequest{
		Target: cand.Target, MessageID: "m_s", ConversationID: "c", Body: "x",
		AdmitExperimental: true, Timeout: time.Second,
	})
	if model.Code(err) != model.ErrTargetUnavailable {
		t.Fatalf("err=%v", err)
	}

	home = t.TempDir()
	startInbox(t, home, "", "sess-v2", func(map[string]any) []string { return nil })
	writeRecord(t, home, 2, "sess-v2", "", "")
	cands, _ := New().Discover(context.Background(), adapter.DiscoverRequest{QwenHome: home})
	if len(cands) != 0 {
		t.Fatalf("schema 2 record was listed: %+v", cands)
	}
}

func TestSocketPermissionsAreEnforced(t *testing.T) {
	home := t.TempDir()
	sock, _ := startInbox(t, home, "", "sess-mode", func(map[string]any) []string { return nil })
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}
	cands, err := New().Discover(context.Background(), adapter.DiscoverRequest{QwenHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Live {
		t.Fatalf("world-writable socket was live: %+v", cands)
	}
}

func TestWriteBoundedByContext(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "amgo-qw-write-")
	if err != nil {
		t.Skipf("cannot create a short socket path: %v", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := <-accepted
	defer peer.Close()

	payload := bytes.Repeat([]byte("x"), 16<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = writeBounded(ctx, conn, payload, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("bounded write returned after %s", elapsed)
	}
}

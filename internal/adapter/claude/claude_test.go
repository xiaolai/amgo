package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/platform"
)

type fakeInbox struct {
	dir    string
	sock   string
	ln     net.Listener
	frames chan []string
}

func startFakeInbox(t *testing.T) *fakeInbox {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "amgo-cl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "inbox.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeInbox{dir: dir, sock: sock, ln: ln, frames: make(chan []string, 1)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(f.frames)
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var lines []string
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		f.frames <- lines
		close(f.frames)
	}()
	return f
}

func writeSession(t *testing.T, home, sock, version string, key *KeyFile) string {
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
	rec := SessionRecord{
		PID:                 pid,
		SessionID:           sid,
		ProcStart:           start,
		Version:             version,
		PeerProtocol:        1,
		PeerFeatures:        []string{"notify_idle"},
		Kind:                "interactive",
		Entrypoint:          "cli",
		PIDDomain:           localDomain(),
		MessagingSocketPath: sock,
		Name:                "fake",
		Status:              "idle",
	}
	raw, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(home, "sessions", strconv.Itoa(pid)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if key != nil {
		kp := KeyPath(home, pid, sock)
		raw, _ := json.Marshal(key)
		if err := os.WriteFile(kp, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return sid
}

func TestDiscoverAndSendFrame(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	key := &KeyFile{PeerToken: strings.Repeat("a", 32), ProcStart: mustStart(t), PIDDomain: localDomain()}
	sid := writeSession(t, home, fake.sock, "2.1.303", key)

	a := New()
	cands, err := a.Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || !cands[0].Live {
		t.Fatalf("candidates=%+v", cands)
	}
	if cands[0].NativeSessionID != sid {
		t.Fatalf("sid=%s want %s", cands[0].NativeSessionID, sid)
	}
	msgID := ids.New("m")
	res, err := a.Send(context.Background(), adapter.SendRequest{
		Target:         cands[0].Target,
		MessageID:      msgID,
		ConversationID: "c_test",
		SenderLabel:    "operator",
		Body:           "hello from amgo",
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" || res.NativeDisposition != model.DispositionUnknown {
		t.Fatalf("result=%+v", res)
	}
	lines := <-fake.frames
	if len(lines) != 2 {
		t.Fatalf("frames=%v", lines)
	}
	var auth authFrame
	if err := json.Unmarshal([]byte(lines[0]), &auth); err != nil || auth.Type != "auth" || auth.Token != key.PeerToken {
		t.Fatalf("auth=%+v err=%v", auth, err)
	}
	var frame userFrame
	if err := json.Unmarshal([]byte(lines[1]), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "user" || frame.MsgV != 1 || frame.SessionID != sid {
		t.Fatalf("frame=%+v", frame)
	}
	uuid, _ := ids.UUIDPart(msgID)
	if frame.MsgID != uuid || frame.UUID != uuid {
		t.Fatalf("ids ms=%s uuid=%s want %s", frame.MsgID, frame.UUID, uuid)
	}
	if !strings.Contains(frame.Message.Content, "[amgo message "+msgID) {
		t.Fatalf("content=%q", frame.Message.Content)
	}
}

func TestSendWithoutKeyIsUnauthenticated(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.1.303", nil)
	a := New()
	cands, _ := a.Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{home}})
	if len(cands) != 1 || !cands[0].Live {
		t.Fatalf("candidates=%+v", cands)
	}
	if _, err := a.Send(context.Background(), adapter.SendRequest{
		Target: cands[0].Target, MessageID: ids.New("m"), ConversationID: "c", Body: "x", Timeout: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	lines := <-fake.frames
	if len(lines) != 1 {
		t.Fatalf("expected one unauthenticated frame, got %v", lines)
	}
	var frame userFrame
	if err := json.Unmarshal([]byte(lines[0]), &frame); err != nil || frame.Type != "user" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
}

func TestKeyDowngradeTable(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.1.303", nil)
	a := New()
	cases := []struct {
		name string
		key  *KeyFile
		raw  string
		live bool
		why  string
	}{
		{"absent", nil, "", true, ""},
		{"malformed", nil, "{not json", false, "key file rejected"},
		{"short-token", &KeyFile{PeerToken: "abcd", PIDDomain: localDomain()}, "", false, "key file rejected"},
		{"procstart-mismatch", &KeyFile{PeerToken: strings.Repeat("b", 32), ProcStart: "Mon Jan  1 00:00:00 2001", PIDDomain: localDomain()}, "", false, "key process start mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := t.TempDir()
			writeSession(t, h, fake.sock, "2.1.303", nil)
			if tc.key != nil {
				kp := KeyPath(h, os.Getpid(), fake.sock)
				raw, _ := json.Marshal(tc.key)
				_ = os.WriteFile(kp, raw, 0o600)
			}
			if tc.raw != "" {
				kp := KeyPath(h, os.Getpid(), fake.sock)
				_ = os.WriteFile(kp, []byte(tc.raw), 0o600)
			}
			cands, _ := a.Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{h}})
			if len(cands) != 1 {
				t.Fatalf("candidates=%+v", cands)
			}
			if cands[0].Live != tc.live {
				t.Fatalf("live=%v want %v reason=%s", cands[0].Live, tc.live, cands[0].Reason)
			}
			if tc.why != "" && !strings.Contains(cands[0].Reason, tc.why) {
				t.Fatalf("reason=%q want %q", cands[0].Reason, tc.why)
			}
		})
	}
}

func TestOversizedKeyRejected(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.1.303", nil)
	kp := KeyPath(home, os.Getpid(), fake.sock)
	big := make([]byte, maxKeyBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(kp, big, 0o600); err != nil {
		t.Fatal(err)
	}
	cands, _ := New().Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{home}})
	if len(cands) != 1 || cands[0].Live || !strings.Contains(cands[0].Reason, "key file rejected") {
		t.Fatalf("candidates=%+v", cands)
	}
}

func TestVersionBelowMinimum(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.0.0", nil)
	cands, _ := New().Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{home}})
	if len(cands) != 1 || cands[0].Live || !strings.Contains(cands[0].Reason, "below") {
		t.Fatalf("candidates=%+v", cands)
	}
}

func TestDialFailure(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(home, "dead.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	target := model.Target{
		Adapter: model.AdapterClaude, HostID: "test", ConfigHome: home,
		NativeSessionID: "sid", SocketPath: sock, PID: os.Getpid(),
	}
	res, err := New().Send(context.Background(), adapter.SendRequest{Target: target, MessageID: ids.New("m"), Body: "x"})
	if err != nil {
		t.Fatalf("dial failure must be a result, not an error: %v", err)
	}
	if res.Outcome != "failed_before_submit" || !strings.Contains(res.Evidence, "dial failed") {
		t.Fatalf("result=%+v", res)
	}
}

func TestSendRejectsEmptySessionID(t *testing.T) {
	fake := startFakeInbox(t)
	target := model.Target{
		Adapter: model.AdapterClaude, HostID: "test", ConfigHome: t.TempDir(),
		SocketPath: fake.sock, PID: os.Getpid(),
	}
	_, err := New().Send(context.Background(), adapter.SendRequest{Target: target, MessageID: ids.New("m"), Body: "x"})
	if model.Code(err) != model.ErrTargetUnavailable || !strings.Contains(err.Error(), "native session ID") {
		t.Fatalf("err=%v", err)
	}
}

func TestSendRejectsKeyPIDDomainMismatch(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	kp := KeyPath(home, os.Getpid(), fake.sock)
	raw, _ := json.Marshal(KeyFile{PeerToken: strings.Repeat("a", 32), PIDDomain: "windows"})
	if err := os.WriteFile(kp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	target := model.Target{
		Adapter: model.AdapterClaude, HostID: "test", ConfigHome: home,
		NativeSessionID: "sid", SocketPath: fake.sock, PID: os.Getpid(),
	}
	_, err := New().Send(context.Background(), adapter.SendRequest{Target: target, MessageID: ids.New("m"), Body: "x"})
	if model.Code(err) != model.ErrTargetUnavailable || !strings.Contains(err.Error(), "pid domain") {
		t.Fatalf("err=%v", err)
	}
}

func TestDiscoveryRejectsEmptySessionID(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.1.303", nil)
	path := filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json")
	rec, err := readRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.SessionID = ""
	raw, _ := json.Marshal(rec)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cands, err := New().Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates=%+v", cands)
	}
}

func TestDiscoveryRejectsSymlinkedRecord(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.1.303", nil)
	real := filepath.Join(home, "sessions", strconv.Itoa(os.Getpid())+".json")
	link := filepath.Join(home, "sessions", "linked.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cands, err := New().Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{home}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("symlinked record was accepted: %+v", cands)
	}
}

func TestDiscoveryHomeIsAbsolute(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.1.303", nil)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, home)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(rel)
	cands, err := New().Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{rel}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Target.ConfigHome != want {
		t.Fatalf("candidates=%+v want home %s", cands, want)
	}
}

func TestDiscoveryRejectsSymlinkedKey(t *testing.T) {
	fake := startFakeInbox(t)
	home := t.TempDir()
	writeSession(t, home, fake.sock, "2.1.303", nil)
	real := filepath.Join(home, "real.key")
	raw, _ := json.Marshal(KeyFile{PeerToken: strings.Repeat("a", 32), PIDDomain: localDomain()})
	if err := os.WriteFile(real, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, KeyPath(home, os.Getpid(), fake.sock)); err != nil {
		t.Fatal(err)
	}
	cands, _ := New().Discover(context.Background(), adapter.DiscoverRequest{Homes: []string{home}})
	if len(cands) != 1 || cands[0].Live || !strings.Contains(cands[0].Reason, "key file rejected") {
		t.Fatalf("candidates=%+v", cands)
	}
}

func mustStart(t *testing.T) string {
	t.Helper()
	s, err := platform.ProcStartUTC(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

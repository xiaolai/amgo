package codex

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"amgo/internal/adapter"
	"amgo/internal/ids"
	"amgo/internal/model"
)

type fakeDaemon struct {
	home string
	sock string
	ln   net.Listener
	srv  *http.Server

	mu           sync.Mutex
	loaded       []string
	threads      []thread
	readThread   thread
	startItem    bool
	wrongThread  bool
	loseResponse bool
	failLoaded   bool
	failRead     bool
	emptyQueueID bool
	emptyClient  bool
	mismatchID   bool
	queueCalls   int
	clientID     string
	userAgent    string
}

func startFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "amgo-cx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	dir := filepath.Join(home, "app-server-control")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "app-server-control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{home: home, sock: sock, ln: ln, userAgent: "codex-tui/0.163.0 (test; arm64) fake/1.0"}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.srv = &http.Server{Handler: mux}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = f.srv.Close()
		_ = ln.Close()
	})
	return f
}

func (f *fakeDaemon) handle(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	defer c.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			continue
		}
		reply := func(result any) {
			if req.ID == nil {
				return
			}
			raw, _ := json.Marshal(map[string]any{"id": *req.ID, "result": result})
			_ = c.Write(ctx, websocket.MessageText, raw)
		}
		switch req.Method {
		case "initialize":
			reply(initializeResponse{UserAgent: f.userAgent, CodexHome: f.home, PlatformFamily: "unix", PlatformOS: "macos"})
		case "initialized":
		case "thread/loaded/list":
			f.mu.Lock()
			loaded := append([]string(nil), f.loaded...)
			fail := f.failLoaded
			f.mu.Unlock()
			if fail {
				if req.ID != nil {
					raw, _ := json.Marshal(map[string]any{"id": *req.ID, "error": map[string]any{"code": -32000, "message": "list unavailable"}})
					_ = c.Write(ctx, websocket.MessageText, raw)
				}
				continue
			}
			reply(loadedListResponse{Data: loaded})
		case "thread/list":
			f.mu.Lock()
			threads := append([]thread(nil), f.threads...)
			f.mu.Unlock()
			reply(threadListResponse{Data: threads})
		case "thread/read":
			var p threadReadParams
			_ = json.Unmarshal(req.Params, &p)
			f.mu.Lock()
			fail := f.failRead
			th := f.readThread
			if th.ID == "" {
				th = thread{ID: p.ThreadID, Status: threadStatus{Type: "idle"}}
			}
			f.mu.Unlock()
			if fail {
				if req.ID != nil {
					raw, _ := json.Marshal(map[string]any{"id": *req.ID, "error": map[string]any{"code": -32000, "message": "read unavailable"}})
					_ = c.Write(ctx, websocket.MessageText, raw)
				}
				continue
			}
			reply(threadReadResponse{Thread: th})
		case "thread/queue/add":
			var p queueAddParams
			_ = json.Unmarshal(req.Params, &p)
			f.mu.Lock()
			f.queueCalls++
			f.clientID = p.ClientUserMessageID
			start := f.startItem
			wrong := f.wrongThread
			lose := f.loseResponse
			emptyID := f.emptyQueueID
			emptyClientID := f.emptyClient
			mismatch := f.mismatchID
			f.mu.Unlock()
			if wrong {
				f.sendItem(c, ctx, "some-other-thread", p.ClientUserMessageID)
			}
			if start {
				f.sendItem(c, ctx, p.ThreadID, p.ClientUserMessageID)
			}
			if lose {
				return
			}
			id := "q1"
			clientID := p.ClientUserMessageID
			if emptyID {
				id = ""
			}
			if emptyClientID {
				clientID = ""
			}
			if mismatch {
				clientID = "someone-else"
			}
			reply(queueAddResponse{QueuedSubmission: queuedSubmission{
				ID: id, ClientUserMessageID: clientID, Input: p.Input,
			}})
		default:
			if req.ID != nil {
				raw, _ := json.Marshal(map[string]any{"id": *req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
				_ = c.Write(ctx, websocket.MessageText, raw)
			}
		}
	}
}

func (f *fakeDaemon) sendItem(c *websocket.Conn, ctx context.Context, threadID, clientID string) {
	n := map[string]any{
		"method": "item/started",
		"params": map[string]any{
			"threadId": threadID,
			"turnId":   "t1",
			"item": map[string]any{
				"type": "userMessage", "id": "i1", "clientId": clientID,
				"content": []map[string]any{{"type": "text", "text": "x"}},
			},
		},
	}
	raw, _ := json.Marshal(n)
	_ = c.Write(ctx, websocket.MessageText, raw)
}

func (f *fakeDaemon) target(threadID string) model.Target {
	return model.Target{
		Adapter: model.AdapterCodex, HostID: "test", ConfigHome: f.home,
		NativeSessionID: threadID, Endpoint: f.sock, SocketPath: f.sock,
	}
}

func TestDiscoverAndLiveClassification(t *testing.T) {
	f := startFakeDaemon(t)
	yes := true
	f.loaded = []string{"t1"}
	f.threads = []thread{
		{ID: "t1", Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes, Cwd: "/tmp"},
		{ID: "t2", Status: threadStatus{Type: "notLoaded"}},
		{ID: "t3", Ephemeral: true, Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes},
	}
	a := New()
	cands, err := a.Discover(context.Background(), adapter.DiscoverRequest{CodexHome: f.home})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates=%+v", cands)
	}
	if !cands[0].Live || cands[0].NativeSessionID != "t1" {
		t.Fatalf("t1=%+v", cands[0])
	}
	if cands[1].Live {
		t.Fatalf("t2 should not be live: %+v", cands[1])
	}
}

func TestSendQueued(t *testing.T) {
	f := startFakeDaemon(t)
	yes := true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes}
	f.loaded = []string{"t1"}
	msgID := ids.New("m")
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: msgID, ConversationID: "c1", Body: "hello", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" || res.NativeDisposition != model.DispositionQueued || res.NativeSubmissionID != "q1" {
		t.Fatalf("result=%+v", res)
	}
	uuid, _ := ids.UUIDPart(msgID)
	f.mu.Lock()
	got := f.clientID
	calls := f.queueCalls
	f.mu.Unlock()
	if calls != 1 || got != uuid {
		t.Fatalf("calls=%d clientID=%q want %q", calls, got, uuid)
	}
}

func TestSendStartedCorrelated(t *testing.T) {
	f := startFakeDaemon(t)
	yes := true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes}
	f.loaded = []string{"t1"}
	f.startItem = true
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), ConversationID: "c1", Body: "hello", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeDisposition != model.DispositionStarted {
		t.Fatalf("result=%+v", res)
	}
}

func TestWrongThreadItemDoesNotMarkStarted(t *testing.T) {
	f := startFakeDaemon(t)
	yes := true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes}
	f.loaded = []string{"t1"}
	f.wrongThread = true
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), ConversationID: "c1", Body: "hello", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.NativeDisposition != model.DispositionQueued {
		t.Fatalf("result=%+v", res)
	}
}

func TestLostResponseWithMatchingItem(t *testing.T) {
	f := startFakeDaemon(t)
	yes := true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes}
	f.loaded = []string{"t1"}
	f.startItem = true
	f.loseResponse = true
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), ConversationID: "c1", Body: "hello", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" || res.NativeDisposition != model.DispositionStarted {
		t.Fatalf("result=%+v", res)
	}
}

func TestUnloadedNeverQueues(t *testing.T) {
	f := startFakeDaemon(t)
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "notLoaded"}}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), ConversationID: "c1", Body: "hello", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "failed_before_submit" || res.ErrorCode != "target_not_live" {
		t.Fatalf("result=%+v", res)
	}
	f.mu.Lock()
	calls := f.queueCalls
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("observed unloaded target queued %d times", calls)
	}
}

func TestEphemeralRefused(t *testing.T) {
	f := startFakeDaemon(t)
	yes := true
	f.readThread = thread{ID: "t1", Ephemeral: true, Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes}
	f.loaded = []string{"t1"}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), ConversationID: "c1", Body: "hello", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "failed_before_submit" || res.ErrorCode != "target_not_live" {
		t.Fatalf("result=%+v", res)
	}
}

func TestHandshakeRejectsUntestedVersion(t *testing.T) {
	f := startFakeDaemon(t)
	f.userAgent = "codex-tui/0.164.0 (test)"
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), ConversationID: "c1", Body: "hello", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "failed_before_submit" || res.ErrorCode != "target_unavailable" {
		t.Fatalf("result=%+v", res)
	}
}

func TestSendRequiresLoadedEvidenceWhenListFails(t *testing.T) {
	f := startFakeDaemon(t)
	f.failLoaded = true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "failed_before_submit" || res.ErrorCode != model.ErrTargetUnavailable {
		t.Fatalf("result=%+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queueCalls != 0 {
		t.Fatalf("queue was called without loaded evidence: %d", f.queueCalls)
	}
}

func TestSendProceedsWhenDirectInputIsExplicit(t *testing.T) {
	f := startFakeDaemon(t)
	yes := true
	f.failLoaded = true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}, CanAcceptDirectInput: &yes}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" {
		t.Fatalf("result=%+v", res)
	}
}

func TestEmptyQueueResponseIsUnknown(t *testing.T) {
	f := startFakeDaemon(t)
	f.emptyQueueID = true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	f.loaded = []string{"t1"}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "unknown" {
		t.Fatalf("result=%+v", res)
	}
}

func TestMismatchedQueueResponseIsUnknown(t *testing.T) {
	f := startFakeDaemon(t)
	f.mismatchID = true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	f.loaded = []string{"t1"}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "unknown" {
		t.Fatalf("result=%+v", res)
	}
}

func TestHandshakeRejectsMissingVersion(t *testing.T) {
	f := startFakeDaemon(t)
	f.userAgent = ""
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	f.loaded = []string{"t1"}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "failed_before_submit" || !strings.Contains(res.ErrorDetail, "handshake") {
		t.Fatalf("result=%+v", res)
	}
}

func TestDiscoverMergesLoadedThreadMissingFromList(t *testing.T) {
	f := startFakeDaemon(t)
	f.loaded = []string{"t-missing"}
	f.readThread = thread{ID: "t-missing", Status: threadStatus{Type: "idle"}, Cwd: "/tmp"}
	cands, err := New().Discover(context.Background(), adapter.DiscoverRequest{CodexHome: f.home})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].NativeSessionID != "t-missing" || !cands[0].Live {
		t.Fatalf("candidates=%+v", cands)
	}
}

func TestDiscoverKeepsLoadedThreadWhenReadFails(t *testing.T) {
	f := startFakeDaemon(t)
	f.loaded = []string{"t-missing"}
	f.failRead = true
	cands, err := New().Discover(context.Background(), adapter.DiscoverRequest{CodexHome: f.home})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].NativeSessionID != "t-missing" || !cands[0].Live {
		t.Fatalf("candidates=%+v", cands)
	}
}

func TestSourceAcceptsStringAndObject(t *testing.T) {
	for _, raw := range []string{`"cli"`, `{"type":"cli"}`} {
		var th thread
		if err := json.Unmarshal([]byte(`{"id":"t1","source":`+raw+`}`), &th); err != nil {
			t.Fatalf("source %s: %v", raw, err)
		}
		if th.ID != "t1" {
			t.Fatalf("thread=%+v", th)
		}
	}
}

func TestLargeBodyAccepted(t *testing.T) {
	f := startFakeDaemon(t)
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	f.loaded = []string{"t1"}
	body := strings.Repeat("x", 64<<10)
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" {
		t.Fatalf("result=%+v", res)
	}
}

func TestHTMLExpandingBodyAccepted(t *testing.T) {
	f := startFakeDaemon(t)
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	f.loaded = []string{"t1"}
	body := strings.Repeat("<", 64<<10)
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" {
		t.Fatalf("result=%+v", res)
	}
}

func TestMissingClientIDIsUnknown(t *testing.T) {
	f := startFakeDaemon(t)
	f.emptyClient = true
	f.readThread = thread{ID: "t1", Status: threadStatus{Type: "idle"}}
	f.loaded = []string{"t1"}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: f.target("t1"), MessageID: ids.New("m"), Body: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "unknown" {
		t.Fatalf("result=%+v", res)
	}
}

func TestSameMinorRejectsNonNumericComponents(t *testing.T) {
	for _, v := range []string{"0.163.garbage", "0.163.", "0.163.0-alpha", "garbage", "0.163"} {
		if sameMinor(v, TestedVersion) {
			t.Errorf("%q was accepted", v)
		}
	}
	if !sameMinor("0.163.0", TestedVersion) || !sameMinor("0.163.9", TestedVersion) {
		t.Fatal("valid versions were rejected")
	}
}

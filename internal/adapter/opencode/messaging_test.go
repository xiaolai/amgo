package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"amgo/internal/adapter"
	"amgo/internal/model"
)

type fakeServer struct {
	mu       sync.Mutex
	prompted []string
	fail     bool
	require  string
}

func newFakeServer(t *testing.T) (*fakeServer, *httptest.Server) {
	t.Helper()
	f := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "1.19.3"})
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "ses_1", "title": "one", "directory": "/work"},
			{"id": "ses_child", "title": "child", "parentID": "ses_1"},
		})
	})
	mux.HandleFunc("/session/ses_1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_1", "title": "one", "directory": "/work"})
	})
	mux.HandleFunc("/session/ses_1/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		if f.require != "" {
			u, p, ok := r.BasicAuth()
			if !ok || u+":"+p != f.require {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		var body struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		for _, p := range body.Parts {
			f.prompted = append(f.prompted, p.Text)
		}
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestDiscoverExplicitEndpoint(t *testing.T) {
	_, srv := newFakeServer(t)
	cands, err := New().Discover(context.Background(), adapter.DiscoverRequest{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || !cands[0].Live || cands[0].Target.NativeSessionID != "ses_1" {
		t.Fatalf("candidates=%+v", cands)
	}
	if cands[0].Target.Version != "1.19.3" || cands[0].Target.Endpoint != srv.URL {
		t.Fatalf("target=%+v", cands[0].Target)
	}
	if cands, _ := New().Discover(context.Background(), adapter.DiscoverRequest{}); len(cands) != 0 {
		t.Fatalf("discovery without an endpoint guessed: %+v", cands)
	}
}

func TestSendAccepted(t *testing.T) {
	f, srv := newFakeServer(t)
	target := model.Target{Adapter: model.AdapterOpenCode, Endpoint: srv.URL, NativeSessionID: "ses_1", Version: "1.19.3"}
	res, err := New().Send(context.Background(), adapter.SendRequest{
		Target: target, MessageID: "m_1", ConversationID: "c_1", Body: "hello opencode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "submitted" || res.NativeDisposition != model.DispositionUnknown {
		t.Fatalf("result=%+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompted) != 1 || !strings.Contains(f.prompted[0], "hello opencode") {
		t.Fatalf("prompted=%v", f.prompted)
	}
}

func TestSendPreflightAndFailureMapping(t *testing.T) {
	f, srv := newFakeServer(t)
	a := New()
	target := model.Target{Adapter: model.AdapterOpenCode, Endpoint: srv.URL, NativeSessionID: "ses_1", Version: "1.19.3"}
	changed := target
	changed.Version = "0.0.1"
	if _, err := a.Send(context.Background(), adapter.SendRequest{Target: changed, MessageID: "m", ConversationID: "c", Body: "x"}); model.Code(err) != model.ErrTargetUnavailable {
		t.Fatalf("version err=%v", err)
	}
	missing := target
	missing.NativeSessionID = "ses_missing"
	if res, err := a.Send(context.Background(), adapter.SendRequest{Target: missing, MessageID: "m", ConversationID: "c", Body: "x"}); err == nil || res.Outcome != "failed_before_submit" {
		t.Fatalf("missing session res=%+v err=%v", res, err)
	}

	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	res, err := a.Send(context.Background(), adapter.SendRequest{Target: target, MessageID: "m", ConversationID: "c", Body: "x"})
	if err != nil || res.Outcome != "unknown" {
		t.Fatalf("500 res=%+v err=%v", res, err)
	}
}

func TestSendBasicAuthFromEnvironment(t *testing.T) {
	f, srv := newFakeServer(t)
	f.require = "opencode:secret"
	t.Setenv("OPENCODE_SERVER_PASSWORD", "secret")
	t.Setenv("OPENCODE_SERVER_USERNAME", "")
	target := model.Target{Adapter: model.AdapterOpenCode, Endpoint: srv.URL, NativeSessionID: "ses_1", Version: "1.19.3"}
	res, err := New().Send(context.Background(), adapter.SendRequest{Target: target, MessageID: "m", ConversationID: "c", Body: "x"})
	if err != nil || res.Outcome != "submitted" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestLoopbackOnly(t *testing.T) {
	for _, endpoint := range []string{"http://example.com:4096", "https://127.0.0.1:4096", "http://127.0.0.1:4096/api"} {
		if _, err := newAPI(endpoint); err == nil {
			t.Fatalf("endpoint %q was accepted", endpoint)
		}
	}
}

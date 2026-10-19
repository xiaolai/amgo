package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"amgo/internal/adapter"
)

func TestJoinAndSendOpenCode(t *testing.T) {
	prompted := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "1.19.3"})
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "ses_1", "title": "one", "directory": "/work"}})
	})
	mux.HandleFunc("/session/ses_1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_1", "title": "one"})
	})
	mux.HandleFunc("/session/ses_1/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		prompted <- string(raw)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	home := t.TempDir()
	code, _, stderr := runJobCLI(t, "", "--home", home, "join", "opencode",
		"--name", "oc", "--session", "ses_1", "--endpoint", srv.URL)
	if code != 0 {
		t.Fatalf("join exit=%d stderr=%s", code, stderr)
	}
	code, env, stderr := runJobCLI(t, "hello opencode", "--home", home, "--json", "send", "oc", "--stdin")
	if code != 0 {
		t.Fatalf("send exit=%d stderr=%s", code, stderr)
	}
	if env.Data["submission"] != "submitted" {
		t.Fatalf("send data=%+v", env.Data)
	}
	select {
	case body := <-prompted:
		if !strings.Contains(body, "hello opencode") {
			t.Fatalf("prompt body=%s", body)
		}
	default:
		t.Fatal("OpenCode server received no prompt")
	}
}

func TestDocumentedOnlyJobsRequireAdmission(t *testing.T) {
	for _, name := range []string{"qwen", "agy"} {
		code, _, stderr := runJobCLI(t, "", "--home", t.TempDir(), "run", name, "--text", "x")
		if code != 6 || !strings.Contains(stderr, "documented-only") {
			t.Fatalf("%s run exit=%d stderr=%s", name, code, stderr)
		}
	}
}

func TestIntegrationSnippetAndContract(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runWith([]string{"integration", "snippet", "--target", "agents"}, strings.NewReader(""), &out, &errOut, map[string]adapter.Adapter{})
	if code != 0 || !strings.Contains(out.String(), "Only completed is success") {
		t.Fatalf("snippet exit=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = runWith([]string{"--json", "integration", "contract"}, strings.NewReader(""), &out, &errOut, map[string]adapter.Adapter{})
	if code != 0 {
		t.Fatalf("contract exit=%d err=%s", code, errOut.String())
	}
	var env cliEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data["envelope"] == nil || env.Data["commands"] == nil {
		t.Fatalf("contract data=%+v", env.Data)
	}
}

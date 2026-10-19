package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"time"
)

const apiTimeout = 5 * time.Second

type api struct {
	base   string
	client *http.Client
	user   string
	pass   string
}

func newAPI(endpoint string) (*api, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("endpoint must be an http URL")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("endpoint must be loopback; remote OpenCode servers are not supported")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("endpoint must not contain a path")
	}
	pass := os.Getenv("OPENCODE_SERVER_PASSWORD")
	user := os.Getenv("OPENCODE_SERVER_USERNAME")
	if user == "" {
		user = "opencode"
	}
	return &api{
		base:   strings.TrimRight(endpoint, "/"),
		client: &http.Client{Timeout: apiTimeout},
		user:   user, pass: pass,
	}, nil
}

func (a *api) request(ctx context.Context, method, path string, body any, out any, connected *bool) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return 0, err
	}
	if connected != nil {
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { *connected = true },
		}))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.pass != "" {
		req.SetBasicAuth(a.user, a.pass)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("invalid response: %w", err)
		}
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("opencode returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp.StatusCode, nil
}

type health struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version"`
}

func (a *api) health(ctx context.Context) (health, error) {
	var h health
	_, err := a.request(ctx, http.MethodGet, "/global/health", nil, &h, nil)
	return h, err
}

type session struct {
	ID        string `json:"id"`
	ParentID  string `json:"parentID"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
}

func (a *api) sessions(ctx context.Context) ([]session, error) {
	var out []session
	_, err := a.request(ctx, http.MethodGet, "/session", nil, &out, nil)
	return out, err
}

func (a *api) session(ctx context.Context, id string) (session, error) {
	var out session
	_, err := a.request(ctx, http.MethodGet, "/session/"+url.PathEscape(id), nil, &out, nil)
	return out, err
}

func (a *api) promptAsync(ctx context.Context, id, text string, connected *bool) (int, error) {
	body := map[string]any{
		"parts": []map[string]any{{"type": "text", "text": text}},
	}
	return a.request(ctx, http.MethodPost, "/session/"+url.PathEscape(id)+"/prompt_async", body, nil, connected)
}

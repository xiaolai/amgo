package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"amgo/internal/model"
	"amgo/internal/runner"
)

const (
	rpcMethodNotFound = -32601
	rpcAuthRequired   = -32000
	requestTimeout    = 30 * time.Second
	maxEvidenceBytes  = 1 << 20
)

// Driver owns one ACP conversation over the runner's stdio pipes.
type Driver struct {
	Workspace string
	Prompt    string
	SessionID string
	Version   string
}

func (d *Driver) Run(ctx context.Context, dio *runner.DriverIO) (runner.Result, error) {
	c := newClient(dio, d.Version)
	caps, authMethods, err := c.initialize(ctx)
	if err != nil {
		return runner.Result{}, err
	}
	_ = c.emit(runner.Event{Kind: "progress", Payload: map[string]any{
		"event": "initialized", "protocol_version": ProtocolVersion,
		"load_session": caps.LoadSession, "auth_methods": len(authMethods),
	}})
	sid := d.SessionID
	if sid != "" {
		if !caps.LoadSession {
			return runner.Result{}, model.NewError(model.ErrUnsupported,
				"ACP agent does not advertise session/load; continuation is refused")
		}
		if err := c.loadSession(ctx, sid, d.Workspace); err != nil {
			var re *rpcError
			if errors.As(err, &re) && re.Code == rpcAuthRequired {
				return runner.Result{
					Outcome: model.RunNeedsAction, ErrorCode: model.ErrProviderNeedsAction,
					ErrorDetail: authDetail(authMethods),
				}, nil
			}
			return runner.Result{}, err
		}
	} else {
		sid, err = c.newSession(ctx, d.Workspace)
		if err != nil {
			var re *rpcError
			if errors.As(err, &re) && re.Code == rpcAuthRequired {
				return runner.Result{
					Outcome: model.RunNeedsAction, ErrorCode: model.ErrProviderNeedsAction,
					ErrorDetail: authDetail(authMethods),
				}, nil
			}
			return runner.Result{}, err
		}
	}
	c.setSession(sid)
	_ = c.emit(runner.Event{Kind: "native_session", Payload: map[string]any{"session_id": sid}})
	return c.prompt(ctx, sid, d.Prompt)
}

func authDetail(methods []authMethod) string {
	if len(methods) == 0 {
		return "ACP agent requires authentication; complete its login flow outside amgo"
	}
	names := make([]string, 0, len(methods))
	for _, m := range methods {
		names = append(names, m.Name)
	}
	return "ACP agent requires authentication; available methods: " + strings.Join(names, ", ")
}

type authMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type agentCaps struct {
	LoadSession bool `json:"loadSession"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("ACP error %d", e.Code)
	}
	return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message)
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type client struct {
	dio     *runner.DriverIO
	version string

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan rpcMessage
	nextID  int64

	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	readErr   error

	stateMu     sync.Mutex
	sessionID   string
	text        strings.Builder
	needsAction bool
	detail      string
	cancelled   bool
}

func newClient(dio *runner.DriverIO, version string) *client {
	c := &client{dio: dio, version: version, pending: map[int64]chan rpcMessage{}, done: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *client) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	c.errMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.errMu.Unlock()
	c.closeOnce.Do(func() { close(c.done) })
}

func (c *client) readError() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.readErr == nil {
		return io.EOF
	}
	return c.readErr
}

func (c *client) readLoop() {
	for {
		line, err := c.dio.Next()
		if err != nil {
			c.fail(err)
			return
		}
		raw := bytes.TrimSpace(line)
		if len(raw) == 0 {
			continue
		}
		if raw[0] == '[' {
			var batch []json.RawMessage
			if err := json.Unmarshal(raw, &batch); err != nil {
				c.fail(model.WrapError(model.ErrEventParse, "ACP emitted an invalid JSON batch", err))
				return
			}
			for _, one := range batch {
				if !c.dispatch(one) {
					return
				}
			}
			continue
		}
		if !c.dispatch(raw) {
			return
		}
	}
}

func (c *client) dispatch(raw []byte) bool {
	var m rpcMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		c.fail(model.WrapError(model.ErrEventParse, "ACP emitted invalid JSON", err))
		return false
	}
	switch {
	case m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null":
		c.handleRequest(m)
	case m.Method != "":
		c.handleNotification(m)
	case len(m.ID) > 0:
		c.handleResponse(m)
	}
	return true
}

func (c *client) handleResponse(m rpcMessage) {
	var id int64
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
	}
}

func (c *client) handleRequest(m rpcMessage) {
	switch {
	case m.Method == "session/request_permission":
		var p struct {
			SessionID string `json:"sessionId"`
			ToolCall  struct {
				Title string `json:"title"`
				Kind  string `json:"kind"`
			} `json:"toolCall"`
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(m.Params, &p)
		choice := ""
		for _, o := range p.Options {
			if o.Kind == "reject_once" {
				choice = o.OptionID
				break
			}
			if o.Kind == "reject_always" {
				choice = o.OptionID
			}
		}
		c.stateMu.Lock()
		cancelled := c.cancelled || (c.sessionID != "" && p.SessionID != "" && p.SessionID != c.sessionID)
		c.stateMu.Unlock()
		outcome := map[string]any{"outcome": "cancelled"}
		if choice != "" && !cancelled {
			outcome = map[string]any{"outcome": "selected", "optionId": choice}
		}
		_ = c.respond(m.ID, map[string]any{"outcome": outcome})
		c.stateMu.Lock()
		c.needsAction = true
		c.detail = "agent requested permission for " + truncate(p.ToolCall.Title, 200) + "; amgo rejected it"
		c.stateMu.Unlock()
		_ = c.emit(runner.Event{Kind: "permission", Payload: map[string]any{
			"tool": truncate(p.ToolCall.Title, 200), "kind": truncate(p.ToolCall.Kind, 40),
			"decision": "rejected",
		}})
	case m.Method == "fs/read_text_file", m.Method == "fs/write_text_file", strings.HasPrefix(m.Method, "terminal/"):
		_ = c.respondError(m.ID, rpcMethodNotFound, "amgo does not provide filesystem or terminal capabilities")
		c.stateMu.Lock()
		c.needsAction = true
		c.detail = "agent requested " + m.Method + ", which amgo does not provide"
		c.stateMu.Unlock()
	default:
		_ = c.respondError(m.ID, rpcMethodNotFound, "method not found")
	}
}

func (c *client) handleNotification(m rpcMessage) {
	if m.Method != "session/update" {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			MessageID     string `json:"messageId"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Title  string `json:"title"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
			Used   uint64 `json:"used"`
			Size   uint64 `json:"size"`
		} `json:"update"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return
	}
	c.stateMu.Lock()
	ownSession := c.sessionID
	c.stateMu.Unlock()
	if p.SessionID != "" && ownSession != "" && p.SessionID != ownSession {
		return
	}
	switch p.Update.SessionUpdate {
	case "agent_message_chunk":
		if p.Update.Content.Type == "text" && p.Update.Content.Text != "" {
			c.stateMu.Lock()
			if c.text.Len() < maxEvidenceBytes {
				n := maxEvidenceBytes - c.text.Len()
				t := p.Update.Content.Text
				if len(t) > n {
					t = t[:n]
				}
				c.text.WriteString(t)
			}
			c.stateMu.Unlock()
			_ = c.emit(runner.Event{Kind: "progress", Payload: map[string]any{
				"event": "agent_message_chunk", "chars": len(p.Update.Content.Text),
			}})
		}
	case "tool_call", "tool_call_update":
		_ = c.emit(runner.Event{Kind: "progress", Payload: map[string]any{
			"event": p.Update.SessionUpdate, "title": truncate(p.Update.Title, 200),
			"kind": truncate(p.Update.Kind, 40), "status": truncate(p.Update.Status, 40),
		}})
	case "usage_update":
		_ = c.emit(runner.Event{Kind: "progress", Payload: map[string]any{
			"event": "usage_update", "used": p.Update.Used, "size": p.Update.Size,
		}})
	default:
		if p.Update.SessionUpdate != "" {
			_ = c.emit(runner.Event{Kind: "progress", Payload: map[string]any{
				"event": p.Update.SessionUpdate,
			}})
		}
	}
}

func (c *client) emit(ev runner.Event) error {
	if c.dio.Emit == nil {
		return nil
	}
	err := c.dio.Emit(ev)
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *client) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.dio.In.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *client) call(ctx context.Context, method string, params any, out any) error {
	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.write(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, out); err != nil {
				return model.WrapError(model.ErrEventParse, "invalid ACP response", err)
			}
		}
		return nil
	case <-c.done:
		return c.readError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *client) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *client) respond(id json.RawMessage, result any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *client) respondError(id json.RawMessage, code int, message string) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
		"code": code, "message": message,
	}})
}

func (c *client) initialize(ctx context.Context) (agentCaps, []authMethod, error) {
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var res struct {
		ProtocolVersion   int          `json:"protocolVersion"`
		AgentCapabilities agentCaps    `json:"agentCapabilities"`
		AuthMethods       []authMethod `json:"authMethods"`
	}
	err := c.call(callCtx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "amgo", "version": c.version},
	}, &res)
	if err != nil {
		return agentCaps{}, nil, err
	}
	if res.ProtocolVersion != ProtocolVersion {
		return agentCaps{}, nil, model.NewError(model.ErrUnsupported,
			fmt.Sprintf("ACP agent selected protocol version %d; amgo supports %d", res.ProtocolVersion, ProtocolVersion))
	}
	return res.AgentCapabilities, res.AuthMethods, nil
}

func (c *client) newSession(ctx context.Context, cwd string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var res struct {
		SessionID string `json:"sessionId"`
	}
	err := c.call(callCtx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, &res)
	if err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", model.NewError(model.ErrProviderFailed, "ACP agent returned no sessionId")
	}
	return res.SessionID, nil
}

func (c *client) loadSession(ctx context.Context, sessionID, cwd string) error {
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return c.call(callCtx, "session/load", map[string]any{
		"sessionId": sessionID, "cwd": cwd, "mcpServers": []any{},
	}, nil)
}

func (c *client) setSession(id string) {
	c.stateMu.Lock()
	c.sessionID = id
	c.stateMu.Unlock()
}

func (c *client) prompt(ctx context.Context, sessionID, text string) (runner.Result, error) {
	var res struct {
		StopReason string `json:"stopReason"`
	}
	err := c.call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	}, &res)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			c.stateMu.Lock()
			c.cancelled = true
			c.stateMu.Unlock()
			_ = c.notify("session/cancel", map[string]any{"sessionId": sessionID})
			return c.result(""), nil
		}
		var re *rpcError
		if errors.As(err, &re) && re.Code == rpcAuthRequired {
			return runner.Result{
				Outcome: model.RunNeedsAction, ErrorCode: model.ErrProviderNeedsAction,
				ErrorDetail: "ACP agent requires authentication before prompting",
			}, nil
		}
		return c.result(""), err
	}
	return c.result(res.StopReason), nil
}

func (c *client) result(stopReason string) runner.Result {
	c.stateMu.Lock()
	text := c.text.String()
	needsAction := c.needsAction
	detail := c.detail
	sid := c.sessionID
	c.stateMu.Unlock()
	res := runner.Result{Outcome: model.RunCompleted, Evidence: text, NativeSessionID: sid}
	switch {
	case needsAction:
		res.Outcome = model.RunNeedsAction
		res.ErrorCode = model.ErrProviderNeedsAction
		res.ErrorDetail = detail
	case stopReason == "end_turn":
		res.Outcome = model.RunCompleted
	case stopReason == "refusal":
		res.Outcome = model.RunNeedsAction
		res.ErrorCode = model.ErrProviderNeedsAction
		res.ErrorDetail = "agent refused to continue"
	case stopReason == "max_tokens", stopReason == "max_turn_requests":
		res.Outcome = model.RunPartial
		res.ErrorDetail = "agent stopped at " + stopReason
	case stopReason == "cancelled":
		res.Outcome = model.RunPartial
		res.ErrorDetail = "agent reported cancellation"
	case stopReason == "":
		res.Outcome = model.RunFailed
		res.ErrorCode = model.ErrProviderFailed
		res.ErrorDetail = "agent returned no stop reason"
	default:
		res.Outcome = model.RunFailed
		res.ErrorCode = model.ErrProviderFailed
		res.ErrorDetail = "unrecognized stop reason " + stopReason
	}
	return res
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

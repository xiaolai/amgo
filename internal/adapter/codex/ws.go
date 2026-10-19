package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const maxNotifications = 256

var (
	// ErrResponseLost means the request may have been delivered but no response
	// was read. Callers must treat the operation as ambiguous.
	ErrResponseLost = errors.New("rpc response lost")
	// ErrNotSent means the request could not be written. No provider side
	// effect is known.
	ErrNotSent = errors.New("rpc request not sent")
)

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message) }

type client struct {
	conn          *websocket.Conn
	nextID        int64
	notifications []rpcNotification
	mu            sync.Mutex
}

func dial(ctx context.Context, socket string, timeout time.Duration) (*client, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, "unix", socket)
		},
	}
	httpClient := &http.Client{Transport: tr}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, _, err := websocket.Dial(dialCtx, "ws://localhost/", &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(8 << 20)
	return &client{conn: c}, nil
}

func (c *client) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *client) call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = c.conn.Write(writeCtx, websocket.MessageText, raw)
	cancel()
	if err != nil {
		// A failed WebSocket write may have transmitted a partial frame. Only a
		// pre-write cancellation proves no byte was sent.
		return fmt.Errorf("%w: %v", ErrResponseLost, err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		_, data, err := c.conn.Read(readCtx)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResponseLost, err)
		}
		var envelope struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		if envelope.Method != "" && envelope.ID == nil {
			switch envelope.Method {
			case "item/started", "item/completed":
				if len(c.notifications) < maxNotifications {
					c.notifications = append(c.notifications, rpcNotification{Method: envelope.Method, Params: envelope.Params})
				}
			}
			continue
		}
		if envelope.ID == nil || *envelope.ID != id {
			continue
		}
		if envelope.Error != nil {
			return envelope.Error
		}
		if result != nil {
			return json.Unmarshal(envelope.Result, result)
		}
		return nil
	}
}

func (c *client) notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := map[string]any{"method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrNotSent, err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("%w: %v", ErrResponseLost, err)
	}
	return nil
}

// takeNotifications returns and clears notifications seen during calls.
func (c *client) takeNotifications() []rpcNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.notifications
	c.notifications = nil
	return out
}

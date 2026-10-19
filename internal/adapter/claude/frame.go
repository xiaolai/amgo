package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/ids"
	"amgo/internal/model"
)

type frameMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type userFrame struct {
	MsgV      int          `json:"msgV"`
	MsgID     string       `json:"msg_id"`
	UUID      string       `json:"uuid"`
	Type      string       `json:"type"`
	SessionID string       `json:"session_id,omitempty"`
	Message   frameMessage `json:"message"`
	Priority  string       `json:"priority"`
}

type authFrame struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

func buildFrame(req adapter.SendRequest, token string) ([]byte, error) {
	uuid, err := ids.UUIDPart(req.MessageID)
	if err != nil {
		uuid = ids.NewUUID()
	}
	uf := userFrame{
		MsgV:      1,
		MsgID:     uuid,
		UUID:      uuid,
		Type:      "user",
		SessionID: req.Target.NativeSessionID,
		Message:   frameMessage{Role: "user", Content: adapter.FrameBody(req)},
		Priority:  "next",
	}
	line, err := json.Marshal(uf)
	if err != nil {
		return nil, err
	}
	if len(line) > maxLineBytes {
		return nil, model.NewError(model.ErrUsage, "encoded Claude frame exceeds the 1 MiB line limit")
	}
	var out []byte
	if token != "" {
		auth, err := json.Marshal(authFrame{Type: "auth", Token: token})
		if err != nil {
			return nil, err
		}
		out = append(out, auth...)
		out = append(out, '\n')
	}
	out = append(out, line...)
	out = append(out, '\n')
	return out, nil
}

func writeFrame(ctx context.Context, socket string, frame []byte, timeout time.Duration) (adapter.SendResult, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if err := ctx.Err(); err != nil {
		return adapter.SendResult{
			Outcome:           "failed_before_submit",
			NativeDisposition: model.DispositionUnknown,
			ErrorCode:         model.ErrInterrupted,
			ErrorDetail:       "interrupted before dialing",
			Evidence:          "no byte was written",
		}, nil
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if ctx.Err() != nil {
			return adapter.SendResult{
				Outcome:           "failed_before_submit",
				NativeDisposition: model.DispositionUnknown,
				ErrorCode:         model.ErrInterrupted,
				ErrorDetail:       "interrupted during dial",
				Evidence:          "no byte was written",
			}, nil
		}
		return adapter.SendResult{
			Outcome:           "failed_before_submit",
			NativeDisposition: model.DispositionUnknown,
			Evidence:          "dial failed before any byte was written",
		}, nil
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetWriteDeadline(deadline)
	// Cancellation cannot interrupt a blocked Write directly; move the write
	// deadline to now so the write returns promptly.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetWriteDeadline(time.Now())
		case <-watchDone:
		}
	}()
	total := 0
	for total < len(frame) {
		if ctx.Err() != nil {
			if total == 0 {
				return adapter.SendResult{
					Outcome:           "failed_before_submit",
					NativeDisposition: model.DispositionUnknown,
					ErrorCode:         model.ErrInterrupted,
					ErrorDetail:       "interrupted before any byte was written",
					Evidence:          "no byte was written",
				}, nil
			}
			return adapter.SendResult{
				Outcome:           "unknown",
				NativeDisposition: model.DispositionUnknown,
				ErrorCode:         model.ErrInterrupted,
				ErrorDetail:       "interrupted after the frame was partially written",
				Evidence:          fmt.Sprintf("write interrupted after %d bytes; delivery is ambiguous", total),
			}, nil
		}
		n, werr := conn.Write(frame[total:])
		total += n
		if werr != nil {
			if ctx.Err() != nil {
				if total == 0 {
					return adapter.SendResult{
						Outcome:           "failed_before_submit",
						NativeDisposition: model.DispositionUnknown,
						ErrorCode:         model.ErrInterrupted,
						ErrorDetail:       "interrupted before any byte was written",
						Evidence:          "no byte was written",
					}, nil
				}
				return adapter.SendResult{
					Outcome:           "unknown",
					NativeDisposition: model.DispositionUnknown,
					ErrorCode:         model.ErrInterrupted,
					ErrorDetail:       "interrupted after the frame was partially written",
					Evidence:          fmt.Sprintf("write interrupted after %d bytes; delivery is ambiguous", total),
				}, nil
			}
			if total == 0 {
				return adapter.SendResult{
					Outcome:           "failed_before_submit",
					NativeDisposition: model.DispositionUnknown,
					Evidence:          "write failed before any byte was written",
				}, nil
			}
			return adapter.SendResult{
				Outcome:           "unknown",
				NativeDisposition: model.DispositionUnknown,
				Evidence:          fmt.Sprintf("write failed after %d bytes; delivery is ambiguous", total),
			}, nil
		}
	}
	return adapter.SendResult{
		Outcome:           "submitted",
		NativeDisposition: model.DispositionUnknown,
		Evidence:          "Claude inbox socket accepted the frame; no native receipt is available to an external sender",
	}, nil
}

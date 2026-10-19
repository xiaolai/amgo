package qwen

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/ids"
	"amgo/internal/model"
)

const (
	ipcTimeout    = 5 * time.Second
	receiptWindow = 2 * time.Second
	maxFrameBytes = 1 << 20
)

type authFrame struct {
	MsgV  int    `json:"msgV"`
	Type  string `json:"type"`
	Token string `json:"token"`
}

type userFrame struct {
	MsgV        int    `json:"msgV"`
	MsgID       string `json:"msgId"`
	Type        string `json:"type"`
	From        string `json:"from,omitempty"`
	ReplyToken  string `json:"replyToken,omitempty"`
	FromName    string `json:"fromName,omitempty"`
	ToSessionID string `json:"toSessionId"`
	Priority    string `json:"priority"`
	Message     struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

type controlFrame struct {
	MsgV       int    `json:"msgV"`
	MsgID      string `json:"msgId"`
	Type       string `json:"type"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	OrigMsgID  string `json:"origMsgId"`
	From       string `json:"from"`
	Reason     string `json:"reason"`
	DropReason string `json:"dropReason"`
}

func send(ctx context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	if !req.AdmitExperimental {
		return adapter.SendResult{}, model.NewError(model.ErrPolicyRefused,
			"qwen cross-session messaging is documented-only; pass --admit-experimental after reviewing the constraints")
	}
	rec, err := recordForTarget(req.Target)
	if err != nil {
		return adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.Code(err), ErrorDetail: err.Error()},
			err
	}
	dir, inboxPath, replyToken, ln, err := newReceiptInbox()
	if err != nil {
		return adapter.SendResult{}, model.WrapError(model.ErrInternal, "cannot create the qwen receipt inbox", err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}()

	msgID := ids.NewUUID()
	frame := userFrame{
		MsgV: 1, MsgID: msgID, Type: "user", From: inboxPath, ReplyToken: replyToken,
		FromName: "amgo", ToSessionID: rec.SessionID, Priority: "next",
	}
	frame.Message.Role = "user"
	frame.Message.Content = adapter.FrameBody(req)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if rec.IPCToken != "" {
		if err := enc.Encode(authFrame{MsgV: 1, Type: "auth", Token: rec.IPCToken}); err != nil {
			return adapter.SendResult{}, err
		}
	}
	if err := enc.Encode(frame); err != nil {
		return adapter.SendResult{}, err
	}

	timeout := req.Timeout
	if timeout <= 0 || timeout > ipcTimeout {
		timeout = ipcTimeout
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", rec.IPCPath)
	if err != nil {
		return adapter.SendResult{
			Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable,
			ErrorDetail: "cannot dial the qwen inbox",
		}, model.WrapError(model.ErrTargetUnavailable, "cannot dial the qwen inbox", err)
	}
	n, werr := conn.Write(buf.Bytes())
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	_ = conn.Close()
	if n == 0 && werr != nil {
		return adapter.SendResult{
			Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable,
			ErrorDetail: "the qwen inbox write failed before any frame byte",
		}, model.WrapError(model.ErrTargetUnavailable, "the qwen inbox write failed", werr)
	}
	if werr != nil || n != buf.Len() {
		return adapter.SendResult{
			Outcome: "unknown", ErrorCode: model.ErrUnknownSubmission,
			NativeDisposition: model.DispositionUnknown,
			ErrorDetail:       "the qwen frame write may have partially reached the inbox",
		}, nil
	}

	status := awaitReceipt(ln, replyToken, msgID)
	disposition := dispositionFor(status)
	evidence := "no qwen receipt within the bounded window; delivery is unverified"
	if status != "" {
		evidence = "qwen receipt: " + status
	}
	return adapter.SendResult{
		Outcome: "submitted", NativeDisposition: disposition, Evidence: evidence,
	}, nil
}

// newReceiptInbox binds the sender's short-lived receipt socket. The protocol
// requires paths under 103 bytes, so /tmp is preferred over a long TMPDIR.
func newReceiptInbox() (dir, path, token string, ln net.Listener, err error) {
	base := "/tmp"
	if fi, statErr := os.Stat(base); statErr != nil || !fi.IsDir() {
		base = os.TempDir()
	}
	dir, err = os.MkdirTemp(base, "amgo-qw-")
	if err != nil {
		return "", "", "", nil, err
	}
	path = filepath.Join(dir, "r.sock")
	if len(path) > 100 {
		_ = os.RemoveAll(dir)
		return "", "", "", nil, fmt.Errorf("receipt socket path is too long")
	}
	ln, err = net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", "", nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return "", "", "", nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return "", "", "", nil, err
	}
	return dir, path, hex.EncodeToString(raw), ln, nil
}

// awaitReceipt accepts authenticated delivery-status frames until a final
// status or the bounded window. Duplicates and out-of-order transitions are
// ignored per the documented state machine.
func awaitReceipt(ln net.Listener, token, msgID string) string {
	if ul, ok := ln.(*net.UnixListener); ok {
		_ = ul.SetDeadline(time.Now().Add(receiptWindow))
	}
	status := ""
	for {
		conn, err := ln.Accept()
		if err != nil {
			return status
		}
		_ = conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 4096), maxFrameBytes)
		authed := false
		for sc.Scan() {
			var head struct {
				MsgV      int    `json:"msgV"`
				Type      string `json:"type"`
				Token     string `json:"token"`
				MsgID     string `json:"msgId"`
				Action    string `json:"action"`
				Status    string `json:"status"`
				OrigMsgID string `json:"origMsgId"`
			}
			if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
				break
			}
			if head.Type == "auth" {
				if head.Token != token {
					break
				}
				authed = true
				continue
			}
			if !authed || head.Type != "control" || head.Action != "delivery_status" || head.OrigMsgID != msgID {
				continue
			}
			status = transition(status, head.Status)
			if finalStatus(status) {
				_ = conn.Close()
				return status
			}
		}
		_ = conn.Close()
	}
}

func transition(current, next string) string {
	if current == "" {
		switch next {
		case "held", "delivered", "denied", "refused", "expired", "misaddressed", "dropped":
			return next
		}
		return current
	}
	switch current {
	case "held":
		switch next {
		case "delivered", "denied", "expired", "misaddressed":
			return next
		}
	case "delivered":
		switch next {
		case "expired", "misaddressed":
			return next
		}
	}
	return current
}

func finalStatus(s string) bool {
	switch s {
	case "delivered", "denied", "refused", "expired", "misaddressed", "dropped":
		return true
	}
	return false
}

func dispositionFor(status string) string {
	switch status {
	case "delivered":
		return model.DispositionQueued
	case "held":
		return model.DispositionHeld
	case "denied":
		return model.DispositionDenied
	case "refused":
		return model.DispositionRefused
	case "expired":
		return model.DispositionExpired
	case "dropped":
		return model.DispositionDropped
	case "misaddressed":
		return model.DispositionMisaddressed
	default:
		return model.DispositionUnknown
	}
}

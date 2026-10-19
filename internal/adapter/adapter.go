// Package adapter defines the provider-independent capability boundary.
package adapter

import (
	"context"
	"fmt"
	"time"

	"amgo/internal/model"
)

type DiscoverRequest struct {
	Homes     []string
	CodexHome string
	Timeout   time.Duration
}

type SendRequest struct {
	Target         model.Target
	MessageID      string
	ConversationID string
	RetryOf        string
	ReplyTo        string
	SenderLabel    string
	Body           string
	ExpectReply    bool
	Timeout        time.Duration
}

type SendResult struct {
	Outcome            string // submitted | failed_before_submit | unknown
	NativeSubmissionID string
	NativeDisposition  string
	Evidence           string
	ErrorCode          string
	ErrorDetail        string
}

type Adapter interface {
	Name() string
	Probe(ctx context.Context) (model.Capabilities, error)
	Discover(ctx context.Context, req DiscoverRequest) ([]model.Candidate, error)
	Send(ctx context.Context, req SendRequest) (SendResult, error)
}

// FrameBody is the plain-text envelope both adapters deliver. It never mimics
// a native peer wrapper and never claims provider authority.
func FrameBody(req SendRequest) string {
	from := req.SenderLabel
	if from == "" {
		from = "operator"
	}
	out := fmt.Sprintf("[amgo message %s from %s conversation %s]\n%s", req.MessageID, from, req.ConversationID, req.Body)
	if req.RetryOf != "" {
		out += fmt.Sprintf("\n[amgo: operator-authorized retry of %s; a duplicate native submission is possible]", req.RetryOf)
	}
	if req.ExpectReply {
		out += fmt.Sprintf("\n[amgo: reply with `amgo reply %s --stdin`]", req.MessageID)
	}
	return out
}

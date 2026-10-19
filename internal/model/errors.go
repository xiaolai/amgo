package model

import (
	"errors"
	"fmt"
)

// Error codes are stable identifiers in the JSON envelope.
const (
	ErrUsage              = "usage_error"
	ErrInternal           = "internal_error"
	ErrUnsupported        = "unsupported_capability"
	ErrAmbiguousTarget    = "ambiguous_target"
	ErrIdemConflict       = "idempotency_conflict"
	ErrConversationClosed = "conversation_closed"
	ErrBudgetExhausted    = "budget_exhausted"
	ErrTargetUnavailable  = "target_unavailable"
	ErrTargetNotLive      = "target_not_live"
	ErrNativeHold         = "native_hold"
	ErrUnknownSubmission  = "unknown_submission"
	ErrOutcomeUnpersisted = "outcome_unpersisted"
	ErrRefundPending      = "refund_pending"
	ErrDeadlineExceeded   = "deadline_exceeded"
	ErrStoreBusy          = "store_busy"
	ErrStateIncompatible  = "state_incompatible"
	ErrInterrupted        = "interrupted"
	ErrDispatchDeadline   = "dispatch_deadline_exceeded"
	ErrPolicyRefused      = "policy_refused"
)

// CodedError is the only error type that carries a public error code.
type CodedError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
	Cause     error          `json:"-"`
}

func (e *CodedError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *CodedError) Unwrap() error { return e.Cause }

func Code(err error) string {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ErrInternal
}

func NewError(code, message string) *CodedError {
	return &CodedError{Code: code, Message: message}
}

func WrapError(code, message string, cause error) *CodedError {
	return &CodedError{Code: code, Message: message, Cause: cause}
}

func Retryable(err error) bool {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Retryable
	}
	return false
}

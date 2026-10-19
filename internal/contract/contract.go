// Package contract defines the versioned CLI envelope, error codes and exit
// mapping. It has no provider dependencies.
package contract

import (
	"encoding/json"
	"errors"
	"io"

	"amgo/internal/model"
)

const SchemaVersion = 1

type Envelope struct {
	SchemaVersion int         `json:"schema_version"`
	Operation     string      `json:"operation"`
	Outcome       string      `json:"outcome"`
	Data          any         `json:"data"`
	Error         *ErrorShape `json:"error"`
}

type ErrorShape struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

func OK(operation string, data any) Envelope {
	return Envelope{SchemaVersion: SchemaVersion, Operation: operation, Outcome: "ok", Data: data}
}

func Fail(operation string, err error, data any) Envelope {
	code := model.Code(err)
	msg := err.Error()
	retryable := model.Retryable(err)
	var details map[string]any
	var ce *model.CodedError
	if errors.As(err, &ce) {
		msg = ce.Message
		details = ce.Details
	}
	return Envelope{
		SchemaVersion: SchemaVersion,
		Operation:     operation,
		Outcome:       "error",
		Data:          data,
		Error:         &ErrorShape{Code: code, Message: msg, Retryable: retryable, Details: details},
	}
}

// ExitCode maps a typed error to the public process exit code.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch model.Code(err) {
	case model.ErrUsage:
		return 2
	case model.ErrUnsupported:
		return 3
	case model.ErrAmbiguousTarget, model.ErrIdemConflict, model.ErrConversationClosed, model.ErrBudgetExhausted:
		return 4
	case model.ErrTargetUnavailable, model.ErrTargetNotLive:
		return 5
	case model.ErrNativeHold, model.ErrPolicyRefused:
		return 6
	case model.ErrUnknownSubmission, model.ErrOutcomeUnpersisted:
		return 7
	case model.ErrDeadlineExceeded, model.ErrDispatchDeadline:
		return 9
	case model.ErrStoreBusy, model.ErrRefundPending:
		return 10
	case model.ErrStateIncompatible:
		return 11
	case model.ErrInterrupted:
		return 130
	default:
		return 1
	}
}

func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

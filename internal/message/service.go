// Package message orchestrates the outbox: durable intent, one native attempt,
// and honest outcome recording.
package message

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/platform"
	"amgo/internal/session"
	"amgo/internal/store"
)

const (
	DefaultOpenTimeout     = 2 * time.Second
	DefaultRequestTimeout  = 5 * time.Second
	DispatchWindow         = 120 * time.Second
	FinalizationGrace      = 10 * time.Second
	DefaultMaxMessages     = 4
	DefaultConversationTTL = time.Hour
	MaxBodyBytes           = 64 << 10
	FramingVersion         = 1
)

type Service struct {
	Home           string
	DBPath         string
	OpenTimeout    time.Duration
	RequestTimeout time.Duration
	Now            func() time.Time
	Adapters       map[string]adapter.Adapter
}

func New(home string, adapters map[string]adapter.Adapter) *Service {
	return &Service{
		Home:           home,
		DBPath:         config.DBPath(home),
		OpenTimeout:    DefaultOpenTimeout,
		RequestTimeout: DefaultRequestTimeout,
		Now:            time.Now,
		Adapters:       adapters,
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) open() (*store.Store, error) {
	st, err := store.Open(s.DBPath, s.OpenTimeout)
	if err != nil {
		return nil, err
	}
	if s.Now != nil {
		st.Now = s.Now
	}
	return st, nil
}

func (s *Service) openRead() (*store.Store, error) {
	st, err := store.OpenReadOnly(s.DBPath, s.OpenTimeout)
	if err != nil {
		return nil, err
	}
	if s.Now != nil {
		st.Now = s.Now
	}
	return st, nil
}

// openReadOptional returns (nil, nil) when no state exists yet, so read
// commands do not create a database merely to report an empty result.
func (s *Service) openReadOptional() (*store.Store, error) {
	if _, err := os.Stat(s.DBPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return s.openRead()
}

func OperatorID() string { return fmt.Sprintf("operator:%d", platform.UID()) }

type SendInput struct {
	RecipientAlias        string
	SenderAlias           string
	ConversationName      string
	ConversationID        string
	ReplyTo               string
	RetryOf               string
	Body                  string
	IdempotencyKey        string
	ExpectReply           bool
	AcceptUnknownDelivery bool
	AdmitExperimental     bool
}

type SendOutput struct {
	Message            model.Message      `json:"message"`
	Attempt            model.Attempt      `json:"attempt"`
	Reservation        model.Reservation  `json:"reservation"`
	Conversation       model.Conversation `json:"conversation"`
	Submission         string             `json:"submission"`
	NativeSubmissionID string             `json:"native_submission_id,omitempty"`
	NativeDisposition  string             `json:"native_disposition"`
	Duplicate          bool               `json:"duplicate"`
	MessagePruned      bool               `json:"message_pruned"`
	IdempotencyKey     string             `json:"idempotency_key"`
	Evidence           string             `json:"evidence,omitempty"`
	Idem               model.IdemRecord   `json:"-"`
}

func (s *Service) Send(ctx context.Context, in SendInput) (out SendOutput, err error) {
	if strings.TrimSpace(in.Body) == "" {
		return SendOutput{}, model.NewError(model.ErrUsage, "empty message body")
	}
	if len(in.Body) > MaxBodyBytes {
		return SendOutput{}, model.NewError(model.ErrUsage, "message body exceeds 64 KiB")
	}
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = ids.NewUUID()
	}
	defer func() {
		// Every failure after key generation still tells the caller which key
		// to reconcile, even when no message record was written.
		if err != nil && out.IdempotencyKey == "" {
			out.IdempotencyKey = in.IdempotencyKey
		}
	}()
	st, err := s.open()
	if err != nil {
		return SendOutput{}, err
	}
	defer func() {
		if st != nil {
			_ = st.Close()
		}
	}()

	senderID, senderLabel, callerID, err := s.resolveSender(st, in.SenderAlias)
	if err != nil {
		return SendOutput{}, err
	}

	// The idempotency lookup comes before target resolution: a replay must
	// still work after the recipient is unregistered or its message pruned.
	rec, found, err := st.LookupIdempotency(callerID, in.IdempotencyKey)
	if err != nil {
		return SendOutput{}, err
	}

	target, recipientSessionID, conversationID, explicitConv, replyTo, localOnly, ackProv, err := s.resolveTarget(st, in, senderID)
	if err != nil {
		if !found {
			return out, err
		}
		if !replayMatches(rec, in) {
			return out, model.NewError(model.ErrIdemConflict, "idempotency key was already used with a different payload or target")
		}
		out = SendOutput{
			Duplicate:      true,
			MessagePruned:  rec.MessagePruned,
			IdempotencyKey: in.IdempotencyKey,
			Submission:     rec.Outcome,
		}
		replayErr := s.replayError(st, rec, &out)
		if out.Evidence == "" {
			out.Evidence = "replayed from idempotency membership; the target could not be revalidated"
		}
		return out, replayErr
	}

	fp := fingerprint(target, conversationID, explicitConv, replyTo, in.ExpectReply, in.Body)
	if found {
		if rec.Fingerprint != fp {
			return SendOutput{}, model.NewError(model.ErrIdemConflict, "idempotency key was already used with a different payload or target")
		}
		out := SendOutput{
			Duplicate:      true,
			MessagePruned:  rec.MessagePruned,
			IdempotencyKey: in.IdempotencyKey,
			Submission:     rec.Outcome,
		}
		if err := s.replayError(st, rec, &out); err != nil {
			return out, err
		}
		return out, nil
	}

	if target.Adapter == model.AdapterClaude && !localOnly {
		sess, err := st.GetSession(recipientSessionID)
		if err != nil {
			return SendOutput{}, err
		}
		if !sess.AcceptUnknownDelivery && !in.AcceptUnknownDelivery {
			return SendOutput{}, &model.CodedError{
				Code:    model.ErrPolicyRefused,
				Message: "first Claude send requires --accept-unknown-delivery; holds and refusals are not observable",
			}
		}
		if in.AcceptUnknownDelivery && !sess.AcceptUnknownDelivery {
			if err := st.SetAcceptUnknownDelivery(sess.ID, true); err != nil {
				return SendOutput{}, err
			}
		}
	}
	var ad adapter.Adapter
	if !localOnly {
		ad = s.Adapters[target.Adapter]
		if ad == nil {
			return SendOutput{}, model.NewError(model.ErrUnsupported, "adapter is not available")
		}
	}

	now := s.now()
	msgID := ids.New("m")
	attemptID := ids.New("a")
	reservationID := ids.New("r")
	expires := now.Add(DefaultConversationTTL)
	if conversationID != "" {
		if c, err := st.GetConversation(conversationID); err == nil && !c.ExpiresAt.IsZero() {
			expires = c.ExpiresAt
		}
	}
	msgSenderID := senderID
	if strings.HasPrefix(senderID, "operator:") {
		msgSenderID = ""
	}
	msg := model.Message{
		RecordVersion:      1,
		ID:                 msgID,
		ConversationID:     conversationID,
		SenderSessionID:    msgSenderID,
		SenderLabel:        senderLabel,
		RecipientSessionID: recipientSessionID,
		ReplyTo:            replyTo,
		RetryOf:            in.RetryOf,
		Body:               in.Body,
		BodySHA256:         bodyHash(in.Body),
		BodyBytes:          len([]byte(in.Body)),
		State:              model.MessageDispatching,
		NativeDisposition:  model.DispositionUnknown,
		ExpectReply:        in.ExpectReply,
		FramingVersion:     FramingVersion,
		CreatedAt:          now,
		ExpiresAt:          expires,
	}
	ownerStart, _ := platform.ProcStart(os.Getpid())
	attempt := model.Attempt{
		RecordVersion:    1,
		ID:               attemptID,
		MessageID:        msgID,
		ReservationID:    reservationID,
		Target:           target,
		State:            model.AttemptDispatching,
		StartedAt:        now,
		DispatchDeadline: now.Add(DispatchWindow),
		OwnerPID:         os.Getpid(),
		OwnerProcStart:   ownerStart,
	}
	reservation := model.Reservation{
		RecordVersion:    1,
		ID:               reservationID,
		MessageID:        msgID,
		ConversationID:   conversationID,
		State:            model.ReservationReserved,
		CreatedAt:        now,
		DispatchDeadline: attempt.DispatchDeadline,
	}

	prep := store.PrepareRequest{
		CallerID:         callerID,
		RecipientAlias:   in.RecipientAlias,
		ConversationName: in.ConversationName,
		IdemKey:          in.IdempotencyKey,
		Fingerprint:      fp,
		Message:          msg,
		Attempt:          attempt,
		Reservation:      reservation,
		ConversationID:   conversationID,
		Now:              now,
	}
	if replyTo != "" {
		prep.AckOriginalID = replyTo
		prep.AckProvenance = ackProv
		prep.AckActor = senderLabel
	}
	if in.ConversationName == "" && in.ConversationID == "" && replyTo == "" {
		prep.NewConversation = &model.Conversation{
			RecordVersion: 1,
			ID:            conversationID,
			Participants:  []string{senderID, recipientSessionID},
			MaxMessages:   DefaultMaxMessages,
			ExpiresAt:     now.Add(DefaultConversationTTL),
			CreatedAt:     now,
		}
	}
	res, err := st.PrepareSend(prep)
	if err != nil {
		return SendOutput{}, err
	}
	if res.Duplicate {
		out := SendOutput{
			Duplicate:      true,
			Idem:           res.Idem,
			MessagePruned:  res.Idem.MessagePruned,
			IdempotencyKey: in.IdempotencyKey,
			Submission:     res.Idem.Outcome,
			Message:        res.Message,
			Conversation:   model.Conversation{ID: res.Idem.ConversationID},
		}
		if err := s.replayError(st, res.Idem, &out); err != nil {
			return out, err
		}
		return out, nil
	}
	_ = st.Close()
	st = nil

	prepOut := SendOutput{
		Message: msg, Attempt: attempt, Reservation: reservation, Conversation: res.Conversation,
		Submission: string(model.MessageDispatching), IdempotencyKey: in.IdempotencyKey,
	}
	if localOnly {
		st2, err := s.open()
		if err != nil {
			return prepOut, err
		}
		finalNow := s.now()
		err = st2.FinalizeAttempt(store.FinalizeRequest{
			AttemptID:         attemptID,
			Outcome:           "submitted",
			NativeDisposition: model.DispositionLocal,
			Now:               finalNow,
		})
		_ = st2.Close()
		if err != nil {
			return prepOut, err
		}
		msg.State = model.MessageSubmitted
		msg.NativeDisposition = model.DispositionLocal
		attempt.State = model.AttemptFinalized
		attempt.Outcome = "submitted"
		attempt.NativeDisposition = model.DispositionLocal
		attempt.FinalizedAt = &finalNow
		reservation.State = model.ReservationConsumed
		out := SendOutput{
			Message: msg, Attempt: attempt, Reservation: reservation, Conversation: res.Conversation,
			Submission: "submitted", NativeDisposition: model.DispositionLocal,
			IdempotencyKey: in.IdempotencyKey, Evidence: "reply stored in the local operator inbox",
		}
		return s.refreshOutput(out), nil
	}

	st2, err := s.open()
	if err != nil {
		return prepOut, err
	}
	if err := st2.MarkDispatched(attemptID, s.RequestTimeout+FinalizationGrace); err != nil {
		// No payload was written. Record a known-unsent outcome if possible.
		finalizeErr := st2.FinalizeAttempt(store.FinalizeRequest{
			AttemptID:         attemptID,
			Outcome:           "failed_before_submit",
			ErrorCode:         model.Code(err),
			ErrorDetail:       err.Error(),
			NativeDisposition: model.DispositionUnknown,
			Now:               s.now(),
		})
		_ = st2.Close()
		msg.State = model.MessageFailedBeforeSubmit
		attempt.State = model.AttemptFinalized
		attempt.Outcome = "failed_before_submit"
		attempt.ErrorCode = model.Code(err)
		reservation.State = model.ReservationRefunded
		out := SendOutput{
			Message: msg, Attempt: attempt, Reservation: reservation, Conversation: res.Conversation,
			Submission: "failed_before_submit", IdempotencyKey: in.IdempotencyKey, Evidence: "no payload was written",
		}
		if finalizeErr != nil {
			return out, &model.CodedError{
				Code:      model.ErrRefundPending,
				Message:   "no payload was written, but the refund could not be persisted",
				Retryable: true,
				Details: map[string]any{
					"reservation_id": reservationID, "attempt_id": attemptID,
					"message_id": msgID, "idempotency_key": in.IdempotencyKey, "payload_written": false,
				},
				Cause: finalizeErr,
			}
		}
		return s.refreshOutput(out), err
	}
	_ = st2.Close()
	sendCtx, cancel := context.WithTimeout(ctx, s.RequestTimeout)
	result, sendErr := ad.Send(sendCtx, adapter.SendRequest{
		Target:            target,
		MessageID:         msgID,
		ConversationID:    conversationID,
		RetryOf:           in.RetryOf,
		ReplyTo:           replyTo,
		SenderLabel:       senderLabel,
		Body:              in.Body,
		ExpectReply:       in.ExpectReply,
		Timeout:           s.RequestTimeout,
		AdmitExperimental: in.AdmitExperimental,
	})
	cancel()

	outcome := result.Outcome
	if sendErr != nil {
		outcome = "failed_before_submit"
		result.ErrorCode = model.Code(sendErr)
		result.ErrorDetail = sendErr.Error()
	}
	if outcome == "" {
		outcome = "unknown"
	}

	st3, err := s.open()
	if err != nil {
		return s.unpersisted(reservationID, attemptID, msgID, res.Conversation, in.IdempotencyKey, outcome, result, err)
	}
	finalNow := s.now()
	if err := st3.FinalizeAttempt(store.FinalizeRequest{
		AttemptID:          attemptID,
		Outcome:            outcome,
		NativeSubmissionID: result.NativeSubmissionID,
		NativeDisposition:  result.NativeDisposition,
		ErrorCode:          result.ErrorCode,
		ErrorDetail:        result.ErrorDetail,
		Now:                finalNow,
	}); err != nil {
		_ = st3.Close()
		return s.unpersisted(reservationID, attemptID, msgID, res.Conversation, in.IdempotencyKey, outcome, result, err)
	}
	_ = st3.Close()
	msg.State = stateForOutcome(outcome)
	msg.NativeDisposition = result.NativeDisposition
	if msg.NativeDisposition == "" {
		msg.NativeDisposition = model.DispositionUnknown
	}
	attempt.State = model.AttemptFinalized
	attempt.Outcome = outcome
	attempt.NativeSubmissionID = result.NativeSubmissionID
	attempt.NativeDisposition = msg.NativeDisposition
	attempt.ErrorCode = result.ErrorCode
	attempt.ErrorDetail = result.ErrorDetail
	attempt.FinalizedAt = &finalNow
	if outcome == "failed_before_submit" {
		reservation.State = model.ReservationRefunded
	} else {
		reservation.State = model.ReservationConsumed
	}
	out = SendOutput{
		Message: msg, Attempt: attempt, Reservation: reservation, Conversation: res.Conversation,
		Submission: string(msg.State), NativeSubmissionID: result.NativeSubmissionID,
		NativeDisposition: msg.NativeDisposition, IdempotencyKey: in.IdempotencyKey, Evidence: result.Evidence,
	}
	out = s.refreshOutput(out)
	return out, outcomeError(outcome, result, out)
}

func outcomeError(outcome string, result adapter.SendResult, out SendOutput) error {
	switch outcome {
	case "submitted":
		return nil
	case "unknown":
		return &model.CodedError{
			Code:    model.ErrUnknownSubmission,
			Message: "the native submission outcome is unknown; do not blindly retry",
			Details: map[string]any{
				"message_id":  out.Message.ID,
				"attempt_id":  out.Attempt.ID,
				"evidence":    out.Evidence,
				"native_id":   out.NativeSubmissionID,
				"disposition": out.NativeDisposition,
			},
		}
	case "failed_before_submit":
		code := result.ErrorCode
		if code == "" {
			code = model.ErrTargetUnavailable
		}
		detail := result.ErrorDetail
		if detail == "" {
			detail = "no payload was written"
		}
		return &model.CodedError{
			Code:    code,
			Message: detail,
			Details: map[string]any{
				"message_id": out.Message.ID,
				"attempt_id": out.Attempt.ID,
			},
		}
	default:
		return &model.CodedError{Code: model.ErrUnknownSubmission, Message: "unclassified send outcome"}
	}
}

func (s *Service) unpersisted(reservationID, attemptID, msgID string, conv model.Conversation, key, outcome string, result adapter.SendResult, cause error) (SendOutput, error) {
	rec := store.OutcomeRecord{
		AttemptID:          attemptID,
		MessageID:          msgID,
		Outcome:            outcome,
		NativeSubmissionID: result.NativeSubmissionID,
		NativeDisposition:  result.NativeDisposition,
		ErrorCode:          result.ErrorCode,
		ErrorDetail:        result.ErrorDetail,
		IdempotencyKey:     key,
		At:                 s.now(),
	}
	_ = store.AppendOutcome(config.OutcomesLog(s.Home), rec)
	out := SendOutput{
		Message:            model.Message{ID: msgID, ConversationID: conv.ID},
		Attempt:            model.Attempt{ID: attemptID},
		Reservation:        model.Reservation{ID: reservationID, ConversationID: conv.ID},
		Conversation:       conv,
		Submission:         outcome,
		NativeSubmissionID: result.NativeSubmissionID,
		NativeDisposition:  result.NativeDisposition,
		IdempotencyKey:     key,
		Evidence:           result.Evidence,
	}
	code := model.ErrOutcomeUnpersisted
	message := "the native outcome could not be persisted; inspect the evidence and run amgo recover"
	payloadWritten := true
	if outcome == "failed_before_submit" {
		code = model.ErrRefundPending
		message = "no payload was written, but the refund could not be persisted"
		payloadWritten = false
	}
	return out, &model.CodedError{
		Code:      code,
		Message:   message,
		Retryable: true,
		Details: map[string]any{
			"attempt_id":           attemptID,
			"message_id":           msgID,
			"conversation_id":      conv.ID,
			"reservation_id":       reservationID,
			"idempotency_key":      key,
			"payload_written":      payloadWritten,
			"native_submission_id": result.NativeSubmissionID,
			"native_disposition":   result.NativeDisposition,
			"evidence":             result.Evidence,
		},
		Cause: cause,
	}
}

// replayError maps an idempotent replay to the error the original attempt
// produced. Returning nil for a historical failure would make a retry look
// successful when nothing was ever submitted.
func replayMatches(rec model.IdemRecord, in SendInput) bool {
	if rec.BodySHA256 == "" {
		// Legacy membership has no replay evidence and the fingerprint cannot
		// be recomputed without a resolvable target. Fail closed.
		return false
	}
	if rec.BodySHA256 != bodyHash(in.Body) {
		return false
	}
	if rec.ReplyTo != in.ReplyTo || rec.ExpectReply != in.ExpectReply {
		return false
	}
	if rec.RecipientAlias != in.RecipientAlias || rec.ConversationName != in.ConversationName {
		return false
	}
	return true
}

// replayError maps an idempotent replay to the error the original attempt
// produced. A finalized attempt is authoritative even when a later
// acknowledgement changed the message state: an ack does not prove delivery.
func (s *Service) replayError(st *store.Store, rec model.IdemRecord, out *SendOutput) error {
	if out.Message.ID == "" {
		out.Message.ID = rec.MessageID
	}
	if out.Message.ConversationID == "" {
		out.Message.ConversationID = rec.ConversationID
	}
	if out.Conversation.ID == "" {
		if c, err := st.GetConversation(rec.ConversationID); err == nil {
			out.Conversation = c
		}
	}
	if r, err := st.GetReservationForMessage(rec.MessageID); err == nil {
		out.Reservation = r
	}
	msg, err := st.GetMessage(rec.MessageID)
	if err != nil {
		out.MessagePruned = true
		if out.Submission == "" {
			out.Submission = rec.Outcome
		}
		switch rec.Outcome {
		case "submitted", "acknowledged":
			return nil
		case "unknown":
			return outcomeError("unknown", adapter.SendResult{Evidence: "historical idempotent replay"}, *out)
		case "failed_before_submit":
			return outcomeError("failed_before_submit", adapter.SendResult{
				ErrorCode:   model.ErrTargetUnavailable,
				ErrorDetail: "the original attempt failed before submission and its details were pruned",
			}, *out)
		default:
			return &model.CodedError{
				Code:    model.ErrUnknownSubmission,
				Message: "the original attempt never reached a final state; do not blindly retry",
				Details: map[string]any{"message_id": rec.MessageID, "idempotency_key": out.IdempotencyKey},
			}
		}
	}
	out.Message = msg
	out.NativeDisposition = msg.NativeDisposition
	out.Submission = string(msg.State)
	attempts, aerr := st.ListAttempts(rec.MessageID)
	if aerr != nil {
		return aerr
	}
	if len(attempts) > 0 {
		out.Attempt = attempts[len(attempts)-1]
	}
	if out.Attempt.ID != "" && out.Attempt.State != model.AttemptFinalized {
		out.Submission = string(out.Attempt.State)
		return &model.CodedError{
			Code:    model.ErrUnknownSubmission,
			Message: "the original attempt has not been finalized; wait for its deadline before retrying",
			Details: map[string]any{
				"message_id": rec.MessageID, "attempt_id": out.Attempt.ID,
				"attempt_state": string(out.Attempt.State), "in_flight": true,
				"idempotency_key": out.IdempotencyKey,
			},
		}
	}
	if out.Attempt.State == model.AttemptFinalized {
		switch out.Attempt.Outcome {
		case "submitted":
			out.Submission = "submitted"
			return nil
		case "unknown":
			out.Submission = "unknown"
			return outcomeError("unknown", adapter.SendResult{Evidence: "historical idempotent replay"}, *out)
		case "failed_before_submit":
			out.Submission = "failed_before_submit"
			return outcomeError("failed_before_submit", adapter.SendResult{
				ErrorCode:   out.Attempt.ErrorCode,
				ErrorDetail: out.Attempt.ErrorDetail,
			}, *out)
		}
	}
	switch msg.State {
	case model.MessageSubmitted, model.MessageAcknowledged:
		return nil
	case model.MessageUnknown:
		return outcomeError("unknown", adapter.SendResult{Evidence: "historical idempotent replay"}, *out)
	case model.MessageFailedBeforeSubmit:
		return outcomeError("failed_before_submit", adapter.SendResult{ErrorCode: model.ErrTargetUnavailable}, *out)
	case model.MessageDispatching:
		return &model.CodedError{
			Code:    model.ErrUnknownSubmission,
			Message: "the original attempt is still in flight; wait for its deadline before retrying",
			Details: map[string]any{
				"message_id": rec.MessageID, "idempotency_key": out.IdempotencyKey, "in_flight": true,
			},
		}
	default:
		return &model.CodedError{
			Code:    model.ErrTargetUnavailable,
			Message: "the original message is no longer sendable",
			Details: map[string]any{"message_id": rec.MessageID, "state": string(msg.State)},
		}
	}
}

// refreshOutput re-reads the authoritative records so a concurrent
// acknowledgement is reflected. A read failure keeps the locally observed
// output: losing the evidence is worse than returning a slightly stale view.
func (s *Service) refreshOutput(out SendOutput) SendOutput {
	st, err := s.openRead()
	if err != nil {
		return out
	}
	defer st.Close()
	if msg, err := st.GetMessage(out.Message.ID); err == nil {
		out.Message = msg
		out.Submission = string(msg.State)
		out.NativeDisposition = msg.NativeDisposition
	}
	if attempts, err := st.ListAttempts(out.Message.ID); err == nil && len(attempts) > 0 {
		out.Attempt = attempts[len(attempts)-1]
		if out.NativeSubmissionID == "" {
			out.NativeSubmissionID = out.Attempt.NativeSubmissionID
		}
	}
	if out.Reservation.ID == "" {
		if r, err := st.GetReservationForMessage(out.Message.ID); err == nil {
			out.Reservation = r
		}
	}
	if out.Conversation.ID == "" {
		if c, err := st.GetConversation(out.Message.ConversationID); err == nil {
			out.Conversation = c
		}
	}
	return out
}

func stateForOutcome(outcome string) model.MessageState {
	switch outcome {
	case "submitted":
		return model.MessageSubmitted
	case "failed_before_submit":
		return model.MessageFailedBeforeSubmit
	default:
		return model.MessageUnknown
	}
}

func (s *Service) resolveSender(st *store.Store, alias string) (id, label, callerID string, err error) {
	if alias != "" {
		sess, err := session.Resolve(st, alias)
		if err != nil {
			return "", "", "", err
		}
		return sess.ID, alias, "session:" + sess.ID, nil
	}
	if sid := session.CurrentClaudeSessionID(); sid != "" {
		sessions, err := st.ListSessions(false)
		if err == nil {
			for _, sess := range sessions {
				if sess.Target.Adapter == model.AdapterClaude && sess.Target.NativeSessionID == sid {
					return sess.ID, sess.Alias, OperatorID(), nil
				}
			}
		}
	}
	return OperatorID(), "operator", OperatorID(), nil
}

func (s *Service) resolveTarget(st *store.Store, in SendInput, senderID string) (target model.Target, recipientSessionID, conversationID string, explicitConv bool, replyTo string, localOnly bool, ackProv model.AckProvenance, err error) {
	if in.ReplyTo != "" && (in.ConversationID != "" || in.ConversationName != "") {
		return target, "", "", false, "", false, "", model.NewError(model.ErrUsage, "reply and conversation selection are mutually exclusive")
	}
	if in.ReplyTo != "" {
		orig, err := st.GetMessage(in.ReplyTo)
		if err != nil {
			return target, "", "", false, "", false, "", model.NewError(model.ErrTargetUnavailable, "reply target not found")
		}
		conversationID = orig.ConversationID
		explicitConv = true
		replyTo = orig.ID
		ackProv = model.ProvenanceOperator
		if senderID == orig.RecipientSessionID {
			ackProv = model.ProvenanceRecipient
		}
		if orig.SenderSessionID != "" {
			sess, err := st.GetSession(orig.SenderSessionID)
			if err != nil {
				return target, "", "", false, "", false, "", model.NewError(model.ErrTargetUnavailable, "original sender session is not registered")
			}
			if sess.UnregisteredAt != nil {
				return target, "", "", false, "", false, "", model.NewError(model.ErrTargetUnavailable, "original sender is no longer registered")
			}
			return sess.Target, sess.ID, conversationID, explicitConv, replyTo, false, ackProv, nil
		}
		return model.Target{
			Adapter:         "local",
			HostID:          config.HostID(),
			ConfigHome:      "",
			NativeSessionID: OperatorID(),
		}, OperatorID(), conversationID, explicitConv, replyTo, true, ackProv, nil
	}

	recipient, err := session.Resolve(st, in.RecipientAlias)
	if err != nil {
		return target, "", "", false, "", false, "", err
	}
	switch {
	case in.ConversationID != "":
		if in.ConversationName != "" {
			return target, "", "", false, "", false, "", model.NewError(model.ErrUsage, "conversation ID and conversation name are mutually exclusive")
		}
		conversationID = in.ConversationID
		explicitConv = true
	case in.ConversationName != "":
		conv, err := st.GetConversationByName(in.ConversationName)
		if err != nil {
			return target, "", "", false, "", false, "", err
		}
		conversationID = conv.ID
		explicitConv = true
	default:
		conversationID = ids.New("c")
		explicitConv = false
	}
	return recipient.Target, recipient.ID, conversationID, explicitConv, "", false, "", nil
}

func bodyHash(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

func fingerprint(target model.Target, conversationID string, explicitConv bool, replyTo string, expectReply bool, body string) string {
	h := sha256.New()
	write := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	write("amgo-message-v1")
	write(target.Adapter)
	write(target.HostID)
	write(target.ConfigHome)
	write(target.NativeSessionID)
	write(replyTo)
	if expectReply {
		write("1")
	} else {
		write("0")
	}
	write(bodyHash(body))
	if explicitConv {
		write(conversationID)
	}
	return hex.EncodeToString(h.Sum(nil))
}

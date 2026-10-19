package message

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"amgo/internal/config"
	"amgo/internal/ids"
	"amgo/internal/model"
	"amgo/internal/platform"
	"amgo/internal/session"
	"amgo/internal/store"
)

type ReplyInput struct {
	MessageID             string
	SenderAlias           string
	Body                  string
	IdempotencyKey        string
	ExpectReply           bool
	AcceptUnknownDelivery bool
	AdmitExperimental     bool
}

func (s *Service) Reply(ctx context.Context, in ReplyInput) (SendOutput, error) {
	return s.Send(ctx, SendInput{
		ReplyTo:               in.MessageID,
		SenderAlias:           in.SenderAlias,
		Body:                  in.Body,
		IdempotencyKey:        in.IdempotencyKey,
		ExpectReply:           in.ExpectReply,
		AcceptUnknownDelivery: in.AcceptUnknownDelivery,
		AdmitExperimental:     in.AdmitExperimental,
	})
}

type RetryInput struct {
	MessageID             string
	SenderAlias           string
	AcceptDuplicateRisk   bool
	AcceptUnknownDelivery bool
	AdmitExperimental     bool
	IdempotencyKey        string
}

// Retry sends a new message for an outcome that is unknown. It is an explicit
// operator override: the original submission may already have reached the
// target, so the caller must accept that duplicate risk. The new message keeps
// the original body and conversation and records RetryOf.
func (s *Service) Retry(ctx context.Context, in RetryInput) (SendOutput, error) {
	if !in.AcceptDuplicateRisk {
		return SendOutput{}, &model.CodedError{
			Code:    model.ErrPolicyRefused,
			Message: "retry requires --accept-duplicate-risk; the original submission may have reached the target",
		}
	}
	st, err := s.open()
	if err != nil {
		return SendOutput{}, err
	}
	orig, err := st.GetMessage(in.MessageID)
	if err != nil {
		_ = st.Close()
		return SendOutput{}, model.NewError(model.ErrTargetUnavailable, "original message not found")
	}
	if orig.State != model.MessageUnknown {
		_ = st.Close()
		return SendOutput{}, model.NewError(model.ErrPolicyRefused, "only an unknown outcome can be retried; use send for a known failure")
	}
	attempts, err := st.ListAttempts(orig.ID)
	if err != nil {
		_ = st.Close()
		return SendOutput{}, err
	}
	sess, err := st.GetSession(orig.RecipientSessionID)
	if err != nil || sess.UnregisteredAt != nil {
		_ = st.Close()
		return SendOutput{}, model.NewError(model.ErrTargetUnavailable, "retry target is not registered")
	}
	if len(attempts) > 0 && !sameRetryTarget(attempts[len(attempts)-1].Target, sess.Target) {
		_ = st.Close()
		return SendOutput{}, model.NewError(model.ErrTargetUnavailable, "retry target changed; send a new message explicitly")
	}
	_ = st.Close()
	return s.Send(ctx, SendInput{
		RecipientAlias:        sess.Alias,
		SenderAlias:           in.SenderAlias,
		ConversationID:        orig.ConversationID,
		RetryOf:               orig.ID,
		Body:                  orig.Body,
		IdempotencyKey:        in.IdempotencyKey,
		ExpectReply:           orig.ExpectReply,
		AcceptUnknownDelivery: in.AcceptUnknownDelivery,
		AdmitExperimental:     in.AdmitExperimental,
	})
}

func sameRetryTarget(a, b model.Target) bool {
	return a.Adapter == b.Adapter &&
		a.HostID == b.HostID &&
		a.ConfigHome == b.ConfigHome &&
		a.NativeSessionID == b.NativeSessionID
}

type AckInput struct {
	MessageID string
	AsAlias   string
}

func (s *Service) Ack(ctx context.Context, in AckInput) (model.Message, []model.Receipt, error) {
	st, err := s.open()
	if err != nil {
		return model.Message{}, nil, err
	}
	defer st.Close()
	msg, err := st.GetMessage(in.MessageID)
	if err != nil {
		return model.Message{}, nil, err
	}
	prov := model.ProvenanceOperator
	actor := OperatorID()
	if in.AsAlias != "" {
		sess, err := session.Resolve(st, in.AsAlias)
		if err != nil {
			return model.Message{}, nil, err
		}
		actor = sess.Alias
		if sess.ID == msg.RecipientSessionID {
			prov = model.ProvenanceRecipient
		}
	} else if sid := session.CurrentClaudeSessionID(); sid != "" {
		sessions, _ := st.ListSessions(false)
		for _, sess := range sessions {
			if sess.Target.Adapter == model.AdapterClaude && sess.Target.NativeSessionID == sid && sess.ID == msg.RecipientSessionID {
				prov = model.ProvenanceRecipient
				actor = sess.Alias
				break
			}
		}
	}
	if err := st.AckMessage(msg.ID, prov, actor, "", s.now()); err != nil {
		return model.Message{}, nil, err
	}
	msg, _ = st.GetMessage(msg.ID)
	receipts, _ := st.ListReceipts(msg.ID)
	return msg, receipts, nil
}

type WaitInput struct {
	MessageID string
	Until     string // acknowledged | replied
	Timeout   time.Duration
}

type WaitResult struct {
	Message  model.Message   `json:"message"`
	Receipts []model.Receipt `json:"receipts"`
}

func (s *Service) Wait(ctx context.Context, in WaitInput) (WaitResult, error) {
	if in.Until != "acknowledged" && in.Until != "replied" {
		return WaitResult{}, model.NewError(model.ErrUsage, "--until must be acknowledged or replied")
	}
	if in.Timeout <= 0 {
		in.Timeout = 60 * time.Second
	}
	deadline := time.NewTimer(in.Timeout)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		res, err := s.readState(in.MessageID)
		if err != nil {
			return WaitResult{}, err
		}
		done := res.Message.State == model.MessageAcknowledged
		if in.Until == "replied" {
			done = false
			for _, r := range res.Receipts {
				if r.Kind == "reply" {
					done = true
					break
				}
			}
		}
		if done {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, model.WrapError(model.ErrInterrupted, "wait interrupted", ctx.Err())
		case <-deadline.C:
			return res, &model.CodedError{
				Code:    model.ErrDeadlineExceeded,
				Message: "wait deadline exceeded",
				Details: map[string]any{"message_id": in.MessageID, "state": string(res.Message.State)},
			}
		case <-tick.C:
		}
	}
}

func (s *Service) readState(messageID string) (WaitResult, error) {
	st, err := s.openReadOptional()
	if err != nil {
		return WaitResult{}, err
	}
	if st == nil {
		return WaitResult{}, model.NewError(model.ErrTargetUnavailable, "state is not initialized")
	}
	defer st.Close()
	msg, err := st.GetMessage(messageID)
	if err != nil {
		return WaitResult{}, err
	}
	receipts, _ := st.ListReceipts(messageID)
	return WaitResult{Message: msg, Receipts: receipts}, nil
}

type Record struct {
	Message     model.Message      `json:"message"`
	Attempts    []model.Attempt    `json:"attempts"`
	Receipts    []model.Receipt    `json:"receipts"`
	Events      []model.Event      `json:"events"`
	Reservation *model.Reservation `json:"reservation,omitempty"`
}

func (s *Service) Read(messageID string) (Record, error) {
	st, err := s.openReadOptional()
	if err != nil {
		return Record{}, err
	}
	if st == nil {
		return Record{}, model.NewError(model.ErrTargetUnavailable, "state is not initialized")
	}
	defer st.Close()
	msg, err := st.GetMessage(messageID)
	if err != nil {
		return Record{}, err
	}
	attempts, _ := st.ListAttempts(messageID)
	receipts, _ := st.ListReceipts(messageID)
	events, _ := st.ListEvents(messageID, 1000)
	rec := Record{Message: msg, Attempts: attempts, Receipts: receipts, Events: events}
	if r, err := st.GetReservationForMessage(messageID); err == nil {
		rec.Reservation = &r
	}
	return rec, nil
}

func (s *Service) Inbox(sessionAlias string, limit int) ([]model.Message, error) {
	st, err := s.openReadOptional()
	if err != nil {
		return nil, err
	}
	if st == nil {
		return []model.Message{}, nil
	}
	defer st.Close()
	id := OperatorID()
	if sessionAlias != "" {
		sess, err := session.Resolve(st, sessionAlias)
		if err != nil {
			return nil, err
		}
		id = sess.ID
	}
	return st.ListInbox(id, limit)
}

func (s *Service) CreateConversation(name string, max int, ttl time.Duration) (model.Conversation, error) {
	if name == "" {
		return model.Conversation{}, model.NewError(model.ErrUsage, "conversation name is required")
	}
	if max <= 0 {
		max = 12
	}
	if max > 1000 {
		return model.Conversation{}, model.NewError(model.ErrUsage, "max messages cannot exceed 1000")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	now := s.now()
	c := model.Conversation{
		RecordVersion: 1,
		ID:            newConversationID(),
		Name:          name,
		MaxMessages:   max,
		ExpiresAt:     now.Add(ttl),
		CreatedAt:     now,
	}
	st, err := s.open()
	if err != nil {
		return model.Conversation{}, err
	}
	defer st.Close()
	if err := st.CreateConversation(c); err != nil {
		return model.Conversation{}, err
	}
	return c, nil
}

func (s *Service) CloseConversation(name string) (model.Conversation, error) {
	st, err := s.open()
	if err != nil {
		return model.Conversation{}, err
	}
	defer st.Close()
	c, err := st.GetConversationByName(name)
	if err != nil {
		return model.Conversation{}, err
	}
	if err := st.CloseConversation(c.ID, s.now()); err != nil {
		return model.Conversation{}, err
	}
	return st.GetConversation(c.ID)
}

func (s *Service) ShowConversation(name string) (model.Conversation, []model.Message, error) {
	st, err := s.openReadOptional()
	if err != nil {
		return model.Conversation{}, nil, err
	}
	if st == nil {
		return model.Conversation{}, nil, model.NewError(model.ErrTargetUnavailable, "state is not initialized")
	}
	defer st.Close()
	c, err := st.GetConversationByName(name)
	if err != nil {
		return model.Conversation{}, nil, err
	}
	msgs, err := st.ListConversationMessages(c.ID, 1000)
	return c, msgs, err
}

func (s *Service) RecoverBatch(limit int) (int, error) {
	st, err := s.open()
	if err != nil {
		return 0, err
	}
	cursor, err := st.ReconcileCursor()
	if err != nil {
		_ = st.Close()
		return 0, err
	}
	batch, err := st.ScanAttempts(cursor, limit)
	_ = st.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range batch.Attempts {
		if a.State == model.AttemptFinalized {
			continue
		}
		action := s.recoveryAction(a)
		if action == "" {
			continue
		}
		st2, err := s.open()
		if err != nil {
			return n, err
		}
		err = st2.RecoverAttempt(a.ID, action, s.now())
		_ = st2.Close()
		if err != nil {
			return n, err
		}
		n++
	}
	st3, err := s.open()
	if err != nil {
		return n, err
	}
	next := batch.Next
	if !batch.More {
		next = ""
	}
	err = st3.SetReconcileCursor(next)
	_ = st3.Close()
	return n, err
}

func (s *Service) recoveryAction(a model.Attempt) string {
	now := s.now()
	alive := platform.PIDAlive(a.OwnerPID)
	if alive && a.OwnerProcStart != "" {
		start, err := platform.ProcStart(a.OwnerPID)
		if err != nil {
			// A failed process query is not proof of death. If the PID is also
			// gone, the owner is dead; otherwise wait for a later pass.
			if platform.PIDAlive(a.OwnerPID) {
				return ""
			}
			alive = false
		} else if start != a.OwnerProcStart {
			alive = false
		}
	}
	switch a.State {
	case model.AttemptDispatching:
		if !alive || now.After(a.DispatchDeadline.Add(FinalizationGrace)) {
			return "failed_before_submit"
		}
	case model.AttemptDispatched:
		if !alive {
			return "unknown"
		}
		if a.FinalizationDeadline != nil && now.After(a.FinalizationDeadline.Add(FinalizationGrace)) {
			return "unknown"
		}
	}
	return ""
}

const maxRecoverBatches = 1000

// RecoverAll makes a full pass from the start of the attempt index. It returns
// an error rather than claiming success when the pass does not finish.
func (s *Service) RecoverAll() (int, error) {
	st, err := s.open()
	if err != nil {
		return 0, err
	}
	if err := st.SetReconcileCursor(""); err != nil {
		_ = st.Close()
		return 0, err
	}
	_ = st.Close()

	total := 0
	for i := 0; i < maxRecoverBatches; i++ {
		n, err := s.RecoverBatch(64)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			st, err := s.open()
			if err != nil {
				return total, err
			}
			cursor, err := st.ReconcileCursor()
			_ = st.Close()
			if err != nil {
				return total, err
			}
			if cursor == "" {
				return total, nil
			}
		}
	}
	return total, &model.CodedError{
		Code:    model.ErrInternal,
		Message: fmt.Sprintf("recovery pass exceeded %d batches; run amgo recover again", maxRecoverBatches),
		Details: map[string]any{"reconciled": total},
	}
}

func (s *Service) ImportJournal() (int, error) {
	st, err := s.open()
	if err != nil {
		return 0, err
	}
	defer st.Close()
	return store.ImportOutcomes(config.OutcomesLog(s.Home), func(rec store.OutcomeRecord) error {
		return st.FinalizeAttempt(store.FinalizeRequest{
			AttemptID:          rec.AttemptID,
			Outcome:            rec.Outcome,
			NativeSubmissionID: rec.NativeSubmissionID,
			NativeDisposition:  rec.NativeDisposition,
			ErrorCode:          rec.ErrorCode,
			ErrorDetail:        rec.ErrorDetail,
			Now:                rec.At,
		})
	})
}

func (s *Service) ExportJSONL(w io.Writer, conversationName string) error {
	st, err := s.openReadOptional()
	if err != nil {
		return err
	}
	if st == nil {
		return nil
	}
	defer st.Close()
	var msgs []model.Message
	if conversationName != "" {
		c, err := st.GetConversationByName(conversationName)
		if err != nil {
			_ = st.Close()
			return err
		}
		msgs, err = st.ListConversationMessages(c.ID, 1000)
		if err != nil {
			_ = st.Close()
			return err
		}
	} else {
		msgs, err = st.ListAllMessages(1000)
		if err != nil {
			_ = st.Close()
			return err
		}
	}
	type record struct {
		Type  string `json:"type"`
		Value any    `json:"value"`
	}
	var records []record
	for _, m := range msgs {
		records = append(records, record{Type: "message", Value: m})
		attempts, _ := st.ListAttempts(m.ID)
		for _, a := range attempts {
			records = append(records, record{Type: "attempt", Value: a})
		}
		receipts, _ := st.ListReceipts(m.ID)
		for _, r := range receipts {
			records = append(records, record{Type: "receipt", Value: r})
		}
		events, _ := st.ListEvents(m.ID, 1000)
		for _, ev := range events {
			records = append(records, record{Type: "event", Value: ev})
		}
	}
	_ = st.Close()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

func newConversationID() string { return ids.New("c") }

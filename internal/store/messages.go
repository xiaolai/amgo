package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

type PrepareRequest struct {
	CallerID         string
	RecipientAlias   string
	ConversationName string
	IdemKey          string
	Fingerprint      string
	Message          model.Message
	Attempt          model.Attempt
	Reservation      model.Reservation
	NewConversation  *model.Conversation
	ConversationID   string
	AckOriginalID    string
	AckProvenance    model.AckProvenance
	AckActor         string
	Now              time.Time
}

type PrepareResult struct {
	Duplicate    bool
	Idem         model.IdemRecord
	Message      model.Message
	Attempt      model.Attempt
	Reservation  model.Reservation
	Conversation model.Conversation
}

func idemBucketKey(callerID, key string) []byte {
	h := sha256.Sum256([]byte(callerID + "\x00" + key))
	return []byte(hex.EncodeToString(h[:]))
}

func (s *Store) LookupIdempotency(callerID, key string) (model.IdemRecord, bool, error) {
	var rec model.IdemRecord
	found := false
	err := s.view(func(tx *bolt.Tx) error {
		var err error
		found, err = getJSONOK(tx.Bucket(bucketIdem), idemBucketKey(callerID, key), &rec)
		return err
	})
	return rec, found, err
}

func (s *Store) PrepareSend(req PrepareRequest) (PrepareResult, error) {
	var out PrepareResult
	err := s.update(func(tx *bolt.Tx) error {
		idem := tx.Bucket(bucketIdem)
		ikey := idemBucketKey(req.CallerID, req.IdemKey)

		var existing model.IdemRecord
		if ok, err := getJSONOK(idem, ikey, &existing); err != nil {
			return err
		} else if ok {
			if existing.Fingerprint != req.Fingerprint {
				return model.NewError(model.ErrIdemConflict, "idempotency key was already used with a different payload or target")
			}
			out.Duplicate = true
			out.Idem = existing
			if msg, err := loadMessage(tx, existing.MessageID); err == nil {
				out.Message = msg
			} else {
				out.Idem.MessagePruned = true
			}
			return nil
		}

		if req.Message.RetryOf != "" {
			var orig model.Message
			if err := getJSON(tx.Bucket(bucketMessages), []byte(req.Message.RetryOf), &orig); err != nil {
				return model.NewError(model.ErrTargetUnavailable, "retry target not found")
			}
			if orig.State != model.MessageUnknown {
				return model.NewError(model.ErrPolicyRefused, "only an unknown outcome can be retried; use send for a known failure")
			}
			if orig.ConversationID != req.Message.ConversationID {
				return model.NewError(model.ErrUsage, "a retry must stay in the original conversation")
			}
			if orig.BodySHA256 != req.Message.BodySHA256 {
				return model.NewError(model.ErrUsage, "a retry must reuse the original body")
			}
			orig.RetriedBy = append(orig.RetriedBy, req.Message.ID)
			if err := putJSON(tx.Bucket(bucketMessages), []byte(orig.ID), orig); err != nil {
				return err
			}
			if err := appendEvent(tx, model.Event{
				MessageID: orig.ID,
				Kind:      "retry_authorized",
				At:        req.Now,
				Payload:   map[string]any{"retry_message_id": req.Message.ID},
			}); err != nil {
				return err
			}
		}

		convs := tx.Bucket(bucketConversations)
		var conv model.Conversation
		if req.NewConversation != nil {
			c := *req.NewConversation
			if c.Name != "" {
				if id := tx.Bucket(bucketConvByName).Get([]byte(c.Name)); id != nil {
					return model.NewError(model.ErrAmbiguousTarget, "conversation name already exists")
				}
			}
			if err := putJSON(convs, []byte(c.ID), c); err != nil {
				return err
			}
			if c.Name != "" {
				if err := tx.Bucket(bucketConvByName).Put([]byte(c.Name), []byte(c.ID)); err != nil {
					return err
				}
			}
			conv = c
		} else {
			if err := getJSON(convs, []byte(req.ConversationID), &conv); err != nil {
				return model.NewError(model.ErrTargetUnavailable, "conversation not found")
			}
		}

		if conv.ClosedAt != nil {
			return model.NewError(model.ErrConversationClosed, "conversation is closed")
		}
		if !req.Now.Before(conv.ExpiresAt) {
			return model.NewError(model.ErrConversationClosed, "conversation has expired")
		}
		if conv.MaxMessages > 0 && conv.SpentCount-conv.RefundedCount >= conv.MaxMessages {
			return model.NewError(model.ErrBudgetExhausted, "conversation message budget is exhausted")
		}
		if len(conv.Participants) == 0 {
			sender := req.Message.SenderSessionID
			if sender == "" {
				sender = req.CallerID
			}
			switch {
			case sender == "":
				conv.Participants = []string{req.Message.RecipientSessionID}
			case req.Message.RecipientSessionID == "" || sender == req.Message.RecipientSessionID:
				conv.Participants = []string{sender}
			default:
				conv.Participants = []string{sender, req.Message.RecipientSessionID}
			}
		} else {
			if !contains(conv.Participants, req.Message.RecipientSessionID) {
				return model.NewError(model.ErrTargetUnavailable, "recipient is not a participant in this conversation")
			}
			if req.Message.SenderSessionID != "" && !contains(conv.Participants, req.Message.SenderSessionID) {
				return model.NewError(model.ErrAmbiguousTarget, "sender is not a participant in this conversation")
			}
		}

		if err := putJSON(tx.Bucket(bucketMessages), []byte(req.Message.ID), req.Message); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMsgByConv).Put(timeIndex(req.Message.ConversationID+":", req.Message.CreatedAt, req.Message.ID), []byte(req.Message.ID)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketInbox).Put(timeIndex(req.Message.RecipientSessionID+":", req.Message.CreatedAt, req.Message.ID), []byte(req.Message.ID)); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket(bucketAttempts), []byte(req.Attempt.ID), req.Attempt); err != nil {
			return err
		}
		if err := tx.Bucket(bucketAttemptByMsg).Put(compound(req.Attempt.MessageID, req.Attempt.ID), []byte(req.Attempt.ID)); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket(bucketReservations), []byte(req.Reservation.ID), req.Reservation); err != nil {
			return err
		}
		if err := tx.Bucket(bucketResByMsg).Put([]byte(req.Reservation.MessageID), []byte(req.Reservation.ID)); err != nil {
			return err
		}

		conv.SpentCount++
		if err := putJSON(convs, []byte(conv.ID), conv); err != nil {
			return err
		}

		rec := model.IdemRecord{
			RecordVersion:    1,
			Fingerprint:      req.Fingerprint,
			MessageID:        req.Message.ID,
			ConversationID:   conv.ID,
			FirstSeen:        req.Now,
			Outcome:          "prepared",
			BodySHA256:       req.Message.BodySHA256,
			ReplyTo:          req.Message.ReplyTo,
			ExpectReply:      req.Message.ExpectReply,
			RecipientAlias:   req.RecipientAlias,
			ConversationName: req.ConversationName,
		}
		if err := putJSON(idem, ikey, rec); err != nil {
			return err
		}
		if err := tx.Bucket(bucketIdemByMsg).Put([]byte(rec.MessageID), ikey); err != nil {
			return err
		}

		if req.AckOriginalID != "" {
			if err := ackInTx(tx, req.AckOriginalID, req.AckProvenance, req.AckActor, req.Message.ID, req.Now); err != nil {
				return err
			}
		}
		if err := appendEvent(tx, model.Event{MessageID: req.Message.ID, AttemptID: req.Attempt.ID, Kind: "prepared", At: req.Now}); err != nil {
			return err
		}

		out.Message = req.Message
		out.Attempt = req.Attempt
		out.Reservation = req.Reservation
		out.Conversation = conv
		return nil
	})
	return out, err
}

func (s *Store) MarkDispatched(attemptID string, finalizationGrace time.Duration) error {
	return s.update(func(tx *bolt.Tx) error {
		now := s.now()
		attempts := tx.Bucket(bucketAttempts)
		var a model.Attempt
		if err := getJSON(attempts, []byte(attemptID), &a); err != nil {
			return err
		}
		if a.State == model.AttemptFinalized {
			return model.NewError(model.ErrAmbiguousTarget, "attempt is already finalized")
		}
		if a.State == model.AttemptDispatched {
			return nil
		}
		if now.After(a.DispatchDeadline) {
			return model.NewError(model.ErrDispatchDeadline, "dispatch deadline expired before payload I/O")
		}
		res := tx.Bucket(bucketReservations)
		var r model.Reservation
		rid := []byte(a.ReservationID)
		if len(rid) == 0 {
			rid = tx.Bucket(bucketResByMsg).Get([]byte(a.MessageID))
		}
		if len(rid) == 0 {
			return model.NewError(model.ErrInternal, "reservation missing for attempt")
		}
		if err := getJSON(res, rid, &r); err != nil {
			return err
		}
		if r.State != model.ReservationReserved {
			return model.NewError(model.ErrDispatchDeadline, "reservation is no longer usable")
		}
		a.State = model.AttemptDispatched
		a.DispatchedAt = &now
		fd := now.Add(finalizationGrace)
		a.FinalizationDeadline = &fd
		if err := putJSON(attempts, []byte(a.ID), a); err != nil {
			return err
		}
		return appendEvent(tx, model.Event{MessageID: a.MessageID, AttemptID: a.ID, Kind: "dispatched", At: now})
	})
}

type FinalizeRequest struct {
	AttemptID          string
	Outcome            string // submitted | failed_before_submit | unknown
	NativeSubmissionID string
	NativeDisposition  string
	ErrorCode          string
	ErrorDetail        string
	Now                time.Time
	Inferred           bool
}

func (s *Store) FinalizeAttempt(req FinalizeRequest) error {
	return s.update(func(tx *bolt.Tx) error {
		return finalizeInTx(tx, req)
	})
}

func finalizeInTx(tx *bolt.Tx, req FinalizeRequest) error {
	attempts := tx.Bucket(bucketAttempts)
	var a model.Attempt
	if err := getJSON(attempts, []byte(req.AttemptID), &a); err != nil {
		return err
	}
	refining := false
	previousOutcome := a.Outcome
	if a.State == model.AttemptFinalized {
		if a.Outcome == req.Outcome {
			if a.Inferred && (req.NativeSubmissionID != "" || req.ErrorDetail != "" || req.NativeDisposition != "") {
				// Same conclusion, now backed by observed evidence.
				if req.NativeSubmissionID != "" {
					a.NativeSubmissionID = req.NativeSubmissionID
				}
				if req.NativeDisposition != "" {
					a.NativeDisposition = req.NativeDisposition
				}
				if req.ErrorCode != "" {
					a.ErrorCode = req.ErrorCode
				}
				if req.ErrorDetail != "" {
					a.ErrorDetail = req.ErrorDetail
				}
				a.Inferred = false
				return putJSON(attempts, []byte(a.ID), a)
			}
			return nil
		}
		if !a.Inferred {
			return model.NewError(model.ErrInternal, "attempt outcome already finalized differently")
		}
		// A recovery-inferred unknown or pre-write failure may be refined by
		// later observed evidence (for example an imported journal record).
		if a.Outcome != "unknown" && a.Outcome != "failed_before_submit" {
			return model.NewError(model.ErrInternal, "attempt outcome already finalized differently")
		}
		refining = true
	}
	switch req.Outcome {
	case "submitted", "unknown", "failed_before_submit":
	default:
		return model.NewError(model.ErrInternal, "unknown attempt outcome")
	}

	msg, err := loadMessage(tx, a.MessageID)
	if err != nil {
		return err
	}
	now := req.Now
	acknowledged := msg.State == model.MessageAcknowledged

	a.State = model.AttemptFinalized
	a.Outcome = req.Outcome
	a.NativeSubmissionID = req.NativeSubmissionID
	if req.NativeDisposition != "" {
		a.NativeDisposition = req.NativeDisposition
	}
	a.ErrorCode = req.ErrorCode
	a.ErrorDetail = req.ErrorDetail
	a.FinalizedAt = &now
	a.Inferred = req.Inferred

	if refining {
		if !acknowledged {
			msg.State = messageStateForOutcome(req.Outcome)
		}
	} else {
		switch req.Outcome {
		case "submitted":
			if msg.State == model.MessageDispatching {
				msg.State = model.MessageSubmitted
			}
		case "unknown":
			if msg.State == model.MessageDispatching {
				msg.State = model.MessageUnknown
			}
		case "failed_before_submit":
			if msg.State == model.MessageDispatching {
				msg.State = model.MessageFailedBeforeSubmit
			}
		}
	}
	if req.NativeDisposition != "" {
		msg.NativeDisposition = req.NativeDisposition
	}

	desired := model.ReservationConsumed
	if req.Outcome == "failed_before_submit" && !acknowledged {
		desired = model.ReservationRefunded
	}
	rid := []byte(a.ReservationID)
	if len(rid) == 0 {
		rid = tx.Bucket(bucketResByMsg).Get([]byte(msg.ID))
	}
	if len(rid) == 0 {
		return model.NewError(model.ErrInternal, "message has no reservation reference")
	}
	res := tx.Bucket(bucketReservations)
	var r model.Reservation
	if err := getJSON(res, rid, &r); err != nil {
		return model.WrapError(model.ErrInternal, "reservation for finalization is missing", err)
	}
	adjust := 0
	kind := ""
	switch {
	case r.State == model.ReservationReserved:
		r.State = desired
		if desired == model.ReservationRefunded {
			adjust = 1
			kind = "refunded"
		} else {
			kind = "consumed"
		}
	case refining && r.State != desired:
		if r.State == model.ReservationRefunded {
			adjust = -1
		} else if desired == model.ReservationRefunded {
			adjust = 1
		}
		r.State = desired
		kind = "refined"
	}
	if kind != "" {
		if err := putJSON(res, []byte(r.ID), r); err != nil {
			return err
		}
		if adjust != 0 {
			convs := tx.Bucket(bucketConversations)
			var conv model.Conversation
			if err := getJSON(convs, []byte(r.ConversationID), &conv); err != nil {
				return model.WrapError(model.ErrInternal, "conversation for finalization is missing", err)
			}
			conv.RefundedCount += adjust
			if conv.RefundedCount < 0 {
				return model.NewError(model.ErrInternal, "refund counter would become negative")
			}
			if err := putJSON(convs, []byte(conv.ID), conv); err != nil {
				return err
			}
		}
		payload := map[string]any{"reservation_state": string(r.State)}
		if refining {
			payload["previous_outcome"] = previousOutcome
		}
		if err := appendEvent(tx, model.Event{MessageID: msg.ID, AttemptID: a.ID, Kind: kind, At: now, Payload: payload}); err != nil {
			return err
		}
	}

	if err := putJSON(attempts, []byte(a.ID), a); err != nil {
		return err
	}
	if err := putJSON(tx.Bucket(bucketMessages), []byte(msg.ID), msg); err != nil {
		return err
	}
	if err := appendEvent(tx, model.Event{MessageID: msg.ID, AttemptID: a.ID, Kind: "outcome", At: now, Payload: map[string]any{"outcome": req.Outcome, "native_id": req.NativeSubmissionID, "disposition": req.NativeDisposition, "inferred": req.Inferred, "refined": refining}}); err != nil {
		return err
	}
	// Update the compact idempotency record's final outcome without touching
	// its fingerprint or binding.
	idem := tx.Bucket(bucketIdem)
	rec := model.IdemRecord{}
	key := tx.Bucket(bucketIdemByMsg).Get([]byte(msg.ID))
	if key != nil {
		if err := getJSON(idem, key, &rec); err != nil {
			return model.WrapError(model.ErrInternal, "idempotency index points at a missing record", err)
		}
		rec.Outcome = req.Outcome
		return putJSON(idem, key, rec)
	}
	// Fallback for state written before the reverse index existed.
	c := idem.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if err := decodeJSON(v, &rec); err != nil {
			return err
		}
		if rec.MessageID != msg.ID {
			continue
		}
		rec.Outcome = req.Outcome
		if err := putJSON(idem, k, rec); err != nil {
			return err
		}
		return tx.Bucket(bucketIdemByMsg).Put([]byte(msg.ID), append([]byte(nil), k...))
	}
	return nil
}

func messageStateForOutcome(outcome string) model.MessageState {
	switch outcome {
	case "submitted":
		return model.MessageSubmitted
	case "failed_before_submit":
		return model.MessageFailedBeforeSubmit
	default:
		return model.MessageUnknown
	}
}

func (s *Store) AckMessage(messageID string, provenance model.AckProvenance, actor, replyMessageID string, now time.Time) error {
	return s.update(func(tx *bolt.Tx) error {
		return ackInTx(tx, messageID, provenance, actor, replyMessageID, now)
	})
}

func ackInTx(tx *bolt.Tx, messageID string, provenance model.AckProvenance, actor, replyMessageID string, now time.Time) error {
	msgs := tx.Bucket(bucketMessages)
	var msg model.Message
	if err := getJSON(msgs, []byte(messageID), &msg); err != nil {
		return err
	}
	if msg.State != model.MessageAcknowledged {
		msg.State = model.MessageAcknowledged
		if err := putJSON(msgs, []byte(messageID), msg); err != nil {
			return err
		}
	}
	kind := "ack"
	if replyMessageID != "" {
		kind = "reply"
	}
	rec := model.Receipt{
		RecordVersion: 1,
		ID:            receiptID(now, messageID, kind),
		MessageID:     messageID,
		Kind:          kind,
		Provenance:    provenance,
		Actor:         actor,
		ReplyMessage:  replyMessageID,
		CreatedAt:     now,
	}
	if err := putJSON(tx.Bucket(bucketReceipts), []byte(rec.ID), rec); err != nil {
		return err
	}
	if err := tx.Bucket(bucketReceiptByMsg).Put(compound(messageID, rec.ID), []byte(rec.ID)); err != nil {
		return err
	}
	return appendEvent(tx, model.Event{MessageID: messageID, Kind: kind, At: now, Payload: map[string]any{"provenance": string(provenance), "actor": actor}})
}

func receiptID(now time.Time, messageID, kind string) string {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(now.UnixNano()))
	return "rcpt_" + hex.EncodeToString(b) + "_" + kind + "_" + messageID
}

func appendEvent(tx *bolt.Tx, ev model.Event) error {
	meta := tx.Bucket(bucketMeta)
	var seq uint64
	if raw := meta.Get([]byte("event_seq")); raw != nil && len(raw) == 8 {
		seq = binary.BigEndian.Uint64(raw)
	}
	seq++
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	if err := meta.Put([]byte("event_seq"), b); err != nil {
		return err
	}
	ev.Seq = seq
	if err := putJSON(tx.Bucket(bucketEvents), b, ev); err != nil {
		return err
	}
	if ev.MessageID != "" {
		if err := tx.Bucket(bucketEventByMsg).Put(compound(ev.MessageID, string(b)), b); err != nil {
			return err
		}
	}
	if ev.AttemptID != "" {
		if err := tx.Bucket(bucketEventByAttempt).Put(compound(ev.AttemptID, string(b)), b); err != nil {
			return err
		}
	}
	return nil
}

func loadMessage(tx *bolt.Tx, id string) (model.Message, error) {
	var m model.Message
	err := getJSON(tx.Bucket(bucketMessages), []byte(id), &m)
	return m, err
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

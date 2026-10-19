package store

import (
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

type Violation struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// CheckInvariants is read-only. It reports inconsistent records; it never
// repairs or prunes them.
func (s *Store) CheckInvariants(now time.Time) ([]Violation, error) {
	var out []Violation
	err := s.view(func(tx *bolt.Tx) error {
		convs := tx.Bucket(bucketConversations)
		msgs := tx.Bucket(bucketMessages)
		attempts := tx.Bucket(bucketAttempts)
		reservations := tx.Bucket(bucketReservations)
		idem := tx.Bucket(bucketIdem)

		// Conversation counters are derived from the immutable reservation
		// ledger, not from surviving message rows.
		total := map[string]int{}
		refunded := map[string]int{}
		if err := scanPrefixN(reservations, nil, 0, func(_, v []byte) error {
			var r model.Reservation
			if err := decodeJSON(v, &r); err != nil {
				return err
			}
			total[r.ConversationID]++
			if r.State == model.ReservationRefunded {
				refunded[r.ConversationID]++
			}
			if r.State == model.ReservationReserved {
				if err := scanAttemptsForMessage(tx.Bucket(bucketAttemptByMsg), attempts, r.MessageID, func(a model.Attempt) {
					if a.ReservationID == r.ID && a.State == model.AttemptFinalized && a.Outcome == "failed_before_submit" {
						out = append(out, Violation{"reserved_after_failed_outcome", r.ID})
					}
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}

		if err := scanPrefixN(convs, nil, 0, func(_, v []byte) error {
			var c model.Conversation
			if err := decodeJSON(v, &c); err != nil {
				return err
			}
			if c.SpentCount != total[c.ID] {
				out = append(out, Violation{"spent_count_mismatch", fmt.Sprintf("%s: record=%d ledger=%d", c.ID, c.SpentCount, total[c.ID])})
			}
			if c.RefundedCount != refunded[c.ID] {
				out = append(out, Violation{"refunded_count_mismatch", fmt.Sprintf("%s: record=%d ledger=%d", c.ID, c.RefundedCount, refunded[c.ID])})
			}
			if c.SpentCount-c.RefundedCount > c.MaxMessages {
				out = append(out, Violation{"budget_overrun", c.ID})
			}
			return nil
		}); err != nil {
			return err
		}

		if err := scanPrefixN(msgs, nil, 0, func(_, v []byte) error {
			var m model.Message
			if err := decodeJSON(v, &m); err != nil {
				return err
			}
			if _, err := loadConversation(convs, m.ConversationID); err != nil {
				out = append(out, Violation{"message_orphan_conversation", m.ID})
			}
			if m.ReplyTo != "" {
				var parent model.Message
				if err := getJSON(msgs, []byte(m.ReplyTo), &parent); err != nil {
					out = append(out, Violation{"reply_missing_parent", m.ID})
				} else if parent.ConversationID != m.ConversationID {
					out = append(out, Violation{"reply_wrong_conversation", m.ID})
				}
			}
			if m.State == model.MessageAcknowledged {
				// A pending attempt is legitimate until its finalization
				// deadline; only a stale one is a violation.
				apply := func(a model.Attempt) {
					if a.State == model.AttemptFinalized {
						return
					}
					if a.State == model.AttemptDispatching && now.After(a.DispatchDeadline.Add(10*time.Second)) {
						out = append(out, Violation{"stale_dispatching_after_ack", a.ID})
					}
					if a.State == model.AttemptDispatched && a.FinalizationDeadline != nil && now.After(a.FinalizationDeadline.Add(10*time.Second)) {
						out = append(out, Violation{"stale_dispatched_after_ack", a.ID})
					}
				}
				if err := scanAttemptsForMessage(tx.Bucket(bucketAttemptByMsg), attempts, m.ID, apply); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}

		if err := scanPrefixN(attempts, nil, 0, func(_, v []byte) error {
			var a model.Attempt
			if err := decodeJSON(v, &a); err != nil {
				return err
			}
			if err := getJSON(msgs, []byte(a.MessageID), &model.Message{}); err != nil {
				out = append(out, Violation{"attempt_orphan_message", a.ID})
			}
			switch a.State {
			case model.AttemptFinalized:
				if a.Outcome == "" {
					out = append(out, Violation{"finalized_without_outcome", a.ID})
				}
			case model.AttemptDispatched:
				if a.DispatchedAt == nil || a.FinalizationDeadline == nil {
					out = append(out, Violation{"dispatched_without_deadline", a.ID})
				}
			case model.AttemptDispatching:
				if now.After(a.DispatchDeadline.Add(10 * time.Second)) {
					out = append(out, Violation{"stale_dispatching", a.ID})
				}
			}
			return nil
		}); err != nil {
			return err
		}

		if err := scanPrefixN(idem, nil, 0, func(_, v []byte) error {
			var r model.IdemRecord
			if err := decodeJSON(v, &r); err != nil {
				return err
			}
			if r.Fingerprint == "" || r.MessageID == "" {
				out = append(out, Violation{"idempotency_incomplete", r.MessageID})
			}
			if err := getJSON(msgs, []byte(r.MessageID), &model.Message{}); err != nil && !r.MessagePruned {
				out = append(out, Violation{"idempotency_orphan_message", r.MessageID})
			}
			return nil
		}); err != nil {
			return err
		}
		return nil
	})
	return out, err
}

func loadConversation(b *bolt.Bucket, id string) (model.Conversation, error) {
	var c model.Conversation
	err := getJSON(b, []byte(id), &c)
	return c, err
}

func scanAttemptsForMessage(index, attempts *bolt.Bucket, messageID string, fn func(model.Attempt)) error {
	c := index.Cursor()
	prefix := compound(messageID, "")
	for k, v := c.Seek(prefix); k != nil; k, v = c.Next() {
		if len(k) < len(prefix) || string(k[:len(prefix)]) != string(prefix) {
			break
		}
		var a model.Attempt
		if err := getJSON(attempts, v, &a); err != nil {
			return err
		}
		fn(a)
	}
	return nil
}

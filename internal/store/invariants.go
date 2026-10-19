package store

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
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

		// Jobs, leases and artifacts are additive records. Active jobs must be
		// leased; terminal jobs must carry a finish time; every owned path must
		// remain under the artifacts root.
		jobs := tx.Bucket(bucketJobs)
		leased := map[string]bool{}
		if err := scanPrefixN(tx.Bucket(bucketLeases), nil, 0, func(_, v []byte) error {
			var l model.Lease
			if err := decodeJSON(v, &l); err != nil {
				return err
			}
			leased[l.JobID] = true
			if jobs.Get([]byte(l.JobID)) == nil {
				out = append(out, Violation{"lease_orphan_job", l.JobID})
			}
			return nil
		}); err != nil {
			return err
		}
		if err := scanPrefixN(jobs, nil, 0, func(_, v []byte) error {
			var j model.Job
			if err := decodeJSON(v, &j); err != nil {
				return err
			}
			if j.State.Terminal() && j.FinishedAt == nil {
				out = append(out, Violation{"job_terminal_without_finish", j.ID})
			}
			if !j.State.Terminal() {
				if now.After(j.Deadline.Add(10 * time.Minute)) {
					out = append(out, Violation{"job_overdue", j.ID})
				}
				if !leased[j.ID] && now.Sub(j.UpdatedAt) > time.Minute {
					out = append(out, Violation{"job_active_without_lease", j.ID})
				}
			}
			if !ownedArtifactPath(j.LogPath) {
				out = append(out, Violation{"job_log_outside_artifacts", j.ID})
			}
			return nil
		}); err != nil {
			return err
		}
		if err := scanPrefixN(tx.Bucket(bucketArtifacts), nil, 0, func(k, v []byte) error {
			i := bytes.IndexByte(k, 0)
			if i <= 0 {
				out = append(out, Violation{"artifact_key_malformed", string(k)})
				return nil
			}
			jobID := string(k[:i])
			if jobs.Get([]byte(jobID)) == nil {
				out = append(out, Violation{"artifact_orphan_job", jobID})
			}
			var a model.Artifact
			if err := decodeJSON(v, &a); err != nil {
				return err
			}
			if !ownedArtifactPath(a.Path) {
				out = append(out, Violation{"artifact_outside_artifacts", a.ID})
			}
			return nil
		}); err != nil {
			return err
		}
		if err := scanPrefixN(tx.Bucket(bucketJobEventByJob), nil, 0, func(k, v []byte) error {
			i := bytes.IndexByte(k, 0)
			if i <= 0 {
				out = append(out, Violation{"job_event_key_malformed", string(k)})
				return nil
			}
			if jobs.Get(k[:i]) == nil {
				out = append(out, Violation{"job_event_orphan_job", string(k[:i])})
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

func ownedArtifactPath(rel string) bool {
	if rel == "" {
		return true
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return strings.HasPrefix(clean, "artifacts"+string(filepath.Separator))
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

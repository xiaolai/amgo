package store

import (
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

type kvPair struct {
	k, v []byte
}

type PruneReport struct {
	DryRun             bool  `json:"dry_run"`
	Messages           int   `json:"messages"`
	BodyBytes          int   `json:"body_bytes"`
	IdempotencyDeleted int   `json:"idempotency_deleted"`
	Compacted          bool  `json:"compacted"`
	BytesAfter         int64 `json:"bytes_after,omitempty"`
}

// pruneCandidate holds only the fields pruning needs. Decoding into this type
// deliberately omits the body, so a large inbox is not materialized in memory.
type pruneCandidate struct {
	ID                 string             `json:"id"`
	ConversationID     string             `json:"conversation_id"`
	RecipientSessionID string             `json:"recipient_session_id"`
	ReplyTo            string             `json:"reply_to"`
	State              model.MessageState `json:"state"`
	CreatedAt          time.Time          `json:"created_at"`
	BodyBytes          int                `json:"body_bytes"`
}

// Prune removes fully acknowledged messages older than olderThan. Unknown,
// active and dispatching records are never removed. A message referenced as a
// reply parent is retained even when it is otherwise eligible. Idempotency
// membership is retained by default; pruneIdem explicitly deletes old
// membership records.
func (s *Store) Prune(now, olderThan time.Time, dryRun, pruneIdem bool) (PruneReport, error) {
	rep := PruneReport{DryRun: dryRun}
	fn := func(tx *bolt.Tx) error {
		msgs := tx.Bucket(bucketMessages)
		protected := map[string]bool{}
		var candidates []pruneCandidate
		if err := scanPrefixN(msgs, nil, 0, func(_, v []byte) error {
			var m pruneCandidate
			if err := decodeJSON(v, &m); err != nil {
				return err
			}
			if m.ReplyTo != "" {
				protected[m.ReplyTo] = true
			}
			if m.State != model.MessageAcknowledged || !m.CreatedAt.Before(olderThan) {
				return nil
			}
			candidates = append(candidates, m)
			return nil
		}); err != nil {
			return err
		}

		var victims []pruneCandidate
		for _, m := range candidates {
			if protected[m.ID] {
				continue
			}
			// Never prune a message with an in-flight attempt.
			live := false
			if err := scanAttemptsForMessage(tx.Bucket(bucketAttemptByMsg), tx.Bucket(bucketAttempts), m.ID, func(a model.Attempt) {
				if a.State != model.AttemptFinalized {
					live = true
				}
			}); err != nil {
				return err
			}
			if live {
				continue
			}
			victims = append(victims, m)
		}
		rep.Messages = len(victims)
		if dryRun {
			for _, m := range victims {
				rep.BodyBytes += m.BodyBytes
			}
			return nil
		}

		for _, m := range victims {
			rep.BodyBytes += m.BodyBytes
			if err := msgs.Delete([]byte(m.ID)); err != nil {
				return err
			}
			if err := tx.Bucket(bucketMsgByConv).Delete(timeIndex(m.ConversationID+":", m.CreatedAt, m.ID)); err != nil {
				return err
			}
			if err := tx.Bucket(bucketInbox).Delete(timeIndex(m.RecipientSessionID+":", m.CreatedAt, m.ID)); err != nil {
				return err
			}
			var attemptPairs []kvPair
			if err := scanPrefixN(tx.Bucket(bucketAttemptByMsg), compound(m.ID, ""), 0, func(k, v []byte) error {
				attemptPairs = append(attemptPairs, kvPair{k: append([]byte(nil), k...), v: append([]byte(nil), v...)})
				return nil
			}); err != nil {
				return err
			}
			for _, p := range attemptPairs {
				if err := tx.Bucket(bucketAttempts).Delete(p.v); err != nil {
					return err
				}
				if err := tx.Bucket(bucketAttemptByMsg).Delete(p.k); err != nil {
					return err
				}
			}
			// The reservation ledger is retained: spent_count must survive
			// message pruning, and the counter invariant is derived from it.
			var receiptPairs []kvPair
			if err := scanPrefixN(tx.Bucket(bucketReceiptByMsg), compound(m.ID, ""), 0, func(k, v []byte) error {
				receiptPairs = append(receiptPairs, kvPair{k: append([]byte(nil), k...), v: append([]byte(nil), v...)})
				return nil
			}); err != nil {
				return err
			}
			for _, p := range receiptPairs {
				if err := tx.Bucket(bucketReceipts).Delete(p.v); err != nil {
					return err
				}
				if err := tx.Bucket(bucketReceiptByMsg).Delete(p.k); err != nil {
					return err
				}
			}
			// Mark idempotency membership as pruned; retain the binding.
			idem := tx.Bucket(bucketIdem)
			c := idem.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var rec model.IdemRecord
				if err := decodeJSON(v, &rec); err != nil {
					return err
				}
				if rec.MessageID != m.ID {
					continue
				}
				rec.MessagePruned = true
				if err := putJSON(idem, append([]byte(nil), k...), rec); err != nil {
					return err
				}
				break
			}
		}
		if pruneIdem {
			idem := tx.Bucket(bucketIdem)
			type doomed struct {
				key     []byte
				message string
			}
			var keys []doomed
			c := idem.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var rec model.IdemRecord
				if err := decodeJSON(v, &rec); err != nil {
					return err
				}
				if rec.FirstSeen.Before(olderThan) {
					keys = append(keys, doomed{key: append([]byte(nil), k...), message: rec.MessageID})
				}
			}
			rep.IdempotencyDeleted = len(keys)
			for _, d := range keys {
				if err := idem.Delete(d.key); err != nil {
					return err
				}
				if d.message != "" {
					if err := tx.Bucket(bucketIdemByMsg).Delete([]byte(d.message)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	var err error
	if dryRun {
		err = s.view(fn)
	} else {
		err = s.update(fn)
	}
	return rep, err
}

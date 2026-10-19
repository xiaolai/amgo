package store

import (
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

type AttemptBatch struct {
	Attempts []model.Attempt
	Next     string
	More     bool
}

// ScanAttempts reads a bounded batch of attempts in stable key order. Liveness
// probes must happen after this method returns and outside the database lock.
func (s *Store) ScanAttempts(cursor string, limit int) (AttemptBatch, error) {
	var out AttemptBatch
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketAttempts).Cursor()
		var k, v []byte
		if cursor == "" {
			k, v = c.First()
		} else {
			k, v = c.Seek([]byte(cursor))
			if k != nil && string(k) == cursor {
				k, v = c.Next()
			}
		}
		for k != nil {
			var a model.Attempt
			if err := decodeJSON(v, &a); err != nil {
				return err
			}
			out.Attempts = append(out.Attempts, a)
			out.Next = string(k)
			if limit > 0 && len(out.Attempts) >= limit {
				nk, _ := c.Next()
				out.More = nk != nil
				return nil
			}
			k, v = c.Next()
		}
		out.More = false
		return nil
	})
	return out, err
}

// RecoverAttempt applies a phase-aware recovery action. It is a no-op when the
// attempt has already advanced past the action's source state.
func (s *Store) RecoverAttempt(attemptID, action string, now time.Time) error {
	return s.update(func(tx *bolt.Tx) error {
		var a model.Attempt
		if err := getJSON(tx.Bucket(bucketAttempts), []byte(attemptID), &a); err != nil {
			return err
		}
		if a.State == model.AttemptFinalized {
			return nil
		}
		switch action {
		case "failed_before_submit":
			if a.State != model.AttemptDispatching {
				return nil
			}
		case "unknown":
			if a.State != model.AttemptDispatching && a.State != model.AttemptDispatched {
				return nil
			}
		default:
			return model.NewError(model.ErrInternal, "unknown recovery action")
		}
		return finalizeInTx(tx, FinalizeRequest{
			AttemptID:         attemptID,
			Outcome:           action,
			NativeDisposition: model.DispositionUnknown,
			Now:               now,
			Inferred:          true,
		})
	})
}

func (s *Store) ReconcileCursor() (string, error) {
	var cursor string
	err := s.view(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get([]byte("reconcile_cursor"))
		if raw != nil {
			cursor = string(raw)
		}
		return nil
	})
	return cursor, err
}

func (s *Store) SetReconcileCursor(cursor string) error {
	return s.update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put([]byte("reconcile_cursor"), []byte(cursor))
	})
}

package store

import (
	"bytes"
	"sort"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

func (s *Store) GetMessage(id string) (model.Message, error) {
	var out model.Message
	err := s.view(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketMessages), []byte(id), &out)
	})
	return out, err
}

func (s *Store) ListAttempts(messageID string) ([]model.Attempt, error) {
	var out []model.Attempt
	err := s.view(func(tx *bolt.Tx) error {
		prefix := compound(messageID, "")
		return scanPrefixN(tx.Bucket(bucketAttemptByMsg), prefix, 0, func(_, v []byte) error {
			var a model.Attempt
			if err := getJSON(tx.Bucket(bucketAttempts), v, &a); err != nil {
				return err
			}
			out = append(out, a)
			return nil
		})
	})
	return out, err
}

func (s *Store) ListReceipts(messageID string) ([]model.Receipt, error) {
	var out []model.Receipt
	err := s.view(func(tx *bolt.Tx) error {
		prefix := compound(messageID, "")
		return scanPrefixN(tx.Bucket(bucketReceiptByMsg), prefix, 0, func(_, v []byte) error {
			var r model.Receipt
			if err := getJSON(tx.Bucket(bucketReceipts), v, &r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, err
}

func (s *Store) ListEvents(messageID string, limit int) ([]model.Event, error) {
	var out []model.Event
	err := s.view(func(tx *bolt.Tx) error {
		prefix := compound(messageID, "")
		return scanPrefixN(tx.Bucket(bucketEventByMsg), prefix, limit, func(_, v []byte) error {
			var ev model.Event
			if err := getJSON(tx.Bucket(bucketEvents), v, &ev); err != nil {
				return err
			}
			out = append(out, ev)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, err
}

func (s *Store) listMessagesByIndex(bucket []byte, prefix string, limit int) ([]model.Message, error) {
	var ids []string
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		start := []byte(prefix + "\x00")
		// Every key with this prefix is below start with its last byte
		// incremented; that bound lets the cursor walk backward from the newest
		// entry without scanning the whole index.
		upper := append([]byte(nil), start...)
		upper[len(upper)-1]++
		c := b.Cursor()
		k, v := c.Seek(upper)
		if k == nil {
			k, v = c.Last()
		} else {
			k, v = c.Prev()
		}
		for ; k != nil && bytes.HasPrefix(k, start); k, v = c.Prev() {
			ids = append(ids, string(v))
			if limit > 0 && len(ids) >= limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]model.Message, 0, len(ids))
	for _, id := range ids {
		m, err := s.GetMessage(id)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) ListConversationMessages(convID string, limit int) ([]model.Message, error) {
	return s.listMessagesByIndex(bucketMsgByConv, convID+":", limit)
}

func (s *Store) ListInbox(sessionID string, limit int) ([]model.Message, error) {
	return s.listMessagesByIndex(bucketInbox, sessionID+":", limit)
}

func (s *Store) ListAllMessages(limit int) ([]model.Message, error) {
	var out []model.Message
	err := s.view(func(tx *bolt.Tx) error {
		return scanPrefixN(tx.Bucket(bucketMessages), nil, limit, func(_, v []byte) error {
			var m model.Message
			if err := decodeJSON(v, &m); err != nil {
				return err
			}
			out = append(out, m)
			return nil
		})
	})
	return out, err
}

func (s *Store) GetReservationForMessage(messageID string) (model.Reservation, error) {
	var out model.Reservation
	err := s.view(func(tx *bolt.Tx) error {
		rid := tx.Bucket(bucketResByMsg).Get([]byte(messageID))
		if rid == nil {
			return model.NewError(model.ErrTargetUnavailable, "reservation not found")
		}
		return getJSON(tx.Bucket(bucketReservations), rid, &out)
	})
	return out, err
}

package store

import (
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

func (s *Store) CreateConversation(c model.Conversation) error {
	return s.update(func(tx *bolt.Tx) error {
		convs := tx.Bucket(bucketConversations)
		names := tx.Bucket(bucketConvByName)
		if c.Name != "" {
			if id := names.Get([]byte(c.Name)); id != nil {
				return model.NewError(model.ErrAmbiguousTarget, "conversation name already exists")
			}
		}
		if err := putJSON(convs, []byte(c.ID), c); err != nil {
			return err
		}
		if c.Name != "" {
			return names.Put([]byte(c.Name), []byte(c.ID))
		}
		return nil
	})
}

func (s *Store) GetConversation(id string) (model.Conversation, error) {
	var out model.Conversation
	err := s.view(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketConversations), []byte(id), &out)
	})
	return out, err
}

func (s *Store) GetConversationByName(name string) (model.Conversation, error) {
	var out model.Conversation
	err := s.view(func(tx *bolt.Tx) error {
		id := tx.Bucket(bucketConvByName).Get([]byte(name))
		if id == nil {
			return model.NewError(model.ErrTargetUnavailable, "no such conversation")
		}
		return getJSON(tx.Bucket(bucketConversations), id, &out)
	})
	return out, err
}

func (s *Store) ListConversations() ([]model.Conversation, error) {
	var out []model.Conversation
	err := s.view(func(tx *bolt.Tx) error {
		return scanPrefixN(tx.Bucket(bucketConversations), nil, 0, func(_, v []byte) error {
			var c model.Conversation
			if err := decodeJSON(v, &c); err != nil {
				return err
			}
			out = append(out, c)
			return nil
		})
	})
	return out, err
}

func (s *Store) CloseConversation(id string, at time.Time) error {
	return s.update(func(tx *bolt.Tx) error {
		convs := tx.Bucket(bucketConversations)
		var c model.Conversation
		if err := getJSON(convs, []byte(id), &c); err != nil {
			return err
		}
		if c.ClosedAt == nil {
			c.ClosedAt = &at
			return putJSON(convs, []byte(id), c)
		}
		return nil
	})
}

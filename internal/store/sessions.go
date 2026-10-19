package store

import (
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

func (s *Store) PutSession(sess model.Session) error {
	return s.update(func(tx *bolt.Tx) error {
		sessions := tx.Bucket(bucketSessions)
		aliases := tx.Bucket(bucketAliases)

		if id := aliases.Get([]byte(sess.Alias)); id != nil && string(id) != sess.ID {
			return model.NewError(model.ErrAmbiguousTarget, "alias is already registered to another session")
		}

		var old model.Session
		ok, err := getJSONOK(sessions, []byte(sess.ID), &old)
		if err != nil {
			return err
		}
		if ok && old.Alias != "" && old.Alias != sess.Alias {
			// Delete only when the mapping still points at this session; a
			// later registration may own the alias now.
			if id := aliases.Get([]byte(old.Alias)); id != nil && string(id) == sess.ID {
				if err := aliases.Delete([]byte(old.Alias)); err != nil {
					return err
				}
			}
		}

		if err := putJSON(sessions, []byte(sess.ID), sess); err != nil {
			return err
		}
		return aliases.Put([]byte(sess.Alias), []byte(sess.ID))
	})
}

func (s *Store) GetSession(id string) (model.Session, error) {
	var out model.Session
	err := s.view(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketSessions), []byte(id), &out)
	})
	return out, err
}

func (s *Store) GetSessionByAlias(alias string) (model.Session, error) {
	var out model.Session
	err := s.view(func(tx *bolt.Tx) error {
		id := tx.Bucket(bucketAliases).Get([]byte(alias))
		if id == nil {
			return model.NewError(model.ErrTargetUnavailable, "no registered session with that alias")
		}
		return getJSON(tx.Bucket(bucketSessions), id, &out)
	})
	return out, err
}

func (s *Store) FindSessionByTarget(adapter, configHome, nativeID string) (model.Session, bool, error) {
	var out model.Session
	found := false
	err := s.view(func(tx *bolt.Tx) error {
		return scanPrefixN(tx.Bucket(bucketSessions), nil, 0, func(_, v []byte) error {
			var sess model.Session
			if err := decodeJSON(v, &sess); err != nil {
				return err
			}
			if sess.Target.Adapter == adapter && sess.Target.ConfigHome == configHome && sess.Target.NativeSessionID == nativeID {
				out = sess
				found = true
				return errStopScan
			}
			return nil
		})
	})
	return out, found, err
}

func (s *Store) ListSessions(includeInactive bool) ([]model.Session, error) {
	var out []model.Session
	err := s.view(func(tx *bolt.Tx) error {
		return scanPrefixN(tx.Bucket(bucketSessions), nil, 0, func(_, v []byte) error {
			var sess model.Session
			if err := decodeJSON(v, &sess); err != nil {
				return err
			}
			if includeInactive || sess.UnregisteredAt == nil {
				out = append(out, sess)
			}
			return nil
		})
	})
	return out, err
}

func (s *Store) UnregisterSession(id string, at time.Time) error {
	return s.update(func(tx *bolt.Tx) error {
		sessions := tx.Bucket(bucketSessions)
		var sess model.Session
		if err := getJSON(sessions, []byte(id), &sess); err != nil {
			return err
		}
		sess.UnregisteredAt = &at
		if err := putJSON(sessions, []byte(id), sess); err != nil {
			return err
		}
		return tx.Bucket(bucketAliases).Delete([]byte(sess.Alias))
	})
}

func (s *Store) SetAcceptUnknownDelivery(id string, value bool) error {
	return s.update(func(tx *bolt.Tx) error {
		sessions := tx.Bucket(bucketSessions)
		var sess model.Session
		if err := getJSON(sessions, []byte(id), &sess); err != nil {
			return err
		}
		sess.AcceptUnknownDelivery = value
		return putJSON(sessions, []byte(id), sess)
	})
}

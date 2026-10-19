// Package store is the transactional state layer. Every operation uses a short
// bbolt transaction; no native I/O happens while the database is open.
package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

var (
	bucketMeta           = []byte("meta")
	bucketSessions       = []byte("sessions")
	bucketAliases        = []byte("aliases")
	bucketConversations  = []byte("conversations")
	bucketConvByName     = []byte("conv_by_name")
	bucketMessages       = []byte("messages")
	bucketMsgByConv      = []byte("msg_by_conv")
	bucketInbox          = []byte("inbox_by_session")
	bucketAttempts       = []byte("attempts")
	bucketAttemptByMsg   = []byte("attempt_by_msg")
	bucketReservations   = []byte("reservations")
	bucketResByMsg       = []byte("res_by_msg")
	bucketEvents         = []byte("events")
	bucketEventByMsg     = []byte("event_by_msg")
	bucketEventByAttempt = []byte("event_by_attempt")
	bucketIdem           = []byte("idempotency_keys")
	bucketIdemByMsg      = []byte("idem_by_msg")
	bucketReceipts       = []byte("receipts")
	bucketReceiptByMsg   = []byte("receipt_by_msg")
	bucketJobs           = []byte("jobs")
	bucketLeases         = []byte("leases")
	bucketArtifacts      = []byte("artifacts")
)

var allBuckets = [][]byte{
	bucketMeta, bucketSessions, bucketAliases, bucketConversations,
	bucketConvByName, bucketMessages, bucketMsgByConv, bucketInbox,
	bucketAttempts, bucketAttemptByMsg, bucketReservations, bucketResByMsg,
	bucketEvents, bucketEventByMsg, bucketEventByAttempt, bucketIdem, bucketIdemByMsg,
	bucketReceipts, bucketReceiptByMsg, bucketJobs, bucketLeases, bucketArtifacts,
}

const schemaVersion = 1

type SchemaMeta struct {
	Version   int       `json:"version"`
	MinReader int       `json:"min_reader"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	db   *bolt.DB
	lock *os.File
	Now  func() time.Time
}

// Open opens the database with a bounded lock wait. The caller must close it
// before performing native transport I/O.
func Open(path string, timeout time.Duration) (*Store, error) {
	if err := secureStateDir(path); err != nil {
		return nil, model.WrapError(model.ErrStoreBusy, "cannot secure state directory", err)
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		if err := secureDBFile(path); err != nil {
			return nil, model.WrapError(model.ErrStoreBusy, "cannot secure state database", err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, model.WrapError(model.ErrStoreBusy, "cannot inspect state database", statErr)
	}
	lock, err := lockFile(path+".lock", false, timeout)
	if err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: timeout})
	if err != nil {
		unlockFile(lock)
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, &model.CodedError{Code: model.ErrStoreBusy, Message: "store lock wait exceeded", Retryable: true, Cause: err}
		}
		return nil, model.WrapError(model.ErrInternal, "cannot open state", err)
	}
	if err := secureDBFile(path); err != nil {
		_ = db.Close()
		unlockFile(lock)
		return nil, model.WrapError(model.ErrStoreBusy, "cannot secure state database", err)
	}
	s := &Store{db: db, lock: lock, Now: func() time.Time { return time.Now().UTC() }}
	if err := s.init(); err != nil {
		_ = db.Close()
		unlockFile(lock)
		return nil, err
	}
	return s, nil
}

// OpenReadOnly validates an existing database without creating buckets or
// writing metadata. It takes the same shared process lock as Open.
func OpenReadOnly(path string, timeout time.Duration) (*Store, error) {
	if err := validateStatePath(path); err != nil {
		return nil, model.WrapError(model.ErrTargetUnavailable, "state is not readable", err)
	}
	lock, err := lockFileReadOnly(path+".lock", timeout)
	if err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: timeout})
	if err != nil {
		unlockFile(lock)
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, &model.CodedError{Code: model.ErrStoreBusy, Message: "store lock wait exceeded", Retryable: true, Cause: err}
		}
		return nil, model.WrapError(model.ErrTargetUnavailable, "cannot open state read-only", err)
	}
	s := &Store{db: db, lock: lock, Now: func() time.Time { return time.Now().UTC() }}
	if err := s.checkSchema(); err != nil {
		_ = db.Close()
		unlockFile(lock)
		return nil, err
	}
	return s, nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	unlockFile(s.lock)
	s.lock = nil
	return err
}

func (s *Store) checkSchema() error {
	return s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if meta == nil {
			return &model.CodedError{Code: model.ErrStateIncompatible, Message: "state has no schema metadata"}
		}
		raw := meta.Get([]byte("schema"))
		if raw == nil {
			return &model.CodedError{Code: model.ErrStateIncompatible, Message: "state has no schema record"}
		}
		var sm SchemaMeta
		if err := json.Unmarshal(raw, &sm); err != nil {
			return model.WrapError(model.ErrStateIncompatible, "state schema is unreadable", err)
		}
		if sm.Version != schemaVersion {
			return &model.CodedError{Code: model.ErrStateIncompatible, Message: fmt.Sprintf("state schema %d, this build reads %d", sm.Version, schemaVersion)}
		}
		return nil
	})
}

func (s *Store) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		raw := meta.Get([]byte("schema"))
		if raw == nil {
			return putJSON(meta, []byte("schema"), SchemaMeta{Version: schemaVersion, MinReader: schemaVersion, CreatedAt: time.Now().UTC()})
		}
		var sm SchemaMeta
		if err := json.Unmarshal(raw, &sm); err != nil {
			return model.WrapError(model.ErrStateIncompatible, "state schema is unreadable", err)
		}
		if sm.Version != schemaVersion {
			return &model.CodedError{Code: model.ErrStateIncompatible, Message: fmt.Sprintf("state schema %d, this build reads %d", sm.Version, schemaVersion)}
		}
		return nil
	})
}

func (s *Store) update(fn func(*bolt.Tx) error) error {
	err := s.db.Update(fn)
	if errors.Is(err, bolt.ErrTimeout) {
		return &model.CodedError{Code: model.ErrStoreBusy, Message: "store lock wait exceeded", Retryable: true, Cause: err}
	}
	return err
}

func (s *Store) view(fn func(*bolt.Tx) error) error {
	err := s.db.View(fn)
	if errors.Is(err, bolt.ErrTimeout) {
		return &model.CodedError{Code: model.ErrStoreBusy, Message: "store lock wait exceeded", Retryable: true, Cause: err}
	}
	return err
}

func getJSON(b *bolt.Bucket, key []byte, v any) error {
	raw := b.Get(key)
	if raw == nil {
		return model.NewError(model.ErrTargetUnavailable, "record not found")
	}
	return json.Unmarshal(raw, v)
}

func getJSONOK(b *bolt.Bucket, key []byte, v any) (bool, error) {
	raw := b.Get(key)
	if raw == nil {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

func putJSON(b *bolt.Bucket, key []byte, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put(key, raw)
}

// compound builds a NUL-separated key. No user-controlled component may contain
// NUL (input validation rejects it).
func compound(parts ...string) []byte {
	return []byte(strings.Join(parts, "\x00"))
}

func timeIndex(prefix string, t time.Time, id string) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(t.UnixNano()))
	return []byte(prefix + "\x00" + string(buf) + "\x00" + id)
}

var errStopScan = errors.New("stop scan")

func scanPrefixN(b *bolt.Bucket, prefix []byte, limit int, fn func(k, v []byte) error) error {
	c := b.Cursor()
	n := 0
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		if err := fn(k, v); err != nil {
			if errors.Is(err, errStopScan) {
				return nil
			}
			return err
		}
		n++
		if limit > 0 && n >= limit {
			break
		}
	}
	return nil
}

func decodeJSON(raw []byte, v any) error { return json.Unmarshal(raw, v) }

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
	"path/filepath"
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

// migration advances a database from one schema version to the next. The map
// is keyed by the version being migrated FROM. Add a step and bump
// schemaVersion in the same change; a missing step fails closed before any
// write and without creating a backup.
type migration func(*bolt.Tx) error

var productionMigrations = map[int]migration{
	// To add schema 2:
	//   1. bump schemaVersion to 2;
	//   2. add 1: migrateV1ToV2 below, creating any new buckets/keys;
	//   3. add a migration test that opens a v1 database, migrates, and
	//      checks both the backup (still v1) and the migrated database.
	// A missing step fails closed before creating a backup.
}

type SchemaMeta struct {
	Version   int       `json:"version"`
	MinReader int       `json:"min_reader"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	db   *bolt.DB
	lock *os.File
	path string
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
	s := &Store{db: db, lock: lock, path: path, Now: func() time.Time { return time.Now().UTC() }}
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
	s := &Store{db: db, lock: lock, path: path, Now: func() time.Time { return time.Now().UTC() }}
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

// init creates schema v1 for an empty database and migrates an existing one.
func (s *Store) init() error {
	fresh, err := s.isFresh()
	if err != nil {
		return err
	}
	if fresh {
		return s.db.Update(initializeSchema)
	}
	return s.migrateTo(schemaVersion, productionMigrations)
}

func (s *Store) isFresh() (bool, error) {
	fresh := false
	err := s.db.View(func(tx *bolt.Tx) error {
		count := 0
		if err := tx.ForEach(func(_ []byte, _ *bolt.Bucket) error {
			count++
			return nil
		}); err != nil {
			return err
		}
		fresh = count == 0
		return nil
	})
	return fresh, err
}

func initializeSchema(tx *bolt.Tx) error {
	for _, name := range allBuckets {
		if _, err := tx.CreateBucketIfNotExists(name); err != nil {
			return err
		}
	}
	return putJSON(tx.Bucket(bucketMeta), []byte("schema"), SchemaMeta{
		Version:   schemaVersion,
		MinReader: schemaVersion,
		CreatedAt: time.Now().UTC(),
	})
}

func readSchema(tx *bolt.Tx) (SchemaMeta, error) {
	var sm SchemaMeta
	meta := tx.Bucket(bucketMeta)
	if meta == nil {
		return sm, &model.CodedError{Code: model.ErrStateIncompatible, Message: "state has no schema metadata"}
	}
	raw := meta.Get([]byte("schema"))
	if raw == nil {
		return sm, &model.CodedError{Code: model.ErrStateIncompatible, Message: "state has no schema record"}
	}
	if err := json.Unmarshal(raw, &sm); err != nil {
		return sm, model.WrapError(model.ErrStateIncompatible, "state schema is unreadable", err)
	}
	return sm, nil
}

func validateReadable(sm SchemaMeta) error {
	if sm.Version < 1 {
		return &model.CodedError{Code: model.ErrStateIncompatible, Message: fmt.Sprintf("state has invalid schema %d", sm.Version)}
	}
	if sm.Version > schemaVersion {
		return &model.CodedError{
			Code:    model.ErrStateIncompatible,
			Message: fmt.Sprintf("state schema %d is newer than this build (%d); upgrade amgo", sm.Version, schemaVersion),
		}
	}
	if sm.MinReader > schemaVersion {
		return &model.CodedError{
			Code:    model.ErrStateIncompatible,
			Message: fmt.Sprintf("state requires reader schema %d; this build reads %d", sm.MinReader, schemaVersion),
		}
	}
	return nil
}

// verifyBuckets makes a schema/implementation mismatch loud instead of letting
// later reads fail on a nil bucket.
func verifyBuckets(tx *bolt.Tx) error {
	for _, name := range allBuckets {
		if tx.Bucket(name) == nil {
			return &model.CodedError{
				Code:    model.ErrStateIncompatible,
				Message: fmt.Sprintf("state is missing bucket %q; it needs a migration", name),
			}
		}
	}
	return nil
}

// migrateTo advances the database to target. It creates a consistent
// pre-migration backup under the state directory before the first write. A
// missing migration step is refused before the backup. A migration error rolls
// back the whole transaction and leaves the backup in place.
func (s *Store) migrateTo(target int, migrations map[int]migration) error {
	var from int
	var backup string
	err := s.db.View(func(tx *bolt.Tx) error {
		sm, err := readSchema(tx)
		if err != nil {
			return err
		}
		if err := validateReadable(sm); err != nil {
			return err
		}
		from = sm.Version
		if from > target {
			return &model.CodedError{
				Code:    model.ErrStateIncompatible,
				Message: fmt.Sprintf("state schema %d is newer than migration target %d", from, target),
			}
		}
		if from == target {
			return verifyBuckets(tx)
		}
		for v := from; v < target; v++ {
			if _, ok := migrations[v]; !ok {
				return &model.CodedError{
					Code:    model.ErrStateIncompatible,
					Message: fmt.Sprintf("no migration from state schema %d to %d", v, v+1),
				}
			}
		}
		backup, err = s.backup(tx, from)
		return err
	})
	if err != nil || from == target {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		for v := from; v < target; v++ {
			if err := migrations[v](tx); err != nil {
				return model.WrapError(
					model.ErrStateIncompatible,
					fmt.Sprintf("schema migration %d->%d failed; the pre-migration backup is %s", v, v+1, backup),
					err,
				)
			}
		}
		if err := verifyBuckets(tx); err != nil {
			return err
		}
		sm, err := readSchema(tx)
		if err != nil {
			return err
		}
		sm.Version = target
		sm.MinReader = target
		return putJSON(tx.Bucket(bucketMeta), []byte("schema"), sm)
	})
}

// backup writes a consistent copy of the database before a migration. The
// name is owner-only and includes the source schema version and a UTC stamp.
func (s *Store) backup(tx *bolt.Tx, from int) (string, error) {
	stamp := s.now().Format("20060102T150405.000000000Z")
	base := fmt.Sprintf("%s.v%d.%s", s.path, from, stamp)
	target := base + ".bak"
	for i := 1; ; i++ {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			break
		} else if err != nil {
			return "", model.WrapError(model.ErrStateIncompatible, "cannot inspect migration backup path", err)
		}
		target = fmt.Sprintf("%s-%d.bak", base, i)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(target)
		}
	}()
	if err := tx.CopyFile(target, 0o600); err != nil {
		return "", model.WrapError(model.ErrStateIncompatible, "cannot back up state before migration", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		return "", model.WrapError(model.ErrStateIncompatible, "cannot secure migration backup", err)
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return "", model.WrapError(model.ErrStateIncompatible, "cannot open the migration backup directory", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return "", model.WrapError(model.ErrStateIncompatible, "cannot sync the migration backup directory", syncErr)
	}
	if closeErr != nil {
		return "", model.WrapError(model.ErrStateIncompatible, "cannot close the migration backup directory", closeErr)
	}
	complete = true
	return target, nil
}

func (s *Store) checkSchema() error {
	return s.db.View(func(tx *bolt.Tx) error {
		sm, err := readSchema(tx)
		if err != nil {
			return err
		}
		if err := validateReadable(sm); err != nil {
			return err
		}
		return verifyBuckets(tx)
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

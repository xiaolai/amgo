package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

func rawUpdate(t *testing.T, path string, fn func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(fn); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func rawReadSchema(t *testing.T, path string) SchemaMeta {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sm SchemaMeta
	if err := db.View(func(tx *bolt.Tx) error {
		var err error
		sm, err = readSchema(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return sm
}

func backupFiles(path string) []string {
	files, _ := filepath.Glob(path + ".v*.bak")
	return files
}

func TestNewDatabaseCreatesSchemaAndBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	sm := rawReadSchema(t, path)
	if sm.Version != schemaVersion || sm.MinReader != schemaVersion {
		t.Fatalf("schema=%+v", sm)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(verifyBuckets); err != nil {
		t.Fatal(err)
	}
	if got := backupFiles(path); len(got) != 0 {
		t.Fatalf("new database created a backup: %v", got)
	}
}

func TestFutureSchemaRefusedWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	rawUpdate(t, path, func(tx *bolt.Tx) error {
		var sm SchemaMeta
		if err := getJSON(tx.Bucket(bucketMeta), []byte("schema"), &sm); err != nil {
			return err
		}
		sm.Version = schemaVersion + 1
		sm.MinReader = schemaVersion + 1
		return putJSON(tx.Bucket(bucketMeta), []byte("schema"), sm)
	})

	if _, err := Open(path, 2*time.Second); model.Code(err) != model.ErrStateIncompatible {
		t.Fatalf("open err=%v", err)
	}
	if sm := rawReadSchema(t, path); sm.Version != schemaVersion+1 {
		t.Fatalf("failed open mutated the schema: %+v", sm)
	}
	if got := backupFiles(path); len(got) != 0 {
		t.Fatalf("failed open created a backup: %v", got)
	}
}

func TestMissingSchemaRefusedWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	_ = st.Close()

	rawUpdate(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Delete([]byte("schema"))
	})
	if _, err := Open(path, 2*time.Second); model.Code(err) != model.ErrStateIncompatible {
		t.Fatalf("open err=%v", err)
	}
	if got := backupFiles(path); len(got) != 0 {
		t.Fatalf("failed open created a backup: %v", got)
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketMessages).Get([]byte("m1")) == nil {
			return errors.New("message was removed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingBucketRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	rawUpdate(t, path, func(tx *bolt.Tx) error {
		return tx.DeleteBucket(bucketJobs)
	})
	if _, err := Open(path, 2*time.Second); model.Code(err) != model.ErrStateIncompatible {
		t.Fatalf("open err=%v", err)
	}
	if _, err := OpenReadOnly(path, 2*time.Second); model.Code(err) != model.ErrStateIncompatible {
		t.Fatalf("read-only open err=%v", err)
	}
	if got := backupFiles(path); len(got) != 0 {
		t.Fatalf("missing bucket created a backup: %v", got)
	}
}

func TestMigrationBacksUpAndAdvances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	_ = st.Close()

	st, err = Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = st.migrateTo(schemaVersion+1, map[int]migration{
		schemaVersion: func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte("migration_test"))
			return err
		},
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	_ = st.Close()

	backups := backupFiles(path)
	if len(backups) != 1 {
		t.Fatalf("backups=%v", backups)
	}
	fi, err := os.Stat(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode=%o", fi.Mode().Perm())
	}

	backup, err := bolt.Open(backups[0], 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.View(func(tx *bolt.Tx) error {
		sm, err := readSchema(tx)
		if err != nil {
			return err
		}
		if sm.Version != schemaVersion {
			return errors.New("backup schema changed")
		}
		if tx.Bucket([]byte("migration_test")) != nil {
			return errors.New("backup contains the migrated bucket")
		}
		if tx.Bucket(bucketMessages).Get([]byte("m1")) == nil {
			return errors.New("backup lost the message")
		}
		return nil
	}); err != nil {
		_ = backup.Close()
		t.Fatal(err)
	}
	_ = backup.Close()

	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		sm, err := readSchema(tx)
		if err != nil {
			return err
		}
		if sm.Version != schemaVersion+1 || sm.MinReader != schemaVersion+1 {
			return errors.New("migrated schema is wrong")
		}
		if tx.Bucket([]byte("migration_test")) == nil {
			return errors.New("migrated bucket is missing")
		}
		if tx.Bucket(bucketMessages).Get([]byte("m1")) == nil {
			return errors.New("message was lost")
		}
		return nil
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	if _, err := Open(path, 2*time.Second); model.Code(err) != model.ErrStateIncompatible {
		t.Fatalf("old reader accepted a newer schema: %v", err)
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = st.migrateTo(schemaVersion+1, map[int]migration{
		schemaVersion: func(tx *bolt.Tx) error {
			if _, err := tx.CreateBucketIfNotExists([]byte("migration_test")); err != nil {
				return err
			}
			return errors.New("migration failed")
		},
	})
	_ = st.Close()
	if err == nil {
		t.Fatal("migration error was swallowed")
	}
	if sm := rawReadSchema(t, path); sm.Version != schemaVersion {
		t.Fatalf("failed migration advanced the schema: %+v", sm)
	}
	backups := backupFiles(path)
	if len(backups) != 1 {
		t.Fatalf("backups=%v", backups)
	}
	backup, err := bolt.Open(backups[0], 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("migration_test")) != nil {
			return errors.New("failed migration leaked into the backup")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingMigrationStepRefusedBeforeBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = st.migrateTo(schemaVersion+2, map[int]migration{
		schemaVersion: func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte("migration_test"))
			return err
		},
	})
	_ = st.Close()
	if model.Code(err) != model.ErrStateIncompatible {
		t.Fatalf("err=%v", err)
	}
	if got := backupFiles(path); len(got) != 0 {
		t.Fatalf("a doomed migration created a backup: %v", got)
	}
	if sm := rawReadSchema(t, path); sm.Version != schemaVersion {
		t.Fatalf("schema changed: %+v", sm)
	}
}

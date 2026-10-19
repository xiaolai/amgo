package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"amgo/internal/model"
)

// OutcomeRecord is the best-effort fallback written when the database cannot
// persist an observed outcome. It is not a durability guarantee.
type OutcomeRecord struct {
	ID                 string    `json:"id"`
	AttemptID          string    `json:"attempt_id"`
	MessageID          string    `json:"message_id"`
	Outcome            string    `json:"outcome"`
	NativeSubmissionID string    `json:"native_submission_id,omitempty"`
	NativeDisposition  string    `json:"native_disposition,omitempty"`
	ErrorCode          string    `json:"error_code,omitempty"`
	ErrorDetail        string    `json:"error_detail,omitempty"`
	IdempotencyKey     string    `json:"idempotency_key,omitempty"`
	At                 time.Time `json:"at"`
}

const maxJournalBytes = 4 << 20

func AppendOutcome(path string, rec OutcomeRecord) error {
	if err := secureStateDir(path); err != nil {
		return err
	}
	lock, err := lockFile(path+".lock", true, 2*time.Second)
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	if rec.ID == "" {
		rec.ID = rec.AttemptID + ":" + rec.Outcome
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ImportOutcomes applies complete journal lines and quarantines malformed
// fragments. apply must be idempotent per record ID. A callback failure is
// returned with the applied count; the failing line and its successors remain.
func ImportOutcomes(path string, apply func(OutcomeRecord) error) (int, error) {
	if err := secureStateDir(path); err != nil {
		return 0, err
	}
	lock, err := lockFile(path+".lock", true, 2*time.Second)
	if err != nil {
		return 0, err
	}
	defer unlockFile(lock)
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if fi.Size() > maxJournalBytes {
		return 0, &model.CodedError{
			Code:    model.ErrUsage,
			Message: fmt.Sprintf("outcomes journal exceeds %d bytes; rotate it before importing", maxJournalBytes),
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	closeErr := f.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if len(raw) > maxJournalBytes {
		return 0, &model.CodedError{
			Code:    model.ErrUsage,
			Message: fmt.Sprintf("outcomes journal grew past %d bytes while reading; rotate it before importing", maxJournalBytes),
		}
	}
	lines := splitLines(raw)
	applied := 0
	var torn []string
	var remaining [][]byte
	var applyErr error
	for i, line := range lines {
		var rec OutcomeRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			torn = append(torn, string(line))
			continue
		}
		if err := apply(rec); err != nil {
			applyErr = err
			remaining = append(remaining, line)
			remaining = append(remaining, lines[i+1:]...)
			break
		}
		applied++
	}
	if len(torn) > 0 {
		tornPath := path + ".torn"
		tf, err := os.OpenFile(tornPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return applied, err
		}
		_, werr := tf.WriteString(strings.Join(torn, "\n") + "\n")
		if werr == nil {
			werr = tf.Sync()
		}
		_ = tf.Close()
		if werr != nil {
			return applied, werr
		}
	}
	if len(remaining) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return applied, err
		}
		return applied, applyErr
	}
	parts := make([]string, len(remaining))
	for i, b := range remaining {
		parts[i] = string(b)
	}
	if err := rewriteJournal(path, []byte(strings.Join(parts, "\n")+"\n")); err != nil {
		return applied, err
	}
	return applied, applyErr
}

func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
	}
	return lines
}

func rewriteJournal(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

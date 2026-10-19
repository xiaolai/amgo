package message

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/config"
	"amgo/internal/model"
	"amgo/internal/store"
)

func TestTargetIdentityIncludesEndpoint(t *testing.T) {
	a := model.Target{Adapter: "opencode", HostID: "h", Endpoint: "http://127.0.0.1:4000", NativeSessionID: "s"}
	b := a
	b.Endpoint = "http://127.0.0.1:4001"
	if sameRetryTarget(a, b) {
		t.Fatal("retry treated two endpoints as one target")
	}
	if fingerprint(a, "c", false, "", false, "body") == fingerprint(b, "c", false, "", false, "body") {
		t.Fatal("idempotency fingerprint ignored the endpoint")
	}
	if model.SameTargetIdentity(a, b) {
		t.Fatal("target identity ignored the endpoint")
	}
}

func TestExportDrainsMessagesEventsAndPropagatesErrors(t *testing.T) {
	home := t.TempDir()
	st, err := store.Open(config.DBPath(home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	path := config.DBPath(home)
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	if err := db.Update(func(tx *bolt.Tx) error {
		msgs := tx.Bucket([]byte("messages"))
		attempts := tx.Bucket([]byte("attempts"))
		byMsg := tx.Bucket([]byte("attempt_by_msg"))
		events := tx.Bucket([]byte("events"))
		eventsByMsg := tx.Bucket([]byte("event_by_msg"))
		for i := 0; i < 1001; i++ {
			id := fmt.Sprintf("m_%04d", i)
			now := base.Add(time.Duration(i) * time.Millisecond)
			msg := model.Message{
				RecordVersion: 1, ID: id, ConversationID: "c_1",
				SenderSessionID: "s_worker", SenderLabel: "worker", RecipientSessionID: "s_worker",
				Body: "x", BodySHA256: "hash", BodyBytes: 1, FramingVersion: 1,
				State: model.MessageAcknowledged, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			}
			raw, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			if err := msgs.Put([]byte(id), raw); err != nil {
				return err
			}
			attempt := model.Attempt{
				RecordVersion: 1, ID: "a_" + id, MessageID: id, ReservationID: "r_" + id,
				Target: model.Target{Adapter: "fake"}, State: model.AttemptFinalized,
				StartedAt: now, DispatchDeadline: now.Add(time.Minute), Outcome: "submitted",
			}
			raw, err = json.Marshal(attempt)
			if err != nil {
				return err
			}
			if err := attempts.Put([]byte(attempt.ID), raw); err != nil {
				return err
			}
			if err := byMsg.Put([]byte(id+"\x00"+attempt.ID), []byte(attempt.ID)); err != nil {
				return err
			}
		}
		for i := 0; i < 1001; i++ {
			seq := uint64(i + 1)
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, seq)
			ev := model.Event{Seq: seq, MessageID: "m_0000", Kind: "ack", At: base}
			raw, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if err := events.Put(key, raw); err != nil {
				return err
			}
			if err := eventsByMsg.Put([]byte("m_0000\x00"+string(key)), key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	svc := New(home, nil)
	var out bytes.Buffer
	if err := svc.ExportJSONL(&out, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), `"type":"message"`); got != 1001 {
		t.Fatalf("exported messages=%d", got)
	}
	if got := strings.Count(out.String(), `"type":"attempt"`); got != 1001 {
		t.Fatalf("exported attempts=%d", got)
	}
	if got := strings.Count(out.String(), `"type":"event"`); got != 1001 {
		t.Fatalf("exported events=%d", got)
	}

	// A corrupt child record must fail the export, not silently omit it.
	db, err = bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("attempts")).Put([]byte("a_m_0000"), []byte("{not json"))
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	if err := svc.ExportJSONL(io.Discard, ""); err == nil {
		t.Fatal("export ignored a corrupt attempt record")
	}
}

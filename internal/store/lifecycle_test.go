package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"amgo/internal/model"
)

// TestLifecycle is an opt-in storage lifecycle measurement. It creates a
// database of roughly 100 MB for several body sizes, measures open/list and a
// durable prepare, prunes 95% of the acknowledged history, and measures again.
// It is not a correctness gate; the contention and invariant tests are.
func TestLifecycle(t *testing.T) {
	if os.Getenv("AMGO_LIFECYCLE") == "" {
		t.Skip("set AMGO_LIFECYCLE=1 to run the lifecycle measurement")
	}
	now := time.Now().UTC()
	for _, body := range []int{1 << 10, 8 << 10, 64 << 10} {
		body := body
		t.Run(fmt.Sprintf("body_%d", body), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			st, err := Open(path, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			n := (100 << 20) / body
			fillLifecycle(t, st, n, body, now)
			_ = st.Close()
			freshSize := fileSize(t, path)
			openSamples, listSamples := measureReads(t, path)
			prepSamples := measurePrepare(t, path, n, now)

			st, err = Open(path, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			rep, err := st.Prune(now, now.Add(-30*24*time.Hour), false, false)
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			prunedSize := fileSize(t, path)
			compactedSize, cerr := Compact(path)
			if cerr != nil {
				t.Fatal(cerr)
			}
			openSamples2, listSamples2 := measureReads(t, path)

			t.Logf("body=%d n=%d fresh_bytes=%d pruned_bytes=%d compacted_bytes=%d pruned_messages=%d",
				body, n, freshSize, prunedSize, compactedSize, rep.Messages)
			logSamples(t, "fresh-open", openSamples)
			logSamples(t, "fresh-list", listSamples)
			logSamples(t, "fresh-prepare", prepSamples)
			logSamples(t, "pruned-open", openSamples2)
			logSamples(t, "pruned-list", listSamples2)
		})
	}
}

func fillLifecycle(t *testing.T, st *Store, n, bodyBytes int, now time.Time) {
	t.Helper()
	conv := model.Conversation{
		RecordVersion: 1, ID: "c_life", Participants: []string{"operator:1", "s_life"},
		MaxMessages: n + 100, SpentCount: n, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
	}
	body := strings.Repeat("x", bodyBytes)
	created := now.Add(-31 * 24 * time.Hour)
	err := st.db.Update(func(tx *bolt.Tx) error {
		if err := putJSON(tx.Bucket(bucketConversations), []byte(conv.ID), conv); err != nil {
			return err
		}
		msgs := tx.Bucket(bucketMessages)
		attempts := tx.Bucket(bucketAttempts)
		reservations := tx.Bucket(bucketReservations)
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("m_life_%d", i)
			state := model.MessageAcknowledged
			if i%20 == 0 {
				state = model.MessageSubmitted
			}
			m := model.Message{
				RecordVersion: 1, ID: id, ConversationID: conv.ID, SenderLabel: "operator",
				RecipientSessionID: "s_life", Body: body, BodySHA256: "h", BodyBytes: bodyBytes,
				State: state, NativeDisposition: model.DispositionQueued,
				CreatedAt: created, ExpiresAt: created.Add(time.Hour),
			}
			if err := putJSON(msgs, []byte(id), m); err != nil {
				return err
			}
			if err := tx.Bucket(bucketMsgByConv).Put(timeIndex(conv.ID+":", created, id), []byte(id)); err != nil {
				return err
			}
			if err := tx.Bucket(bucketInbox).Put(timeIndex("s_life:", created, id), []byte(id)); err != nil {
				return err
			}
			a := model.Attempt{
				RecordVersion: 1, ID: "a_" + id, MessageID: id, ReservationID: "r_" + id,
				State: model.AttemptFinalized, Outcome: "submitted",
				StartedAt: created, DispatchDeadline: created.Add(time.Minute), FinalizedAt: &created,
			}
			if err := putJSON(attempts, []byte(a.ID), a); err != nil {
				return err
			}
			if err := tx.Bucket(bucketAttemptByMsg).Put(compound(id, a.ID), []byte(a.ID)); err != nil {
				return err
			}
			r := model.Reservation{
				RecordVersion: 1, ID: "r_" + id, MessageID: id, ConversationID: conv.ID,
				State: model.ReservationConsumed, CreatedAt: created, DispatchDeadline: created.Add(time.Minute),
			}
			if err := putJSON(reservations, []byte(r.ID), r); err != nil {
				return err
			}
			if err := tx.Bucket(bucketResByMsg).Put([]byte(id), []byte(r.ID)); err != nil {
				return err
			}
			rec := model.IdemRecord{
				RecordVersion: 1, Fingerprint: "fp-" + id, MessageID: id,
				ConversationID: conv.ID, FirstSeen: created, Outcome: "submitted",
			}
			key := []byte("k-" + id)
			if err := putJSON(tx.Bucket(bucketIdem), key, rec); err != nil {
				return err
			}
			if err := tx.Bucket(bucketIdemByMsg).Put([]byte(id), key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func measureReads(t *testing.T, path string) (openSamples, listSamples []time.Duration) {
	t.Helper()
	for i := 0; i < 5; i++ {
		start := time.Now()
		st, err := Open(path, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		openSamples = append(openSamples, time.Since(start))
		start = time.Now()
		if _, err := st.ListInbox("s_life", 100); err != nil {
			t.Fatal(err)
		}
		listSamples = append(listSamples, time.Since(start))
		_ = st.Close()
	}
	return
}

func measurePrepare(t *testing.T, path string, base int, now time.Time) []time.Duration {
	t.Helper()
	var out []time.Duration
	for i := 0; i < 5; i++ {
		st, err := Open(path, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("m_measure_%d", i)
		start := time.Now()
		_, err = st.PrepareSend(PrepareRequest{
			CallerID: "operator:1", IdemKey: fmt.Sprintf("measure-%d", i), Fingerprint: "fp",
			Message: model.Message{
				RecordVersion: 1, ID: id, ConversationID: "c_life", SenderLabel: "operator",
				RecipientSessionID: "s_life", Body: "measure", BodySHA256: "h", BodyBytes: 7,
				State: model.MessageDispatching, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			},
			Attempt: model.Attempt{
				RecordVersion: 1, ID: "a_" + id, MessageID: id, ReservationID: "r_" + id,
				State: model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(time.Minute),
			},
			Reservation: model.Reservation{
				RecordVersion: 1, ID: "r_" + id, MessageID: id, ConversationID: "c_life",
				State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(time.Minute),
			},
			ConversationID: "c_life", Now: now,
		})
		elapsed := time.Since(start)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, elapsed)
	}
	return out
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func logSamples(t *testing.T, label string, samples []time.Duration) {
	t.Helper()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	t.Logf("%s p50=%s p95=%s max=%s", label, samples[len(samples)/2], samples[int(float64(len(samples)-1)*0.95)], samples[len(samples)-1])
}

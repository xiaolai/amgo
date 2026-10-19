package store

import (
	"path/filepath"
	"testing"
	"time"

	"amgo/internal/model"
)

func TestCompactPreservesRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", NativeSubmissionID: "n1", Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AckMessage("m1", model.ProvenanceRecipient, "recipient", "", now); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	if _, err := Compact(path); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	msg, err := st.GetMessage("m1")
	if err != nil || msg.State != model.MessageAcknowledged {
		t.Fatalf("message=%+v err=%v", msg, err)
	}
	if msg.Body != "hello" || msg.BodySHA256 != "hash" || msg.BodyBytes != 5 {
		t.Fatalf("body contents were not preserved: %+v", msg)
	}
	rec, found, err := st.LookupIdempotency("operator:1", "k1")
	if err != nil || !found || rec.MessageID != "m1" || rec.Outcome != "submitted" {
		t.Fatalf("idem=%+v found=%v err=%v", rec, found, err)
	}
	attempts, err := st.ListAttempts("m1")
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
	if attempts[0].NativeSubmissionID != "n1" || attempts[0].Outcome != "submitted" || attempts[0].Inferred {
		t.Fatalf("native evidence was lost: %+v", attempts[0])
	}
	receipts, err := st.ListReceipts("m1")
	if err != nil || len(receipts) != 1 || receipts[0].Provenance != model.ProvenanceRecipient {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	v, err := st.CheckInvariants(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		if x.Code == "spent_count_mismatch" || x.Code == "refunded_count_mismatch" {
			t.Fatalf("compact broke counters: %+v", x)
		}
	}
}

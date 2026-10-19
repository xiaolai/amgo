package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amgo/internal/model"
)

func TestJournalImportIsIdempotent(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "outcomes.log")
	rec := OutcomeRecord{
		AttemptID: "a_m1", MessageID: "m1", Outcome: "submitted",
		NativeSubmissionID: "n1", NativeDisposition: model.DispositionQueued, At: now,
	}
	if err := AppendOutcome(path, rec); err != nil {
		t.Fatal(err)
	}
	apply := func(r OutcomeRecord) error {
		return st.FinalizeAttempt(FinalizeRequest{
			AttemptID: r.AttemptID, Outcome: r.Outcome, NativeSubmissionID: r.NativeSubmissionID,
			NativeDisposition: r.NativeDisposition, Now: r.At,
		})
	}
	n, err := ImportOutcomes(path, apply)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal was not consumed: %v", err)
	}
	// A second import is a no-op.
	n, err = ImportOutcomes(path, apply)
	if err != nil || n != 0 {
		t.Fatalf("second import n=%d err=%v", n, err)
	}
	msg, _ := st.GetMessage("m1")
	if msg.State != model.MessageSubmitted {
		t.Fatalf("state=%s", msg.State)
	}
}

func TestJournalTornTailIsRetained(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "outcomes.log")
	valid := `{"id":"a_m1:submitted","attempt_id":"a_m1","message_id":"m1","outcome":"submitted","at":"` + now.Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(valid+"\n{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	apply := func(r OutcomeRecord) error {
		return st.FinalizeAttempt(FinalizeRequest{AttemptID: r.AttemptID, Outcome: r.Outcome, Now: r.At})
	}
	n, err := ImportOutcomes(path, apply)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	raw, err := os.ReadFile(path + ".torn")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "not json") {
		t.Fatalf("torn tail was lost: %s", raw)
	}
	msg, _ := st.GetMessage("m1")
	if msg.State != model.MessageSubmitted {
		t.Fatalf("state=%s", msg.State)
	}
}

func TestJournalApplyFailureKeepsRemainingRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.log")
	now := time.Now().UTC()
	line1 := `{"id":"a_1:submitted","attempt_id":"a_1","message_id":"m_1","outcome":"submitted","at":"` + now.Format(time.RFC3339Nano) + `"}`
	line2 := `{"id":"a_2:unknown","attempt_id":"a_2","message_id":"m_2","outcome":"unknown","at":"` + now.Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(line1+"\n"+line2+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	applyErr := model.NewError(model.ErrInternal, "apply failed")
	n, err := ImportOutcomes(path, func(OutcomeRecord) error { return applyErr })
	if n != 0 || err == nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("journal was removed after a failed apply: %v", rerr)
	}
	if !strings.Contains(string(raw), "a_1") || !strings.Contains(string(raw), "a_2") {
		t.Fatalf("remaining records were lost: %s", raw)
	}
}

func TestOversizedJournalIsRejectedAndRetained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.log")
	big := make([]byte, maxJournalBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportOutcomes(path, func(OutcomeRecord) error { return nil }); err == nil {
		t.Fatal("oversized journal was accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("oversized journal was removed: %v", err)
	}
}

func TestJournalRefinesInferredFailure(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.RecoverAttempt("a_m1", "failed_before_submit", now); err != nil {
		t.Fatal(err)
	}
	msg, _ := st.GetMessage("m1")
	if msg.State != model.MessageFailedBeforeSubmit {
		t.Fatalf("recovered state=%s", msg.State)
	}
	conv, _ := st.GetConversation("c1")
	if conv.RefundedCount != 1 {
		t.Fatalf("refund was not recorded: %+v", conv)
	}
	attempts, _ := st.ListAttempts("m1")
	if len(attempts) != 1 || !attempts[0].Inferred {
		t.Fatalf("recovery outcome was not marked inferred: %+v", attempts)
	}

	path := filepath.Join(t.TempDir(), "outcomes.log")
	if err := AppendOutcome(path, OutcomeRecord{
		AttemptID: "a_m1", MessageID: "m1", Outcome: "submitted",
		NativeSubmissionID: "n1", NativeDisposition: model.DispositionQueued, At: now,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := ImportOutcomes(path, func(r OutcomeRecord) error {
		return st.FinalizeAttempt(FinalizeRequest{
			AttemptID: r.AttemptID, Outcome: r.Outcome, NativeSubmissionID: r.NativeSubmissionID,
			NativeDisposition: r.NativeDisposition, Now: r.At,
		})
	})
	if err != nil || n != 1 {
		t.Fatalf("import n=%d err=%v", n, err)
	}
	msg, _ = st.GetMessage("m1")
	if msg.State != model.MessageSubmitted {
		t.Fatalf("refined state=%s", msg.State)
	}
	conv, _ = st.GetConversation("c1")
	if conv.RefundedCount != 0 || conv.SpentCount != 1 {
		t.Fatalf("refund accounting was not reversed: %+v", conv)
	}
	attempts, _ = st.ListAttempts("m1")
	if len(attempts) != 1 || attempts[0].Inferred || attempts[0].NativeSubmissionID != "n1" {
		t.Fatalf("refined attempt=%+v", attempts)
	}
	res, err := st.GetReservationForMessage("m1")
	if err != nil || res.State != model.ReservationConsumed {
		t.Fatalf("reservation=%+v err=%v", res, err)
	}
}

func TestJournalCannotOverwriteObservedOutcome(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", Now: now}); err != nil {
		t.Fatal(err)
	}
	err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "failed_before_submit", Now: now})
	if err == nil {
		t.Fatal("observed outcome was overwritten")
	}
}

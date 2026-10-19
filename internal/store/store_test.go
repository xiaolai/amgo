package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"amgo/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func baseConversation(t *testing.T, st *Store, id string, max int, now time.Time) model.Conversation {
	t.Helper()
	c := model.Conversation{
		RecordVersion: 1,
		ID:            id,
		Participants:  []string{"operator:1", "s_recipient"},
		MaxMessages:   max,
		ExpiresAt:     now.Add(time.Hour),
		CreatedAt:     now,
	}
	if err := st.CreateConversation(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func prepare(t *testing.T, st *Store, convID, msgID, idemKey, fp string, now time.Time) PrepareResult {
	t.Helper()
	attemptID := "a_" + msgID
	resID := "r_" + msgID
	msg := model.Message{
		RecordVersion:      1,
		ID:                 msgID,
		ConversationID:     convID,
		SenderSessionID:    "operator:1",
		SenderLabel:        "operator",
		RecipientSessionID: "s_recipient",
		Body:               "hello",
		BodySHA256:         "hash",
		BodyBytes:          5,
		State:              model.MessageDispatching,
		NativeDisposition:  model.DispositionUnknown,
		FramingVersion:     1,
		CreatedAt:          now,
		ExpiresAt:          now.Add(time.Hour),
	}
	attempt := model.Attempt{
		RecordVersion:    1,
		ID:               attemptID,
		MessageID:        msgID,
		ReservationID:    resID,
		Target:           model.Target{Adapter: "claude", HostID: "test", NativeSessionID: "native"},
		State:            model.AttemptDispatching,
		StartedAt:        now,
		DispatchDeadline: now.Add(120 * time.Second),
		OwnerPID:         1,
	}
	res := model.Reservation{
		RecordVersion:    1,
		ID:               resID,
		MessageID:        msgID,
		ConversationID:   convID,
		State:            model.ReservationReserved,
		CreatedAt:        now,
		DispatchDeadline: attempt.DispatchDeadline,
	}
	out, err := st.PrepareSend(PrepareRequest{
		CallerID:       "operator:1",
		IdemKey:        idemKey,
		Fingerprint:    fp,
		Message:        msg,
		Attempt:        attempt,
		Reservation:    res,
		ConversationID: convID,
		Now:            now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPrepareFinalizeAck(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	out := prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if out.Duplicate {
		t.Fatal("unexpected duplicate")
	}
	conv, _ := st.GetConversation("c1")
	if conv.SpentCount != 1 {
		t.Fatalf("spent=%d", conv.SpentCount)
	}
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", NativeDisposition: model.DispositionQueued, NativeSubmissionID: "n1", Now: now}); err != nil {
		t.Fatal(err)
	}
	msg, _ := st.GetMessage("m1")
	if msg.State != model.MessageSubmitted || msg.NativeDisposition != model.DispositionQueued {
		t.Fatalf("msg=%+v", msg)
	}
	attempts, _ := st.ListAttempts("m1")
	if len(attempts) != 1 || attempts[0].State != model.AttemptFinalized || attempts[0].NativeSubmissionID != "n1" {
		t.Fatalf("attempts=%+v", attempts)
	}
	if err := st.AckMessage("m1", model.ProvenanceRecipient, "recipient", "", now); err != nil {
		t.Fatal(err)
	}
	msg, _ = st.GetMessage("m1")
	if msg.State != model.MessageAcknowledged {
		t.Fatalf("state=%s", msg.State)
	}
	receipts, _ := st.ListReceipts("m1")
	if len(receipts) != 1 || receipts[0].Provenance != model.ProvenanceRecipient {
		t.Fatalf("receipts=%+v", receipts)
	}
	v, err := st.CheckInvariants(now)
	if err != nil || len(v) != 0 {
		t.Fatalf("violations=%+v err=%v", v, err)
	}
}

func TestIdempotencyDuplicateConflictAndAdmissionOrder(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", Now: now}); err != nil {
		t.Fatal(err)
	}
	dup := prepare(t, st, "c1", "m2", "k1", "fp1", now)
	if !dup.Duplicate || dup.Idem.MessageID != "m1" {
		t.Fatalf("dup=%+v", dup)
	}
	conv, _ := st.GetConversation("c1")
	if conv.SpentCount != 1 {
		t.Fatalf("duplicate consumed budget: %d", conv.SpentCount)
	}
	// A retry after the conversation is closed still returns the historical
	// result, proving the lookup precedes admission checks.
	if err := st.CloseConversation("c1", now); err != nil {
		t.Fatal(err)
	}
	dup = prepare(t, st, "c1", "m3", "k1", "fp1", now)
	if !dup.Duplicate || dup.Idem.MessageID != "m1" {
		t.Fatalf("closed-conversation replay=%+v", dup)
	}
	_, err := st.PrepareSend(PrepareRequest{
		CallerID:       "operator:1",
		IdemKey:        "k1",
		Fingerprint:    "different",
		Message:        model.Message{ID: "m4", ConversationID: "c1", SenderSessionID: "operator:1", RecipientSessionID: "s_recipient", Body: "x", CreatedAt: now, ExpiresAt: now},
		Attempt:        model.Attempt{ID: "a_m4", MessageID: "m4", ReservationID: "r_m4", State: model.AttemptDispatching, DispatchDeadline: now},
		Reservation:    model.Reservation{ID: "r_m4", MessageID: "m4", ConversationID: "c1", State: model.ReservationReserved},
		ConversationID: "c1",
		Now:            now,
	})
	if model.Code(err) != model.ErrIdemConflict {
		t.Fatalf("want idempotency conflict, got %v", err)
	}
}

func TestConcurrentFirstUseSameKey(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 10, now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, duplicates, errs := 0, 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := st.PrepareSend(PrepareRequest{
				CallerID:    "operator:1",
				IdemKey:     "same",
				Fingerprint: "same-fp",
				Message: model.Message{
					ID: fmt.Sprintf("m%d", i), ConversationID: "c1", SenderSessionID: "operator:1",
					RecipientSessionID: "s_recipient", Body: "x", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
					State: model.MessageDispatching,
				},
				Attempt:        model.Attempt{ID: fmt.Sprintf("a%d", i), MessageID: fmt.Sprintf("m%d", i), ReservationID: fmt.Sprintf("r%d", i), State: model.AttemptDispatching, DispatchDeadline: now.Add(time.Minute)},
				Reservation:    model.Reservation{ID: fmt.Sprintf("r%d", i), MessageID: fmt.Sprintf("m%d", i), ConversationID: "c1", State: model.ReservationReserved},
				ConversationID: "c1",
				Now:            now,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			if out.Duplicate {
				duplicates++
			} else {
				created++
			}
		}(i)
	}
	wg.Wait()
	if created != 1 || duplicates != 7 || errs != 0 {
		t.Fatalf("created=%d duplicates=%d errs=%d", created, duplicates, errs)
	}
	conv, _ := st.GetConversation("c1")
	if conv.SpentCount != 1 {
		t.Fatalf("spent=%d", conv.SpentCount)
	}
}

func TestRefundIsExactlyOnce(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 1, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "failed_before_submit", ErrorCode: "x", Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "failed_before_submit", ErrorCode: "x", Now: now}); err != nil {
		t.Fatal(err)
	}
	conv, _ := st.GetConversation("c1")
	if conv.RefundedCount != 1 || conv.Remaining() != 1 {
		t.Fatalf("conv=%+v remaining=%d", conv, conv.Remaining())
	}
	// The refunded budget is usable again.
	if _, err := st.PrepareSend(PrepareRequest{
		CallerID: "operator:1", IdemKey: "k2", Fingerprint: "fp2",
		Message:        model.Message{ID: "m2", ConversationID: "c1", SenderSessionID: "operator:1", RecipientSessionID: "s_recipient", Body: "y", CreatedAt: now, ExpiresAt: now.Add(time.Hour), State: model.MessageDispatching},
		Attempt:        model.Attempt{ID: "a_m2", MessageID: "m2", ReservationID: "r_m2", State: model.AttemptDispatching, DispatchDeadline: now.Add(time.Minute)},
		Reservation:    model.Reservation{ID: "r_m2", MessageID: "m2", ConversationID: "c1", State: model.ReservationReserved},
		ConversationID: "c1", Now: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAckBeforeOutcomeCommit(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := st.AckMessage("m1", model.ProvenanceRecipient, "recipient", "", now); err != nil {
		t.Fatal(err)
	}
	v, err := st.CheckInvariants(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("in-flight ack flagged: %+v", v)
	}
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", NativeSubmissionID: "n1", NativeDisposition: model.DispositionQueued, Now: now}); err != nil {
		t.Fatal(err)
	}
	msg, _ := st.GetMessage("m1")
	if msg.State != model.MessageAcknowledged {
		t.Fatalf("state=%s", msg.State)
	}
	attempts, _ := st.ListAttempts("m1")
	if attempts[0].State != model.AttemptFinalized || attempts[0].NativeSubmissionID != "n1" {
		t.Fatalf("attempt=%+v", attempts[0])
	}
}

func TestRecoverBranches(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	prepare(t, st, "c1", "m2", "k2", "fp2", now)
	if err := st.MarkDispatched("a_m2", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	// A dispatching attempt that died before the marker is known-unsent.
	if err := st.RecoverAttempt("a_m1", "failed_before_submit", now); err != nil {
		t.Fatal(err)
	}
	msg, _ := st.GetMessage("m1")
	if msg.State != model.MessageFailedBeforeSubmit {
		t.Fatalf("m1 state=%s", msg.State)
	}
	// A dispatched attempt may have written; recovery must say unknown.
	if err := st.RecoverAttempt("a_m2", "unknown", now); err != nil {
		t.Fatal(err)
	}
	msg, _ = st.GetMessage("m2")
	if msg.State != model.MessageUnknown {
		t.Fatalf("m2 state=%s", msg.State)
	}
	conv, _ := st.GetConversation("c1")
	if conv.RefundedCount != 1 || conv.SpentCount != 2 {
		t.Fatalf("conv=%+v", conv)
	}
	batch, err := st.ScanAttempts("", 10)
	if err != nil || len(batch.Attempts) != 2 {
		t.Fatalf("batch=%+v err=%v", batch, err)
	}
}

func TestInvariantsDetectStaleDispatching(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	old := now.Add(-200 * time.Second)
	baseConversation(t, st, "c1", 4, old)
	prepare(t, st, "c1", "m1", "k1", "fp1", old)
	v, err := st.CheckInvariants(now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range v {
		if x.Code == "stale_dispatching" {
			found = true
		}
	}
	if !found {
		t.Fatalf("violations=%+v", v)
	}
}

func TestPruneRetainsIdempotencyMembership(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AckMessage("m1", model.ProvenanceRecipient, "r", "", now); err != nil {
		t.Fatal(err)
	}
	rep, err := st.Prune(now.Add(31*24*time.Hour), now.Add(time.Second), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 1 || rep.BodyBytes != 5 {
		t.Fatalf("report=%+v", rep)
	}
	if _, err := st.GetMessage("m1"); err == nil {
		t.Fatal("message was not pruned")
	}
	rec, found, err := st.LookupIdempotency("operator:1", "k1")
	if err != nil || !found || !rec.MessagePruned {
		t.Fatalf("idem=%+v found=%v err=%v", rec, found, err)
	}
	conv, _ := st.GetConversation("c1")
	if conv.SpentCount != 1 {
		t.Fatalf("prune changed spent count: %+v", conv)
	}
	v, err := st.CheckInvariants(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		if x.Code == "spent_count_mismatch" || x.Code == "refunded_count_mismatch" {
			t.Fatalf("counter invariant broke after prune: %+v", x)
		}
	}
}

func TestSessionAliasUniquenessAndLeave(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	s1 := model.Session{RecordVersion: 1, ID: "s1", Alias: "arch", RegisteredAt: now, Target: model.Target{Adapter: "claude", NativeSessionID: "n1"}}
	s2 := model.Session{RecordVersion: 1, ID: "s2", Alias: "arch", RegisteredAt: now, Target: model.Target{Adapter: "claude", NativeSessionID: "n2"}}
	if err := st.PutSession(s1); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSession(s2); model.Code(err) != model.ErrAmbiguousTarget {
		t.Fatalf("want alias conflict, got %v", err)
	}
	if err := st.UnregisterSession("s1", now); err != nil {
		t.Fatal(err)
	}
	s2.ID = "s3"
	s2.Alias = "arch"
	if err := st.PutSession(s2); err != nil {
		t.Fatalf("rejoin after leave: %v", err)
	}
}

func TestPruneRetainsReplyParent(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AckMessage("m1", model.ProvenanceRecipient, "r", "", now); err != nil {
		t.Fatal(err)
	}
	out, err := st.PrepareSend(PrepareRequest{
		CallerID:    "operator:1",
		IdemKey:     "k2",
		Fingerprint: "fp2",
		Message: model.Message{
			RecordVersion: 1, ID: "m2", ConversationID: "c1",
			SenderSessionID: "operator:1", SenderLabel: "operator",
			RecipientSessionID: "s_recipient", ReplyTo: "m1",
			Body: "reply", BodySHA256: "hash", BodyBytes: 5,
			State: model.MessageDispatching, NativeDisposition: model.DispositionUnknown,
			FramingVersion: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		Attempt: model.Attempt{
			RecordVersion: 1, ID: "a_m2", MessageID: "m2", ReservationID: "r_m2",
			Target: model.Target{Adapter: "claude", HostID: "test", NativeSessionID: "native"},
			State:  model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(120 * time.Second),
			OwnerPID: 1,
		},
		Reservation: model.Reservation{
			RecordVersion: 1, ID: "r_m2", MessageID: "m2", ConversationID: "c1",
			State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(120 * time.Second),
		},
		ConversationID: "c1", Now: now,
	})
	if err != nil || out.Duplicate {
		t.Fatalf("prepare m2: out=%+v err=%v", out, err)
	}
	rep, err := st.Prune(now.Add(31*24*time.Hour), now.Add(time.Second), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 0 {
		t.Fatalf("pruned a reply parent: %+v", rep)
	}
	if _, err := st.GetMessage("m1"); err != nil {
		t.Fatalf("reply parent was removed: %v", err)
	}
}

func TestOpenRejectsSymlinkedDatabaseBeforeInit(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.db")
	st, err := Open(real, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, 2*time.Second); err == nil {
		t.Fatal("symlinked database was opened")
	}
}

func TestOpenReadOnlyDoesNotCreateLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	st, err := Open(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = ro.Close()
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("read-only open created a lock file: %v", err)
	}
}

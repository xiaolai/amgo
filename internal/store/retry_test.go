package store

import (
	"testing"
	"time"

	"amgo/internal/model"
)

func retryRequest(now time.Time, id, originalID, convID, bodyHash string) PrepareRequest {
	return PrepareRequest{
		CallerID:    "operator:1",
		IdemKey:     "k-" + id,
		Fingerprint: "fp-" + id,
		Message: model.Message{
			RecordVersion: 1, ID: id, ConversationID: convID,
			SenderSessionID: "operator:1", SenderLabel: "operator",
			RecipientSessionID: "s_recipient", RetryOf: originalID,
			Body: "hello", BodySHA256: bodyHash, BodyBytes: 5,
			State: model.MessageDispatching, NativeDisposition: model.DispositionUnknown,
			FramingVersion: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		Attempt: model.Attempt{
			RecordVersion: 1, ID: "a_" + id, MessageID: id, ReservationID: "r_" + id,
			Target: model.Target{Adapter: "claude", HostID: "test", NativeSessionID: "native"},
			State:  model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(120 * time.Second),
			OwnerPID: 1,
		},
		Reservation: model.Reservation{
			RecordVersion: 1, ID: "r_" + id, MessageID: id, ConversationID: convID,
			State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(120 * time.Second),
		},
		ConversationID: convID, Now: now,
	}
}

func markOriginalUnknown(t *testing.T, st *Store, now time.Time) {
	t.Helper()
	if err := st.MarkDispatched("a_m1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeAttempt(FinalizeRequest{
		AttemptID: "a_m1", Outcome: "unknown", NativeDisposition: model.DispositionUnknown, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareSendRetryRecordsAuthorization(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	markOriginalUnknown(t, st, now)

	if _, err := st.PrepareSend(retryRequest(now, "m2", "m1", "c1", "hash")); err != nil {
		t.Fatal(err)
	}
	orig, err := st.GetMessage("m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(orig.RetriedBy) != 1 || orig.RetriedBy[0] != "m2" {
		t.Fatalf("retried_by=%v", orig.RetriedBy)
	}
	events, err := st.ListEvents("m1", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "retry_authorized" && ev.Payload["retry_message_id"] == "m2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events=%+v", events)
	}
	conv, _ := st.GetConversation("c1")
	if conv.SpentCount != 2 {
		t.Fatalf("spent=%d", conv.SpentCount)
	}
}

func TestPrepareSendRetryRejectsKnownOutcome(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	if err := st.FinalizeAttempt(FinalizeRequest{AttemptID: "a_m1", Outcome: "submitted", Now: now}); err != nil {
		t.Fatal(err)
	}
	_, err := st.PrepareSend(retryRequest(now, "m2", "m1", "c1", "hash"))
	if model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("err=%v", err)
	}
	if _, err := st.GetMessage("m2"); err == nil {
		t.Fatal("rejected retry created a message")
	}
}

func TestPrepareSendRetryRejectsDifferentBody(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	markOriginalUnknown(t, st, now)
	_, err := st.PrepareSend(retryRequest(now, "m2", "m1", "c1", "different"))
	if model.Code(err) != model.ErrUsage {
		t.Fatalf("err=%v", err)
	}
}

func TestPrepareSendRetryRejectsDifferentConversation(t *testing.T) {
	st := testStore(t)
	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	markOriginalUnknown(t, st, now)
	_, err := st.PrepareSend(retryRequest(now, "m2", "m1", "c2", "hash"))
	if model.Code(err) != model.ErrUsage {
		t.Fatalf("err=%v", err)
	}
}

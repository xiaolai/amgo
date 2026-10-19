package message

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/contract"
	"amgo/internal/model"
	"amgo/internal/store"
)

type fakeAdapter struct {
	name   string
	mu     sync.Mutex
	result adapter.SendResult
	err    error
	calls  int
	reqs   []adapter.SendRequest
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) Probe(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Adapter: f.name, LiveMessaging: true}, nil
}
func (f *fakeAdapter) Discover(context.Context, adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}
func (f *fakeAdapter) Send(_ context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.reqs = append(f.reqs, req)
	return f.result, f.err
}
func (f *fakeAdapter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func setup(t *testing.T, adapterName string, accept bool) (*Service, *fakeAdapter, model.Session) {
	t.Helper()
	home := t.TempDir()
	fa := &fakeAdapter{
		name: adapterName,
		result: adapter.SendResult{
			Outcome:            "submitted",
			NativeDisposition:  model.DispositionQueued,
			NativeSubmissionID: "native-1",
		},
	}
	svc := New(home, map[string]adapter.Adapter{adapterName: fa})
	st, err := store.Open(config.DBPath(home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sess := model.Session{
		RecordVersion:         1,
		ID:                    "s_worker",
		Alias:                 "worker",
		Target:                model.Target{Adapter: adapterName, HostID: "test", NativeSessionID: "n1"},
		Capabilities:          model.Capabilities{Adapter: adapterName, LiveMessaging: true},
		RegisteredAt:          time.Now().UTC(),
		AcceptUnknownDelivery: accept,
	}
	if err := st.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	return svc, fa, sess
}

func TestSendSubmittedAndIdempotentReplay(t *testing.T) {
	svc, fa, _ := setup(t, "fake", false)
	ctx := context.Background()
	in := SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k1"}
	out, err := svc.Send(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Message.State != model.MessageSubmitted || out.NativeSubmissionID != "native-1" || out.NativeDisposition != model.DispositionQueued {
		t.Fatalf("out=%+v", out)
	}
	if fa.callCount() != 1 {
		t.Fatalf("calls=%d", fa.callCount())
	}
	dup, err := svc.Send(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Duplicate || dup.Message.ID != out.Message.ID {
		t.Fatalf("duplicate=%+v", dup)
	}
	if fa.callCount() != 1 {
		t.Fatalf("duplicate triggered a send: calls=%d", fa.callCount())
	}
	in.Body = "different"
	_, err = svc.Send(ctx, in)
	if model.Code(err) != model.ErrIdemConflict {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestUnknownOutcomeExit7(t *testing.T) {
	svc, fa, _ := setup(t, "fake", false)
	fa.result = adapter.SendResult{Outcome: "unknown", NativeDisposition: model.DispositionUnknown, Evidence: "response lost"}
	out, err := svc.Send(context.Background(), SendInput{RecipientAlias: "worker", Body: "hello"})
	if contract.ExitCode(err) != 7 || model.Code(err) != model.ErrUnknownSubmission {
		t.Fatalf("err=%v exit=%d", err, contract.ExitCode(err))
	}
	if out.Message.State != model.MessageUnknown {
		t.Fatalf("out=%+v", out)
	}
}

func TestFailedBeforeSubmitRefunds(t *testing.T) {
	svc, fa, _ := setup(t, "fake", false)
	fa.result = adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable, ErrorDetail: "dial failed"}
	out, err := svc.Send(context.Background(), SendInput{RecipientAlias: "worker", Body: "hello"})
	if contract.ExitCode(err) != 5 {
		t.Fatalf("err=%v exit=%d", err, contract.ExitCode(err))
	}
	if out.Message.State != model.MessageFailedBeforeSubmit {
		t.Fatalf("out=%+v", out)
	}
	conv, err := svc.ShowConversationForTest(out.Message.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.RefundedCount != 1 || conv.SpentCount != 1 {
		t.Fatalf("conv=%+v", conv)
	}
}

func TestClaudePolicyGate(t *testing.T) {
	svc, fa, sess := setup(t, model.AdapterClaude, false)
	ctx := context.Background()
	_, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", Body: "hello"})
	if model.Code(err) != model.ErrPolicyRefused {
		t.Fatalf("want policy refusal, got %v", err)
	}
	if fa.callCount() != 0 {
		t.Fatalf("refused send reached the adapter")
	}
	out, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", Body: "hello", AcceptUnknownDelivery: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Message.State != model.MessageSubmitted || fa.callCount() != 1 {
		t.Fatalf("out=%+v calls=%d", out, fa.callCount())
	}
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetSession(sess.ID)
	_ = st.Close()
	if !got.AcceptUnknownDelivery {
		t.Fatal("acknowledgement was not recorded on the session")
	}
}

func TestReplyToOperatorIsLocal(t *testing.T) {
	svc, fa, _ := setup(t, "fake", false)
	ctx := context.Background()
	orig, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", Body: "question", ExpectReply: true})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := svc.Reply(ctx, ReplyInput{MessageID: orig.Message.ID, SenderAlias: "worker", Body: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.NativeDisposition != model.DispositionLocal {
		t.Fatalf("reply=%+v", reply)
	}
	if fa.callCount() != 1 {
		t.Fatalf("local reply reached a native adapter: calls=%d", fa.callCount())
	}
	origMsg, err := svc.Read(orig.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if origMsg.Message.State != model.MessageAcknowledged {
		t.Fatalf("original state=%s", origMsg.Message.State)
	}
	foundReply := false
	for _, r := range origMsg.Receipts {
		if r.Kind == "reply" && r.ReplyMessage == reply.Message.ID {
			foundReply = true
		}
	}
	if !foundReply {
		t.Fatalf("receipts=%+v", origMsg.Receipts)
	}
	inbox, err := svc.Inbox("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].ID != reply.Message.ID {
		t.Fatalf("inbox=%+v", inbox)
	}
}

func TestAckWaitAndTimeout(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	ctx := context.Background()
	out, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Ack(ctx, AckInput{MessageID: out.Message.ID, AsAlias: "worker"}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Wait(ctx, WaitInput{MessageID: out.Message.ID, Until: "acknowledged", Timeout: time.Second})
	if err != nil || res.Message.State != model.MessageAcknowledged {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	out2, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", Body: "second"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Wait(ctx, WaitInput{MessageID: out2.Message.ID, Until: "acknowledged", Timeout: 100 * time.Millisecond})
	if contract.ExitCode(err) != 9 {
		t.Fatalf("want deadline exit 9, got %v", err)
	}
}

func TestBudgetAndClose(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	ctx := context.Background()
	if _, err := svc.CreateConversation("review", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", ConversationName: "review", Body: "one"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", ConversationName: "review", Body: "two"})
	if model.Code(err) != model.ErrBudgetExhausted {
		t.Fatalf("want budget exhausted, got %v", err)
	}
	if _, err := svc.CloseConversation("review"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Send(ctx, SendInput{RecipientAlias: "worker", ConversationName: "review", Body: "three"})
	if model.Code(err) != model.ErrConversationClosed {
		t.Fatalf("want closed, got %v", err)
	}
}

func TestRecoverAfterDeadOwner(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	now := time.Now().UTC().Add(-10 * time.Minute)
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateConversation(model.Conversation{
		RecordVersion: 1, ID: "c_dead", Participants: []string{"operator:1", "s_worker"},
		MaxMessages: 4, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = st.PrepareSend(store.PrepareRequest{
		CallerID: "operator:1", IdemKey: "dead", Fingerprint: "fp",
		Message: model.Message{
			RecordVersion: 1, ID: "m_dead", ConversationID: "c_dead", SenderSessionID: "operator:1",
			RecipientSessionID: "s_worker", Body: "x", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			State: model.MessageDispatching,
		},
		Attempt: model.Attempt{
			RecordVersion: 1, ID: "a_dead", MessageID: "m_dead", ReservationID: "r_dead",
			State: model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(time.Minute),
			OwnerPID: 999999, OwnerProcStart: "never",
		},
		Reservation: model.Reservation{
			RecordVersion: 1, ID: "r_dead", MessageID: "m_dead", ConversationID: "c_dead",
			State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(time.Minute),
		},
		ConversationID: "c_dead", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	n, err := svc.RecoverAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reconciled=%d", n)
	}
	rec, err := svc.Read("m_dead")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Message.State != model.MessageFailedBeforeSubmit {
		t.Fatalf("state=%s", rec.Message.State)
	}
}

func (s *Service) ShowConversationForTest(id string) (model.Conversation, error) {
	st, err := s.open()
	if err != nil {
		return model.Conversation{}, err
	}
	defer st.Close()
	return st.GetConversation(id)
}

func TestOperatorReplyDoesNotUseRecipientProvenance(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	ctx := context.Background()
	orig, err := svc.Send(ctx, SendInput{RecipientAlias: "worker", Body: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reply(ctx, ReplyInput{MessageID: orig.Message.ID, Body: "operator reply"}); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.Read(orig.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rec.Receipts {
		if r.Kind == "reply" {
			found = true
			if r.Provenance != model.ProvenanceOperator {
				t.Fatalf("operator reply provenance=%s, want operator", r.Provenance)
			}
		}
	}
	if !found {
		t.Fatal("no reply receipt")
	}
}

func TestIdempotentReplayOfFailedOutcomeReturnsError(t *testing.T) {
	svc, fa, _ := setup(t, "fake", false)
	fa.result = adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable, ErrorDetail: "dial failed"}
	in := SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k-fail"}
	first, err := svc.Send(context.Background(), in)
	if err == nil {
		t.Fatal("first send reported success")
	}
	if first.Reservation.State != model.ReservationRefunded {
		t.Fatalf("reservation=%+v", first.Reservation)
	}
	second, err := svc.Send(context.Background(), in)
	if model.Code(err) != model.ErrTargetUnavailable {
		t.Fatalf("replay err=%v want target_unavailable", err)
	}
	if !second.Duplicate || second.Message.State != model.MessageFailedBeforeSubmit {
		t.Fatalf("second=%+v", second)
	}
	if fa.callCount() != 1 {
		t.Fatalf("replay reached the adapter: calls=%d", fa.callCount())
	}
}

func TestIdempotentReplayOfUnknownOutcomeReturnsExit7(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	in := SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k-unknown"}
	fa := svc.Adapters["fake"].(*fakeAdapter)
	fa.result = adapter.SendResult{Outcome: "unknown", NativeDisposition: model.DispositionUnknown, Evidence: "lost"}
	if _, err := svc.Send(context.Background(), in); model.Code(err) != model.ErrUnknownSubmission {
		t.Fatalf("first err=%v", err)
	}
	second, err := svc.Send(context.Background(), in)
	if contract.ExitCode(err) != 7 || model.Code(err) != model.ErrUnknownSubmission {
		t.Fatalf("replay err=%v exit=%d", err, contract.ExitCode(err))
	}
	if !second.Duplicate {
		t.Fatalf("second=%+v", second)
	}
}

func TestSendOutputIncludesConsumedReservation(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	out, err := svc.Send(context.Background(), SendInput{RecipientAlias: "worker", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reservation.ID == "" || out.Reservation.State != model.ReservationConsumed {
		t.Fatalf("reservation=%+v", out.Reservation)
	}
	if out.Attempt.State != model.AttemptFinalized || out.Attempt.NativeSubmissionID != "native-1" {
		t.Fatalf("attempt=%+v", out.Attempt)
	}
	if out.Conversation.ID == "" || out.Conversation.SpentCount != 1 {
		t.Fatalf("conversation=%+v", out.Conversation)
	}
}

func TestUnpersistedOutcomeSelectsPhaseError(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	out, err := svc.unpersisted("r1", "a1", "m1", model.Conversation{ID: "c1"}, "k1", "submitted", adapter.SendResult{NativeSubmissionID: "n1"}, errStub{})
	if model.Code(err) != model.ErrOutcomeUnpersisted {
		t.Fatalf("err=%v", err)
	}
	if out.Reservation.ID != "r1" || out.IdempotencyKey != "k1" {
		t.Fatalf("out=%+v", out)
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) || ce.Details["payload_written"] != true {
		t.Fatalf("details=%+v", ce.Details)
	}

	out, err = svc.unpersisted("r1", "a1", "m1", model.Conversation{ID: "c1"}, "k1", "failed_before_submit", adapter.SendResult{}, errStub{})
	if model.Code(err) != model.ErrRefundPending {
		t.Fatalf("err=%v", err)
	}
	if !errors.As(err, &ce) || ce.Details["payload_written"] != false || out.Reservation.ID != "r1" {
		t.Fatalf("details=%+v out=%+v", ce.Details, out)
	}
}

type errStub struct{}

func (errStub) Error() string { return "store unavailable" }

func TestRefreshFailureKeepsLocalEvidence(t *testing.T) {
	svc := New(filepath.Join(t.TempDir(), "missing"), nil)
	in := SendOutput{
		Message:        model.Message{ID: "m1", ConversationID: "c1"},
		Submission:     "submitted",
		IdempotencyKey: "k1",
		Evidence:       "socket accepted the frame",
	}
	out := svc.refreshOutput(in)
	if out.Submission != "submitted" || out.Evidence != "socket accepted the frame" || out.Message.ID != "m1" {
		t.Fatalf("output=%+v", out)
	}
}

func TestReplayAfterAckStillReportsUnknown(t *testing.T) {
	svc, fa, _ := setup(t, "fake", false)
	fa.result = adapter.SendResult{Outcome: "unknown", NativeDisposition: model.DispositionUnknown, Evidence: "response lost"}
	in := SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k-ack"}
	out, err := svc.Send(context.Background(), in)
	if model.Code(err) != model.ErrUnknownSubmission {
		t.Fatalf("first err=%v", err)
	}
	if _, _, err := svc.Ack(context.Background(), AckInput{MessageID: out.Message.ID, AsAlias: "worker"}); err != nil {
		t.Fatal(err)
	}
	dup, err := svc.Send(context.Background(), in)
	if contract.ExitCode(err) != 7 {
		t.Fatalf("replay after ack err=%v exit=%d", err, contract.ExitCode(err))
	}
	if !dup.Duplicate || dup.Attempt.ID == "" || dup.Reservation.ID == "" {
		t.Fatalf("replay lost reconciliation IDs: %+v", dup)
	}
}

func TestSuccessfulReplayIncludesLedger(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	in := SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k-ledger"}
	out, err := svc.Send(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	dup, err := svc.Send(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if dup.Reservation.ID != out.Reservation.ID || dup.Reservation.State != model.ReservationConsumed {
		t.Fatalf("reservation=%+v want %s", dup.Reservation, out.Reservation.ID)
	}
	if dup.Attempt.ID != out.Attempt.ID || dup.Conversation.ID != out.Conversation.ID {
		t.Fatalf("replay=%+v", dup)
	}
}

func TestReplayAfterUnregisterChecksBody(t *testing.T) {
	svc, _, sess := setup(t, "fake", false)
	in := SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k-unreg"}
	out, err := svc.Send(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UnregisterSession(sess.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	dup, err := svc.Send(context.Background(), in)
	if err != nil {
		t.Fatalf("same request replay failed: %v", err)
	}
	if !dup.Duplicate || dup.Message.ID != out.Message.ID {
		t.Fatalf("replay=%+v", dup)
	}
	in.Body = "different"
	if _, err := svc.Send(context.Background(), in); model.Code(err) != model.ErrIdemConflict {
		t.Fatalf("different body with the same key was accepted: %v", err)
	}
}

type poisonAdapter struct {
	db     string
	result adapter.SendResult
}

func (p *poisonAdapter) Name() string { return "fake" }
func (p *poisonAdapter) Probe(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Adapter: "fake", LiveMessaging: true}, nil
}
func (p *poisonAdapter) Discover(context.Context, adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}
func (p *poisonAdapter) Send(context.Context, adapter.SendRequest) (adapter.SendResult, error) {
	_ = os.Remove(p.db)
	return p.result, nil
}

func TestPostSubmissionPersistenceFailureIsLoud(t *testing.T) {
	cases := []struct {
		name      string
		result    adapter.SendResult
		wantCode  string
		wantWrote string
	}{
		{"submitted", adapter.SendResult{Outcome: "submitted", NativeDisposition: model.DispositionQueued, NativeSubmissionID: "n1"}, model.ErrOutcomeUnpersisted, `"outcome":"submitted"`},
		{"failed", adapter.SendResult{Outcome: "failed_before_submit", ErrorCode: model.ErrTargetUnavailable}, model.ErrRefundPending, `"outcome":"failed_before_submit"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			db := config.DBPath(home)
			pa := &poisonAdapter{db: db, result: tc.result}
			svc := New(home, map[string]adapter.Adapter{"fake": pa})
			st, err := store.Open(db, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutSession(model.Session{
				RecordVersion: 1, ID: "s_worker", Alias: "worker",
				Target:       model.Target{Adapter: "fake", HostID: "test", NativeSessionID: "n1"},
				Capabilities: model.Capabilities{Adapter: "fake", LiveMessaging: true},
				RegisteredAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			out, err := svc.Send(context.Background(), SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k-poison"})
			if model.Code(err) != tc.wantCode {
				t.Fatalf("err=%v want %s", err, tc.wantCode)
			}
			if out.Message.ID == "" || out.Message.ConversationID == "" || out.Conversation.ID == "" ||
				out.Attempt.ID == "" || out.Reservation.ID == "" || out.IdempotencyKey != "k-poison" {
				t.Fatalf("out=%+v", out)
			}
			raw, rerr := os.ReadFile(config.OutcomesLog(home))
			if rerr != nil {
				t.Fatalf("journal was not written: %v", rerr)
			}
			if !strings.Contains(string(raw), tc.wantWrote) || !strings.Contains(string(raw), "k-poison") {
				t.Fatalf("journal=%s", raw)
			}
		})
	}
}

func TestGeneratedKeyReturnedOnEarlyError(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	out, err := svc.Send(context.Background(), SendInput{RecipientAlias: "missing", Body: "x"})
	if model.Code(err) != model.ErrTargetUnavailable {
		t.Fatalf("err=%v", err)
	}
	if out.IdempotencyKey == "" {
		t.Fatal("generated idempotency key was lost")
	}
}

func TestInFlightAttemptReplayIsNeverSuccess(t *testing.T) {
	svc, _, _ := setup(t, "fake", false)
	st, err := svc.open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.CreateConversation(model.Conversation{
		RecordVersion: 1, ID: "c_inflight", Participants: []string{"operator:1", "s_worker"},
		MaxMessages: 4, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = st.PrepareSend(store.PrepareRequest{
		CallerID: "operator:1", IdemKey: "k-inflight", Fingerprint: "fp",
		Message: model.Message{
			RecordVersion: 1, ID: "m_inflight", ConversationID: "c_inflight",
			SenderSessionID: "operator:1", SenderLabel: "operator",
			RecipientSessionID: "s_worker", Body: "x", BodySHA256: bodyHash("x"), BodyBytes: 1,
			State: model.MessageDispatching, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		Attempt: model.Attempt{
			RecordVersion: 1, ID: "a_inflight", MessageID: "m_inflight", ReservationID: "r_inflight",
			State: model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(time.Minute),
			OwnerPID: os.Getpid(),
		},
		Reservation: model.Reservation{
			RecordVersion: 1, ID: "r_inflight", MessageID: "m_inflight", ConversationID: "c_inflight",
			State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(time.Minute),
		},
		ConversationID: "c_inflight", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AckMessage("m_inflight", model.ProvenanceRecipient, "worker", "", now); err != nil {
		t.Fatal(err)
	}
	rec, found, err := st.LookupIdempotency("operator:1", "k-inflight")
	if err != nil || !found {
		t.Fatalf("idem found=%v err=%v", found, err)
	}
	out := SendOutput{IdempotencyKey: "k-inflight"}
	err = svc.replayError(st, rec, &out)
	if model.Code(err) != model.ErrUnknownSubmission {
		t.Fatalf("err=%v", err)
	}
	if out.Attempt.ID != "a_inflight" || out.Reservation.ID != "r_inflight" {
		t.Fatalf("out=%+v", out)
	}
}

func TestReplayChecksConversationName(t *testing.T) {
	svc, _, sess := setup(t, "fake", false)
	if _, err := svc.CreateConversation("review", 4, time.Hour); err != nil {
		t.Fatal(err)
	}
	in := SendInput{RecipientAlias: "worker", ConversationName: "review", Body: "hello", IdempotencyKey: "k-conv"}
	if _, err := svc.Send(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(config.DBPath(svc.Home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UnregisterSession(sess.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if _, err := svc.Send(context.Background(), in); err != nil {
		t.Fatalf("same conversation replay failed: %v", err)
	}
	in.ConversationName = "other"
	if _, err := svc.Send(context.Background(), in); model.Code(err) != model.ErrIdemConflict {
		t.Fatalf("different conversation was accepted: %v", err)
	}
}

type breakingAdapter struct {
	db     string
	result adapter.SendResult
}

func (b *breakingAdapter) Name() string { return "fake" }
func (b *breakingAdapter) Probe(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Adapter: "fake", LiveMessaging: true}, nil
}
func (b *breakingAdapter) Discover(context.Context, adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}
func (b *breakingAdapter) Send(context.Context, adapter.SendRequest) (adapter.SendResult, error) {
	_ = os.Remove(b.db)
	_ = os.Mkdir(b.db, 0o700)
	return b.result, nil
}

func TestReopenFailureKeepsPreparedIDs(t *testing.T) {
	home := t.TempDir()
	db := config.DBPath(home)
	ba := &breakingAdapter{db: db, result: adapter.SendResult{Outcome: "submitted", NativeDisposition: model.DispositionQueued}}
	svc := New(home, map[string]adapter.Adapter{"fake": ba})
	st, err := store.Open(db, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSession(model.Session{
		RecordVersion: 1, ID: "s_worker", Alias: "worker",
		Target:       model.Target{Adapter: "fake", HostID: "test", NativeSessionID: "n1"},
		Capabilities: model.Capabilities{Adapter: "fake", LiveMessaging: true}, RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	out, err := svc.Send(context.Background(), SendInput{RecipientAlias: "worker", Body: "hello", IdempotencyKey: "k-reopen"})
	if err == nil {
		t.Fatal("reopen failure was not reported")
	}
	if out.Message.ID == "" || out.Attempt.ID == "" || out.Reservation.ID == "" || out.IdempotencyKey != "k-reopen" {
		t.Fatalf("prepared IDs were lost: %+v", out)
	}
}

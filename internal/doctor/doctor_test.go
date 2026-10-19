package doctor

import (
	"context"
	"testing"
	"time"

	"amgo/internal/adapter"
	"amgo/internal/config"
	"amgo/internal/message"
	"amgo/internal/model"
	"amgo/internal/store"
)

type fakeAdapter struct {
	ch chan string
}

func (f *fakeAdapter) Name() string { return "fake" }
func (f *fakeAdapter) Probe(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Adapter: "fake", LiveMessaging: true}, nil
}
func (f *fakeAdapter) Discover(context.Context, adapter.DiscoverRequest) ([]model.Candidate, error) {
	return nil, nil
}
func (f *fakeAdapter) Send(_ context.Context, req adapter.SendRequest) (adapter.SendResult, error) {
	f.ch <- req.MessageID
	return adapter.SendResult{Outcome: "submitted", NativeDisposition: model.DispositionQueued}, nil
}

func setupProbe(t *testing.T) (*message.Service, *fakeAdapter) {
	t.Helper()
	home := t.TempDir()
	fa := &fakeAdapter{ch: make(chan string, 10)}
	svc := message.New(home, map[string]adapter.Adapter{"fake": fa})
	st, err := store.Open(config.DBPath(home), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sess := model.Session{
		RecordVersion: 1, ID: "s_worker", Alias: "worker",
		Target:       model.Target{Adapter: "fake", HostID: "test", NativeSessionID: "n1"},
		Capabilities: model.Capabilities{Adapter: "fake", LiveMessaging: true},
		RegisteredAt: time.Now().UTC(),
	}
	if err := st.PutSession(sess); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	return svc, fa
}

func nextProbeID(t *testing.T, fa *fakeAdapter, done <-chan error) string {
	t.Helper()
	select {
	case id := <-fa.ch:
		return id
	case err := <-done:
		t.Fatalf("probe ended before reaching the adapter: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not send within 2s")
	}
	return ""
}

func TestOperatorAckCannotSatisfyProbe(t *testing.T) {
	svc, fa := setupProbe(t)
	done := make(chan error, 1)
	go func() {
		_, err := ProbeSession(context.Background(), svc, "worker", 400*time.Millisecond)
		done <- err
	}()
	id := nextProbeID(t, fa, done)
	if _, _, err := svc.Ack(context.Background(), message.AckInput{MessageID: id}); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if model.Code(err) != model.ErrDeadlineExceeded {
		t.Fatalf("operator ack satisfied the probe: %v", err)
	}
}

func TestRecipientAckSatisfiesProbe(t *testing.T) {
	svc, fa := setupProbe(t)
	done := make(chan error, 1)
	go func() {
		_, err := ProbeSession(context.Background(), svc, "worker", 2*time.Second)
		done <- err
	}()
	id := nextProbeID(t, fa, done)
	if _, _, err := svc.Ack(context.Background(), message.AckInput{MessageID: id, AsAlias: "worker"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("recipient ack did not satisfy the probe: %v", err)
	}
}

func TestOperatorReplyCannotSatisfyProbe(t *testing.T) {
	svc, fa := setupProbe(t)
	done := make(chan error, 1)
	go func() {
		_, err := ProbeSession(context.Background(), svc, "worker", 400*time.Millisecond)
		done <- err
	}()
	id := nextProbeID(t, fa, done)
	if _, err := svc.Reply(context.Background(), message.ReplyInput{MessageID: id, Body: "operator reply"}); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if model.Code(err) != model.ErrDeadlineExceeded {
		t.Fatalf("operator reply satisfied the probe: %v", err)
	}
}

package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"amgo/internal/model"
)

func BenchmarkPrepareSend(b *testing.B) {
	st, err := Open(filepath.Join(b.TempDir(), "state.db"), 2*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.CreateConversation(model.Conversation{
		RecordVersion: 1, ID: "c_bench", Participants: []string{"operator:1", "s_recipient"},
		MaxMessages: 1_000_000, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
	}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("m_bench_%d", i)
		_, err := st.PrepareSend(PrepareRequest{
			CallerID: "operator:1", IdemKey: fmt.Sprintf("k%d", i), Fingerprint: "fp",
			Message: model.Message{
				RecordVersion: 1, ID: id, ConversationID: "c_bench", SenderLabel: "operator",
				RecipientSessionID: "s_recipient", Body: "body", BodySHA256: "h", BodyBytes: 4,
				State: model.MessageDispatching, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			},
			Attempt: model.Attempt{
				RecordVersion: 1, ID: "a_" + id, MessageID: id, ReservationID: "r_" + id,
				State: model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(time.Minute),
			},
			Reservation: model.Reservation{
				RecordVersion: 1, ID: "r_" + id, MessageID: id, ConversationID: "c_bench",
				State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(time.Minute),
			},
			ConversationID: "c_bench", Now: now,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListSessions(b *testing.B) {
	st, err := Open(filepath.Join(b.TempDir(), "state.db"), 2*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	b.StopTimer()
	for i := 0; i < 1000; i++ {
		s := model.Session{
			RecordVersion: 1, ID: fmt.Sprintf("s_%d", i), Alias: fmt.Sprintf("a%d", i),
			Target:       model.Target{Adapter: "claude", NativeSessionID: fmt.Sprintf("n%d", i)},
			RegisteredAt: time.Now().UTC(),
		}
		if err := st.PutSession(s); err != nil {
			b.Fatal(err)
		}
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.ListSessions(false); err != nil {
			b.Fatal(err)
		}
	}
}

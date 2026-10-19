package store

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"amgo/internal/model"
)

// contentionOpenTimeout is deliberately wider than the production default.
// This test asserts serialization correctness, not a two-second latency bound;
// CI can deschedule a worker past the default while another holds the lock.
const contentionOpenTimeout = 10 * time.Second

// TestContentionChild is the worker side of the multi-process test. It is a
// no-op unless the parent supplies AMGO_CONTENTION_DB.
func TestContentionChild(t *testing.T) {
	db := os.Getenv("AMGO_CONTENTION_DB")
	if db == "" {
		t.Skip("child-only test")
	}
	conv := os.Getenv("AMGO_CONTENTION_CONV")
	prefix := os.Getenv("AMGO_CONTENTION_PREFIX")
	n, _ := strconv.Atoi(os.Getenv("AMGO_CONTENTION_N"))
	st, err := Open(db, contentionOpenTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m_%s_%d", prefix, i)
		_, err := st.PrepareSend(PrepareRequest{
			CallerID: "operator:1", IdemKey: prefix + "-" + strconv.Itoa(i), Fingerprint: "fp",
			Message: model.Message{
				RecordVersion: 1, ID: id, ConversationID: conv, SenderSessionID: "",
				SenderLabel: "operator", RecipientSessionID: "s_recipient", Body: "x",
				BodySHA256: "h", BodyBytes: 1, State: model.MessageDispatching,
				CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			},
			Attempt: model.Attempt{
				RecordVersion: 1, ID: "a_" + id, MessageID: id, ReservationID: "r_" + id,
				State: model.AttemptDispatching, StartedAt: now, DispatchDeadline: now.Add(time.Minute),
				OwnerPID: os.Getpid(),
			},
			Reservation: model.Reservation{
				RecordVersion: 1, ID: "r_" + id, MessageID: id, ConversationID: conv,
				State: model.ReservationReserved, CreatedAt: now, DispatchDeadline: now.Add(time.Minute),
			},
			ConversationID: conv, Now: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestContentionMultiProcess(t *testing.T) {
	if os.Getenv("AMGO_CONTENTION") == "" {
		t.Skip("set AMGO_CONTENTION=1 to run the multi-process contention test")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	st, err := Open(db, contentionOpenTimeout)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.CreateConversation(model.Conversation{
		RecordVersion: 1, ID: "c_contention", Participants: []string{"operator:1", "s_recipient"},
		MaxMessages: 10000, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	const procs, each = 16, 10
	cmds := make([]*exec.Cmd, procs)
	outs := make([]*bytes.Buffer, procs)
	for i := 0; i < procs; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestContentionChild$", "-test.v")
		cmd.Env = append(os.Environ(),
			"AMGO_CONTENTION_DB="+db,
			"AMGO_CONTENTION_CONV=c_contention",
			"AMGO_CONTENTION_PREFIX=p"+strconv.Itoa(i),
			"AMGO_CONTENTION_N="+strconv.Itoa(each),
		)
		outs[i] = &bytes.Buffer{}
		cmd.Stdout = outs[i]
		cmd.Stderr = outs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("worker %d: %v\n%s", i, err, outs[i].String())
		}
	}
	st, err = Open(db, contentionOpenTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conv, err := st.GetConversation("c_contention")
	if err != nil {
		t.Fatal(err)
	}
	if conv.SpentCount != procs*each {
		t.Fatalf("spent=%d want %d", conv.SpentCount, procs*each)
	}
	v, err := st.CheckInvariants(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		if x.Code == "spent_count_mismatch" || x.Code == "budget_overrun" || x.Code == "attempt_orphan_message" {
			t.Fatalf("violation: %+v", x)
		}
	}
}

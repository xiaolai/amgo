package store

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amgo/internal/model"
)

// TestCrashChild is the worker side of the crash test. It opens a write
// transaction, writes one record, announces readiness, and then waits to be
// killed before the transaction commits.
func TestCrashChild(t *testing.T) {
	db := os.Getenv("AMGO_CRASH_DB")
	if db == "" {
		t.Skip("child-only test")
	}
	st, err := Open(db, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	conv := model.Conversation{
		RecordVersion: 1, ID: "c_crash", Participants: []string{"operator:1", "s_recipient"},
		MaxMessages: 4, ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC(),
	}
	if err := putJSON(tx.Bucket(bucketConversations), []byte(conv.ID), conv); err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	_ = os.Stdout.Sync()
	time.Sleep(5 * time.Minute)
}

func TestCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	st, err := Open(path, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
	cmd.Env = append(os.Environ(), "AMGO_CRASH_DB="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	t.Cleanup(func() {
		if !done {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "READY" {
			ready = true
			break
		}
	}
	if !ready {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("crash child did not reach READY; stderr=%s", stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()
	done = true

	st, err = Open(path, 20*time.Second)
	if err != nil {
		t.Fatalf("open after crash: %v", err)
	}
	defer st.Close()
	if _, err := st.GetConversation("c_crash"); err == nil {
		t.Fatal("an uncommitted transaction survived the crash")
	}

	now := time.Now().UTC()
	baseConversation(t, st, "c1", 4, now)
	prepare(t, st, "c1", "m1", "k1", "fp1", now)
	msg, err := st.GetMessage("m1")
	if err != nil || msg.ID != "m1" {
		t.Fatalf("message=%+v err=%v", msg, err)
	}
	conv, err := st.GetConversation("c1")
	if err != nil {
		t.Fatal(err)
	}
	if conv.SpentCount != 1 || conv.RefundedCount != 0 {
		t.Fatalf("conversation=%+v", conv)
	}
	v, err := st.CheckInvariants(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v {
		if x.Code == "spent_count_mismatch" || x.Code == "refunded_count_mismatch" || x.Code == "attempt_orphan_message" {
			t.Fatalf("violation after crash: %+v", x)
		}
	}
}

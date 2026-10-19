package process

import (
	"os"
	"testing"

	"amgo/internal/platform"
)

func TestCheckIsTriState(t *testing.T) {
	pid := os.Getpid()
	start, err := platform.ProcStartUTC(pid)
	if err != nil {
		t.Fatal(err)
	}
	if got := Check(pid, start); got != Live {
		t.Fatalf("matching identity: %v", got)
	}
	if got := Check(pid, ""); got != Unknown {
		t.Fatalf("missing identity must be unknown, got %v", got)
	}
	if got := Check(pid, "Mon Jan _2 00:00:00 2000"); got != Dead {
		t.Fatalf("mismatched identity must be dead, got %v", got)
	}
	if got := Check(999999, "anything"); got != Dead {
		t.Fatalf("absent pid must be dead, got %v", got)
	}
	if !Alive(pid, "") {
		t.Fatal("an unverifiable identity must count as possibly alive")
	}
	if Alive(999999, "anything") {
		t.Fatal("an absent pid must not count as alive")
	}
}

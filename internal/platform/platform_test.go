//go:build unix

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcStartUTCIsTheSameInstant(t *testing.T) {
	local, err := ProcStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	utc, err := ProcStartUTC(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	var lt, ut time.Time
	for _, layout := range []string{"Mon Jan _2 15:04:05 2006", "Mon Jan 2 15:04:05 2006"} {
		if lt.IsZero() {
			lt, _ = time.ParseInLocation(layout, local, time.Local)
		}
		if ut.IsZero() {
			ut, _ = time.ParseInLocation(layout, utc, time.UTC)
		}
	}
	if lt.IsZero() || ut.IsZero() {
		t.Fatalf("unparseable: local=%q utc=%q", local, utc)
	}
	if !lt.Equal(ut) {
		t.Fatalf("local=%q (%s) and utc=%q (%s) are different instants", local, lt, utc, ut)
	}
}

func TestReadBoundedFileRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo unavailable")
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadBoundedFile(fifo, 1024)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO read blocked")
	}
}

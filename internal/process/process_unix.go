//go:build unix

// Package process starts provider processes in their own process group and
// confirms termination of the whole group, not just the leader.
package process

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"amgo/internal/platform"
)

// Configure gives cmd its own process group, so cancellation reaches provider
// grandchildren as well as the leader.
func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// ConfigureDetached starts cmd in a new session so a background supervisor
// survives the CLI that spawned it. Setsid also gives it a new process group.
func ConfigureDetached(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// Liveness is a tri-state process check. Dead is the only value that proves the
// original process is gone. Unknown means the PID exists but its identity could
// not be verified; callers must not signal it, release its lease, or treat it as
// dead.
type Liveness int

const (
	Dead Liveness = iota
	Live
	Unknown
)

// Check verifies that pid is the same process that was started. A missing or
// unreadable start identity yields Unknown rather than trusting the PID alone.
func Check(pid int, procStart string) Liveness {
	if pid <= 0 || !platform.PIDAlive(pid) {
		return Dead
	}
	if procStart == "" {
		return Unknown
	}
	start, err := platform.ProcStartUTC(pid)
	if err != nil {
		return Unknown
	}
	if start == procStart {
		return Live
	}
	return Dead
}

// Alive reports whether the process may still be the original owner. It is
// intentionally conservative: an unverifiable identity counts as possibly
// alive. Signaling requires Check(pid, procStart) == Live.
func Alive(pid int, procStart string) bool { return Check(pid, procStart) != Dead }

// GroupAlive reports whether any process remains in the group led by pid.
func GroupAlive(pid int) bool { return groupAlive(pid) }

func groupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// TerminateGroup asks the provider to cancel its own turn first with SIGINT,
// then escalates to SIGTERM and SIGKILL. Codex uses SIGINT to abort the turn
// and kill sandbox commands it spawned; a SIGTERM first can orphan those
// commands. An error means termination could not be confirmed; callers must
// not report the work as cleanly cancelled.
func TerminateGroup(pid int, grace time.Duration) error {
	if pid <= 0 || !groupAlive(pid) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGINT)
	if waitGroup(pid, grace) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitGroup(pid, 2*time.Second) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if waitGroup(pid, 3*time.Second) {
		return nil
	}
	return fmt.Errorf("process group %d did not exit after SIGKILL", pid)
}

func waitGroup(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !groupAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

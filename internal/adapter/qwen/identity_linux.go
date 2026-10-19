//go:build linux

package qwen

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func rawIDString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

func pidNSInode(pid int) (string, error) {
	fi, err := os.Stat(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("no inode metadata")
	}
	return strconv.FormatUint(st.Ino, 10), nil
}

func startTicks(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", fmt.Errorf("malformed stat")
	}
	fields := strings.Fields(s[i+1:])
	// After comm, fields[0] is state (field 3); starttime is field 22.
	if len(fields) < 20 {
		return "", fmt.Errorf("short stat")
	}
	return fields[19], nil
}

func linuxIdentityMatches(r record) (bool, string) {
	if r.PIDNS == nil || rawIDString(r.PIDNS) == "" {
		return false, "record has no pidNs"
	}
	if r.ProcStart == nil || *r.ProcStart == "" {
		return false, "record has no procStart"
	}
	ns, err := pidNSInode(r.PID)
	if err != nil {
		return false, "cannot read the process namespace"
	}
	if rawIDString(r.PIDNS) != ns {
		return false, "pid namespace does not match"
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false, "cannot read the boot ID"
	}
	ticks, err := startTicks(r.PID)
	if err != nil {
		return false, "cannot read the process start ticks"
	}
	want := strings.TrimSpace(string(boot)) + ":" + ticks
	if *r.ProcStart != want {
		return false, "process start does not match the record"
	}
	return true, ""
}

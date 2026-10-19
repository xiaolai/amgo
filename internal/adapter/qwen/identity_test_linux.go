//go:build linux

package qwen

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func testIdentity() (*string, json.RawMessage) {
	fi, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return nil, nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, nil
	}
	ns := strconv.FormatUint(st.Ino, 10)
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, nil
	}
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return nil, nil
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil, nil
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return nil, nil
	}
	start := strings.TrimSpace(string(boot)) + ":" + fields[19]
	rawNS, _ := json.Marshal(ns)
	return &start, rawNS
}

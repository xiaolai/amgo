//go:build !linux

package qwen

// The official record leaves procStart and pidNs null outside Linux, so there
// is no portable process-identity binding to check here. PID liveness and the
// filename/record PID match still apply.
func linuxIdentityMatches(r record) (bool, string) { return true, "" }

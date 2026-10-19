//go:build unix

// Package platform contains OS-specific process and filesystem checks.
package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func UID() int { return os.Getuid() }

// PIDAlive reports whether pid exists. EPERM still means it exists.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// ProcStart returns the process start time in the same local format Claude
// Code publishes (for example "Mon Oct 19 02:36:12 2026").
func ProcStart(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	s := strings.Join(strings.Fields(string(out)), " ")
	if s == "" {
		return "", fmt.Errorf("no start time for pid %d", pid)
	}
	return s, nil
}

func ProcStartTime(pid int) (time.Time, error) {
	s, err := ProcStart(pid)
	if err != nil {
		return time.Time{}, err
	}
	for _, layout := range []string{"Mon Jan _2 15:04:05 2006", "Mon Jan 2 15:04:05 2006"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized process start %q", s)
}

// ProcStartUTC returns the process start in the format Claude Code publishes
// (UTC wall clock with no zone suffix). ps reports local time, so compare
// absolute instants rather than raw strings.
func ProcStartUTC(pid int) (string, error) {
	t, err := ProcStartTime(pid)
	if err != nil {
		return "", err
	}
	return t.UTC().Format("Mon Jan _2 15:04:05 2006"), nil
}

// CheckSocket validates a Unix socket used as a native endpoint.
func CheckSocket(path string, uid int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a socket", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has no ownership metadata", path)
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not %d", path, st.Uid, uid)
	}
	// Only the socket's immediate containing directory must be private. Shared
	// ancestors such as /tmp are normal.
	dir := parent(path)
	if dir == "." || dir == "" {
		return fmt.Errorf("%s has no containing directory", path)
	}
	di, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !di.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	dst, ok := di.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has no ownership metadata", dir)
	}
	if int(dst.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not %d", dir, dst.Uid, uid)
	}
	if di.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible by group/other (mode %o)", dir, di.Mode().Perm())
	}
	return nil
}

// CheckRegularFile validates an owner-only regular file, its maximum size and
// its non-symlink status.
func CheckRegularFile(path string, maxSize int64, uid int) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if maxSize > 0 && fi.Size() > maxSize {
		return nil, fmt.Errorf("%s is %d bytes, limit %d", path, fi.Size(), maxSize)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("%s has no ownership metadata", path)
	}
	if int(st.Uid) != uid {
		return nil, fmt.Errorf("%s is owned by uid %d, not %d", path, st.Uid, uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %o; want owner-only", path, fi.Mode().Perm())
	}
	return fi, nil
}

// ReadRegularFile opens path without following symlinks, validates the opened
// inode, and reads at most maxSize bytes. Validation and the read share one
// descriptor, so a replacement or growth in between cannot bypass the checks.
func ReadRegularFile(path string, maxSize int64, uid int) ([]byte, error) {
	return readNoFollow(path, maxSize, uid, true)
}

// ReadBoundedFile is ReadRegularFile without the owner-only requirement, for
// world-readable metadata such as a session registry record.
func ReadBoundedFile(path string, maxSize int64) ([]byte, error) {
	return readNoFollow(path, maxSize, 0, false)
}

func readNoFollow(path string, maxSize int64, uid int, ownerOnly bool) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if maxSize > 0 && fi.Size() > maxSize {
		return nil, fmt.Errorf("%s is %d bytes, limit %d", path, fi.Size(), maxSize)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("%s has no ownership metadata", path)
	}
	if ownerOnly {
		if int(st.Uid) != uid {
			return nil, fmt.Errorf("%s is owned by uid %d, not %d", path, st.Uid, uid)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s has mode %o; want owner-only", path, fi.Mode().Perm())
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if maxSize > 0 && int64(len(raw)) > maxSize {
		return nil, fmt.Errorf("%s grew past the %d byte limit while reading", path, maxSize)
	}
	return raw, nil
}

func parent(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

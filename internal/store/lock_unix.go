//go:build unix

package store

import (
	"errors"
	"os"
	"syscall"
	"time"

	"amgo/internal/model"
)

func lockFile(path string, exclusive bool, timeout time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := flockUntil(f, how, timeout); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// lockFileReadOnly takes a shared lock without creating or writing the lock
// file. A missing lock file means no writer has initialized state yet.
func lockFileReadOnly(path string, timeout time.Duration) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if err := flockUntil(f, syscall.LOCK_SH, timeout); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func flockUntil(f *os.File, how int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return &model.CodedError{Code: model.ErrStoreBusy, Message: "state lock wait exceeded", Retryable: true}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func unlockFile(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

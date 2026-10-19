//go:build !unix

package store

import (
	"os"
	"time"
)

// lockFile is a no-op on platforms where amgo does not claim native support.
func lockFile(path string, _ bool, _ time.Duration) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

func lockFileReadOnly(path string, _ time.Duration) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

func unlockFile(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

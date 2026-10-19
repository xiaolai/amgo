//go:build !unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

func secureStateDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0o700)
}

func secureDBFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("state database %s is not a regular file", path)
	}
	return nil
}

func validateStatePath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return nil
}

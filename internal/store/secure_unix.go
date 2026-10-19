//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func secureStateDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state directory %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("state path %s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("state directory %s has no ownership metadata", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("state directory %s is owned by uid %d", dir, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("cannot secure state directory %s: %w", dir, err)
		}
	}
	return nil
}

func secureDBFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state database %s is a symlink", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("state database %s is not a regular file", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("state database %s has no ownership metadata", path)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("state database %s is owned by uid %d", path, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("cannot secure state database %s: %w", path, err)
		}
	}
	return nil
}

func validateStatePath(path string) error {
	dir := filepath.Dir(path)
	dfi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if dfi.Mode()&os.ModeSymlink != 0 || !dfi.IsDir() {
		return fmt.Errorf("state directory %s is not a real directory", dir)
	}
	dst, ok := dfi.Sys().(*syscall.Stat_t)
	if !ok || int(dst.Uid) != os.Getuid() {
		return fmt.Errorf("state directory %s is not owned by the current user", dir)
	}
	if dfi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state directory %s is accessible by group/other", dir)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("state database %s is not a regular file", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("state database %s is not owned by the current user", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state database %s is accessible by group/other", path)
	}
	return nil
}

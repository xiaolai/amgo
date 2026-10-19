package store

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Compact rebuilds the database into a sibling file and swaps it in only after
// every bucket has the same key count. The source must be closed by the caller.
func Compact(path string) (int64, error) {
	lock, err := lockFile(path+".lock", true, 2*time.Second)
	if err != nil {
		return 0, err
	}
	defer unlockFile(lock)
	src, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return 0, err
	}
	tmp := path + ".compact"
	_ = os.Remove(tmp)
	dst, err := bolt.Open(tmp, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		_ = src.Close()
		return 0, err
	}
	if err := bolt.Compact(dst, src, 64<<20); err != nil {
		_ = dst.Close()
		_ = src.Close()
		_ = os.Remove(tmp)
		return 0, err
	}
	_ = dst.Close()
	_ = src.Close()

	if err := compareBucketCounts(path, tmp); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, err
	}
	_ = f.Close()
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return 0, syncErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func compareBucketCounts(a, b string) error {
	ca, err := countBuckets(a)
	if err != nil {
		return err
	}
	cb, err := countBuckets(b)
	if err != nil {
		return err
	}
	for _, name := range allBuckets {
		if ca[string(name)] != cb[string(name)] {
			return fmt.Errorf("compaction count mismatch in %s: %d -> %d", name, ca[string(name)], cb[string(name)])
		}
	}
	return nil
}

func countBuckets(path string) (map[string]int, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string]int{}
	err = db.View(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			b := tx.Bucket(name)
			if b == nil {
				out[string(name)] = 0
				continue
			}
			n := 0
			_ = b.ForEach(func(_, _ []byte) error {
				n++
				return nil
			})
			out[string(name)] = n
		}
		return nil
	})
	return out, err
}

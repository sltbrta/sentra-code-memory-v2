// Package durablefile writes files that survive a crash.
//
// The repository had nine separate implementations of "write this atomically",
// and they did not agree. Only one -- codecrawl's index Save -- did the whole
// sequence: write a temp file, fsync it, close with the error checked, rename,
// then fsync the parent directory so the new name itself is durable. Six
// others skipped the parent fsync. Two skipped everything: they truncated the
// live file in place with os.Create, discarded every write error, and one then
// deleted the only other copy of the data.
//
// This package is that one correct sequence, in one place. It lives under
// services/internal so the broker and gateway trees can use it too; the brain
// tree's own internal packages are not importable from there.
package durablefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultMode is the permission for a file holding derived or cached data.
// Callers holding user content should pass 0o600 explicitly.
const DefaultMode os.FileMode = 0o600

// Write atomically replaces path with body.
//
// The live file is untouched until the rename, so a crash at any point leaves
// either the old contents or the new ones, never a truncated mixture.
func Write(path string, body []byte, mode os.FileMode) error {
	return WriteFunc(path, mode, func(w io.Writer) error {
		_, err := w.Write(body)
		return err
	})
}

// WriteFunc atomically replaces path with whatever emit writes.
//
// Use it for content produced incrementally -- a JSON encoder, a gob stream --
// so a large corpus need not be assembled in memory first. emit's error is
// propagated: a partial write leaves the original file in place.
func WriteFunc(path string, mode os.FileMode, emit func(io.Writer) error) (retErr error) {
	if path == "" {
		return errors.New("durablefile: empty path")
	}
	if mode == 0 {
		mode = DefaultMode
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("durablefile: create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("durablefile: temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	// Every failure path below removes the temp file. A crashed writer leaves
	// one behind, which is why the name is unique rather than fixed: a stale
	// temp must never collide with a live write.
	defer func() {
		if retErr != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()

	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("durablefile: chmod %s: %w", name, err)
	}
	if err := emit(tmp); err != nil {
		return fmt.Errorf("durablefile: write %s: %w", path, err)
	}
	// fsync before rename: rename only orders the directory entry, it does not
	// flush the data. Without this a crash can leave the new name pointing at
	// unwritten blocks.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("durablefile: sync %s: %w", name, err)
	}
	// Close is checked because a buffered-write failure -- ENOSPC being the
	// one that matters -- surfaces here and nowhere else. Discarding it is how
	// a full disk becomes a silently truncated file that reports success.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("durablefile: close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("durablefile: rename into %s: %w", path, err)
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory so a rename into it is durable.
//
// Filesystems that provide atomic rename but reject directory fsync
// (EINVAL/ENOTSUP) are tolerated; every other failure fails closed.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("durablefile: open %s: %w", dir, err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, syscall.ENOTSUP) {
		return fmt.Errorf("durablefile: sync %s: %w", dir, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("durablefile: close %s: %w", dir, closeErr)
	}
	return nil
}

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
	// Write through a symlink rather than over it.
	//
	// Renaming onto the link path replaces the link with a regular file, so a
	// deliberate layout -- a corpus linked onto another volume, a config
	// linked into place -- is silently dissolved by the first write. The
	// callers here were migrated from os.WriteFile, which writes through, so
	// replacing was also a change of contract none of them asked for.
	path, dir, err := resolveTarget(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("durablefile: create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("durablefile: temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	// The temp file is removed unless the rename succeeded.
	//
	// This was conditioned on retErr != nil, which is nil while a panic is
	// unwinding -- so a panic inside emit left the descriptor open and the
	// temp file on disk, once per call. emit is caller-supplied (a JSON
	// encoder, a gob stream), so a panic in it is not hypothetical, and a
	// long-lived service leaks an fd and a file every time.
	renamed := false
	defer func() {
		if !renamed {
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
	renamed = true
	return SyncDir(dir)
}

// resolveTarget returns the path the write should land on and its directory.
//
// When path names a symlink, the write follows it to its final target so the
// link survives and the temp file is created on the target's filesystem --
// renaming across filesystems fails, so creating the temp beside the link
// would break the write as well as the layout. A broken link, or any other
// path, is used as given.
func resolveTarget(path string) (string, string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return path, filepath.Dir(path), nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// A dangling link: nothing to write through to. Replacing it is the
		// only option left, and is what the caller asked for by naming it.
		return path, filepath.Dir(path), nil
	}
	if !filepath.IsAbs(resolved) {
		abs, absErr := filepath.Abs(resolved)
		if absErr != nil {
			return "", "", fmt.Errorf("durablefile: resolve %s: %w", path, absErr)
		}
		resolved = abs
	}
	return resolved, filepath.Dir(resolved), nil
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

package codecrawl

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// safeRootPath validates a path and then hands back a string for someone else
// to open, which is two resolutions of the same name with a gap in between.
//
// The gap is real and does not need a race to demonstrate. Validate
// root/pkg/file.go: EvalSymlinks confirms it resolves inside the root, and the
// resolved absolute path is returned. Then replace the *directory* root/pkg
// with a symlink pointing outside. The subsequent os.Open resolves
// root/pkg/file.go from scratch, follows the new link, and reads a file the
// check never saw. Returning the resolved path -- which this repository
// already does -- narrows the window to exactly this: the leaf was pinned, the
// components leading to it were not.
//
// os.Root closes it. It holds a descriptor for the root directory and walks
// each component relative to it, refusing any symlink that leaves the root, so
// the resolution and the open are one operation with nothing in between to
// swap. There is no path string handed between a check and a use, because
// there is no separate check.
//
// The old function stays for callers that genuinely need a path rather than an
// open file -- reporting one, comparing one -- and its own escape check is
// unchanged. What moves here is every read.

// ErrRootEscape reports a path that does not resolve inside its root.
var ErrRootEscape = errors.New("codecrawl: path escapes its root")

// OpenRooted opens rel beneath root, refusing anything that resolves outside
// it. The caller closes the returned file.
func OpenRooted(root, rel string) (*os.File, error) {
	rooted, cleaned, err := openRoot(root, rel)
	if err != nil {
		return nil, err
	}
	defer rooted.Close()
	f, err := rooted.Open(cleaned)
	if err != nil {
		return nil, rootedError(root, rel, err)
	}
	return f, nil
}

// ReadRooted reads at most limit bytes of rel beneath root.
//
// A limit of zero or less reads the whole file. The read is bounded here
// rather than by the caller so a file that grows between the stat and the read
// cannot return more than the caller asked for.
func ReadRooted(root, rel string, limit int64) ([]byte, error) {
	f, err := OpenRooted(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if limit <= 0 {
		return io.ReadAll(f)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

// StatRooted stats rel beneath root without following a link out of it.
func StatRooted(root, rel string) (fs.FileInfo, error) {
	rooted, cleaned, err := openRoot(root, rel)
	if err != nil {
		return nil, err
	}
	defer rooted.Close()
	info, err := rooted.Stat(cleaned)
	if err != nil {
		return nil, rootedError(root, rel, err)
	}
	return info, nil
}

// openRoot opens the root directory and normalises rel against it.
//
// An absolute rel is accepted when it is already inside the root, because
// callers hold both forms: the index stores repository-relative paths, while a
// request may name an absolute one that the pin has already admitted.
func openRoot(root, rel string) (*os.Root, string, error) {
	rootAbs, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, "", fmt.Errorf("codecrawl: resolve root: %w", err)
	}
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(rel)))
	if cleaned == "" || cleaned == "." {
		return nil, "", ErrRootEscape
	}
	if filepath.IsAbs(cleaned) {
		within, relErr := filepath.Rel(rootAbs, cleaned)
		if relErr != nil {
			return nil, "", ErrRootEscape
		}
		cleaned = within
	}
	// A leading ".." is refused before the root is even opened, so an obvious
	// escape does not cost a descriptor.
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return nil, "", ErrRootEscape
	}
	rooted, err := os.OpenRoot(rootAbs)
	if err != nil {
		return nil, "", fmt.Errorf("codecrawl: open root %s: %w", rootAbs, err)
	}
	return rooted, cleaned, nil
}

// rootedError converts an escape into ErrRootEscape and leaves every other
// failure as itself, so "not found" stays distinguishable from "refused".
func rootedError(root, rel string, err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w: %s in %s: %v", ErrRootEscape, rel, root, err)
}

// RootRelative returns the root-relative, slash-separated form of rel.
//
// It performs no filesystem resolution: the name is normalised and checked for
// traversal only. Callers that need to know a path is real must open or stat
// it through the root, which is the operation that decides -- deriving a
// relative name from an EvalSymlinks result and then acting on it is the
// pattern this package exists to remove.
func RootRelative(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return "", fmt.Errorf("codecrawl: resolve root: %w", err)
	}
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(rel)))
	if cleaned == "" || cleaned == "." {
		return "", ErrRootEscape
	}
	if filepath.IsAbs(cleaned) {
		within, relErr := filepath.Rel(rootAbs, cleaned)
		if relErr != nil {
			return "", ErrRootEscape
		}
		cleaned = within
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", ErrRootEscape
	}
	return filepath.ToSlash(cleaned), nil
}

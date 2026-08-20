// Package pathguard resolves and confines filesystem paths.
//
// The repository had roughly a dozen containment checks and they disagreed in
// ways that mattered. Some resolved symlinks and some did not. Two fell back to
// the *unresolved* path when resolution failed, which weakened the check
// precisely when resolution was uncertain. Several used
// strings.HasPrefix(rel, "..") or strings.Contains(rel, ".."), which rejects a
// legitimate file named "..config" while catching nothing the separator-aware
// form misses. One could not handle a path that did not exist yet, so callers
// that create files had to skip the check entirely.
//
// This is one implementation of the two operations everything needed.
package pathguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscapes reports a candidate that resolves outside its root.
var ErrEscapes = errors.New("pathguard: path escapes its root")

// Resolve returns an absolute, symlink-resolved form of path.
//
// A path that does not exist yet still resolves: the longest existing ancestor
// is resolved and the remaining components are appended. That matters because
// the alternative -- refusing to check a path until after it is created -- is
// how a containment check gets skipped on exactly the operation that creates
// something.
func Resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("pathguard: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("pathguard: absolute %q: %w", path, err)
	}
	current := filepath.Clean(abs)
	remainder := ""
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !os.IsNotExist(err) {
			// A permission error is not "does not exist"; treating it as such
			// would let an unreadable ancestor weaken the check.
			return "", fmt.Errorf("pathguard: resolve %q: %w", path, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(abs), nil
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// Within reports whether candidate is root or a descendant of it, after both
// are resolved. It fails closed: any resolution error denies.
//
// filepath.Rel is used rather than a string prefix so that a sibling sharing a
// name prefix -- "/srv/repo-backup" against a "/srv/repo" root -- is not
// mistaken for a descendant.
func Within(root, candidate string) bool {
	rootReal, err := Resolve(root)
	if err != nil {
		return false
	}
	candidateReal, err := Resolve(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootReal, candidateReal)
	if err != nil {
		return false
	}
	return !escapes(rel)
}

// Contain resolves candidate and returns it only if it stays inside root.
func Contain(root, candidate string) (string, error) {
	rootReal, err := Resolve(root)
	if err != nil {
		return "", err
	}
	candidateReal, err := Resolve(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootReal, candidateReal)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEscapes, err)
	}
	if escapes(rel) {
		return "", ErrEscapes
	}
	return candidateReal, nil
}

// escapes reports whether a relative path leaves its base. It compares whole
// segments: "..config" is an ordinary name, "../config" is not.
func escapes(rel string) bool {
	if rel == ".." {
		return true
	}
	for _, segment := range strings.Split(rel, string(filepath.Separator)) {
		if segment == ".." {
			return true
		}
	}
	return false
}

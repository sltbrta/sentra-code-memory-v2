package localbootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	databaseLeaf = "authority.sqlite3"
	socketLeaf   = "authority.sock"
	objectLeaf   = "objects"
)

// validateStateLayout establishes the bounded v1 state layout without
// creating or opening mutable state. Downstream code must retain a descriptor
// for StateRoot, open each child relative to it with no-follow semantics, and
// immediately re-check the opened leaf identity before use.
func validateStateLayout(manifest BootstrapV1, manifestPath string) error {
	if !validStatePaths(manifest) {
		return ErrInvalidManifest
	}
	rootInfo, err := os.Lstat(manifest.StateRoot)
	if err != nil || !safeStateRoot(rootInfo) || !safePathAncestors(manifest.StateRoot) ||
		!resolvesToSelf(manifest.StateRoot) || pathsOverlap(manifest.StateRoot, manifestPath) {
		return ErrUnsafeManifest
	}
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return ErrUnsafeManifest
	}
	manifestParent, err := os.Lstat(filepath.Dir(manifestPath))
	if err != nil || os.SameFile(rootInfo, manifestParent) {
		return ErrUnsafeManifest
	}
	sourceRootInfo, err := os.Lstat(manifest.ApprovedSourceRoot)
	if err != nil || !safeApprovedSourceRoot(sourceRootInfo) ||
		!safePathAncestors(manifest.ApprovedSourceRoot) ||
		!resolvesToSelf(manifest.ApprovedSourceRoot) ||
		pathsOverlap(manifest.ApprovedSourceRoot, manifestPath) {
		return ErrUnsafeManifest
	}
	checks := []struct {
		path string
		safe func(os.FileInfo) bool
	}{
		{path: manifest.DatabasePath, safe: safeDatabaseLeaf},
		{path: manifest.SocketPath, safe: safeSocketLeaf},
		{path: manifest.ObjectRoot, safe: safeObjectLeaf},
	}
	for _, check := range checks {
		if pathsOverlap(check.path, manifestPath) {
			return ErrUnsafeManifest
		}
		info, statErr := os.Lstat(check.path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil || !check.safe(info) || os.SameFile(info, manifestInfo) ||
			!resolvesToSelf(check.path) {
			return ErrUnsafeManifest
		}
	}
	rootAfter, err := os.Lstat(manifest.StateRoot)
	if err != nil || !os.SameFile(rootInfo, rootAfter) || !safeStateRoot(rootAfter) ||
		!safePathAncestors(manifest.StateRoot) || !resolvesToSelf(manifest.StateRoot) {
		return ErrUnsafeManifest
	}
	sourceRootAfter, err := os.Lstat(manifest.ApprovedSourceRoot)
	if err != nil || !os.SameFile(sourceRootInfo, sourceRootAfter) ||
		!safeApprovedSourceRoot(sourceRootAfter) ||
		!safePathAncestors(manifest.ApprovedSourceRoot) ||
		!resolvesToSelf(manifest.ApprovedSourceRoot) {
		return ErrUnsafeManifest
	}
	return nil
}

func safePathAncestors(path string) bool {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
			info.Mode().Perm()&0o022 != 0 {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return true
		}
	}
}

func safeStateRoot(info os.FileInfo) bool {
	return info.IsDir() && exactMode(info.Mode(), 0o700) && ownedByCurrentUser(info)
}

func safeApprovedSourceRoot(info os.FileInfo) bool {
	const specialBits = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode()&specialBits == 0 && info.Mode().Perm()&0o022 == 0 && ownedByCurrentUser(info)
}

func safeDatabaseLeaf(info os.FileInfo) bool {
	return info.Mode().IsRegular() && exactMode(info.Mode(), 0o600) && ownedByCurrentUser(info)
}

func safeSocketLeaf(info os.FileInfo) bool {
	return info.Mode()&os.ModeSocket != 0 && ownerOnlyMode(info.Mode(), 0o700) && ownedByCurrentUser(info)
}

func safeObjectLeaf(info os.FileInfo) bool {
	return info.IsDir() && exactMode(info.Mode(), 0o700) && ownedByCurrentUser(info)
}

func ownerOnlyMode(mode os.FileMode, allowed os.FileMode) bool {
	const specialBits = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	return mode&specialBits == 0 && mode.Perm()&0o077 == 0 && mode.Perm()&^allowed == 0
}

func pathsOverlap(left, right string) bool {
	return containsPath(left, right) || containsPath(right, left)
}

func containsPath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

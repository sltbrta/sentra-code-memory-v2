package pathguard_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/pathguard"
)

func TestWithinAdmitsTheRootAndItsDescendants(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{root, filepath.Join(root, "a"), filepath.Join(root, "a", "b")} {
		if !pathguard.Within(root, candidate) {
			t.Fatalf("%q should be inside %q", candidate, root)
		}
	}
}

func TestWithinRejectsTraversalAndSiblings(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	sibling := filepath.Join(parent, "repo-backup")
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, candidate := range []string{
		parent,
		sibling, // shares a name prefix; a HasPrefix check admits this
		filepath.Join(root, "..", "repo-backup"),
	} {
		if pathguard.Within(root, candidate) {
			t.Fatalf("%q must not be inside %q", candidate, root)
		}
	}
}

// TestWithinAdmitsANameBeginningWithDots is the false positive that the
// substring and prefix forms produced.
func TestWithinAdmitsANameBeginningWithDots(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "..config")
	if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !pathguard.Within(root, name) {
		t.Fatal(`a file named "..config" is not a traversal`)
	}
}

// TestResolveHandlesPathsThatDoNotExistYet is what lets a caller check
// containment *before* creating something, rather than skipping the check.
func TestResolveHandlesPathsThatDoNotExistYet(t *testing.T) {
	root := t.TempDir()
	future := filepath.Join(root, "not", "yet", "created.txt")
	got, err := pathguard.Resolve(future)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !pathguard.Within(root, got) {
		t.Fatalf("%q should resolve inside %q", got, root)
	}
}

// TestWithinResolvesSymlinkedRoots covers the macOS /var -> /private/var shape
// that made unresolved comparisons wrong.
func TestWithinResolvesSymlinkedRoots(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	target := filepath.Join(real, "file.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !pathguard.Within(link, target) {
		t.Fatal("a symlinked root must resolve to the same place as its target")
	}
}

// TestWithinRejectsASymlinkPointingOutOfTheRoot is the containment property
// that matters most.
func TestWithinRejectsASymlinkPointingOutOfTheRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if pathguard.Within(root, link) {
		t.Fatal("a symlink resolving outside the root must be refused")
	}
}

func TestContainReturnsTheResolvedPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := pathguard.Contain(root, target)
	if err != nil {
		t.Fatalf("Contain: %v", err)
	}
	if got == "" {
		t.Fatal("Contain returned an empty path")
	}
	if _, err := pathguard.Contain(root, filepath.Join(root, "..")); err == nil {
		t.Fatal("Contain must refuse a traversal")
	}
}

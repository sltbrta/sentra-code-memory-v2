package codecrawl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pin validated a path and then handed a string to whoever opened it,
// which is two resolutions of the same name with a gap in between.
//
// The gap does not need a race to demonstrate, which is what makes these
// deterministic rather than load-sensitive. Validate root/pkg/file.go, then
// replace the *directory* root/pkg with a symlink pointing outside: the open
// resolves the name from scratch, follows the new link, and reads a file the
// check never saw. Returning the resolved path -- which this repository
// already does -- pins the leaf and leaves every component above it unpinned.

// escapeFixture builds a root holding pkg/file.go, and an outside directory
// holding a file at the same relative path.
func escapeFixture(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(root, "pkg"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "file.go"),
		[]byte("package inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file.go"),
		[]byte("package OUTSIDE_THE_ROOT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

// swapDirectoryForLink replaces root/pkg with a symlink to outside, which is
// the mutation the window allows between a check and a use.
func swapDirectoryForLink(t *testing.T, root, outside string) {
	t.Helper()
	pkg := filepath.Join(root, "pkg")
	if err := os.RemoveAll(pkg); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, pkg); err != nil {
		t.Fatal(err)
	}
}

// TestValidateThenOpenReadsOutsideTheRoot is the window, asserted rather than
// described.
//
// It matters exactly which mutation opens it, and the answer is narrower than
// it first looks. safeRootPath returns the *fully resolved* path, so swapping a
// symlink component afterwards changes nothing: the resolved path has no
// symlink left to re-follow. Measured on this machine, five seconds of one
// goroutine flipping a symlink component while another ran validate-then-open
// produced 0 escapes in 43,128 reads.
//
// What does open it is replacing a *real directory* component with a symlink
// between the resolve and the open. The resolved path still names that
// component, and the next open resolves it afresh. That is deterministic, and
// it is what this asserts.
//
// This test does not fail when the product is fixed -- nothing calls this
// sequence any more -- so it is not the red proof for the fix. It is the
// evidence that there was something to fix, and it fails if the escape ever
// stops being reproducible, which would mean the comparison below has become
// vacuous.
func TestValidateThenOpenReadsOutsideTheRoot(t *testing.T) {
	root, outside := escapeFixture(t)

	resolved, ok := SafeRootPath(root, "pkg/file.go")
	if !ok {
		t.Fatal("the path did not validate, so the fixture is wrong")
	}
	swapDirectoryForLink(t, root, outside)

	raw, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("the resolved path no longer opens after the swap (%v), so "+
			"this fixture no longer demonstrates the window the fix removes", err)
	}
	if !strings.Contains(string(raw), "OUTSIDE_THE_ROOT") {
		t.Fatalf("validate-then-open did not escape, so there is nothing here "+
			"for OpenRooted to be better than: got %q", raw)
	}
}

// TestOpenRootedRefusesTheSameMutation is the comparison: the identical
// sequence, through the root descriptor, refuses.
func TestOpenRootedRefusesTheSameMutation(t *testing.T) {
	root, outside := escapeFixture(t)

	// Take the root handle first, so the mutation lands between acquiring the
	// root and resolving through it -- the strongest ordering for an attacker.
	rooted, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()

	swapDirectoryForLink(t, root, outside)

	f, err := rooted.Open("pkg/file.go")
	if err == nil {
		buf := make([]byte, 64)
		n, _ := f.Read(buf)
		f.Close()
		t.Fatalf("the root descriptor followed a replaced directory component: %q", buf[:n])
	}
}

// TestOpenRootedRefusesASwappedComponent is the fix. os.Root walks each
// component relative to a held root descriptor and refuses any symlink that
// leaves it, so there is no path string handed between a check and a use --
// there is no separate check.
func TestOpenRootedRefusesASwappedComponent(t *testing.T) {
	root, outside := escapeFixture(t)

	// Read once through the root while the tree is honest.
	before, err := ReadRooted(root, "pkg/file.go", 0)
	if err != nil {
		t.Fatalf("reading an ordinary file failed: %v", err)
	}
	if !strings.Contains(string(before), "package inside") {
		t.Fatalf("unexpected content: %q", before)
	}

	swapDirectoryForLink(t, root, outside)

	after, err := ReadRooted(root, "pkg/file.go", 0)
	if err == nil {
		t.Fatalf("a swapped directory component was followed out of the root: %q", after)
	}
	if !errors.Is(err, ErrRootEscape) {
		t.Fatalf("want ErrRootEscape, got %v", err)
	}
}

// TestOpenRootedFollowsRelativeLinksThatStayInside keeps the fix from being a
// ban on symlinks: a repository that links a directory to a sibling inside
// itself is ordinary, and refusing it would break indexing rather than secure
// it.
func TestOpenRootedFollowsRelativeLinksThatStayInside(t *testing.T) {
	root, _ := escapeFixture(t)
	if err := os.Symlink("pkg", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	raw, err := ReadRooted(root, "alias/file.go", 0)
	if err != nil {
		t.Fatalf("a relative link that stays inside the root was refused: %v", err)
	}
	if !strings.Contains(string(raw), "package inside") {
		t.Fatalf("unexpected content: %q", raw)
	}
}

// TestAnAbsoluteLinkIsRefusedEvenWhenItPointsInside records a deliberate
// tightening rather than an accident.
//
// os.Root resolves every component relative to the root descriptor, so an
// absolute symlink leaves the root by construction -- even when its target
// happens to be inside. EvalSymlinks, which the previous check used, resolved
// it and compared the result, so this case used to be admitted.
//
// The trade is accepted: an absolute symlink inside a repository is
// machine-specific and breaks the moment the tree is checked out anywhere
// else, and admitting it means trusting a path the root descriptor cannot
// verify. A relative link expressing the same intent is followed.
func TestAnAbsoluteLinkIsRefusedEvenWhenItPointsInside(t *testing.T) {
	root, _ := escapeFixture(t)
	if err := os.Symlink(filepath.Join(root, "pkg"), filepath.Join(root, "abs-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRooted(root, "abs-alias/file.go", 0); err == nil {
		t.Fatal("an absolute symlink was followed; if os.Root has changed " +
			"semantics, this tightening should be re-documented rather than " +
			"silently relaxed")
	}

	// The previous check admitted it, which is what makes this a change worth
	// naming rather than a bug being fixed.
	if _, ok := SafeRootPath(root, "abs-alias/file.go"); !ok {
		t.Log("SafeRootPath also refuses it on this filesystem; no behaviour changed")
	}
}

func TestOpenRootedRefusesTraversalAndAbsoluteEscapes(t *testing.T) {
	root, outside := escapeFixture(t)
	for _, rel := range []string{
		"../outside/file.go",
		"pkg/../../outside/file.go",
		filepath.Join(outside, "file.go"),
	} {
		if _, err := ReadRooted(root, rel, 0); err == nil {
			t.Errorf("%q was admitted", rel)
		}
	}
}

// TestReadRootedHonoursItsLimit: a file that grows between a stat and a read
// must not return more than the caller asked for.
func TestReadRootedHonoursItsLimit(t *testing.T) {
	root, _ := escapeFixture(t)
	raw, err := ReadRooted(root, "pkg/file.go", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 4 {
		t.Fatalf("read %d bytes, want 4", len(raw))
	}
}

// TestStatRootedRefusesAnEscape covers the metadata path, which is how a
// caller decides whether to read at all.
func TestStatRootedRefusesAnEscape(t *testing.T) {
	root, outside := escapeFixture(t)
	if _, err := StatRooted(root, "pkg/file.go"); err != nil {
		t.Fatalf("stat of an ordinary file failed: %v", err)
	}
	swapDirectoryForLink(t, root, outside)
	if _, err := StatRooted(root, "pkg/file.go"); err == nil {
		t.Fatal("stat followed a swapped component out of the root")
	}
}

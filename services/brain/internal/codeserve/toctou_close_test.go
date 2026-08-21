package codeserve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pin narrowed the check/use gap; the read paths now close it, by
// resolving and opening in one operation through a root descriptor.
//
// These are characterisation, not red proofs, and the difference is worth
// stating plainly. A swap that lands *between two requests* was already
// refused before this change, because the handler re-validated on every call:
// reverting the read paths leaves every test in this file green. The window
// only ever existed inside a single request, between that call's resolve and
// its open.
//
// The evidence that the window was real is deterministic and lives one layer
// down, in codecrawl/rooted_open_test.go: validate a path, replace a real
// directory component with a symlink, open the resolved path, and the read
// escapes the root. The measurement that bounds it is there too -- five
// seconds of concurrently flipping a *symlink* component produced 0 escapes in
// 43,128 reads, because safeRootPath returns a fully resolved path with no
// symlink left to re-follow.
//
// What these pin is that the verbs behave correctly through the new path:
// escapes refused, ordinary reads and relative in-root links unaffected. That
// is worth having even though it would also have passed before.

func escapeWorkspace(t *testing.T) (root, outside string) {
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
		[]byte("package inside\n\nfunc Inside() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file.go"),
		[]byte("package secrets\n\nconst OUTSIDE_THE_ROOT = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func swapComponent(t *testing.T, root, outside string) {
	t.Helper()
	pkg := filepath.Join(root, "pkg")
	if err := os.RemoveAll(pkg); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, pkg); err != nil {
		t.Fatal(err)
	}
}

func TestCodeReadRefusesASwappedDirectoryComponent(t *testing.T) {
	root, outside := escapeWorkspace(t)
	ctx := WithRootPin(context.Background(), root)

	// The file is readable while the tree is honest, so a refusal after the
	// swap is the swap being refused rather than the fixture not working.
	first := Handle(ctx, Request{
		"verb": "code_read", "root": root, "path": "pkg/file.go",
		"allow_ignored": true, "allow_unindexed": true,
	})
	if first["ok"] != true {
		t.Fatalf("reading an ordinary file failed: %v", first)
	}

	swapComponent(t, root, outside)

	second := Handle(ctx, Request{
		"verb": "code_read", "root": root, "path": "pkg/file.go",
		"allow_ignored": true, "allow_unindexed": true,
	})
	body, _ := second["content"].(string)
	if strings.Contains(body, "OUTSIDE_THE_ROOT") {
		t.Fatal("code_read followed a swapped directory component out of the " +
			"workspace root and returned a file the pin never admitted")
	}
	if second["ok"] == true {
		t.Fatalf("the read succeeded against a replaced component: %v", second)
	}
}

// TestCodeReadStillReadsThroughARelativeLink keeps the close from being a ban
// on symlinks. A repository that links a directory to a sibling inside itself
// is ordinary.
func TestCodeReadStillReadsThroughARelativeLink(t *testing.T) {
	root, _ := escapeWorkspace(t)
	if err := os.Symlink("pkg", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	ctx := WithRootPin(context.Background(), root)

	resp := Handle(ctx, Request{
		"verb": "code_read", "root": root, "path": "alias/file.go",
		"allow_ignored": true, "allow_unindexed": true,
	})
	if resp["ok"] != true {
		t.Fatalf("a relative link inside the root was refused: %v", resp)
	}
	if body, _ := resp["content"].(string); !strings.Contains(body, "package inside") {
		t.Fatalf("unexpected content: %q", body)
	}
}

// TestFindRelevantPreviewsRefuseASwappedComponent covers the other read path:
// context-pack sources and hit previews, which read one file per hit and were
// the same validate-then-open sequence.
func TestFindRelevantPreviewsRefuseASwappedComponent(t *testing.T) {
	root, outside := escapeWorkspace(t)
	ctx := WithRootPin(context.Background(), root)

	if resp := Handle(ctx, Request{"verb": "code_index", "root": root}); resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	swapComponent(t, root, outside)

	resp := Handle(ctx, Request{
		"verb": "code_find_relevant", "root": root, "q": "Inside", "preview": true,
	})
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "OUTSIDE_THE_ROOT") {
		t.Fatalf("a preview read a file outside the root after a component "+
			"swap:\n%s", raw)
	}
}

package codecrawl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndDeltaRefresh(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	b := filepath.Join(root, "b.go")
	if err := os.WriteFile(a, []byte("package a\nfunc PersistAlpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("package b\nfunc PersistBeta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gobPath := filepath.Join(t.TempDir(), "code-index.gob")
	idx1, st1, wrote1, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote1 || st1.FilesIndexed != 2 {
		t.Fatalf("first index wrote=%v st=%+v", wrote1, st1)
	}
	if hits := idx1.Search("PersistAlpha", 3); len(hits) == 0 {
		t.Fatal("missing PersistAlpha after first index")
	}
	// No change → delta should report unchanged; search still works.
	idx2, st2, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Unchanged < 1 {
		t.Fatalf("expected unchanged reuse on second open: %+v", st2)
	}
	if hits := idx2.Search("PersistAlpha", 3); len(hits) == 0 {
		t.Fatal("lost PersistAlpha after refresh")
	}
	// Mutate b only.
	if err := os.WriteFile(b, []byte("package b\nfunc PersistBetaV2() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx3, st3, wrote3, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if st3.Changed < 1 || st3.Unchanged < 1 {
		t.Fatalf("want mixed delta: %+v", st3)
	}
	if !wrote3 {
		t.Fatal("expected gob rewrite after change")
	}
	if hits := idx3.Search("PersistBetaV2", 3); len(hits) == 0 {
		t.Fatal("delta did not index change")
	}
	if hits := idx3.Search("PersistAlpha", 3); len(hits) == 0 {
		t.Fatal("reuse lost Alpha after delta")
	}
}

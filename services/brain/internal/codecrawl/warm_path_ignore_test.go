package codecrawl

import (
	"os"
	"path/filepath"
	"testing"
)

// The warm fast path was structurally unreachable in any repository with an
// ignore rule.
//
// stAllStampsMatch walked with the hardcoded skipDir set while every other
// walk in this package loads repoignore. Its census was therefore a strict
// superset of the indexed set, so `len(live) != len(prev.fileStamps)` was
// permanently true and the path was never taken -- including in this
// repository, which has a .pytest_cache/ entry. The README claim was corrected
// in the hardening branch; the code was not.
//
// Telling the warm path from the delta walk needs care. An unchanged
// repository reaches the delta walk too, and it also reports BytesRead == 0
// with every file skipped by stamp -- so that signature alone does not
// distinguish them, and a first draft of these tests passed against the
// unfixed code because of it. Only the warm path returns a zero Duration.

// ignoredRepo builds a repository holding indexable sources plus content that
// an ignore rule excludes, which is what makes the two censuses disagree.
func ignoredRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n\nfunc main() {}\n")
	write("lib/util.go", "package lib\n\nfunc Helper() int { return 1 }\n")
	write(".gitignore", "generated/\nvendored.go\n")
	// Ignored, but carrying an indexable extension: these are exactly the
	// files the two walks disagreed about.
	write("generated/pb.go", "package generated\n\nfunc Gen() {}\n")
	write("vendored.go", "package main\n\nfunc Vendored() {}\n")
	return root
}

func TestWarmPathIsReachableInARepositoryWithIgnoreRules(t *testing.T) {
	root := ignoredRepo(t)
	gobPath := filepath.Join(t.TempDir(), "index.gob")

	// First open: a cold crawl that writes the index.
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatalf("cold open: %v", err)
	}

	// Second open with nothing changed must take the warm path.
	idx, stats, wrote, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatalf("warm open: %v", err)
	}
	if idx == nil {
		t.Fatal("no index returned")
	}
	if wrote {
		t.Fatal("an unchanged repository rewrote its index")
	}
	if stats.BytesRead != 0 {
		t.Fatalf("the warm path read %d bytes: the stamp census disagreed with "+
			"the index, so the delta walk ran instead", stats.BytesRead)
	}
	if stats.SkippedByStamp != len(idx.files) || stats.SkippedByStamp == 0 {
		t.Fatalf("skipped %d of %d files by stamp: nothing was reused",
			stats.SkippedByStamp, len(idx.files))
	}
	// The delta walk reports the time it took; the warm path reports zero
	// because it did not run one. This is what separates the two.
	if stats.Duration != 0 {
		t.Fatalf("the delta walk ran for %s on an unchanged repository: the "+
			"stamp census disagreed with the index, so the warm path was "+
			"skipped even though nothing had changed", stats.Duration)
	}
}

// TestWarmPathCensusExcludesIgnoredFiles states the cause directly: the two
// sides of the comparison must count the same files.
func TestWarmPathCensusExcludesIgnoredFiles(t *testing.T) {
	root := ignoredRepo(t)
	gobPath := filepath.Join(t.TempDir(), "index.gob")

	idx, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, ignored := range []string{"generated/pb.go", "vendored.go"} {
		if _, ok := idx.fileStamps[ignored]; ok {
			t.Fatalf("%s is ignored but was indexed; the fixture proves nothing", ignored)
		}
	}
	if !stAllStampsMatch(root, idx) {
		t.Fatal("the stamp census does not match an index built from the same " +
			"tree moments earlier: it is counting files the indexer excluded")
	}
}

// TestWarmPathStillDetectsARealChange keeps the fix from turning the census
// into something that always matches.
func TestWarmPathStillDetectsARealChange(t *testing.T) {
	root := ignoredRepo(t)
	gobPath := filepath.Join(t.TempDir(), "index.gob")
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "lib", "util.go"),
		[]byte("package lib\n\nfunc Helper() int { return 2 }\nfunc Extra() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stats, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BytesRead == 0 {
		t.Fatal("an edited file was served from the warm path without being read")
	}

	// A new file that the ignore rules exclude must not defeat the warm path.
	if err := os.WriteFile(filepath.Join(root, "generated", "more.go"),
		[]byte("package generated\n\nfunc More() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}
	_, stats, _, _, err = OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BytesRead != 0 {
		t.Fatalf("adding an ignored file forced a re-read of %d bytes", stats.BytesRead)
	}
}

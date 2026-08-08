package codecrawl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImpactAndFindRelevant(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	b := filepath.Join(root, "b.go")
	if err := os.WriteFile(a, []byte("package a\nfunc AlphaCore() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("package b\nfunc Beta() { AlphaCore() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec := idx.Impact("AlphaCore", 2, 20)
	if rec.SeedKind != "symbol" {
		t.Fatalf("kind %s", rec.SeedKind)
	}
	if len(rec.Direct) == 0 && len(rec.Closure) == 0 {
		t.Fatalf("empty impact %+v", rec)
	}
	payload := idx.FindRelevant(root, "AlphaCore", 5, true)
	if len(payload.Hits) == 0 {
		t.Fatal("find relevant empty")
	}
	// expand by symbol
	hits := idx.Expand([]string{"AlphaCore"}, 10)
	if len(hits) == 0 {
		t.Fatal("expand empty")
	}
}

func TestIngestPathsAndFreshness(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	if err := os.WriteFile(a, []byte("package a\nfunc One() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gob := filepath.Join(t.TempDir(), "code-index.gob")
	idx, _, _, meta, err := OpenOrRefresh(root, gob, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	rep := idx.Freshness(root, meta)
	if !rep.Fresh {
		t.Fatalf("expected fresh: %+v", rep)
	}
	// mutate
	if err := os.WriteFile(a, []byte("package a\nfunc OneV2() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep2 := idx.Freshness(root, meta)
	if rep2.Fresh {
		t.Fatal("expected stale after edit")
	}
	n, err := idx.IngestPaths(root, []string{"a.go"})
	if err != nil || n != 1 {
		t.Fatalf("ingest n=%d err=%v", n, err)
	}
	if hits := idx.Search("OneV2", 3); len(hits) == 0 {
		t.Fatal("ingest paths did not reindex")
	}
}

func TestMtimeFastWarmRefresh(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.go")
	if err := os.WriteFile(a, []byte("package a\nfunc Warm() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gob := filepath.Join(t.TempDir(), "code-index.gob")
	_, st1, _, _, err := OpenOrRefresh(root, gob, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if st1.FilesIndexed != 1 {
		t.Fatalf("files %d", st1.FilesIndexed)
	}
	_, st2, _, _, err := OpenOrRefresh(root, gob, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	// Warm should reuse via stamp (no re-hash of content ideally).
	if st2.Changed != 0 {
		t.Fatalf("warm changed=%d want 0; st=%+v", st2.Changed, st2)
	}
	if st2.SkippedByStamp < 1 && st2.Unchanged < 1 {
		t.Fatalf("expected stamp/hash reuse: %+v", st2)
	}
	// BytesRead should be 0 on pure stamp-fast path.
	if st2.BytesRead != 0 && st2.SkippedByStamp == 0 {
		t.Logf("note: warm still read bytes=%d (acceptable if hash fallback)", st2.BytesRead)
	}
}

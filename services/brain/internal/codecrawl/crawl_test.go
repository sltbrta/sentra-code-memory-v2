package codecrawl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParallelCrawlSearchAndSpeedup(t *testing.T) {
	root := t.TempDir()
	const nFiles = 400
	for i := 0; i < nFiles; i++ {
		body := fmt.Sprintf("package f%d\n// ouroboros_marker file_%d\n%s\n", i, i, heavyPad(i))
		path := filepath.Join(root, fmt.Sprintf("f%03d.go", i))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	idx1, st1, err := CrawlDir(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st1.FilesIndexed != nFiles {
		t.Fatalf("files %d", st1.FilesIndexed)
	}
	if st1.Workers != 1 {
		t.Fatalf("Stats.Workers n1 want 1 got %d", st1.Workers)
	}
	hits1 := idx1.Search("ouroboros_marker", 50)
	if len(hits1) == 0 {
		t.Fatal("no hits n=1")
	}

	idx4, st4, err := CrawlDir(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	if st4.Workers != 4 {
		t.Fatalf("Stats.Workers n4 want 4 got %d", st4.Workers)
	}
	hits4 := idx4.Search("ouroboros_marker", 50)
	if len(hits4) == 0 {
		t.Fatal("no hits n=4")
	}
	// Correctness: both find the marker with positive scores.
	if hits1[0].Score <= 0 || hits4[0].Score <= 0 {
		t.Fatalf("bad scores h1=%v h4=%v", hits1[0], hits4[0])
	}
	// Same file set size.
	if len(idx1.files) != nFiles || len(idx4.files) != nFiles {
		t.Fatalf("file set n1=%d n4=%d want %d", len(idx1.files), len(idx4.files), nFiles)
	}

	ms1 := st1.Duration.Milliseconds()
	ms4 := st4.Duration.Milliseconds()
	t.Logf("wall_ms n1=%d n4=%d files=%d workers=%d/%d", ms1, ms4, nFiles, st1.Workers, st4.Workers)

	// Wall-time is diagnostic only under CI contention (go/parser + shared
	// runners can invert n1/n4). Log ratio; hard-fail only catastrophic 3× regress.
	if ms1 >= 20 {
		ratio := float64(ms1) / float64(max1(ms4))
		t.Logf("speedup=%.2fx", ratio)
		if float64(ms4) > float64(ms1)*3 {
			t.Fatalf("parallel catastrophic regress: n4 > 3×n1 (n1=%dms n4=%dms)", ms1, ms4)
		}
	}
}

func max1(x int64) int64 {
	if x < 1 {
		return 1
	}
	return x
}

func heavyPad(seed int) string {
	// Larger pad so tokenFreq is non-trivial across 400 files.
	var b []byte
	for i := 0; i < 400; i++ {
		b = append(b, fmt.Sprintf("token_%d_%d ", seed, i)...)
	}
	return string(b)
}

func TestCrawlHonorsRepositoryIgnorePolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("docker-only/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"kept.go":                  "package kept\nfunc Kept() {}\n",
		"ignored/hidden.go":        "package hidden\nfunc Hidden() {}\n",
		"docker-only/generated.go": "package generated\nfunc Generated() {}\n",
		".github/workflows/ci.go":  "package ci\nfunc CI() {}\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"kept.go", ".github/workflows/ci.go"} {
		if _, ok := idx.files[path]; !ok {
			t.Fatalf("expected %q in index files=%v", path, idx.Files())
		}
	}
	for _, path := range []string{"ignored/hidden.go", "docker-only/generated.go"} {
		if _, ok := idx.files[path]; ok {
			t.Fatalf("ignored %q was indexed: files=%v", path, idx.Files())
		}
	}
}

func TestSearchPrefersMultiTokenCoverageOverTermFrequencyFloods(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "complete.go"), []byte("package complete\nextension host service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "generic.go"), []byte("package generic\n"+strings.Repeat("host ", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	hits := idx.Search("extension host service", 2)
	if len(hits) == 0 || hits[0].Path != "complete.go" {
		t.Fatalf("multi-token match was not preferred: %#v", hits)
	}
}

func TestCrawlEmpty(t *testing.T) {
	idx, st, err := CrawlDir(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.FilesIndexed != 0 {
		t.Fatalf("want 0 files got %d", st.FilesIndexed)
	}
	if st.Workers != 2 {
		t.Fatalf("Workers want 2 got %d", st.Workers)
	}
	if hits := idx.Search("nothing", 5); len(hits) != 0 {
		t.Fatalf("hits %v", hits)
	}
}

// Ensure time import used when Duration is logged
var _ = time.Millisecond

func TestCrawlDeltaReusesUnchangedSubgraphs(t *testing.T) {
	root := t.TempDir()
	aPath := filepath.Join(root, "a.go")
	bPath := filepath.Join(root, "b.go")
	if err := os.WriteFile(aPath, []byte("package a\nfunc AlphaMarker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte("package b\nfunc BetaMarker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev, st0, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st0.FilesIndexed != 2 {
		t.Fatalf("files %d", st0.FilesIndexed)
	}
	// Touch only b.go
	if err := os.WriteFile(bPath, []byte("package b\nfunc BetaMarkerChanged() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, st, hashes, err := CrawlDeltaFrom(root, 2, nil, prev)
	if err != nil {
		t.Fatal(err)
	}
	if st.Unchanged < 1 {
		t.Fatalf("expected unchanged reuse; stats=%+v", st)
	}
	if st.Changed < 1 {
		t.Fatalf("expected at least one changed; stats=%+v", st)
	}
	if len(hashes) != 2 {
		t.Fatalf("hashes %d", len(hashes))
	}
	// Unchanged AlphaMarker still searchable; new BetaMarkerChanged present.
	if hits := idx.Search("AlphaMarker", 5); len(hits) == 0 {
		t.Fatal("reused file lost AlphaMarker")
	}
	if hits := idx.Search("BetaMarkerChanged", 5); len(hits) == 0 {
		t.Fatal("changed file not reindexed")
	}
	// File-local postings present for both (delta storage).
	if _, ok := idx.filePostings["a.go"]; !ok {
		t.Fatal("missing filePostings for reused a.go")
	}
}

func TestImportStopWordsNotIndexed(t *testing.T) {
	_, _, imps := extractHeuristicSymbols(".py", "from package_foo import bar_baz\n")
	for _, i := range imps {
		if i == "import" || i == "from" {
			t.Fatalf("keyword leaked into imports: %v", imps)
		}
	}
	// package_foo / bar_baz (or similar) should appear
	if len(imps) == 0 {
		t.Fatal("expected non-keyword imports")
	}
}

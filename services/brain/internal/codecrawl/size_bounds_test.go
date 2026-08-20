package codecrawl_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
)

// Nothing bounded a file read. os.ReadFile plus string(raw) held two full
// copies of every file in memory, across up to 256 worker goroutines, and the
// symbol extractor allocated a line slice over a third. One multi-gigabyte
// generated bundle was enough to OOM the indexer, the warm serve loop, the HTTP
// server and the watch daemon.

func TestCrawlSkipsFilesOverTheSizeBound(t *testing.T) {
	root := t.TempDir()
	small := filepath.Join(root, "small.go")
	if err := os.WriteFile(small, []byte("package a\n\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Just over the bound; content is irrelevant.
	huge := filepath.Join(root, "huge.go")
	if err := os.WriteFile(huge,
		[]byte("package a\n"+strings.Repeat("// filler\n", (codecrawl.MaxIndexableFileBytes/10)+64)),
		0o644); err != nil {
		t.Fatal(err)
	}

	idx, stats, err := codecrawl.CrawlDir(root, 2)
	if err != nil {
		t.Fatalf("CrawlDir: %v", err)
	}
	if !idx.HasFile("small.go") {
		t.Fatal("an ordinary source file must still be indexed")
	}
	if idx.HasFile("huge.go") {
		t.Fatalf("a file over %d bytes was indexed (stats=%+v)", codecrawl.MaxIndexableFileBytes, stats)
	}
}

func TestSizeBoundIsGenerousEnoughForRealSource(t *testing.T) {
	// The largest file in this repository is far below the bound; a limit that
	// excluded real source would be a retrieval regression, not a fix.
	if codecrawl.MaxIndexableFileBytes < 1<<20 {
		t.Fatalf("MaxIndexableFileBytes = %d, too small for ordinary source files",
			codecrawl.MaxIndexableFileBytes)
	}
}

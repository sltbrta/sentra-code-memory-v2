package continual

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

func TestWatchDocsPrunesDeletedFile(t *testing.T) {
	docsDir := t.TempDir()
	brainDir := t.TempDir()
	// Two files.
	if err := os.WriteFile(filepath.Join(docsDir, "a.md"), []byte("alpha quantum lattice"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "b.md"), []byte("beta pasta recipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := hosted.OpenResidual("wprune", hosted.SubstrateConfig{
		Dir: brainDir, Chunks: hosted.SubstrateChunksFS, Queue: hosted.SubstrateQueueSQLite,
		Cortex: hosted.SubstrateCortexFS, Dense: hosted.SubstrateDenseNone,
		Embed: hosted.SubstrateAPINone, LLM: hosted.SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// First cycle: both docs.
	if err := WatchDocs(ctx, DocWatchOptions{
		Client: c, DocsPath: docsDir, Interval: 50 * time.Millisecond,
		Debounce: 10 * time.Millisecond, MaxCycles: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Delete b.md — stamp must flip (count-aware pathStamp).
	if err := os.Remove(filepath.Join(docsDir, "b.md")); err != nil {
		t.Fatal(err)
	}
	// Touch a so some platforms also bump dir mtime.
	_ = os.WriteFile(filepath.Join(docsDir, "a.md"), []byte("alpha quantum lattice revised"), 0o644)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel2()
	if err := WatchDocs(ctx2, DocWatchOptions{
		Client: c, DocsPath: docsDir, Interval: 50 * time.Millisecond,
		Debounce: 10 * time.Millisecond, MaxCycles: 1,
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := c.Store().LexicalSearch(context.Background(), "wprune", "pasta", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.DSID == "b" {
			t.Fatalf("deleted b still retrieved: %+v", h)
		}
	}
}

func TestPathStampDetectsDelete(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "x.md")
	p2 := filepath.Join(dir, "y.md")
	if err := os.WriteFile(p1, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, err := pathStamp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p2); err != nil {
		t.Fatal(err)
	}
	s2, err := pathStamp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s1 == s2 {
		t.Fatalf("stamp did not change on delete: %d", s1)
	}
}

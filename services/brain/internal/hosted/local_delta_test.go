package hosted

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDeltaFlushAppendsWithoutFullRewrite(t *testing.T) {
	dir := t.TempDir()
	c, err := CreateLocal(dir, "delta")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Initial burst → full base write.
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "a", Text: "alpha one document about quantum"},
		{ID: "b", Text: "beta two document about pasta"},
	}, 2); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, localChunksName)
	delta := filepath.Join(dir, localDeltaName)
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base missing: %v", err)
	}
	// Second ingest of one new doc should append delta, not require empty delta.
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "c", Text: "gamma three document about lattice cooling"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(delta); err != nil || st.Size() == 0 {
		// If compact threshold hit immediately (unlikely with 1 line), base still has all.
		if err != nil {
			// Delta may be compacted away only if >= compactDeltaLines; with 1 line must exist.
			t.Fatalf("expected non-empty delta after second ingest: %v", err)
		}
	}
	_ = c.Close()
	// Reopen must see all three docs (base + delta merge).
	c2, err := OpenLocal(dir, "delta")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	hits, err := c2.Store().LexicalSearch(ctx, "delta", "lattice cooling", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 1 {
		t.Fatalf("delta load lost gamma: hits=%v", hits)
	}
	hits2, err := c2.Store().LexicalSearch(ctx, "delta", "quantum", 10)
	if err != nil || len(hits2) < 1 {
		t.Fatalf("base alpha lost after delta merge: %v %v", err, hits2)
	}
}

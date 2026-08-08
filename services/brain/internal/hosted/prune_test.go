package hosted

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPruneMissingDocumentsRemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenResidual("prune1", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseNone,
		Embed: SubstrateAPINone, LLM: SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "keep", Text: "document that remains in the source tree"},
		{ID: "gone", Text: "document that will be deleted from source"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	// Simulate source tree with only "keep".
	live := map[string]struct{}{"keep": {}}
	n, err := c.PruneMissingDocuments(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("expected prune count >=1, got %d", n)
	}
	// Lexical should not surface gone.
	hits, err := c.Store().LexicalSearch(ctx, "prune1", "deleted from source", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.DSID == "gone" {
			t.Fatalf("gone doc still searchable: %+v", h)
		}
	}
	hits2, err := c.Store().LexicalSearch(ctx, "prune1", "remains in the source", 10)
	if err != nil || len(hits2) < 1 {
		t.Fatalf("keep doc lost: %v %v", err, hits2)
	}
	// Reopen durable store — delete must survive.
	_ = c.Close()
	c2, err := OpenResidual("prune1", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	ids := c2.store.(*durableStore).DocumentIDs("prune1")
	for _, id := range ids {
		if id == "gone" {
			t.Fatalf("gone still on disk after reopen: %v", ids)
		}
	}
	// chunks.jsonl should exist
	if _, err := os.Stat(filepath.Join(dir, "chunks.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestContinualDeltaEmptyDocsOk(t *testing.T) {
	c := OpenMemory("empty-delta")
	defer c.Close()
	res, err := c.ContinualDeltaLocal(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "delta" {
		t.Fatalf("mode=%s", res.Mode)
	}
}

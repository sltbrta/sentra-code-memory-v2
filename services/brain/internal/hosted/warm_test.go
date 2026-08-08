package hosted

import (
	"context"
	"testing"
)

func TestWarmSidecarsMemory(t *testing.T) {
	c := OpenMemory("warm-test")
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := c.WarmSidecars(ctx, "warm-test", []SidecarWrite{
		{DocumentID: "d1", Kind: "d2q", Text: "What is MedThink RPO?"},
		{DocumentID: "d1", Kind: "context_header", Text: "MedThink policy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("n=%d", n)
	}
}

func TestQueryCacheHit(t *testing.T) {
	c := OpenMemory("cache-test")
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	_ = c.UpsertChunks(ctx, "cache-test", []ChunkWrite{
		{DocumentID: "d1", ChunkID: "c1", Text: "Alpha Redis sessions for product cache test unique token ZZYXCACHE99"},
	})
	ps1, d1, err := c.Retrieve(ctx, "ZZYXCACHE99 Redis sessions", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps1) == 0 {
		t.Fatal("empty first retrieve")
	}
	ps2, d2, err := c.Retrieve(ctx, "ZZYXCACHE99 Redis sessions", 4)
	if err != nil {
		t.Fatal(err)
	}
	if d2["cache_hit"] != true {
		t.Fatalf("expected cache hit diag=%v first=%v", d2, d1)
	}
	if len(ps2) == 0 {
		t.Fatal("empty cached")
	}
	// Mutating returned diag must not poison the cached entry.
	d2["poison"] = true
	_, d3, err := c.Retrieve(ctx, "ZZYXCACHE99 Redis sessions", 4)
	if err != nil {
		t.Fatal(err)
	}
	if d3["poison"] == true {
		t.Fatal("cached diag was shared/mutable")
	}
}

func TestQueryCacheInvalidatedOnUpsert(t *testing.T) {
	c := OpenMemory("cache-inv")
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	_ = c.UpsertChunks(ctx, "cache-inv", []ChunkWrite{
		{DocumentID: "d1", ChunkID: "c1", Text: "token BEFORE_MUT unique"},
	})
	_, _, err := c.Retrieve(ctx, "BEFORE_MUT unique", 4)
	if err != nil {
		t.Fatal(err)
	}
	// Second retrieve hits cache.
	_, dHit, err := c.Retrieve(ctx, "BEFORE_MUT unique", 4)
	if err != nil {
		t.Fatal(err)
	}
	if dHit["cache_hit"] != true {
		t.Fatalf("precondition: want cache hit, got %#v", dHit)
	}
	// Mutation must drop cache so new chunk text is visible.
	_ = c.UpsertChunks(ctx, "cache-inv", []ChunkWrite{
		{DocumentID: "d2", ChunkID: "c2", Text: "token AFTER_MUT unique"},
	})
	ps, d, err := c.Retrieve(ctx, "AFTER_MUT unique", 4)
	if err != nil {
		t.Fatal(err)
	}
	if d["cache_hit"] == true {
		t.Fatal("expected cache miss after upsert")
	}
	if len(ps) == 0 {
		t.Fatal("expected hit on new chunk")
	}
}

func TestWithBrainID(t *testing.T) {
	c := OpenMemory("brain-a")
	c2 := c.WithBrainID("brain-b")
	if c.Config().BrainID != "brain-a" {
		t.Fatalf("original mutated: %s", c.Config().BrainID)
	}
	if c2.Config().BrainID != "brain-b" {
		t.Fatalf("override %s", c2.Config().BrainID)
	}
}

func TestSidecarBoostSearch(t *testing.T) {
	c := OpenMemory("side-boost")
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	_ = c.UpsertChunks(ctx, "side-boost", []ChunkWrite{
		// Chunk text has weak overlap; sidecar carries the distinctive term.
		{DocumentID: "d1", ChunkID: "c1", Text: "policy document body without the rare marker"},
	})
	// Without sidecar, rare marker may not rank — warm then search.
	_, err := c.WarmSidecars(ctx, "side-boost", []SidecarWrite{
		{DocumentID: "d1", Kind: "d2q", Text: "What is SIDECAR_MARKER_ZZYX?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := c.store.LexicalSearch(ctx, "side-boost", "SIDECAR_MARKER_ZZYX", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected sidecar-boosted hit")
	}
}

package hosted

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recordingStore wraps MemoryChunkStore and records shard-facing UpsertChunks calls.
type recordingStore struct {
	*MemoryChunkStore
	mu    sync.Mutex
	calls []int // batch sizes
}

func (r *recordingStore) UpsertChunks(ctx context.Context, brainID string, chunks []ChunkWrite) error {
	r.mu.Lock()
	r.calls = append(r.calls, len(chunks))
	r.mu.Unlock()
	return r.MemoryChunkStore.UpsertChunks(ctx, brainID, chunks)
}

func TestBurstUpsertShardingAndReceipts(t *testing.T) {
	mem := NewMemoryChunkStore()
	rec := &recordingStore{MemoryChunkStore: mem}
	c := &Client{
		cfg:          Config{BrainID: "t-brain", LexicalLimit: 20, TopK: 6, MaxPassageChars: 2000},
		store:        rec,
		productOwned: true,
	}
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}

	const n = 12
	chunks := make([]ChunkWrite, n)
	for i := 0; i < n; i++ {
		chunks[i] = ChunkWrite{
			DocumentID: fmt.Sprintf("doc-%d", i%3),
			ChunkID:    fmt.Sprintf("chunk-%d", i),
			Text:       fmt.Sprintf("ouroboros product chunk alpha_%d beta token", i),
			SourceURI:  fmt.Sprintf("mem://doc-%d#%d", i%3, i),
		}
	}

	res, err := c.BurstUpsert(ctx, "t-brain", chunks, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ProductOwned {
		t.Fatal("want product_owned")
	}
	if res.Mode != "burst" {
		t.Fatalf("mode %q", res.Mode)
	}
	if res.Workers != 4 {
		t.Fatalf("workers %d", res.Workers)
	}
	if res.Ingested != n || res.Upserted != n {
		t.Fatalf("ingested=%d upserted=%d want %d", res.Ingested, res.Upserted, n)
	}
	if len(res.Receipts) != n {
		t.Fatalf("receipts %d", len(res.Receipts))
	}
	// Every receipt OK and shard in [0, workers).
	shardSeen := map[int]int{}
	for i, r := range res.Receipts {
		if !r.OK {
			t.Fatalf("receipt %d not ok: %s", i, r.Error)
		}
		if r.ChunkID != chunks[i].ChunkID {
			t.Fatalf("receipt order broken at %d", i)
		}
		if r.Shard < 0 || r.Shard >= 4 {
			t.Fatalf("bad shard %d", r.Shard)
		}
		shardSeen[r.Shard]++
	}
	if len(shardSeen) != 4 {
		t.Fatalf("expected 4 shards used, got %v", shardSeen)
	}
	// Upsert was sharded (multiple store calls).
	rec.mu.Lock()
	calls := len(rec.calls)
	rec.mu.Unlock()
	if calls != 4 {
		t.Fatalf("store UpsertChunks calls=%d want 4", calls)
	}
	if mem.ChunkCount("t-brain") != n {
		t.Fatalf("store count %d", mem.ChunkCount("t-brain"))
	}
}

func TestOpenMemoryCreateBurstRetrieve(t *testing.T) {
	c := OpenMemory("mem-brain")
	if c == nil {
		t.Fatal("nil client")
	}
	if !c.ProductOwned() {
		t.Fatal("want product owned")
	}
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}

	chunks := []ChunkWrite{
		{
			DocumentID: "d1",
			ChunkID:    "c1",
			Text:       "The quantum flux capacitor enables time travel experiments.",
			SourceURI:  "mem://d1#0",
		},
		{
			DocumentID: "d1",
			ChunkID:    "c2",
			Text:       "Safety protocols for flux experiments require dual containment.",
			SourceURI:  "mem://d1#1",
		},
		{
			DocumentID: "d2",
			ChunkID:    "c3",
			Text:       "Unrelated gardening tips for tomatoes and basil.",
			SourceURI:  "mem://d2#0",
		},
	}
	res, err := c.BurstUpsert(ctx, "", chunks, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ingested != 3 {
		t.Fatalf("ingested %d", res.Ingested)
	}
	if res.BrainID != "mem-brain" {
		t.Fatalf("brain %s", res.BrainID)
	}

	// Without dense backend, UpsertDense fails closed (no silent no-op).
	if err := c.UpsertDense(ctx, []DensePoint{{
		ID:     "c1",
		Vector: []float32{0.1, 0.2},
		Payload: map[string]any{
			"chunk_id": "c1", "dsid": "d1", "brain_id": "mem-brain",
		},
	}}); err == nil {
		t.Fatal("UpsertDense expected error when no dense backend bound")
	}

	passages, diag, err := c.Retrieve(ctx, "flux capacitor time travel", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 {
		t.Fatal("no passages")
	}
	if diag["product_owned"] != true {
		t.Fatalf("diag product_owned: %v", diag)
	}
	// Top hit should be the flux doc, not gardening.
	joined := strings.ToLower(passages[0].Text)
	if !strings.Contains(joined, "flux") && !strings.Contains(joined, "quantum") {
		t.Fatalf("unexpected top passage: %q", passages[0].Text)
	}
	if passages[0].DocumentID != "d1" {
		t.Fatalf("want d1 got %s", passages[0].DocumentID)
	}
}

func TestUpsertChunksValidation(t *testing.T) {
	c := OpenMemory("v")
	ctx := context.Background()
	err := c.UpsertChunks(ctx, "v", []ChunkWrite{{ChunkID: "x", Text: "no dsid"}})
	if err == nil {
		t.Fatal("want error for missing document_id")
	}
	err = c.UpsertChunks(ctx, "v", nil)
	if err == nil {
		t.Fatal("want error for empty")
	}
}

func TestBurstUpsertWorkersClamped(t *testing.T) {
	c := OpenMemory("w")
	ctx := context.Background()
	chunks := []ChunkWrite{
		{DocumentID: "d", ChunkID: "a", Text: "alpha token one"},
		{DocumentID: "d", ChunkID: "b", Text: "beta token two"},
	}
	res, err := c.BurstUpsert(ctx, "w", chunks, 99)
	if err != nil {
		t.Fatal(err)
	}
	if res.Workers != 2 {
		t.Fatalf("workers clamped want 2 got %d", res.Workers)
	}
}

// Compile-time check: MemoryChunkStore implements ChunkStore.
var _ ChunkStore = (*MemoryChunkStore)(nil)
var _ ChunkStore = (*neonChunkStore)(nil)

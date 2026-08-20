package hosted

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// BurstUpsert computed a "one or more burst shards failed" error and then
// returned nil, because the error only fed a deferred telemetry span. The
// caller saw success, IngestResult.Ingested carried the reduced count that
// nobody compares against len(chunks), and the documents silently never
// entered the corpus -- so retrieval later answered "not found" for data an
// operator watched succeed.

// failingChunkStore fails every write after the first n chunks, standing in for
// a database that goes away mid-ingest.
type failingChunkStore struct {
	ChunkStore
	failAfter int
	// BurstUpsert fans out across shard goroutines, so the counter needs its
	// own synchronisation -- the race detector caught this fixture before it
	// caught anything in production code.
	seen atomic.Int64
}

var errShardFailed = errors.New("shard write failed")

func (f *failingChunkStore) UpsertChunks(ctx context.Context, brainID string, chunks []ChunkWrite) error {
	if f.seen.Add(int64(len(chunks))) > int64(f.failAfter) {
		return errShardFailed
	}
	return f.ChunkStore.UpsertChunks(ctx, brainID, chunks)
}

func TestBurstUpsertReportsShardFailureInsteadOfReturningNil(t *testing.T) {
	inner := NewMemoryChunkStore()
	client := &Client{cfg: Config{BrainID: "brain-1"}, store: &failingChunkStore{ChunkStore: inner, failAfter: 2}}

	chunks := make([]ChunkWrite, 0, 8)
	for i := 0; i < 8; i++ {
		chunks = append(chunks, ChunkWrite{
			ChunkID:    "chunk-" + string(rune('a'+i)),
			DocumentID: "doc-1",
			Text:       "content",
		})
	}

	result, err := client.BurstUpsert(context.Background(), "brain-1", chunks, 2)
	if err == nil {
		t.Fatalf("a failed shard must surface as an error; got ok result %+v", result)
	}
	if !strings.Contains(err.Error(), "shard") {
		t.Fatalf("error should name the shard failure, got %v", err)
	}
	if result.Ingested == len(chunks) {
		t.Fatalf("Ingested should not claim the full batch: %+v", result)
	}
}

func TestBurstUpsertStillSucceedsWhenEveryShardSucceeds(t *testing.T) {
	client := &Client{cfg: Config{BrainID: "brain-1"}, store: NewMemoryChunkStore()}
	chunks := []ChunkWrite{
		{ChunkID: "chunk-a", DocumentID: "doc-1", Text: "alpha"},
		{ChunkID: "chunk-b", DocumentID: "doc-1", Text: "beta"},
	}
	result, err := client.BurstUpsert(context.Background(), "brain-1", chunks, 2)
	if err != nil {
		t.Fatalf("healthy burst must succeed: %v", err)
	}
	if result.Ingested != len(chunks) {
		t.Fatalf("Ingested = %d, want %d", result.Ingested, len(chunks))
	}
}

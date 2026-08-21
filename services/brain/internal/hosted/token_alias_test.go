package hosted

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// memChunk is copied by value into the flush snapshot, and a by-value copy of
// a struct shares its maps -- so every copy of a chunk in the process aliases
// one tokens map. That is safe only because the map is built once and never
// written again. Nothing documented the invariant and nothing checked it, so a
// later `ch.tokens[t]++` would introduce a write against a map the flush
// goroutine may be ranging over, under a different mutex: a fatal error that
// no recover catches.
//
// This is a hostile version of the existing ingest/flush hammer. It reads the
// token maps from the serving path while ingest replaces chunks and the
// gardener flushes, so a future in-place mutation is a race the detector
// reports rather than a crash in production.

func TestTokenMapsAreNeverMutatedAfterConstruction(t *testing.T) {
	store := &durableStore{
		inner: NewMemoryChunkStore(), dir: t.TempDir(), brainID: "brain-1",
		dirty: map[string]struct{}{}, dirtySidecars: map[string]struct{}{},
		forceFullFlush: true, hotDirty: true,
	}
	store.gen.Store("gen-0")
	ctx := context.Background()

	// Seed, so the readers below have something to alias immediately.
	if err := store.UpsertChunks(ctx, "brain-1", []ChunkWrite{{
		ChunkID: "seed", DocumentID: "doc-seed", Text: "alpha beta gamma delta",
	}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: replace chunks, which rebuilds each tokens map.
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = store.UpsertChunks(ctx, "brain-1", []ChunkWrite{{
					ChunkID:    fmt.Sprintf("chunk-%d", worker),
					DocumentID: fmt.Sprintf("doc-%d", worker),
					Text:       fmt.Sprintf("alpha beta gamma round-%d", i),
				}})
			}
		}(w)
	}

	// Readers: the serving path, which reads ch.tokens.
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = store.LexicalSearch(ctx, "brain-1", "alpha beta gamma", 8)
			}
		}()
	}

	// Flusher: the gardener loop, forced onto the full-corpus path so it
	// really ranges the chunk map and its aliased token maps.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			store.mu.Lock()
			store.forceFullFlush = true
			store.mu.Unlock()
			_ = store.flush()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The invariant, stated directly: a token map must be exactly what
	// memTokenFreq produces for the chunk's current text.
	store.inner.mu.RLock()
	defer store.inner.mu.RUnlock()
	for chunkID, chunk := range store.inner.chunks["brain-1"] {
		want := memTokenFreq(chunk.text)
		if len(want) != len(chunk.tokens) {
			t.Fatalf("chunk %s holds %d tokens, its text yields %d: the map has "+
				"been edited in place since construction", chunkID, len(chunk.tokens), len(want))
		}
		for token, n := range want {
			if chunk.tokens[token] != n {
				t.Fatalf("chunk %s token %q is %d, want %d: the map has been "+
					"edited in place since construction",
					chunkID, token, chunk.tokens[token], n)
			}
		}
	}
}

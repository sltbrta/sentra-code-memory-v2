package hosted

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// durableStore.flush read the live chunk and sidecar maps under inner.mu,
// released the lock, and then ranged over them. Those are the same maps
// UpsertChunks writes into -- and it holds inner.mu, a different mutex from
// the one flush holds. The auto-gardener calls flush on a 500ms loop while
// ingest is running, so the two overlap in normal operation.
//
// Go reports "concurrent map iteration and map write" as a fatal error, not a
// panic: no recover catches it and the process dies.

func TestConcurrentIngestAndFlushDoNotRace(t *testing.T) {
	dir := t.TempDir()
	store := &durableStore{
		inner: NewMemoryChunkStore(), dir: dir, brainID: "brain-1",
		dirty: map[string]struct{}{}, dirtySidecars: map[string]struct{}{},
		forceFullFlush: true, hotDirty: true,
	}
	store.gen.Store("gen-0")

	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: ingest, holding inner.mu.
	for w := 0; w < 4; w++ {
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
					ChunkID:    fmt.Sprintf("chunk-%d-%d", worker, i),
					DocumentID: fmt.Sprintf("doc-%d", worker),
					Text:       "alpha beta gamma",
				}})
			}
		}(w)
	}

	// Flusher: the auto-gardener's 500ms loop, compressed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Force the full-corpus write path. Without this most flushes take
			// the "nothing dirty" branch and never iterate the chunk map, so
			// the race window is never entered -- which is exactly why the
			// first version of this test passed against the unfixed code.
			store.mu.Lock()
			store.forceFullFlush = true
			store.mu.Unlock()
			_ = store.flush()
		}
	}()

	time.Sleep(400 * time.Millisecond)
	close(stop)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("ingest and flush deadlocked")
	}

	if err := store.flush(); err != nil {
		t.Fatalf("final flush: %v", err)
	}
}

// TestFlushDoesNotAliasTheLiveChunkMap drives the real flush against the real
// writer.
//
// The first version of this test was worthless and a fresh-eyes review caught
// it: it never constructed a durableStore and never called flush, it
// reimplemented the *fixed* logic inline and asserted that copy did not race.
// Gutting flush() to `return nil` left it green. The ledger cited it as proof
// of a race it could not observe.
//
// The reachable pairing is specific: durableStore.UpsertChunks calls
// d.inner.UpsertChunks BEFORE taking d.mu, so the inner maps are written under
// inner.mu while flush holds only d.mu. Contending on d.mu -- which the other
// test in this file does -- establishes happens-before and hides the window, so
// the writer here goes at the inner store directly, exactly as the production
// path does for the duration of that call.
func TestFlushDoesNotAliasTheLiveChunkMap(t *testing.T) {
	dir := t.TempDir()
	store := &durableStore{
		inner: NewMemoryChunkStore(), dir: dir, brainID: "b",
		dirty: map[string]struct{}{}, dirtySidecars: map[string]struct{}{},
		forceFullFlush: true, hotDirty: true,
	}
	store.gen.Store("gen-0")
	ctx := context.Background()
	if err := store.inner.UpsertChunks(ctx, "b", []ChunkWrite{
		{ChunkID: "c0", DocumentID: "d", Text: "seed"},
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: the inner store, without d.mu, as UpsertChunks does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.inner.UpsertChunks(ctx, "b", []ChunkWrite{
				{ChunkID: fmt.Sprintf("c%d", i), DocumentID: "d", Text: "x"},
			})
		}
	}()

	// Flusher: forced onto the full-corpus branch so it actually iterates.
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

	time.Sleep(500 * time.Millisecond)
	close(stop)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("writer and flusher deadlocked")
	}
}

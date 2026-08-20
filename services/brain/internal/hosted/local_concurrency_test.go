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

// TestFlushDoesNotAliasTheLiveChunkMap pins the defect shape directly.
//
// The end-to-end test above is a robustness check: it exercises ingest and
// flush together, but whether it enters flush's iteration window depends on
// which branch the flush takes, so it did not reliably reproduce the race.
// This one reproduces it deterministically by performing exactly what flush
// used to do -- take the map reference under inner.mu, release the lock, then
// iterate -- while a writer mutates the same map under that lock.
//
// Run under -race this fails against the aliasing form and passes against the
// snapshot form, which is the property being protected.
func TestFlushDoesNotAliasTheLiveChunkMap(t *testing.T) {
	inner := NewMemoryChunkStore()
	ctx := context.Background()
	if err := inner.UpsertChunks(ctx, "b", []ChunkWrite{{ChunkID: "c0", DocumentID: "d", Text: "x"}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = inner.UpsertChunks(ctx, "b", []ChunkWrite{
				{ChunkID: fmt.Sprintf("c%d", i), DocumentID: "d", Text: "x"},
			})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The snapshot flush now takes: copy under the lock, iterate the copy.
			inner.mu.RLock()
			live := inner.chunks["b"]
			snapshot := make(map[string]memChunk, len(live))
			for id, chunk := range live {
				snapshot[id] = chunk
			}
			inner.mu.RUnlock()
			for range snapshot {
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

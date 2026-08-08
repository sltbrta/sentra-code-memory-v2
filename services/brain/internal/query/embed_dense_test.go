package query

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

func TestHybridEmbedDenseFallsBackToBag(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"a.md": "MedThink recovery point objective RPO fifteen minutes",
		"b.md": "picnic weather forecast sunny",
	}
	h := NewHybridEmbedDense(nil, func(gen string) map[string]string {
		if gen == "g1" {
			return bodies
		}
		return nil
	})
	ids := h.Search(context.Background(), "g1", "MedThink RPO", 2)
	if len(ids) == 0 {
		t.Fatal("expected bag fallback hits")
	}
	if ids[0] != "a.md" {
		t.Fatalf("top=%v want a.md", ids)
	}
}

// TestHybridEmbedDenseCacheThreading proves that HybridEmbedDense threads
// generation and tenant into the embedding cache so generation- and
// tenant-scoped cache keys are populated as expected (issue #329).
func TestHybridEmbedDenseCacheThreading(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"a.md": "alpha bravo charlie delta echo",
		"b.md": "foxtrot golf hotel india juliet",
	}
	// Embedder stub: counts calls, returns non-zero vectors.
	calls := int32(0)
	stub := &threadingEmbedder{calls: &calls, dim: 4}
	cache, err := rerank.NewEmbedCache(stub, 60_000_000_000 /* 60s */, 32)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHybridEmbedDense(cache, func(gen string) map[string]string {
		if gen == "gen-A" {
			return bodies
		}
		return nil
	})
	h.Tenant = "tenant-A"

	// First search: cold miss + build store.
	ids1 := h.Search(context.Background(), "gen-A", "alpha bravo", 2)
	if len(ids1) == 0 {
		t.Fatal("expected hits from first search")
	}

	// Second search on the same generation/tenant: hits only — no extra embed calls.
	before := atomic.LoadInt32(&calls)
	ids2 := h.Search(context.Background(), "gen-A", "alpha bravo", 2)
	after := atomic.LoadInt32(&calls)
	if after != before {
		t.Fatalf("expected zero extra inner calls on warm cache; got %d -> %d", before, after)
	}
	if len(ids2) == 0 {
		t.Fatal("expected hits from second search")
	}

	// Stats must reflect at least one miss (initial embed) and one hit
	// (subsequent embed of the same query on warm cache).
	st := h.CacheStats()
	if st.Hits == 0 || st.Misses == 0 {
		t.Fatalf("expected non-zero hits and misses; got %+v", st)
	}

	// CacheStats for a non-cache embedder returns zero.
	plain := NewHybridEmbedDense(stub, nil)
	if got := plain.CacheStats(); got != (rerank.CacheStats{}) {
		t.Fatalf("plain embedder must report zero stats; got %+v", got)
	}
}

// TestHybridEmbedDenseInvalidateGeneration proves the write-invalidation
// path removes cache entries scoped to a specific generation, forcing a
// rebuild on the next search.
func TestHybridEmbedDenseInvalidateGeneration(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"a.md": "lorem ipsum dolor sit amet",
	}
	calls := int32(0)
	stub := &threadingEmbedder{calls: &calls, dim: 4}
	cache, err := rerank.NewEmbedCache(stub, 60_000_000_000, 32)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHybridEmbedDense(cache, func(gen string) map[string]string {
		if gen == "gen-A" {
			return bodies
		}
		return nil
	})
	h.Tenant = "tenant-A"
	// Warm the cache for gen-A.
	_ = h.Search(context.Background(), "gen-A", "lorem ipsum", 1)

	before := atomic.LoadInt32(&calls)
	// Re-warm search on gen-A should be a hit (no new inner call).
	_ = h.Search(context.Background(), "gen-A", "lorem ipsum", 1)
	if got := atomic.LoadInt32(&calls); got != before {
		t.Fatalf("expected warm cache hit on gen-A; calls %d -> %d", before, got)
	}

	// Invalidate gen-A only.
	removed := h.InvalidateEmbeddingCache(func(_ string, _ rerank.Identity, _, generation, _ string) bool {
		return generation == "gen-A"
	})
	if removed == 0 {
		t.Fatal("expected at least one entry removed for gen-A")
	}

	before2 := atomic.LoadInt32(&calls)
	_ = h.Search(context.Background(), "gen-A", "lorem ipsum", 1)
	if got := atomic.LoadInt32(&calls); got <= before2 {
		t.Fatalf("post-invalidate search must rebuild the store; calls %d -> %d", before2, got)
	}
}

// threadingEmbedder is a hermetic Embedder stub used by the threading tests.
// It implements IdentityProvider so it composes naturally with EmbedCache.
type threadingEmbedder struct {
	calls *int32
	dim   int
}

func (e *threadingEmbedder) Embed(_ context.Context, texts []string, _ string) ([][]float32, error) {
	atomic.AddInt32(e.calls, int32(len(texts)))
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, e.dim)
		vec[0] = float32(len(text))
		out[i] = vec
	}
	return out, nil
}

func (e *threadingEmbedder) EmbedIdentity() rerank.Identity {
	return rerank.Identity{Model: "threading", Dimension: e.dim, Normalization: "l2"}
}

// TestHybridEmbedDenseNonCacheEmbedderStats reports zero stats when the
// embedder is not an EmbedCache — no diagnostic leak.
func TestHybridEmbedDenseNonCacheEmbedderStats(t *testing.T) {
	t.Parallel()
	h := NewHybridEmbedDense(&threadingEmbedder{}, nil)
	if st := h.CacheStats(); st != (rerank.CacheStats{}) {
		t.Fatalf("non-cache embedder must report zero stats; got %+v", st)
	}
	if n := h.InvalidateEmbeddingCache(nil); n != 0 {
		t.Fatalf("non-cache embedder must report zero invalidations; got %d", n)
	}
}

package query

import (
	"context"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

// HybridEmbedDense implements DenseSearcher with live embeddings when an
// Embedder is configured, else falls back to BagOfWordsDense.
//
// BodiesFunc returns generation document texts for embedding (path → body).
// Results are cached in MemoryStore per generation for subsequent queries.
//
// When Embedder is a *rerank.EmbedCache, generation and Tenant are threaded
// into the cache key so a generation change, tenant change, or model
// identity change never reuses an incompatible vector (issue #329).
type HybridEmbedDense struct {
	Embedder   rerank.Embedder
	BodiesFunc func(generationID string) map[string]string
	// Fallback is used when Embedder is nil or embedding fails.
	Fallback DenseSearcher
	// Tenant scopes the embedding cache (no-op for non-cache embedders).
	// An empty Tenant disables tenant scoping without affecting correctness.
	Tenant string

	mu     sync.Mutex
	stores map[string]*dense.MemoryStore // generationID → store
}

// NewHybridEmbedDense builds a DenseSearcher. embedder may be nil.
func NewHybridEmbedDense(embedder rerank.Embedder, bodies func(string) map[string]string) *HybridEmbedDense {
	return &HybridEmbedDense{
		Embedder:   embedder,
		BodiesFunc: bodies,
		Fallback:   &BagOfWordsDense{},
		stores:     make(map[string]*dense.MemoryStore),
	}
}

// CacheStats returns embedding-cache counters if the underlying Embedder is
// a *rerank.EmbedCache; otherwise a zero value. Callers should treat the
// result as diagnostics only — never propagate raw text through the
// returned counters (issue #329).
func (h *HybridEmbedDense) CacheStats() rerank.CacheStats {
	if h == nil {
		return rerank.CacheStats{}
	}
	if c, ok := h.Embedder.(*rerank.EmbedCache); ok && c != nil {
		return c.Stats()
	}
	return rerank.CacheStats{}
}

// InvalidateEmbeddingCache removes embedding-cache entries matching pred.
// Returns 0 when the underlying Embedder is not a *rerank.EmbedCache, so
// callers can use this unconditionally as the write-invalidation path.
func (h *HybridEmbedDense) InvalidateEmbeddingCache(pred rerank.KeyPredicate) int {
	if h == nil {
		return 0
	}
	if c, ok := h.Embedder.(*rerank.EmbedCache); ok && c != nil {
		return c.Invalidate(pred)
	}
	return 0
}

// Search embeds the query and returns dense nearest document ids.
func (h *HybridEmbedDense) Search(ctx context.Context, generationID, query string, topK int) []string {
	if h == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	if strings.TrimSpace(query) == "" || generationID == "" {
		return nil
	}
	bodies := map[string]string{}
	if h.BodiesFunc != nil {
		bodies = h.BodiesFunc(generationID)
	}
	if len(bodies) == 0 {
		return h.fallback(ctx, generationID, query, topK, bodies)
	}

	// Prefer live embed when available.
	if h.Embedder != nil {
		store, err := h.ensureStore(ctx, generationID, bodies)
		if err == nil && store != nil {
			embedCtx := rerank.WithTenant(rerank.WithGeneration(ctx, generationID), h.Tenant)
			qvecs, err := h.Embedder.Embed(embedCtx, []string{query}, "query")
			if err == nil && len(qvecs) > 0 {
				hits := store.Search(qvecs[0], topK)
				out := make([]string, 0, len(hits))
				for _, hit := range hits {
					out = append(out, hit.DocumentID)
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}
	return h.fallback(ctx, generationID, query, topK, bodies)
}

func (h *HybridEmbedDense) fallback(ctx context.Context, generationID, query string, topK int, bodies map[string]string) []string {
	if h.Fallback != nil {
		if bag, ok := h.Fallback.(*BagOfWordsDense); ok {
			// Ensure bodies registered for generation.
			if bag.Bodies == nil {
				bag.Bodies = map[string]map[string]string{}
			}
			if len(bodies) > 0 {
				bag.Bodies[generationID] = bodies
			}
		}
		return h.Fallback.Search(ctx, generationID, query, topK)
	}
	return NewBagOfWordsDense(generationID, bodies).Search(ctx, generationID, query, topK)
}

func (h *HybridEmbedDense) ensureStore(ctx context.Context, generationID string, bodies map[string]string) (*dense.MemoryStore, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if store, ok := h.stores[generationID]; ok && store.Len() >= len(bodies)/2 {
		return store, nil
	}
	store := dense.NewMemoryStore()
	// Batch embed in chunks of 16.
	ids := make([]string, 0, len(bodies))
	texts := make([]string, 0, len(bodies))
	for id, text := range bodies {
		if id == "" || strings.TrimSpace(text) == "" {
			continue
		}
		text = textbound.Bytes(text, 6_000)
		ids = append(ids, id)
		texts = append(texts, text)
	}
	if len(ids) == 0 {
		return store, nil
	}
	embedCtx := rerank.WithTenant(rerank.WithGeneration(ctx, generationID), h.Tenant)
	const chunk = 16
	for i := 0; i < len(texts); i += chunk {
		j := i + chunk
		if j > len(texts) {
			j = len(texts)
		}
		vecs, err := h.Embedder.Embed(embedCtx, texts[i:j], "document")
		if err != nil {
			return nil, err
		}
		for k, vec := range vecs {
			_ = store.Upsert(ids[i+k], vec)
		}
	}
	h.stores[generationID] = store
	return store, nil
}

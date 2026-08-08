package rerank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// CachedEmbedder wraps an Embedder with an in-memory cache keyed by
// sha256(inputType + "\x00" + text). Concurrent-safe.
type CachedEmbedder struct {
	inner Embedder
	mu    sync.RWMutex
	cache map[string][]float32
}

// NewCachedEmbedder returns a caching wrapper around inner.
// inner must be non-nil.
func NewCachedEmbedder(inner Embedder) (*CachedEmbedder, error) {
	if inner == nil {
		return nil, fmt.Errorf("rerank: nil embedder")
	}
	return &CachedEmbedder{
		inner: inner,
		cache: make(map[string][]float32),
	}, nil
}

// Embed returns cached vectors when present and embeds only cache misses.
// Results preserve input order. Cache keys include inputType so query vs
// document embeddings of the same text do not collide.
func (c *CachedEmbedder) Embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	if c == nil || c.inner == nil {
		return nil, fmt.Errorf("rerank: nil cached embedder")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	missIdx := make([]int, 0, len(texts))
	missTexts := make([]string, 0, len(texts))

	c.mu.RLock()
	for i, text := range texts {
		key := cacheKey(inputType, text)
		if vec, ok := c.cache[key]; ok {
			cp := make([]float32, len(vec))
			copy(cp, vec)
			out[i] = cp
			continue
		}
		missIdx = append(missIdx, i)
		missTexts = append(missTexts, text)
	}
	c.mu.RUnlock()

	if len(missTexts) == 0 {
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	embedded, err := c.inner.Embed(ctx, missTexts, inputType)
	if err != nil {
		return nil, err
	}
	if len(embedded) != len(missTexts) {
		return nil, fmt.Errorf("rerank: embed count mismatch: got %d want %d", len(embedded), len(missTexts))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for j, i := range missIdx {
		vec := embedded[j]
		cp := make([]float32, len(vec))
		copy(cp, vec)
		c.cache[cacheKey(inputType, missTexts[j])] = cp
		out[i] = make([]float32, len(vec))
		copy(out[i], vec)
	}
	return out, nil
}

// Len returns the number of cached entries (for tests/diagnostics).
func (c *CachedEmbedder) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

func cacheKey(inputType, text string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(inputType))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

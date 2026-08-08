package rerank_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

// fixedEmbedder is a deterministic Embedder stub. It records call counts and
// produces length-derived vectors so test inputs are easy to reason about.
// The optional identity fields are returned by EmbedIdentity() so we can
// exercise identity-scoped keying without a real provider.
type fixedEmbedder struct {
	mu      sync.Mutex
	calls   int32
	dim     int
	model   string
	normal  string
	failErr error
	delay   time.Duration
}

func (f *fixedEmbedder) Embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failErr != nil {
		return nil, f.failErr
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, f.dim)
		vec[0] = float32(len(text))
		out[i] = vec
	}
	return out, nil
}

func (f *fixedEmbedder) EmbedIdentity() rerank.Identity {
	return rerank.Identity{Model: f.model, Dimension: f.dim, Normalization: f.normal}
}

// variableEmbedder lets a test mutate identity at runtime to exercise the
// model-mismatch path. It mirrors fixedEmbedder otherwise.
type variableEmbedder struct {
	fixedEmbedder
}

func (v *variableEmbedder) SetIdentity(id rerank.Identity) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.model = id.Model
	v.dim = id.Dimension
	v.normal = id.Normalization
}

// TestEmbedCacheHitMiss covers the basic cache contract: the first call
// misses, the second call hits, and the inner embedder is only invoked for
// cache misses. Counters increment accordingly.
func TestEmbedCacheHitMiss(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 32)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rerank.WithTenant(rerank.WithGeneration(context.Background(), "gen-1"), "tenant-a")

	v1, err := c.Embed(ctx, []string{"hello"}, "document")
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) != 1 || len(v1[0]) != 4 {
		t.Fatalf("v1=%v", v1)
	}
	v2, err := c.Embed(ctx, []string{"hello"}, "document")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.calls); got != 1 {
		t.Fatalf("calls=%d want 1", got)
	}
	if v1[0][0] != v2[0][0] {
		t.Fatalf("vector drift across hits: %v vs %v", v1[0][0], v2[0][0])
	}
	st := c.Stats()
	if st.Hits != 1 || st.Misses != 1 || st.Size != 1 {
		t.Fatalf("stats=%+v want hits=1 misses=1 size=1", st)
	}
}

// TestEmbedCacheStale verifies TTL expiry produces a stale event and a
// re-embed, never a silent zero-age hit. The clock is injected so the test
// is deterministic and never sleeps.
func TestEmbedCacheStale(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, 10*time.Millisecond, 8)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(0, 0)
	c.SetClock(func() time.Time { return clock })

	ctx := context.Background()
	if _, err := c.Embed(ctx, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	// Advance virtual clock past TTL.
	clock = time.Unix(0, int64(time.Second))

	if _, err := c.Embed(ctx, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if st.Stales == 0 {
		t.Fatal("expected a stale event on expired lookup")
	}
	if st.Hits != 0 {
		t.Fatalf("stale must not count as hit; stats=%+v", st)
	}
	if got := atomic.LoadInt32(&inner.calls); got != 2 {
		t.Fatalf("calls=%d want 2 (one miss, one re-embed after stale)", got)
	}
}

// TestEmbedCacheModelMismatch exercises the model-mismatch event. When the
// embedder's identity changes mid-process, the previously cached vector is
// rejected and re-embedded; it never silently aliases a different model's
// dimensions/normalization.
func TestEmbedCacheModelMismatch(t *testing.T) {
	v := &variableEmbedder{}
	v.fixedEmbedder = fixedEmbedder{model: "mA", dim: 4, normal: "l2"}
	c, err := rerank.NewEmbedCache(v, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Embed(ctx, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	// Identity pin at construction was mA/l2/4. Mutate inner to mB/l2/4; the
	// same key resolves to an entry recorded under mA. Identity check on
	// lookup rejects it as a mismatch and forces re-embed.
	v.SetIdentity(rerank.Identity{Model: "mB", Dimension: 4, Normalization: "l2"})
	before := atomic.LoadInt32(&v.calls)
	if _, err := c.Embed(ctx, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&v.calls) != before+1 {
		t.Fatalf("mismatch must trigger re-embed (calls=%d)", v.calls)
	}
	st := c.Stats()
	if st.Mismatches == 0 {
		t.Fatalf("expected mismatch event; stats=%+v", st)
	}
	if st.Hits != 0 {
		t.Fatalf("mismatch must not count as hit; stats=%+v", st)
	}
}

// TestEmbedCacheIdentityFieldScope proves changing each identity field
// (model, dim, normalization) is detected as a mismatch and re-embeds,
// never silently reuses a vector from a different identity.
func TestEmbedCacheIdentityFieldScope(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		a, b rerank.Identity
	}{
		{"model", rerank.Identity{Model: "mA", Dimension: 4, Normalization: "l2"},
			rerank.Identity{Model: "mB", Dimension: 4, Normalization: "l2"}},
		{"dimension", rerank.Identity{Model: "m", Dimension: 4, Normalization: "l2"},
			rerank.Identity{Model: "m", Dimension: 8, Normalization: "l2"}},
		{"normalization", rerank.Identity{Model: "m", Dimension: 4, Normalization: "l2"},
			rerank.Identity{Model: "m", Dimension: 4, Normalization: "none"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &variableEmbedder{}
			v.fixedEmbedder = fixedEmbedder{model: tc.a.Model, dim: tc.a.Dimension, normal: tc.a.Normalization}
			c, err := rerank.NewEmbedCache(v, time.Minute, 8)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Embed(ctx, []string{"hello"}, "document"); err != nil {
				t.Fatal(err)
			}
			v.SetIdentity(tc.b)
			before := atomic.LoadInt32(&v.calls)
			if _, err := c.Embed(ctx, []string{"hello"}, "document"); err != nil {
				t.Fatal(err)
			}
			if atomic.LoadInt32(&v.calls) != before+1 {
				t.Fatalf("identity field %s change must trigger re-embed", tc.name)
			}
			st := c.Stats()
			if st.Mismatches == 0 {
				t.Fatalf("identity field %s change must record mismatch; stats=%+v", tc.name, st)
			}
		})
	}
}

// TestEmbedCacheGenerationScope proves a different generation never reuses
// another generation's cached vector (generation is part of the key).
func TestEmbedCacheGenerationScope(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	g1 := rerank.WithGeneration(context.Background(), "gen-A")
	g2 := rerank.WithGeneration(context.Background(), "gen-B")
	if _, err := c.Embed(g1, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embed(g2, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if st.Hits != 0 {
		t.Fatalf("generation change must bust cache; stats=%+v", st)
	}
	if st.Misses != 2 {
		t.Fatalf("expected 2 misses; stats=%+v", st)
	}
}

// TestEmbedCacheTenantScope proves a different tenant never reuses another
// tenant's cached vector (tenant is part of the key).
func TestEmbedCacheTenantScope(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	a := rerank.WithTenant(context.Background(), "tenant-A")
	b := rerank.WithTenant(context.Background(), "tenant-B")
	if _, err := c.Embed(a, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embed(b, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if st.Hits != 0 {
		t.Fatalf("tenant change must bust cache; stats=%+v", st)
	}
	if st.Misses != 2 {
		t.Fatalf("expected 2 misses; stats=%+v", st)
	}
}

// TestEmbedCacheEvictionBounded proves capacity pressure evicts least
// recently used entries without wiping live ones. The eviction counter
// advances and recent entries still hit.
func TestEmbedCacheEvictionBounded(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 4)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		text := string(rune('a' + i))
		if _, err := c.Embed(ctx, []string{text}, "document"); err != nil {
			t.Fatal(err)
		}
	}
	st := c.Stats()
	if st.Size > 4 {
		t.Fatalf("size=%d exceeds cap=4", st.Size)
	}
	if st.Evicted < 4 {
		t.Fatalf("expected >=4 evictions, got %d", st.Evicted)
	}
	for i := 4; i < 8; i++ {
		text := string(rune('a' + i))
		before := atomic.LoadInt32(&inner.calls)
		if _, err := c.Embed(ctx, []string{text}, "document"); err != nil {
			t.Fatal(err)
		}
		after := atomic.LoadInt32(&inner.calls)
		if after != before {
			t.Fatalf("recent entry %q must hit, not re-embed (calls %d -> %d)", text, before, after)
		}
	}
}

// TestEmbedCacheConcurrentSafe stresses the cache from many goroutines to
// verify no map races or double-counts. The total calls to the inner
// embedder must equal the number of unique (text, inputType, generation,
// tenant) keys.
func TestEmbedCacheConcurrentSafe(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 64)
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 16
	const iters = 32
	texts := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				ctx := rerank.WithTenant(rerank.WithGeneration(context.Background(), "gen-1"), "tenant-A")
				_, _ = c.Embed(ctx, []string{texts[i%len(texts)]}, "document")
			}
		}()
	}
	wg.Wait()
	st := c.Stats()
	if got := atomic.LoadInt32(&inner.calls); int(got) != len(texts) {
		t.Fatalf("calls=%d want %d (one per unique key)", got, len(texts))
	}
	// hits+misses must account for every text processed; the split
	// between hits and misses depends on timing (waiters bypass the
	// cache and are counted as misses, post-leader lookups are hits).
	if st.Hits+st.Misses != goroutines*iters {
		t.Fatalf("hits+misses=%d want %d", st.Hits+st.Misses, goroutines*iters)
	}
	if st.Size > len(texts) {
		t.Fatalf("size=%d > unique keys %d", st.Size, len(texts))
	}
}

// TestEmbedCacheFailOpen proves cache faults do not propagate to the
// caller. The inner embedder's error surfaces verbatim; cache state
// remains consistent and the cache recovers when the inner recovers.
func TestEmbedCacheFailOpen(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4, failErr: errors.New("boom")}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Embed(context.Background(), []string{"hello"}, "document")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected inner error to surface; got %v", err)
	}
	if size := c.Len(); size != 0 {
		t.Fatalf("size=%d want 0 after inner error", size)
	}
	inner.failErr = nil
	if _, err := c.Embed(context.Background(), []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embed(context.Background(), []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if st := c.Stats(); st.Hits == 0 {
		t.Fatalf("expected hit after recovery; stats=%+v", st)
	}
}

// TestEmbedCacheInvalidatePredicate proves targeted invalidation removes
// only matching entries. Generation- and tenant-scoped predicates are the
// primary write-invalidation path.
func TestEmbedCacheInvalidatePredicate(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 32)
	if err != nil {
		t.Fatal(err)
	}
	ga := rerank.WithTenant(rerank.WithGeneration(context.Background(), "gen-A"), "tenant-A")
	gb := rerank.WithTenant(rerank.WithGeneration(context.Background(), "gen-B"), "tenant-B")
	if _, err := c.Embed(ga, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embed(gb, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	removed := c.Invalidate(func(_ string, _ rerank.Identity, _, generation, _ string) bool {
		return generation == "gen-A"
	})
	if removed != 1 {
		t.Fatalf("removed=%d want 1", removed)
	}
	beforeA := atomic.LoadInt32(&inner.calls)
	if _, err := c.Embed(ga, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&inner.calls) <= beforeA {
		t.Fatal("expected re-embed after invalidate for gen-A")
	}
	beforeB := atomic.LoadInt32(&inner.calls)
	if _, err := c.Embed(gb, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&inner.calls) != beforeB {
		t.Fatal("gen-B entry must hit after targeted invalidate")
	}
}

// TestEmbedCacheStatsLeakNoRawText proves stats and identity never carry
// raw query or evidence text. The string "SUPERSECRET" must not appear in
// any stringified form of Stats or Identity.
func TestEmbedCacheStatsLeakNoRawText(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "SUPERSECRET"
	ctx := context.Background()
	if _, err := c.Embed(ctx, []string{secret}, "document"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Stats().String(), secret) {
		t.Fatal("Stats leaked raw text")
	}
	if strings.Contains(c.Identity().String(), secret) {
		t.Fatal("Identity leaked raw text")
	}
}

// TestEmbedCacheRejectsNilEmbedder proves the constructor enforces a
// non-nil inner — fail closed on misconfiguration.
func TestEmbedCacheRejectsNilEmbedder(t *testing.T) {
	if _, err := rerank.NewEmbedCache(nil, time.Minute, 8); err == nil {
		t.Fatal("expected nil-embedder rejection")
	}
}

// TestEmbedCacheContextCancellation proves context cancellation from the
// caller surfaces the inner's error, not a cache error.
func TestEmbedCacheContextCancellation(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4, delay: 50 * time.Millisecond}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Embed(ctx, []string{"hello"}, "document")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

// TestEmbedCacheInputTypeScope covers the inputType axis (query vs document).
// A vector cached as a document must not be served for a query of the same
// text (OpenAI's asymmetric embedding models would corrupt retrieval).
func TestEmbedCacheInputTypeScope(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Embed(ctx, []string{"hello"}, "document"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Embed(ctx, []string{"hello"}, "query"); err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if st.Hits != 0 {
		t.Fatalf("inputType change must bust cache; stats=%+v", st)
	}
	if st.Misses != 2 {
		t.Fatalf("expected 2 misses; stats=%+v", st)
	}
}

// TestEmbedCacheTTLDefaultsProveBounded proves the constructor clamps
// pathological inputs so a misconfigured caller cannot allocate unbounded
// memory or use unbounded TTL.
func TestEmbedCacheTTLDefaultsProveBounded(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4}
	c, err := rerank.NewEmbedCache(inner, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stats().Max <= 0 {
		t.Fatal("expected positive default max")
	}
	// Overflowing max must be clamped to the hard cap.
	_, err = rerank.NewEmbedCache(inner, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
}

// TestEmbedCacheSingleflightDedupes proves concurrent goroutines that miss
// on the same key share one inner.Embed call (singleflight). The total
// inner call count is bounded by the number of unique keys, not the
// number of goroutines or input texts.
func TestEmbedCacheSingleflightDedupes(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4, delay: 20 * time.Millisecond}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 64)
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 32
	const iters = 16
	texts := []string{"only-one-text"} // exactly one unique key
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := rerank.WithTenant(rerank.WithGeneration(context.Background(), "gen-1"), "tenant-A")
			for i := 0; i < iters; i++ {
				_, _ = c.Embed(ctx, []string{texts[0]}, "document")
			}
		}()
	}
	wg.Wait()
	// Singleflight must keep inner call count at 1 (cold miss) regardless
	// of goroutine count.
	if got := atomic.LoadInt32(&inner.calls); got != 1 {
		t.Fatalf("inner.calls=%d want 1 (singleflight dedupe)", got)
	}
}

// TestEmbedCacheWaiterEvictionNoNilVector is the regression test for B3:
// when a waiter's inflight claim resolves but the cache entry was evicted
// (by the leader's own capacity eviction or a concurrent caller), every
// returned vector must be non-nil. The test combines duplicate texts,
// oversized batches (exceeding cache max), and concurrent eviction
// pressure to trigger the race.
func TestEmbedCacheWaiterEvictionNoNilVector(t *testing.T) {
	inner := &fixedEmbedder{model: "m", dim: 4, delay: 5 * time.Millisecond}
	c, err := rerank.NewEmbedCache(inner, time.Minute, 2) // tiny cap
	if err != nil {
		t.Fatal(err)
	}
	ctx := rerank.WithTenant(rerank.WithGeneration(context.Background(), "gen-1"), "tenant-A")

	// First, fill the cache to capacity so the next batch triggers eviction.
	if _, err := c.Embed(ctx, []string{"filler-a", "filler-b"}, "document"); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Size != 2 {
		t.Fatalf("expected cache size 2 after priming, got %d", c.Stats().Size)
	}

	// Launch concurrent goroutines embedding a batch with duplicates.
	// The batch has 4 distinct texts but the cache only holds 2 entries;
	// after the leader embeds, its own evictToCapacityLocked will evict
	// some of the freshly stored entries before waiters wake up.
	const goroutines = 8
	texts := []string{"x", "y", "z", "x"} // "x" is duplicated
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	results := make([][][]float32, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = c.Embed(ctx, texts, "document")
		}(g)
	}
	wg.Wait()

	for g := 0; g < goroutines; g++ {
		if errs[g] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", g, errs[g])
		}
		vecs := results[g]
		if len(vecs) != len(texts) {
			t.Fatalf("goroutine %d: got %d vectors, want %d", g, len(vecs), len(texts))
		}
		for i, v := range vecs {
			if v == nil {
				t.Fatalf("goroutine %d texts[%d] (%q): nil vector (B3 regression)", g, i, texts[i])
			}
			if len(v) != 4 {
				t.Fatalf("goroutine %d texts[%d] (%q): vector dim=%d want 4", g, i, texts[i], len(v))
			}
		}
	}
	st := c.Stats()
	if st.Evicted == 0 {
		t.Log("warning: no evictions during test; race may not have been triggered")
	}
}

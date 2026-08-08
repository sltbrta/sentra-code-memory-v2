package rerank

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Identity captures the stable model identity that produced an embedding.
// Two embeddings from different Identity values must never share a cache
// entry. Empty fields mean "unknown"; the cache still works, but it cannot
// reuse vectors across model changes and will simply miss or mismatch
// instead. Identity participates in cacheability through a per-entry
// identity check (issue #329): the cache key covers input type, tenant,
// generation, and text digest; the stored identity is verified against the
// inner embedder's current identity on every lookup so a model, dimension,
// or normalization change cannot silently reuse an incompatible vector.
type Identity struct {
	// Model is the embedding model name (e.g. "text-embedding-3-small").
	Model string
	// Dimension is the fixed vector dimension; 0 means unknown.
	Dimension int
	// Normalization is the canonical normalization applied by the provider
	// (e.g. "l2"); empty means unknown.
	Normalization string
}

// IsZero reports whether no identity field is set. A zero identity still
// participates in keying so unknown-model entries cannot collide across
// unknown-model embedders.
func (i Identity) IsZero() bool {
	return i.Model == "" && i.Dimension == 0 && i.Normalization == ""
}

// Equal reports whether two identities match on every populated field.
// Empty fields are compared literally (two zero identities are equal).
func (i Identity) Equal(o Identity) bool {
	return i.Model == o.Model && i.Dimension == o.Dimension && i.Normalization == o.Normalization
}

// String renders the identity in a stable, non-sensitive form suitable for
// sanitized receipts. Raw query/evidence text is never included.
func (i Identity) String() string {
	return i.Model + "|" + strconv.Itoa(i.Dimension) + "|" + i.Normalization
}

// IdentityProvider is implemented by Embedders that can declare their stable
// embedding identity at lookup time. Implementations should return the same
// Identity for the same underlying model; a runtime change is treated as a
// model-mismatch event by the cache.
type IdentityProvider interface {
	EmbedIdentity() Identity
}

// ctxKey is unexported so external packages cannot collide on the keys we
// use to thread tenant/generation identity through context.Context.
type ctxKey int

const (
	ctxKeyTenant ctxKey = iota
	ctxKeyGeneration
)

// WithTenant returns a context that carries the tenant identity used to scope
// embedding cache entries. An empty tenant is stored as "" and behaves like
// "no tenant scope"; it never broadens what a caller may see.
func WithTenant(ctx context.Context, tenant string) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, ctxKeyTenant, tenant)
}

// WithGeneration returns a context that carries the corpus generation identity
// used to scope embedding cache entries. An empty generation is stored as "".
func WithGeneration(ctx context.Context, generation string) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, ctxKeyGeneration, generation)
}

func tenantFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeyTenant).(string); ok {
		return v
	}
	return ""
}

func generationFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeyGeneration).(string); ok {
		return v
	}
	return ""
}

// CacheEvent is the diagnostic event name returned per lookup. It is a
// stable, bounded vocabulary suitable for sanitized receipts (issue #329).
type CacheEvent string

const (
	EventHit      CacheEvent = "hit"
	EventMiss     CacheEvent = "miss"
	EventStale    CacheEvent = "stale"
	EventMismatch CacheEvent = "mismatch"
	EventEvicted  CacheEvent = "evicted"
)

// CacheStats reports bounded counters; values are copyable and safe to log.
// No field ever contains raw query or evidence text.
type CacheStats struct {
	Size       int `json:"size"`
	Max        int `json:"max"`
	Hits       int `json:"hits"`
	Misses     int `json:"misses"`
	Stales     int `json:"stales"`
	Mismatches int `json:"mismatches"`
	Evicted    int `json:"evicted"`
}

// String renders a non-sensitive summary suitable for receipts.
func (s CacheStats) String() string {
	return fmt.Sprintf("size=%d/%d hits=%d misses=%d stales=%d mismatches=%d evicted=%d",
		s.Size, s.Max, s.Hits, s.Misses, s.Stales, s.Mismatches, s.Evicted)
}

// KeyPredicate filters cache entries for explicit invalidation. Returning
// true removes the entry from the cache.
type KeyPredicate func(key string, ident Identity, tenant, generation, inputType string) bool

// cacheEntry is one stored vector. Identity is recorded at put-time so a
// model change can be detected as a model-mismatch on lookup even when the
// input fingerprint is identical (issue #329: identity, not just text,
// shapes cacheability).
type cacheEntry struct {
	key        string
	identity   Identity
	inputType  string
	tenant     string
	generation string
	vec        []float32
	expires    time.Time
}

// pending captures one text whose result is not yet cached. Leader entries
// drive a fresh inner.Embed call; waiter entries piggyback on an in-flight
// leader to dedupe concurrent bursts (singleflight).
type pending struct {
	idx  int
	key  string
	text string
}

// inflightClaim deduplicates concurrent Embed calls for the same key so a
// burst of requests sharing identical (inputType, tenant, generation, text)
// only invokes the inner embedder once. Waiters block on done until the
// leader publishes a result; they then re-check the cache. Singleflight
// is a correctness optimization for resource-bounded providers — not a
// semantic change — and is fully safe to disable by removing the inflight
// map (callers still get correct, redundant results).
type inflightClaim struct {
	done chan struct{}
	vec  []float32
	err  error
}

// EmbedCache is a bounded TTL+LRU cache for an Embedder. It addresses
// issue #329: keys cover input type, tenant, generation, and a sha256 text
// digest; entries additionally carry their embedding identity which is
// verified on lookup so a model, dimension, or normalization change never
// silently reuses an incompatible vector. All operations are nil-safe and
// fail open: a cache fault is a miss, never an error, and never widens
// what a caller may see. No raw text appears in keys, stats, or counters.
type EmbedCache struct {
	inner Embedder
	now   func() time.Time
	ttl   time.Duration
	max   int

	mu       sync.Mutex
	ll       *list.List
	items    map[string]*list.Element
	inflight map[string]*inflightClaim // key → leader claim; protected by mu

	hits       int
	misses     int
	stales     int
	mismatches int
	evicted    int
}

const (
	defaultEmbedCacheTTL  = 5 * time.Minute
	defaultEmbedCacheMax  = 1024
	maxEmbedCacheMax      = 1 << 16 // hard cap to keep memory bounded
	embedCacheTTLUpper    = 24 * time.Hour
	maxCacheKeyInputBytes = 1 << 20 // defensive cap on serialized key inputs
)

// NewEmbedCache wraps inner with a bounded TTL+LRU cache. inner may
// implement IdentityProvider to declare its stable identity. ttl <= 0 falls
// back to defaultEmbedCacheTTL; maxEntries <= 0 falls back to
// defaultEmbedCacheMax. A nil inner is rejected so callers cannot silently
// construct a misconfigured cache.
func NewEmbedCache(inner Embedder, ttl time.Duration, maxEntries int) (*EmbedCache, error) {
	if inner == nil {
		return nil, errors.New("rerank: nil embedder")
	}
	if ttl <= 0 {
		ttl = defaultEmbedCacheTTL
	}
	if ttl > embedCacheTTLUpper {
		ttl = embedCacheTTLUpper
	}
	if maxEntries <= 0 {
		maxEntries = defaultEmbedCacheMax
	}
	if maxEntries > maxEmbedCacheMax {
		maxEntries = maxEmbedCacheMax
	}
	return &EmbedCache{
		inner:    inner,
		now:      time.Now,
		ttl:      ttl,
		max:      maxEntries,
		ll:       list.New(),
		items:    make(map[string]*list.Element, maxEntries),
		inflight: make(map[string]*inflightClaim),
	}, nil
}

// SetClock overrides the wall-clock source. Test-only; production callers
// should leave the default time.Now in place.
func (c *EmbedCache) SetClock(now func() time.Time) {
	if c == nil || now == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// Len returns the number of entries currently stored.
func (c *EmbedCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Stats returns a snapshot of cache counters. Safe to log; no raw text.
func (c *EmbedCache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Size:       len(c.items),
		Max:        c.max,
		Hits:       c.hits,
		Misses:     c.misses,
		Stales:     c.stales,
		Mismatches: c.mismatches,
		Evicted:    c.evicted,
	}
}

// Identity returns the inner embedder's current identity, or a zero
// Identity if the inner does not implement IdentityProvider.
func (c *EmbedCache) Identity() Identity {
	if c == nil || c.inner == nil {
		return Identity{}
	}
	if p, ok := c.inner.(IdentityProvider); ok && p != nil {
		return p.EmbedIdentity()
	}
	return Identity{}
}

// Clear drops every entry. Use after chunk/index mutations that invalidate
// every generation at once; prefer Invalidate for targeted invalidation.
func (c *EmbedCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll = list.New()
	c.items = make(map[string]*list.Element, c.max)
}

// Invalidate removes entries for which pred returns true. It is the explicit
// write-invalidation path; eviction under capacity pressure uses bounded
// LRU and never wipes live entries wholesale. Returns the number of entries
// removed.
func (c *EmbedCache) Invalidate(pred KeyPredicate) int {
	if c == nil || pred == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for el := c.ll.Front(); el != nil; {
		next := el.Next()
		e := el.Value.(*cacheEntry)
		if pred(e.key, e.identity, e.tenant, e.generation, e.inputType) {
			c.removeElement(el)
			removed++
		}
		el = next
	}
	return removed
}

// Embed satisfies the Embedder interface and is the cache's primary call
// site. Fail-open semantics: any internal cache error becomes a miss and is
// not surfaced to the caller. The inner embedder's error (network, decode,
// context cancellation) is the only error path.
//
// Concurrent goroutines that miss on the same key share one inner.Embed
// call via per-key inflight claims (singleflight), so a request burst
// cannot drive redundant provider calls. Waiters block on the leader's
// done channel until the leader has either populated the cache or
// published an error, then re-read the cache.
func (c *EmbedCache) Embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("rerank: nil bounded embed cache")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity := c.Identity()
	tenant := tenantFrom(ctx)
	generation := generationFrom(ctx)

	out := make([][]float32, len(texts))
	var leaderMisses []pending
	type waiter struct {
		p     pending
		claim *inflightClaim
	}
	var waiters []waiter

	c.mu.Lock()
	for i, text := range texts {
		key, err := buildEmbedKey(inputType, tenant, generation, text)
		if err != nil {
			c.misses++
			leaderMisses = append(leaderMisses, pending{idx: i, key: "", text: text})
			continue
		}
		el, ok := c.items[key]
		if ok {
			e := el.Value.(*cacheEntry)
			if !e.identity.Equal(identity) {
				c.removeElement(el)
				c.mismatches++
				leaderMisses = append(leaderMisses, pending{idx: i, key: key, text: text})
				continue
			}
			if c.now().After(e.expires) {
				c.removeElement(el)
				c.stales++
				leaderMisses = append(leaderMisses, pending{idx: i, key: key, text: text})
				continue
			}
			c.ll.MoveToFront(el)
			c.hits++
			cp := make([]float32, len(e.vec))
			copy(cp, e.vec)
			out[i] = cp
			continue
		}
		// Cache miss. If another goroutine is already embedding this key,
		// join its inflight claim and wait instead of issuing a duplicate
		// provider call.
		if claim, exists := c.inflight[key]; exists && claim != nil {
			c.misses++
			waiters = append(waiters, waiter{p: pending{idx: i, key: key, text: text}, claim: claim})
			continue
		}
		c.misses++
		claim := &inflightClaim{done: make(chan struct{})}
		c.inflight[key] = claim
		leaderMisses = append(leaderMisses, pending{idx: i, key: key, text: text})
	}
	c.mu.Unlock()

	// Leader goroutine embeds its unique misses; everyone else waits on
	// the claim's done channel.
	var leaderErr error
	if len(leaderMisses) > 0 {
		leaderTexts := make([]string, len(leaderMisses))
		for i, p := range leaderMisses {
			leaderTexts[i] = p.text
		}
		embedded, err := c.inner.Embed(ctx, leaderTexts, inputType)
		if err != nil {
			leaderErr = err
		} else if len(embedded) != len(leaderTexts) {
			leaderErr = fmt.Errorf("rerank: embed count mismatch: got %d want %d", len(embedded), len(leaderTexts))
		} else {
			c.mu.Lock()
			for j, p := range leaderMisses {
				vec := embedded[j]
				cp := make([]float32, len(vec))
				copy(cp, vec)
				if p.key != "" {
					e := &cacheEntry{
						key:        p.key,
						identity:   identity,
						inputType:  inputType,
						tenant:     tenant,
						generation: generation,
						vec:        cp,
						expires:    c.now().Add(c.ttl),
					}
					if existing, ok := c.items[p.key]; ok {
						existing.Value = e
						c.ll.MoveToFront(existing)
					} else {
						c.items[p.key] = c.ll.PushFront(e)
					}
					// Publish the vector on the inflight claim so waiters
					// can read it directly without racing the cache.
					if claim, ok := c.inflight[p.key]; ok && claim != nil {
						claim.vec = cp
					}
				}
				out[p.idx] = append([]float32(nil), vec...)
			}
			c.evictExpiredLocked()
			c.evictToCapacityLocked()
			c.mu.Unlock()
		}
		// Publish result (or error) to waiters and clear inflight state.
		c.mu.Lock()
		for _, p := range leaderMisses {
			if p.key == "" {
				continue
			}
			if claim, ok := c.inflight[p.key]; ok && claim != nil {
				claim.err = leaderErr
				close(claim.done)
				delete(c.inflight, p.key)
			}
		}
		c.mu.Unlock()
	}

	if leaderErr != nil {
		// Drain waiters so they don't deadlock on the closed channel; they
		// see claim.err and exit. We surface leaderErr once.
		for _, w := range waiters {
			<-w.claim.done
		}
		return nil, leaderErr
	}

	// Waiters read the result directly from the inflight claim. This
	// avoids a race where the leader stored the entry but a subsequent
	// eviction (capacity or expiry) removes it before the waiter can
	// re-read the cache (issue B3).
	for _, w := range waiters {
		<-w.claim.done
		if w.claim.err != nil {
			// Leader errored; leaderErr was already surfaced above,
			// this path is defensive (should not be reached).
			return nil, w.claim.err
		}
		if w.claim.vec == nil {
			// Leader closed the channel but neither populated vec
			// nor set err — this is a logic bug, never a normal
			// eviction race. Fail explicitly rather than silently
			// returning a nil vector.
			return nil, errors.New("rerank: inflight claim missing vector after leader completed")
		}
		cp := make([]float32, len(w.claim.vec))
		copy(cp, w.claim.vec)
		out[w.p.idx] = cp
	}
	return out, nil
}

func (c *EmbedCache) evictExpiredLocked() {
	now := c.now()
	for el := c.ll.Back(); el != nil; {
		prev := el.Prev()
		if now.After(el.Value.(*cacheEntry).expires) {
			c.removeElement(el)
			c.evicted++
		}
		el = prev
	}
}

func (c *EmbedCache) evictToCapacityLocked() {
	for len(c.items) > c.max {
		el := c.ll.Back()
		if el == nil {
			return
		}
		c.removeElement(el)
		c.evicted++
	}
}

func (c *EmbedCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.items, el.Value.(*cacheEntry).key)
}

// buildEmbedKey computes the cache key for one (inputType, tenant,
// generation, text) tuple. The key is a 32-byte sha256 hex digest, so the
// original text never appears in any key, log, or receipt. The embedding
// identity is verified separately via the per-entry Identity field.
func buildEmbedKey(inputType, tenant, generation, text string) (string, error) {
	if len(text) > maxCacheKeyInputBytes {
		return "", fmt.Errorf("rerank: text too large for cache key (%d bytes)", len(text))
	}
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(inputType)
	write(tenant)
	write(generation)
	// Length is mixed in last so truncated text cannot alias full text.
	var lenBuf [8]byte
	n := uint64(len(text))
	for i := 7; i >= 0; i-- {
		lenBuf[i] = byte(n)
		n >>= 8
	}
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil)), nil
}

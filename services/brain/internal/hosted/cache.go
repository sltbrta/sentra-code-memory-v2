package hosted

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// queryCache is a small in-process TTL+LRU cache for retrieve results.
// Giant-search framing: amortize expensive multi-arm retrieve across identical
// requests within a generation window (Modal container or long-lived process).
//
// Keys include every request-shaping input (tenant, generation, security
// profile, normalized question, resolved plan, mode, source types, topK,
// prod profile, retrieval config digest) so stale/cross-mode/cross-generation/
// cross-tenant reuse cannot happen (issue #295). Eviction is bounded LRU —
// never a full-map wipe — so live entries are not destroyed by unrelated
// pressure. All operations are nil-safe and fail open: a cache fault is a
// miss, never an error, and never widens what a caller can see.
type queryCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	ll    *list.List // front = most recently used
	items map[string]*list.Element

	hits    int
	misses  int
	stales  int
	evicted int
}

type cacheEntry struct {
	key      string
	passages []Passage
	diag     map[string]any
	expires  time.Time
}

// Cache lookup events for diagnostics (no raw evidence or gold fields).
const (
	cacheEventHit   = "hit"
	cacheEventMiss  = "miss"
	cacheEventStale = "stale"
)

const defaultQueryCacheMax = 256

func newQueryCache(ttl time.Duration) *queryCache {
	return newQueryCacheBounded(ttl, defaultQueryCacheMax)
}

func newQueryCacheBounded(ttl time.Duration, maxEntries int) *queryCache {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = defaultQueryCacheMax
	}
	return &queryCache{
		ttl:   ttl,
		max:   maxEntries,
		ll:    list.New(),
		items: map[string]*list.Element{},
	}
}

// cacheKeyParts captures every request-shaping input for retrieve caching.
// Missing any of these risks stale/cross-mode/cross-tenant reuse (issue #295).
type cacheKeyParts struct {
	BrainID      string
	Generation   string
	Security     string // profile/principal/owner digest — never grants or raw ACL data
	Question     string // normalized before hashing
	TopK         int
	QuestionType string
	Mode         string
	Plan         QueryPlan
	SourceTypes  []string
	ExpandLite   bool
	Profile      ProdProfile
	ConfigDigest string
	// FilterIdentity is the governed metadata-filter digest (issue #328) so
	// filtered and unfiltered windows never share a cache entry.
	FilterIdentity string
}

// normalizeCacheQuestion collapses case/whitespace-only differences so
// semantically identical asks share one entry (cache efficiency).
func normalizeCacheQuestion(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

func cacheKey(p cacheKeyParts) string {
	src := append([]string(nil), p.SourceTypes...)
	sort.Strings(src)
	var b strings.Builder
	write := func(s string) {
		b.WriteString(s)
		b.WriteByte(0)
	}
	write(p.BrainID)
	write(p.Generation)
	write(p.Security)
	write(normalizeCacheQuestion(p.Question))
	write(itoa(p.TopK))
	write(strings.ToLower(strings.TrimSpace(p.QuestionType)))
	write(strings.ToLower(strings.TrimSpace(p.Mode)))
	fmt.Fprintf(&b, "%+v", p.Plan)
	b.WriteByte(0)
	write(strings.Join(src, "\x01"))
	if p.ExpandLite {
		b.WriteByte('1')
	}
	b.WriteByte(0)
	fmt.Fprintf(&b, "%+v", p.Profile)
	b.WriteByte(0)
	write(p.ConfigDigest)
	write(p.FilterIdentity)
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:16])
}

// retrieveCacheKey builds the full request-shaping cache key for one retrieve:
// brain (tenant), generation pin, security profile, normalized question,
// resolved plan/mode/source types, effective topK, prod profile, and a digest
// of the retrieval config knobs that shape the window.
func (c *Client) retrieveCacheKey(question string, topK int, opts RetrieveOptions, plan QueryPlan, prod ProdProfile, filter *MetadataFilter) string {
	cfg := c.cfg
	secDigest := strings.Join([]string{
		string(c.Security.Profile),
		strings.TrimSpace(c.Security.Principal),
		strings.TrimSpace(c.Security.Owner),
	}, "\x01")
	configDigest := strings.Join([]string{
		cfg.ChunkCollection,
		cfg.ChunkVectorName,
		cfg.CohereModel,
		itoa(cfg.CohereDim),
		itoa(cfg.TopK),
		itoa(cfg.PoolK),
		itoa(cfg.LexicalLimit),
		itoa(cfg.DenseLimit),
		itoa(cfg.RRFK),
		"hot_lex_docs=" + itoa(cachedHotLexDocs(c)),
		"prod=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_PROD", true)),
		"quality=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_QUALITY", false)),
		"benchmax=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_BENCHMAX", false)),
		"bench_max=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_BENCH_MAX", false)),
		"mode_env=" + strings.ToLower(strings.TrimSpace(envOr("OUROBOROS_ERB_MODE", ""))),
		"skip_fts=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_SKIP_FTS", false)),
		"skip_dense=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_SKIP_DENSE", false)),
		"official=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_OFFICIAL", false)),
		"official_judge=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_OFFICIAL_JUDGE", false)),
		"force_fts=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_FORCE_FTS", false)),
		"force_residual=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_FORCE_RESIDUAL", false)),
		"quality_residual=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_QUALITY_RESIDUAL", false)),
		"lean=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_LEAN", true)),
		"llm_multiquery=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_LLM_MULTIQUERY", false)),
		"doc2query=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_DOC2QUERY", false)),
		"force_neon_fts=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_FORCE_NEON_FTS", false)),
		"force_path2_structure=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_FORCE_PATH2_STRUCTURE", false)),
		"phrase_hop=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_PHRASE_HOP", true)),
		"skip_sibling=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_SKIP_SIBLING", false)),
		"always_recovery=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_ALWAYS_RECOVERY", false)),
		"skip_recovery=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_SKIP_RECOVERY", false)),
		"recovery_llm=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_RECOVERY_LLM", false)),
		"rerank_enabled=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_RERANK", true)),
		"force_lexical_ce=" + strconv.FormatBool(envTruthy("OUROBOROS_ERB_FORCE_LEXICAL_CE", false)),
		"cohere_rerank_available=" + strconv.FormatBool(cohereKey() != ""),
		"ze_rerank_available=" + strconv.FormatBool(zeKey() != ""),
		"rerank_prefilter_n=" + itoa(rerankPrefilterMax()),
		"ranker=" + strings.ToLower(strings.TrimSpace(envOr("OUROBOROS_BRAIN_RANKER", ""))),
		"cohere_rerank_model=" + strings.TrimSpace(envOr("OUROBOROS_ERB_COHERE_RERANK_MODEL", "rerank-v3.5")),
		"ze_rerank_model=" + strings.TrimSpace(envOr("OUROBOROS_ERB_ZE_RERANK_MODEL", "zerank-2")),
		"mlx_rerank_model=" + mlxRankModel(),
		"agentic_mode=" + strings.ToLower(strings.TrimSpace(envOr("OUROBOROS_ERB_AGENTIC_MODE", "auto"))),
		"conf_top=" + strconv.FormatFloat(confTopThreshold(), 'f', -1, 64),
		"conf_mean3=" + strconv.FormatFloat(confMean3Threshold(), 'f', -1, 64),
		"dense_timeout_ms=" + strings.TrimSpace(envOr("OUROBOROS_ERB_DENSE_TIMEOUT_MS", "")),
		"hydrate_timeout_ms=" + strings.TrimSpace(envOr("OUROBOROS_ERB_HYDRATE_TIMEOUT_MS", "")),
		"structure_sql_ms=" + strings.TrimSpace(envOr("OUROBOROS_BRAIN_STRUCTURE_SQL_MS", "")),
		"structure_hydrate_ms=" + strings.TrimSpace(envOr("OUROBOROS_BRAIN_STRUCTURE_HYDRATE_MS", "")),
	}, "\x01")
	return cacheKey(cacheKeyParts{
		BrainID:        cfg.BrainID,
		Generation:     c.GenerationID(),
		Security:       secDigest,
		Question:       question,
		TopK:           topK,
		QuestionType:   opts.QuestionType,
		Mode:           opts.Mode,
		Plan:           plan,
		SourceTypes:    opts.SourceTypes,
		ExpandLite:     opts.ExpandLite,
		Profile:        prod,
		ConfigDigest:   configDigest,
		FilterIdentity: filter.Identity(),
	})
}

func cachedHotLexDocs(c *Client) int {
	if c == nil || c.hot == nil {
		return 0
	}
	return c.hot.Len()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func cloneDiag(d map[string]any) map[string]any {
	if d == nil {
		return nil
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		out[k] = v
	}
	return out
}

// get returns copies of the cached window and the lookup event
// (hit | miss | stale). Expired entries are lazily dropped, never served.
func (c *queryCache) get(key string) ([]Passage, map[string]any, string) {
	if c == nil {
		return nil, nil, cacheEventMiss
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, nil, cacheEventMiss
	}
	e := el.Value.(*cacheEntry)
	if time.Now().After(e.expires) {
		c.removeElement(el)
		c.stales++
		return nil, nil, cacheEventStale
	}
	c.ll.MoveToFront(el)
	c.hits++
	// Copy passages + diag so callers cannot mutate the cache entry (or race).
	ps := append([]Passage(nil), e.passages...)
	return ps, cloneDiag(e.diag), cacheEventHit
}

// put stores copies under key and returns how many entries were evicted.
// Eviction is bounded: expired entries first, then least-recently-used, until
// within max. Live entries are never wiped wholesale.
func (c *queryCache) put(key string, passages []Passage, diag map[string]any) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := &cacheEntry{
		key:      key,
		passages: append([]Passage(nil), passages...),
		diag:     cloneDiag(diag),
		expires:  time.Now().Add(c.ttl),
	}
	if el, ok := c.items[key]; ok {
		el.Value = entry
		c.ll.MoveToFront(el)
		return 0
	}
	c.items[key] = c.ll.PushFront(entry)
	evicted := 0
	now := time.Now()
	for el := c.ll.Back(); el != nil; {
		prev := el.Prev()
		if now.After(el.Value.(*cacheEntry).expires) {
			c.removeElement(el)
			c.evicted++
			evicted++
		}
		el = prev
	}
	for len(c.items) > c.max {
		el := c.ll.Back()
		if el == nil {
			break
		}
		c.removeElement(el)
		c.evicted++
		evicted++
	}
	return evicted
}

func (c *queryCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.items, el.Value.(*cacheEntry).key)
}

type queryCacheStats struct {
	Size    int
	Hits    int
	Misses  int
	Stales  int
	Evicted int
}

func (c *queryCache) stats() queryCacheStats {
	if c == nil {
		return queryCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return queryCacheStats{
		Size:    len(c.items),
		Hits:    c.hits,
		Misses:  c.misses,
		Stales:  c.stales,
		Evicted: c.evicted,
	}
}

// clear drops all entries (call after chunk / sidecar mutations). Explicit
// write-invalidation only — eviction pressure uses bounded LRU, not this.
func (c *queryCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll = list.New()
	c.items = map[string]*list.Element{}
}

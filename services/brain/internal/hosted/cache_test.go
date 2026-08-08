package hosted

import (
	"context"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

// Regression tests for issue #295: retrieve-cache keys must cover every
// request-shaping input, eviction must be bounded LRU/TTL (never a full-map
// wipe of live entries), gold diagnostics always bypass the cache, and cache
// failures fail open without broadening privilege.

func testCacheClient() *Client {
	return &Client{
		cfg: Config{
			BrainID:      "brain-a",
			PoolK:        40,
			TopK:         8,
			LexicalLimit: 30,
			RRFK:         60,
		},
		Security: productsec.Context{
			Profile:   productsec.ProfileSingleUser,
			Principal: "p1",
			Owner:     "o1",
		},
		qcache: newQueryCache(time.Minute),
	}
}

func cacheKeyOf(c *Client, question string, topK int, opts RetrieveOptions, plan QueryPlan, prod ProdProfile) string {
	filter, err := NormalizeMetadataFilter(opts.Filter, FilterAuthority{Tenant: c.cfg.BrainID})
	if err != nil {
		return ""
	}
	return c.retrieveCacheKey(question, topK, opts, plan, prod, filter)
}

// TestCacheKeyCoversRequestShapingInputs proves that changing any
// request-shaping input changes the key (no stale/cross-mode/cross-tenant reuse).
func TestCacheKeyCoversRequestShapingInputs(t *testing.T) {
	base := testCacheClient()
	baseOpts := RetrieveOptions{TopK: 8, QuestionType: "basic", Mode: "light"}
	basePlan := QueryPlan{EffectiveType: "basic", Mode: "light"}
	baseProd := ProdProfile{Enabled: true}
	key := cacheKeyOf(base, "what is the rpo?", 8, baseOpts, basePlan, baseProd)

	type mutation struct {
		name  string
		apply func(c *Client, opts *RetrieveOptions, plan *QueryPlan, prod *ProdProfile)
	}
	mutations := []mutation{
		{"brain", func(c *Client, _ *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {
			c.cfg.BrainID = "brain-b"
		}},
		{"question", func(_ *Client, _ *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {}}, // handled below
		{"topk", func(_ *Client, o *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) { o.TopK = 16 }},
		{"question_type", func(_ *Client, o *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {
			o.QuestionType = "multi_doc"
		}},
		{"mode", func(_ *Client, o *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) { o.Mode = "deep" }},
		{"plan", func(_ *Client, _ *RetrieveOptions, p *QueryPlan, _ *ProdProfile) { p.MultiDoc = true }},
		{"plan_mode", func(_ *Client, _ *RetrieveOptions, p *QueryPlan, _ *ProdProfile) { p.Mode = "research" }},
		{"source_types", func(_ *Client, o *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {
			o.SourceTypes = []string{"policy"}
		}},
		{"expand_lite", func(_ *Client, o *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {
			o.ExpandLite = true
		}},
		{"prod_profile", func(_ *Client, _ *RetrieveOptions, _ *QueryPlan, pr *ProdProfile) {
			pr.Quality = true
		}},
		{"config", func(c *Client, _ *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {
			c.cfg.PoolK = 96
		}},
		{"security_principal", func(c *Client, _ *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {
			c.Security.Principal = "p2"
		}},
		{"security_profile", func(c *Client, _ *RetrieveOptions, _ *QueryPlan, _ *ProdProfile) {
			c.Security.Profile = productsec.ProfileMultiPrincipal
		}},
	}
	for _, m := range mutations {
		c := testCacheClient()
		opts := baseOpts
		plan := basePlan
		prod := baseProd
		m.apply(c, &opts, &plan, &prod)
		q := "what is the rpo?"
		if m.name == "question" {
			q = "what is the rto?"
		}
		if got := cacheKeyOf(c, q, opts.TopK, opts, plan, prod); got == key {
			t.Fatalf("cache key must change when %s changes", m.name)
		}
	}
}

func TestCacheKeyCoversEffectiveConfidenceThresholds(t *testing.T) {
	c := testCacheClient()
	opts := RetrieveOptions{TopK: 8, QuestionType: "basic", Mode: "light"}
	plan := QueryPlan{EffectiveType: "basic", Mode: "light"}
	prod := ProdProfile{Enabled: true}
	t.Setenv("OUROBOROS_ERB_CONF_TOP", "")
	t.Setenv("OUROBOROS_ERB_CONF_MEAN3", "")
	before := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
	t.Setenv("OUROBOROS_ERB_CONF_TOP", "0.50")
	explicitDefault := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
	if before != explicitDefault {
		t.Fatal("cache key must canonicalize implicit and explicit confidence top defaults")
	}
	for _, invalid := range []string{"abc", "NaN", "+Inf", "-Inf", "-1", "2"} {
		t.Setenv("OUROBOROS_ERB_CONF_TOP", invalid)
		invalidDefault := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
		if before != invalidDefault {
			t.Fatalf("cache key must canonicalize invalid confidence top %q to its effective default", invalid)
		}
	}
	t.Setenv("OUROBOROS_ERB_CONF_TOP", "0.9")
	afterTop := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
	if before == afterTop {
		t.Fatal("cache key ignored effective confidence top threshold")
	}
	t.Setenv("OUROBOROS_ERB_CONF_TOP", "")
	t.Setenv("OUROBOROS_ERB_CONF_MEAN3", "0.9")
	afterMean := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
	if before == afterMean {
		t.Fatal("cache key ignored effective confidence mean threshold")
	}
}

func TestCacheKeyCoversArmBudgetAndOfficialFlags(t *testing.T) {
	c := testCacheClient()
	opts := RetrieveOptions{TopK: 8, QuestionType: "basic", Mode: "light"}
	plan := QueryPlan{EffectiveType: "basic", Mode: "light"}
	prod := ProdProfile{Enabled: true}
	boolFlags := []string{
		"OUROBOROS_ERB_SKIP_FTS",
		"OUROBOROS_ERB_SKIP_DENSE",
		"OUROBOROS_ERB_OFFICIAL",
		"OUROBOROS_ERB_OFFICIAL_JUDGE",
		"OUROBOROS_ERB_FORCE_FTS",
		"OUROBOROS_ERB_FORCE_RESIDUAL",
		"OUROBOROS_ERB_QUALITY_RESIDUAL",
		"OUROBOROS_ERB_FORCE_NEON_FTS",
		"OUROBOROS_ERB_FORCE_PATH2_STRUCTURE",
		"OUROBOROS_ERB_SKIP_SIBLING",
		"OUROBOROS_ERB_ALWAYS_RECOVERY",
		"OUROBOROS_ERB_SKIP_RECOVERY",
		"OUROBOROS_ERB_RECOVERY_LLM",
		"OUROBOROS_ERB_RERANK",
		"OUROBOROS_ERB_FORCE_LEXICAL_CE",
	}
	for _, flag := range boolFlags {
		t.Run(flag, func(t *testing.T) {
			t.Setenv(flag, "0")
			before := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
			t.Setenv(flag, "1")
			after := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
			if before == after {
				t.Fatalf("cache key ignored %s", flag)
			}
		})
	}
	for _, provider := range []struct {
		name    string
		primary string
		aliases []string
	}{
		{name: "cohere", primary: "COHERE_API_KEY", aliases: []string{"CO_API_KEY", "SENTRA_COHERE_API_KEY"}},
		{name: "zeroentropy", primary: "ZEROENTROPY_API_KEY", aliases: []string{"SENTRA_ZEROENTROPY_API_KEY", "ZE_API_KEY"}},
	} {
		t.Run(provider.name+"_availability", func(t *testing.T) {
			t.Setenv(provider.primary, "")
			for _, alias := range provider.aliases {
				t.Setenv(alias, "")
			}
			before := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
			t.Setenv(provider.primary, "configured")
			after := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
			if before == after {
				t.Fatalf("cache key ignored %s provider availability", provider.name)
			}
		})
	}
	valueFlags := []string{
		"OUROBOROS_ERB_RERANK_PREFILTER_N",
		"OUROBOROS_BRAIN_RANKER",
		"OUROBOROS_ERB_COHERE_RERANK_MODEL",
		"OUROBOROS_ERB_ZE_RERANK_MODEL",
		"OUROBOROS_BRAIN_MLX_RANK_MODEL",
		"OUROBOROS_ERB_AGENTIC_MODE",
		"OUROBOROS_ERB_DENSE_TIMEOUT_MS",
		"OUROBOROS_ERB_HYDRATE_TIMEOUT_MS",
		"OUROBOROS_BRAIN_STRUCTURE_SQL_MS",
		"OUROBOROS_BRAIN_STRUCTURE_HYDRATE_MS",
	}
	for _, flag := range valueFlags {
		t.Run(flag, func(t *testing.T) {
			t.Setenv(flag, "")
			before := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
			t.Setenv(flag, "37")
			after := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
			if before == after {
				t.Fatalf("cache key ignored %s", flag)
			}
		})
	}
}

func TestRetrieveCacheKeyIncludesForceNeonFTSArmSelection(t *testing.T) {
	c := testCacheClient()
	opts := RetrieveOptions{TopK: 8, QuestionType: "basic", Mode: "light"}
	plan := QueryPlan{EffectiveType: "basic", Mode: "light"}
	prod := ProdProfile{Enabled: true}

	t.Setenv("OUROBOROS_ERB_FORCE_NEON_FTS", "0")
	normal := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
	t.Setenv("OUROBOROS_ERB_FORCE_NEON_FTS", "1")
	forced := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
	if normal == forced {
		t.Fatal("force-Neon arm selection must not reuse an unforced retrieval window")
	}
}

func TestCacheKeyCapturesHotLexAvailability(t *testing.T) {
	c := testCacheClient()
	opts := RetrieveOptions{TopK: 8}
	before := cacheKeyOf(c, "what is the rpo?", 8, opts, QueryPlan{}, ProdProfile{Enabled: true})
	c.hot = NewHotLex(c.cfg.BrainID)
	c.hot.AddChunk("chunk", "doc", "RPO is fifteen minutes", "")
	c.hot.Finalize()
	after := cacheKeyOf(c, "what is the rpo?", 8, opts, QueryPlan{}, ProdProfile{Enabled: true})
	if before == after {
		t.Fatal("cache key ignored request-visible HotLex projection state")
	}
}

// TestCacheKeyGenerationChangeBusts proves a generation bump yields a new key,
// so post-ingest retrieves cannot reuse pre-ingest windows.
func TestCacheKeyGenerationChangeBusts(t *testing.T) {
	c := testCacheClient()
	d := &durableStore{}
	d.gen.Store("gen-1")
	c.local = d
	opts := RetrieveOptions{TopK: 8}
	k1 := cacheKeyOf(c, "q", 8, opts, QueryPlan{}, ProdProfile{})
	d.gen.Store("gen-2")
	k2 := cacheKeyOf(c, "q", 8, opts, QueryPlan{}, ProdProfile{})
	if k1 == k2 {
		t.Fatal("cache key must change across generations")
	}
}

// TestCacheKeyQuestionNormalization proves case/whitespace-only differences
// collapse to one entry (cache efficiency) while real text changes do not.
func TestCacheKeyQuestionNormalization(t *testing.T) {
	c := testCacheClient()
	opts := RetrieveOptions{TopK: 8}
	plan := QueryPlan{}
	prod := ProdProfile{}
	k1 := cacheKeyOf(c, "  What  IS   the RPO? ", 8, opts, plan, prod)
	k2 := cacheKeyOf(c, "what is the rpo?", 8, opts, plan, prod)
	if k1 != k2 {
		t.Fatal("case/whitespace-only differences must normalize to the same key")
	}
	k3 := cacheKeyOf(c, "what is the rto?", 8, opts, plan, prod)
	if k3 == k1 {
		t.Fatal("different question text must produce a different key")
	}
}

// TestQueryCacheBoundedEviction proves eviction is bounded LRU — never a
// full-map wipe that destroys live entries under pressure.
func TestQueryCacheBoundedEviction(t *testing.T) {
	c := newQueryCacheBounded(time.Minute, 4)
	mk := func(id string) ([]Passage, map[string]any) {
		return []Passage{{DocumentID: id, Text: "text " + id}}, map[string]any{"id": id}
	}
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6"}
	for _, k := range keys {
		ps, d := mk(k)
		c.put(k, ps, d)
	}
	st := c.stats()
	if st.Size > 4 {
		t.Fatalf("cache size %d exceeds bound 4", st.Size)
	}
	if st.Evicted == 0 {
		t.Fatal("expected eviction pressure to be recorded")
	}
	// Oldest entries evicted...
	if _, _, ev := c.get("k1"); ev == cacheEventHit {
		t.Fatal("oldest entry k1 must be evicted under pressure")
	}
	if _, _, ev := c.get("k2"); ev == cacheEventHit {
		t.Fatal("second-oldest entry k2 must be evicted under pressure")
	}
	// ...but live recent entries survive (no full-map wipe).
	for _, k := range []string{"k3", "k4", "k5", "k6"} {
		if _, _, ev := c.get(k); ev != cacheEventHit {
			t.Fatalf("recent entry %s must survive bounded eviction (got event %q)", k, ev)
		}
	}
}

// TestQueryCacheLRURefreshOnHit proves a hit refreshes recency, so hot entries
// survive pressure that evicts colder ones.
func TestQueryCacheLRURefreshOnHit(t *testing.T) {
	c := newQueryCacheBounded(time.Minute, 3)
	for _, k := range []string{"a", "b", "c"} {
		c.put(k, []Passage{{DocumentID: k}}, nil)
	}
	// Touch "a" so "b" becomes the least-recently-used.
	if _, _, ev := c.get("a"); ev != cacheEventHit {
		t.Fatal("expected hit for a")
	}
	c.put("d", []Passage{{DocumentID: "d"}}, nil)
	if _, _, ev := c.get("b"); ev == cacheEventHit {
		t.Fatal("b was least-recently-used and must be evicted first")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, _, ev := c.get(k); ev != cacheEventHit {
			t.Fatalf("entry %s must survive (got event %q)", k, ev)
		}
	}
}

// TestQueryCacheTTLStale proves expired entries miss with a stale event and are
// lazily dropped rather than served.
func TestQueryCacheTTLStale(t *testing.T) {
	c := newQueryCacheBounded(15*time.Millisecond, 4)
	c.put("k", []Passage{{DocumentID: "d"}}, nil)
	time.Sleep(30 * time.Millisecond)
	if _, _, ev := c.get("k"); ev != cacheEventStale {
		t.Fatalf("expired entry must report stale, got %q", ev)
	}
	st := c.stats()
	if st.Stales != 1 {
		t.Fatalf("stale counter = %d, want 1", st.Stales)
	}
	if st.Size != 0 {
		t.Fatalf("stale entry must be lazily removed, size = %d", st.Size)
	}
}

// TestQueryCacheCopiesOnGetPut proves callers cannot mutate cached state.
func TestQueryCacheCopiesOnGetPut(t *testing.T) {
	c := newQueryCacheBounded(time.Minute, 4)
	ps := []Passage{{DocumentID: "d1", Text: "original"}}
	diag := map[string]any{"k": "v"}
	c.put("k", ps, diag)
	// Mutate caller-owned inputs after put.
	ps[0].Text = "mutated"
	diag["k"] = "mutated"
	got, gd, ev := c.get("k")
	if ev != cacheEventHit {
		t.Fatalf("expected hit, got %q", ev)
	}
	if got[0].Text != "original" || gd["k"] != "v" {
		t.Fatal("cache must store copies, not caller references")
	}
	// Mutate returned values; next get must be unaffected.
	got[0].Text = "mutated-again"
	gd["k"] = "mutated-again"
	got2, gd2, _ := c.get("k")
	if got2[0].Text != "original" || gd2["k"] != "v" {
		t.Fatal("cache must return copies, not cached references")
	}
}

// TestQueryCacheFailOpen proves a nil cache never breaks retrieval paths and
// never widens what a caller can see (every get is a miss).
func TestQueryCacheFailOpen(t *testing.T) {
	var c *queryCache
	if _, _, ev := c.get("k"); ev != cacheEventMiss {
		t.Fatalf("nil cache get must fail open as miss, got %q", ev)
	}
	if n := c.put("k", []Passage{{DocumentID: "d"}}, nil); n != 0 {
		t.Fatalf("nil cache put must be a no-op, evicted %d", n)
	}
	if st := c.stats(); st != (queryCacheStats{}) {
		t.Fatalf("nil cache stats must be zero, got %+v", st)
	}
	c.clear() // must not panic
}

// --- integration: full retrieve path through the in-process cache ---

func seedCacheTestBrain(t *testing.T, c *Client, brainID string) {
	t.Helper()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, brainID, []ChunkWrite{
		{DocumentID: "doc_rpo", ChunkID: "c1", Text: "MedThink cache RPO fifteen minutes recovery tier alpha."},
		{DocumentID: "doc_rto", ChunkID: "c2", Text: "MedThink cache RTO four hours failover tier beta."},
		{DocumentID: "doc_noise", ChunkID: "c3", Text: "Picnic weather unrelated noise document."},
	}); err != nil {
		t.Fatal(err)
	}
}

func cachePassageIDs(ps []Passage) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.DocumentID + "/" + p.ChunkID
	}
	return out
}

// TestRetrieveCacheColdWarmIdentical proves cold and warm asks produce
// identical results within the same pinned config, and the warm ask reports
// hit diagnostics without gold/raw-evidence cache fields.
func TestRetrieveCacheColdWarmIdentical(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("cache-coldwarm")
	defer c.Close()
	seedCacheTestBrain(t, c, "cache-coldwarm")
	ctx := context.Background()
	opts := RetrieveOptions{TopK: 4, QuestionType: "basic"}
	cold, _, err := c.RetrieveOpts(ctx, "MedThink cache recovery failover", opts)
	if err != nil {
		t.Fatal(err)
	}
	warm, diagWarm, err := c.RetrieveOpts(ctx, "MedThink cache recovery failover", opts)
	if err != nil {
		t.Fatal(err)
	}
	if diagWarm["cache_hit"] != true {
		t.Fatalf("warm ask must be a cache hit, diag cache_hit=%#v", diagWarm["cache_hit"])
	}
	if diagWarm["cache_event"] != cacheEventHit {
		t.Fatalf("warm ask must report cache_event=hit, got %#v", diagWarm["cache_event"])
	}
	coldIDs, warmIDs := cachePassageIDs(cold), cachePassageIDs(warm)
	if len(coldIDs) == 0 {
		t.Fatal("cold ask returned no passages")
	}
	if len(coldIDs) != len(warmIDs) {
		t.Fatalf("cold/warm passage count differs: %v vs %v", coldIDs, warmIDs)
	}
	for i := range coldIDs {
		if coldIDs[i] != warmIDs[i] {
			t.Fatalf("cold/warm passage %d differs: %v vs %v", i, coldIDs, warmIDs)
		}
	}
	// Cache diagnostics must not carry gold or raw evidence fields.
	for _, k := range []string{"pool_recall", "window_recall", "gold_in_pool", "gold_in_window"} {
		if _, ok := diagWarm[k]; ok {
			t.Fatalf("cache-hit diag must not carry gold field %q", k)
		}
	}
}

// TestRetrieveCacheModeChangeBusts proves a product-mode change cannot reuse a
// window cached under a different mode.
func TestRetrieveCacheModeChangeBusts(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("cache-mode")
	defer c.Close()
	seedCacheTestBrain(t, c, "cache-mode")
	ctx := context.Background()
	q := "MedThink cache recovery failover"
	if _, _, err := c.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4, Mode: "light"}); err != nil {
		t.Fatal(err)
	}
	_, diag, err := c.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4, Mode: "deep"})
	if err != nil {
		t.Fatal(err)
	}
	if diag["cache_hit"] == true {
		t.Fatal("mode change must bust the retrieve cache")
	}
}

// TestRetrieveCachePlanChangeBusts proves an explicit capability-plan change
// cannot reuse a window cached under a different resolved plan.
func TestRetrieveCachePlanChangeBusts(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("cache-plan")
	defer c.Close()
	seedCacheTestBrain(t, c, "cache-plan")
	ctx := context.Background()
	q := "MedThink cache recovery failover"
	planA := QueryPlan{EffectiveType: "basic"}
	if _, _, err := c.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4, Plan: &planA}); err != nil {
		t.Fatal(err)
	}
	planB := QueryPlan{EffectiveType: "basic", MultiDoc: true, Completeness: true}
	_, diag, err := c.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4, Plan: &planB})
	if err != nil {
		t.Fatal(err)
	}
	if diag["cache_hit"] == true {
		t.Fatal("plan change must bust the retrieve cache")
	}
}

// TestRetrieveCacheTenantIsolation proves two tenants sharing a client/cache
// cannot see each other's cached windows (brain is part of the key).
func TestRetrieveCacheTenantIsolation(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	ca := OpenMemory("tenant-a")
	defer ca.Close()
	seedCacheTestBrain(t, ca, "tenant-a")
	cb := ca.WithBrainID("tenant-b")
	seedCacheTestBrain(t, cb, "tenant-b")
	ctx := context.Background()
	q := "MedThink cache recovery failover"
	if _, _, err := ca.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4}); err != nil {
		t.Fatal(err)
	}
	psB, diagB, err := cb.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if diagB["cache_hit"] == true {
		t.Fatal("tenant-b must not cache-hit tenant-a's window")
	}
	for _, p := range psB {
		if p.DocumentID != "" && p.DocumentID != "doc_rpo" && p.DocumentID != "doc_rto" && p.DocumentID != "doc_noise" {
			t.Fatalf("tenant-b saw foreign passage %q", p.DocumentID)
		}
	}
}

// TestRetrieveCacheSecurityChangeBusts proves a security-principal change
// cannot reuse windows cached under a different principal (fail closed on
// identity, fail open on cache errors).
func TestRetrieveCacheSecurityChangeBusts(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("cache-sec")
	defer c.Close()
	seedCacheTestBrain(t, c, "cache-sec")
	ctx := context.Background()
	q := "MedThink cache recovery failover"
	c.SetSecurity(productsec.Context{Profile: productsec.ProfileSingleUser, Principal: "alice", Owner: "alice"})
	if _, _, err := c.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4}); err != nil {
		t.Fatal(err)
	}
	c.SetSecurity(productsec.Context{Profile: productsec.ProfileSingleUser, Principal: "bob", Owner: "bob"})
	_, diag, err := c.RetrieveOpts(ctx, q, RetrieveOptions{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if diag["cache_hit"] == true {
		t.Fatal("security principal change must bust the retrieve cache")
	}
}

// TestRetrieveCacheWriteInvalidates proves product writes still invalidate
// cached windows so post-write retrieves see fresh chunks.
func TestRetrieveCacheWriteInvalidates(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("cache-invalidate")
	defer c.Close()
	seedCacheTestBrain(t, c, "cache-invalidate")
	ctx := context.Background()
	q := "MedThink cache recovery failover"
	opts := RetrieveOptions{TopK: 4}
	if _, _, err := c.RetrieveOpts(ctx, q, opts); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertChunks(ctx, "cache-invalidate", []ChunkWrite{
		{DocumentID: "doc_new", ChunkID: "c4", Text: "MedThink cache recovery failover brand new evidence."},
	}); err != nil {
		t.Fatal(err)
	}
	_, diag, err := c.RetrieveOpts(ctx, q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if diag["cache_hit"] == true {
		t.Fatal("post-write retrieve must not serve the pre-write cached window")
	}
}

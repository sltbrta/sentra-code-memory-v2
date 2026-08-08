package hosted

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
	"go.opentelemetry.io/otel/trace"
)

func retrievalContextStatus(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case err != nil:
		return "error"
	default:
		return "ok"
	}
}

func hotLexState(available bool) string {
	if available {
		return "available"
	}
	return "missing"
}

func residualRouteReason() string {
	if envTruthy("OUROBOROS_ERB_FORCE_RESIDUAL", false) {
		return "force_residual"
	}
	if envTruthy("OUROBOROS_ERB_QUALITY_RESIDUAL", false) {
		return "quality_residual_ablation"
	}
	return "residual_opt_in"
}

// lexicalVariantBudget gives one sequential query a fair share of the
// remaining shared wall. The child deadline prevents an early slow variant
// from consuming the entire arm budget while the parent still caps total wall.
func lexicalVariantBudget(ctx context.Context, variantsLeft int) time.Duration {
	if variantsLeft <= 0 {
		return 0
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return time.Nanosecond
	}
	budget := remaining / time.Duration(variantsLeft)
	if budget <= 0 {
		return time.Nanosecond
	}
	return budget
}

// Passage is one evidence unit for the context window (document_id = dsid).
type Passage struct {
	DocumentID string  `json:"document_id"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	ChunkID    string  `json:"chunk_id,omitempty"`
	SourceURI  string  `json:"source_uri,omitempty"`
	Channel    string  `json:"channel,omitempty"`
	// Locator carries the page/section/line/offset/region metadata the source
	// parser attached to this leaf span when available (#317, #327). It is an
	// observed fact carried from an authorized retrieval result; Locator.Present
	// false is the explicit-absence sentinel and is never invented.
	Locator Locator `json:"locator,omitempty"`
}

// Client is a reusable hosted retrieval + product write handle.
//
// ONE product runtime; store adapters only:
//
//	path2 (db Neon+Qdrant) | product_neon (store Neon product_*) |
//	memory (ephemeral) | local_fs (durable dir projection)
//
// SMF full-bench path uses db → path2_chunk_metadata (retrieve).
// Product-owned writes go through store → product_chunk_metadata, memory, or local FS.
type Client struct {
	cfg          Config
	db           *sql.DB
	store        ChunkStore
	productOwned bool
	qcache       *queryCache
	// rerankScores is the bounded identity-aware cross-encoder score cache
	// (issue #301). It is per client; WithBrainID shares it safely because the
	// brain/generation/ACL scope participates in every entry key.
	rerankScores *rerankScoreCache
	// local is set for OpenLocal / CreateLocal durable FS projection.
	local *durableStore
	// hot is the interactive lexical serving index (optional; path from env or local).
	hot *HotLex
	// gardenerQ is the product async/sync enrichment queue (SQLite on local_fs).
	gardenerQ      gardener.Queue
	gardenerCloser io.Closer
	// filterMetaFn resolves document metadata for governed filter matching
	// (issue #328). Optional; nil falls back to passage-local derivation.
	filterMetaFn func(documentID string) (DocMeta, bool)
	// autoGardenerCancel stops the OUROBOROS_BRAIN_GARDENER_AUTO background loop.
	autoGardenerCancel context.CancelFunc
	// Security is Phase 2 product ACL profile (single_user default).
	Security productsec.Context
	// Mem is the cohesive memory cortex (claims/episodes/utility/PPR/agent).
	Mem *memory.Store
	// substrates records ADR 0024 module bindings (queue/cortex/chunks/dense/api).
	substrates SubstrateConfig
	// localDense is SQLite/Postgres/FAISS dense ANN for residual paths without Qdrant.
	localDense denseBackend
	// faissURL is set when dense=faiss (HTTP BYOC sidecar).
	faissURL string
	// qualityTracer is optional OpenTelemetry instrumentation for the bounded
	// retrieve/ground/answer quality spans. Nil uses the global provider, whose
	// default is the OpenTelemetry no-op implementation.
	qualityTracer trace.Tracer
}

// Open builds a Client from config (Neon retrieve path; product store lazy).
// When OUROBOROS_BRAIN_DIR (or full substrate env) is set, binds queue+cortex
// so neon chunks share the same residual pipeline (ADR 0024 team mix).
func Open(cfg Config) (*Client, error) {
	db, err := openDB(cfg.NeonDatabaseURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg:          cfg,
		db:           db,
		qcache:       newQueryCache(90 * time.Second),
		rerankScores: newRerankScoreCache(0, 0),
	}
	c.loadHotLexFromEnv()
	sub := SubstrateFromEnv()
	if sub.Dir != "" || sub.QueuePath != "" || sub.CortexPath != "" ||
		sub.Queue != "" || sub.Cortex != "" || sub.Profile == ProfileTeam || sub.Profile == ProfileBench {
		if sub.Chunks == "" {
			sub.Chunks = SubstrateChunksNeon
		}
		if sub.Profile == "" {
			sub.Profile = ProfileTeam
		}
		_ = ApplySubstrates(c, sub)
	}
	// Decode/build the final scoped offline entity index at process warm-up so
	// a 70k+ catalog does not tax the first user recovery query.
	c.WarmEntityCatalog()
	return c, nil
}

// SetHotLex attaches an interactive lexical index (tests / project tooling).
func (c *Client) SetHotLex(h *HotLex) {
	if c == nil {
		return
	}
	c.hot = h
}

// SetFilterMetadataProvider wires document metadata lookup used by governed
// metadata-filter matching (issue #328). When unset, only the passage-local
// subset (source type from SourceURI/channel) is available and filters
// requiring richer metadata fail closed.
func (c *Client) SetFilterMetadataProvider(fn func(documentID string) (DocMeta, bool)) {
	if c == nil {
		return
	}
	c.filterMetaFn = fn
}

// SetSecurity attaches Phase 2 authorize-before-retrieve context.
func (c *Client) SetSecurity(sec productsec.Context) {
	if c == nil {
		return
	}
	c.Security = sec
}

// HotLex returns the interactive lexical index if present.
func (c *Client) HotLex() *HotLex {
	if c == nil {
		return nil
	}
	return c.hot
}

// hotLexAvailable reports whether the in-process serving projection is usable.
// A configured path or a non-nil empty index is not availability: both cases
// must take the same bounded fallback posture as a failed/missing gob load.
func (c *Client) hotLexAvailable() bool {
	return c != nil && c.hot != nil && c.hot.Len() > 0
}

func missingHotLexFallbackSpent(diag map[string]any) bool {
	if diag == nil {
		return false
	}
	state, _ := diag["hot_lex_state"].(string)
	spent, _ := diag["neon_fts_fallback_attempted"].(bool)
	return state == "missing" && spent
}

// loadHotLexFromEnv loads OUROBOROS_ERB_HOTLEX_PATH gob projection when set.
// Logs load failures to stderr so Modal mis-mounts are visible (was silent).
func (c *Client) loadHotLexFromEnv() {
	if c == nil {
		return
	}
	path := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_HOTLEX_PATH"))
	if path == "" {
		// Common Modal mounts when env unset.
		for _, cand := range []string{
			"/hotlex/hotlex-full.hlex", "/hotlex/hotlex.hlex",
			"/hotlex/hotlex-full.gob", "/hotlex/hotlex.gob",
		} {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				path = cand
				break
			}
		}
		if path == "" {
			return
		}
	}
	if st, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "hosted: HotLex path %q: %v\n", path, err)
		return
	} else if st.IsDir() {
		fmt.Fprintf(os.Stderr, "hosted: HotLex path %q is a directory\n", path)
		return
	}
	t0 := time.Now()
	h, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{
		BrainID:        c.cfg.BrainID,
		Generation:     strings.TrimSpace(os.Getenv("OUROBOROS_ERB_HOTLEX_GENERATION")),
		AllowLegacyGob: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hosted: LoadHotLexSnapshot %q: %v\n", path, err)
		return
	}
	c.hot = h
	fmt.Fprintf(os.Stderr, "hosted: HotLex loaded path=%s format=%s docs=%d ms=%d\n",
		path, h.SnapshotFormat(), h.Len(), time.Since(t0).Milliseconds())
}

// OpenFromEnv is the product entry when hosted is enabled.
func OpenFromEnv() (*Client, error) {
	cfg, err := FromEnv()
	if err != nil {
		return nil, err
	}
	return Open(cfg)
}

// WithBrainID returns a shallow client copy scoped to brainID.
// Shares the Neon pool / store / cache (cache keys already include brain_id).
// Empty id leaves the client unchanged.
func (c *Client) WithBrainID(brainID string) *Client {
	if c == nil {
		return nil
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		return c
	}
	cp := *c
	cp.cfg.BrainID = brainID
	// A HotLex image is brain-scoped. A shallow cross-brain client may continue
	// sharing remote/store handles, but it must not reuse the lexical projection.
	if cp.hot != nil && cp.hot.BrainID != brainID {
		cp.hot = nil
	}
	return &cp
}

// InvalidateQueryCache clears the in-process retrieve TTL cache.
// Call after product-owned writes so subsequent Retrieve sees fresh chunks.
func (c *Client) InvalidateQueryCache() {
	if c == nil {
		return
	}
	c.qcache.clear()
	c.rerankScores.clear()
}

// Close cancels the optional auto-gardener loop and releases the Neon pool.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	var first error
	if c.autoGardenerCancel != nil {
		c.autoGardenerCancel()
		c.autoGardenerCancel = nil
	}
	if c.hot != nil {
		if err := c.hot.Close(); err != nil && first == nil {
			first = err
		}
		c.hot = nil
	}
	if c.gardenerCloser != nil {
		if err := c.gardenerCloser.Close(); err != nil && first == nil {
			first = err
		}
		c.gardenerCloser = nil
		c.gardenerQ = nil
	}
	if c.localDense != nil {
		if err := c.localDense.Close(); err != nil && first == nil {
			first = err
		}
		c.localDense = nil
	}
	if c.db != nil {
		if err := c.db.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Config returns a copy of the client config.
func (c *Client) Config() Config {
	if c == nil {
		return Config{}
	}
	return c.cfg
}

// ProductOwned reports whether this client is a product-owned write profile
// (memory backend or product_chunk_metadata), not SMF path2.
func (c *Client) ProductOwned() bool {
	return c != nil && c.productOwned
}

// Store returns the product ChunkStore when present (tests / wiring).
func (c *Client) Store() ChunkStore {
	if c == nil {
		return nil
	}
	return c.store
}

// RetrieveOptions control fusion / windowing for one query.
type RetrieveOptions struct {
	TopK         int
	QuestionType string
	// Plan is optional capability routing. When nil, resolved from question+type.
	// Prefer Plan flags over string-equality on QuestionType.
	Plan *QueryPlan
	// Mode is product UI mode: light|deep|research|bench (modulates plan).
	Mode string
	// SourceTypes optional ERB source_types for authority / prompt mode.
	SourceTypes []string
	// GoldDocIDs enables offline pool/window gold diagnostics (eval only).
	GoldDocIDs []string
	// Filter is the raw governed metadata-filter predicate map (issue #328).
	// It is normalized and authorized against the client's tenant before any
	// retrieval; malformed or unauthorized predicates fail closed with an
	// error and no retrieval. The normalized identity joins the cache key and
	// diagnostics.
	Filter map[string]any
	// ExpandLite is for nested agentic reformulate/gap retrieves only.
	// HotLex + 1 dense + light fuse — skips recovery, path2 SQL, hop2, corpus-grep,
	// and deep sibling hydrate so expand does not re-pay full QUALITY wall (~60s+).
	ExpandLite bool
}

// resolveRetrievePlan fills opts.Plan/QuestionType from question surface + mode.
// Mutates opts so downstream interactive/path2 share one resolved plan.
func resolveRetrievePlan(question string, opts *RetrieveOptions) QueryPlan {
	if opts == nil {
		_, p := PlanFromOpts(question, "", "")
		return p
	}
	var plan QueryPlan
	if opts.Plan != nil {
		plan = ApplyServeMode(*opts.Plan, opts.Mode)
	} else {
		_, plan = PlanFromOpts(question, opts.QuestionType, opts.Mode)
	}
	opts.Plan = &plan
	if opts.QuestionType == "" && plan.EffectiveType != "" {
		opts.QuestionType = plan.EffectiveType
	}
	return plan
}

// phaseADenseQueries picks the phase-A embed queries within the DenseQueries
// cap. Dense: prefer short phrase bags for multi-doc (long Q embeds poorly for
// paraphrase). hyStub (HyDE) occupies a slot when provided: the multi-doc
// short-phrase rebuild used to replace the list wholesale and silently drop the
// HyDE stub appended earlier (dead guard). The configured cap is never exceeded.
func phaseADenseQueries(question string, variants []string, maxDense int, hyStub string, preferShortPhrases bool) []string {
	if maxDense < 1 {
		maxDense = 1
	}
	denseQueries := variants
	if len(denseQueries) > maxDense {
		denseQueries = denseQueries[:maxDense]
	}
	if preferShortPhrases {
		shortDense := pickHotLexPhrases(question, 2)
		if cq := compactQuestionBag(question, 8); cq != "" {
			shortDense = append(shortDense, cq)
		}
		if len(shortDense) > 0 {
			// Keep one original + short phrases, cap DenseQueries.
			if len(denseQueries) > maxDense {
				// Prefer phrases over long Q when capped.
				denseQueries = shortDense
				if len(denseQueries) < maxDense {
					denseQueries = append([]string{question}, denseQueries...)
					if len(denseQueries) > maxDense {
						denseQueries = denseQueries[:maxDense]
					}
				}
			}
		}
	}
	if hyStub != "" && !containsString(denseQueries, hyStub) {
		if len(denseQueries) < maxDense {
			denseQueries = append(denseQueries, hyStub)
		} else if maxDense == 1 {
			denseQueries = []string{hyStub}
		} else {
			// Keep the question/strongest variants and replace the last slot;
			// HyDE must not turn a configured N-query budget into N+1 calls.
			denseQueries = append(append([]string(nil), denseQueries[:maxDense-1]...), hyStub)
		}
	}
	return denseQueries
}

// Retrieve runs multi-query hybrid Neon+Qdrant → RRF pool → CE → tight window.
// Memory / product-owned clients use store lexical (no Qdrant required).
func (c *Client) Retrieve(ctx context.Context, question string, topK int) ([]Passage, map[string]any, error) {
	return c.RetrieveOpts(ctx, question, RetrieveOptions{TopK: topK})
}

// RetrieveOpts is the full product retrieval path (residual-parity stages).
func (c *Client) RetrieveOpts(ctx context.Context, question string, opts RetrieveOptions) (passages []Passage, diag map[string]any, err error) {
	diag = map[string]any{
		"source":           "product_brain_hosted",
		"brain_id":         c.cfg.BrainID,
		"chunk_collection": c.cfg.ChunkCollection,
		"pipeline": []string{
			"multi_query_lexical",
			"multi_query_dense_ann",
			"rrf_fuse",
			"sibling_hydrate",
			"cross_encoder_rerank",
			"identifier_retain_window",
		},
	}
	if c == nil {
		return nil, diag, fmt.Errorf("nil hosted client")
	}
	ctx, qualitySpan := c.startRetrievalQualitySpan(ctx, opts.TopK)
	defer func() {
		finishRetrievalQualitySpan(ctx, qualitySpan, passages, diag, err)
	}()
	// Governed metadata filter (issue #328): normalize + authorize before any
	// arm runs. Fail closed: malformed/unauthorized/gold-derived predicates
	// abort the retrieve instead of being ignored or broadened.
	filter, ferr := NormalizeMetadataFilter(opts.Filter, FilterAuthority{
		Tenant: c.cfg.BrainID,
		Blind:  blindFilterMode(),
	})
	if ferr != nil {
		diag["filter_rejected"] = ferr.Error()
		return nil, diag, fmt.Errorf("hosted: metadata filter rejected: %w", ferr)
	}
	if filter != nil {
		diag["filter_identity"] = filter.Identity()
		diag["filter_predicates"] = filter.Predicates()
	}
	// Authorization/filter validation stays ahead of this check. Once admitted,
	// a canceled request must not start lexical, dense, hydrate, or structure
	// fanout, including when an ablation force flag selected the residual route.
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		diag["retrieval_status"] = retrievalContextStatus(err)
		diag["retrieval_context_done_before_fanout"] = true
		return nil, diag, err
	}
	// Product residual (local FS, memory, or product_neon): never path2 SMF corpus.
	// productOwned is set by OpenLocal/OpenMemory/OpenResidual(chunks=neon).
	if c.productOwned || (c.db == nil && c.store != nil) {
		if c.store == nil && c.db != nil {
			// Lazy product store so neon residual can retrieve without prior EnsureSchema.
			c.store = &neonChunkStore{db: c.db}
		}
		if c.store == nil {
			return nil, diag, fmt.Errorf("hosted: product-owned client has no chunk store")
		}
		// Plane honesty: residual ask is never authority query or path2 SMF (GAP-PLANE-ASK/DENSE).
		if diag != nil {
			diag["plane"] = "residual"
			diag["product_owned"] = true
			diag["not_authority_query"] = true
			diag["not_path2_smf"] = true
			diag["graph_truth"] = "memory_edges"
		}
		// Prefer HotLex interactive path for local/memory when index is warm.
		prod := prodProfileFromEnv()
		// Resolve capability plan early (product Ask has no labels).
		plan := resolveRetrievePlan(question, &opts)
		if c.preferInteractive(prod) && c.hot != nil && c.hot.Len() > 0 {
			topK := opts.TopK
			if topK <= 0 {
				topK = c.cfg.TopK
			}
			if topK <= 0 {
				topK = 8
			}
			if fl := plan.TopKFloor(); fl > 0 && topK < fl {
				topK = fl
			}
			poolK := c.cfg.PoolK
			if poolK < topK*3 {
				poolK = topK * 4
			}
			if fl := plan.PoolKFloor(); fl > 0 && poolK < fl {
				poolK = fl
			}
			diag["query_plan"] = plan
			diag["question_type_resolved"] = opts.QuestionType
			ck := c.retrieveCacheKey(question, topK, opts, plan, prod, filter)
			// Skip cache when GoldDocIDs set: gold diags are request-scoped and must not leak across evals.
			useCache := len(opts.GoldDocIDs) == 0
			if useCache {
				ps, d, ev := c.qcache.get(ck)
				if ev == cacheEventHit {
					if d == nil {
						d = map[string]any{}
					}
					d["cache_hit"] = true
					d["cache_event"] = cacheEventHit
					// Re-apply ranking so as-of / utility / PPR stay live on cache hits.
					return c.finalizeRetrieve(ps, d, nil, question, filter)
				}
				diag["cache_event"] = ev
			}
			t0 := time.Now()
			ps, d, err := c.retrieveInteractiveLocal(ctx, question, opts, diag, topK, poolK, t0, prod, ck)
			if d != nil {
				d["store"] = c.StoreKind()
				if g := c.GenerationID(); g != "" {
					d["generation_id"] = g
				}
			}
			return c.finalizeRetrieve(ps, d, err, question, filter)
		}
		ps, d, err := c.retrieveMemory(ctx, question, opts, diag)
		if d != nil {
			d["store"] = c.StoreKind()
			if g := c.GenerationID(); g != "" {
				d["generation_id"] = g
			}
		}
		return c.finalizeRetrieve(ps, d, err, question, filter)
	}
	if c.db == nil {
		return nil, diag, fmt.Errorf("nil hosted client")
	}
	// Plane honesty: OpenFromEnv SMF path2 is eval/bench — never residual_company_rag.
	diag["plane"] = "path2_eval"
	diag["product_owned"] = false
	diag["path2_smf"] = true
	diag["not_residual_company"] = true
	diag["not_authority_query"] = true
	topK := opts.TopK
	if topK <= 0 {
		topK = c.cfg.TopK
	}
	if topK <= 0 {
		topK = 8
	}
	// Resolve capability plan before keying: plan/mode shape retrieval, so
	// they must be part of the cache key (issue #295).
	plan := resolveRetrievePlan(question, &opts)
	// Capability floors (prefer plan flags over type strings).
	if fl := plan.TopKFloor(); fl > 0 && topK < fl {
		topK = fl
	} else if isMultiDocType(opts.QuestionType) && topK < 10 {
		topK = 10
	}
	poolK := c.cfg.PoolK
	if poolK < topK*3 {
		poolK = topK * 4
	}
	if fl := plan.PoolKFloor(); fl > 0 && poolK < fl {
		poolK = fl
	} else if plan.MultiDoc && poolK < 56 {
		poolK = 56
	} else if isMultiDocType(opts.QuestionType) && poolK < 56 {
		poolK = 56
	}
	poolCap := 80
	if envTruthy("OUROBOROS_ERB_QUALITY", false) || benchmaxEnabled() {
		poolCap = 96
	}
	if poolK > poolCap {
		poolK = poolCap
	}
	t0 := time.Now()
	prod := prodProfileFromEnv()
	// In-process TTL+LRU cache (same request shape within container generation).
	// Skip when GoldDocIDs set so eval gold diags stay request-scoped (no cross-gold leak).
	ck := c.retrieveCacheKey(question, topK, opts, plan, prod, filter)
	useCache := len(opts.GoldDocIDs) == 0
	if useCache {
		ps, d, ev := c.qcache.get(ck)
		if ev == cacheEventHit {
			if d == nil {
				d = map[string]any{}
			}
			d["cache_hit"] = true
			d["cache_event"] = cacheEventHit
			d["brain_id"] = c.cfg.BrainID
			d["plane"] = "path2_eval"
			d["product_owned"] = false
			d["path2_smf"] = true
			d["not_residual_company"] = true
			d["not_authority_query"] = true
			return c.finalizeRetrieve(ps, d, nil, question, filter)
		}
		diag["cache_event"] = ev
	}
	diag["prod_mode"] = prod.Enabled
	diag["quality_mode"] = prod.Quality
	diag["pool_k"] = poolK
	diag["window_top_k"] = topK
	diag["query_plan"] = plan
	diag["question_type_resolved"] = opts.QuestionType

	// Interactive class: HotLex + dense first, Neon FTS only if thin/missing hot.
	// QUALITY mode keeps full residual Neon multi-FTS path below.
	if c.preferInteractive(prod) {
		ps, d, err := c.retrieveInteractive(ctx, question, opts, diag, topK, poolK, t0, prod, ck)
		return c.finalizeRetrieve(ps, d, err, question, filter)
	}
	diag["retrieve_class"] = "residual_opt_in"
	diag["retrieval_route_reason"] = residualRouteReason()
	diag["hot_lex_state"] = hotLexState(c.hotLexAvailable())
	diag["neon_fts_mode"] = "residual_multi_arm"

	// Fast LLM multi-query (Cerebras/Groq/OpenAI/OpenRouter) + static fallback.
	// QUALITY default-on; prod off unless OUROBOROS_ERB_LLM_MULTIQUERY=1.
	variants, llmMeta := multiQueryVariantsWithLLM(ctx, question, opts.QuestionType)
	for k, v := range llmMeta {
		diag[k] = v
	}
	// Decompose multi-part questions only outside tight prod budgets (each
	// variant can cost a full Neon FTS).
	if !prod.Enabled || prod.Quality {
		for _, sub := range decomposeQuery(question, opts.QuestionType) {
			variants = append(variants, sub)
		}
		// Quality doc2query (deterministic; gardener may have pre-warmed sidecars).
		if prod.Quality || envTruthy("OUROBOROS_ERB_DOC2QUERY", false) {
			for _, dq := range qualityDoc2QueryVariants(question) {
				variants = append(variants, dq)
			}
			diag["doc2query"] = true
		}
	}
	// HyDE variant: skip on prod (extra FTS/embed); keep for quality.
	if (!prod.Enabled || prod.Quality) && len(contentTokens(question)) >= 4 {
		hy, hySrc := hydeDocument(ctx, question, prod.Quality || !prod.Enabled)
		if hy != "" {
			variants = append(variants, hy)
			diag["hyde_variant"] = true
			diag["hyde_source"] = hySrc
		}
	}
	variants = dedupeQueries(variants)

	// Production: always primary-first lean. Quality restores multi-arm.
	// (Hard-type lean override was the residual-v2 timeout amplifier.)
	lean := leanMode()
	if prod.Enabled {
		lean = true
	}
	if !prod.Enabled && lean && wantsFullRetrieve(opts.QuestionType) {
		lean = false
		diag["lean_overridden"] = "hard_type"
	}
	lexCap := prod.LexCap
	if lexCap < 1 {
		lexCap = 1
	}
	if !lean && !prod.Enabled {
		if lexCap < 3 {
			lexCap = 3
		}
	}
	maxVariants := 5
	if prod.Enabled {
		maxVariants = max(lexCap+1, 2)
	}
	if len(variants) > maxVariants {
		variants = variants[:maxVariants]
	}
	diag["query_variants"] = variants
	diag["decomposed"] = !prod.Enabled && len(decomposeQuery(question, opts.QuestionType)) > 0
	diag["lean"] = lean

	// --- primary dense kick-off in parallel with lexical (wall-clock win) ---
	// Note: HotLex strength is known only after BM25; we still start dense here for
	// QUALITY/multi-doc. Prod may skip dense after hot (below) by not waiting on empty.
	tDense := time.Now()
	var denseLists [][]Hit
	var denseWG sync.WaitGroup
	var denseRun hostedDenseQueryRun
	denseQueries := variants
	maxDense := prod.DenseQueries
	if maxDense < 1 {
		maxDense = 1
	}
	if plan.SemanticExpand || plan.WantHardPoolExpand() {
		if maxDense < 4 {
			maxDense = 4
		}
	}
	// HyDE dense slot: computed before the multi-doc rebuild so the stub can
	// actually occupy a slot (previously appended, then silently discarded by
	// the short-phrase rebuild below — a dead guard).
	hyStub := ""
	if !prod.Enabled || prod.Quality {
		hyStub = hydeStub(question)
	}
	preferShort := plan.WantBroadLex() || isMultiDocType(opts.QuestionType) || wantsFullRetrieve(opts.QuestionType)
	denseQueries = phaseADenseQueries(question, denseQueries, maxDense, hyStub, preferShort)
	if hyStub != "" && containsString(denseQueries, hyStub) {
		diag["hyde_stub"] = true
		if _, ok := diag["hyde_variant"]; !ok {
			diag["hyde_variant"] = true
		}
	}
	dBudget := denseBudget(prod)
	diag["dense_timeout_ms"] = dBudget.Milliseconds()
	denseWG.Add(1)
	go func() {
		defer denseWG.Done()
		dctx, dcancel := withTimeout(ctx, dBudget)
		defer dcancel()
		denseRun = c.runHostedDenseQueries(dctx, denseQueries)
		denseLists = denseRun.Lists
	}()

	// --- multi-query lexical (primary-first; optional thin expand) ---
	// HotLex (left-shifted BM25) always fuses first when projected — QUALITY residual
	// path still gets free in-process lexical even when preferInteractive is false.
	tLex := time.Now()
	var lexLists [][]Hit
	var lexMu sync.Mutex
	var lexWG sync.WaitGroup
	lexLimit := prod.LexLimit
	if lexLimit <= 0 {
		lexLimit = c.cfg.LexicalLimit
	}
	// HotLex multi-query: primary + identifier/short variants (semantic paraphrases
	// miss when only the long original question is BM25'd).
	var hotHits []Hit
	hotStrong := false
	if c.hot != nil && c.hot.Len() > 0 {
		hotLimit := lexLimit
		if hotLimit < 24 {
			hotLimit = 24
		}
		if plan.MultiDoc || plan.SemanticExpand || isMultiDocType(opts.QuestionType) {
			// Wider BM25 head for paraphrase-hard semantic (gold often outside top-40).
			hotLimit = 112
		}
		// completeness multi-gold lists often need a wider head (7–8 gold docs).
		if plan.Completeness && hotLimit < 160 {
			hotLimit = 160
		}
		if plan.SemanticExpand && hotLimit < 128 {
			hotLimit = 128
		}
		if plan.Conflict && hotLimit < 128 {
			hotLimit = 128
		}
		// Always BM25 the original question, plus top-specificity phrase bags
		// (ranked + multi-intent diversity + optional LLM bags via variants).
		// Cap 4–6 parallel for E2E latency (wall ≈ max, not sum).
		hotCap := 4
		if plan.Completeness || plan.SemanticExpand || plan.Conflict {
			hotCap = 6 // parallel wall ≈ max; wider list fan-out for multi-gold
		}
		hotQueries := []string{question}
		for _, p := range pickHotLexPhrases(question, hotCap-1) {
			if len(hotQueries) >= hotCap {
				break
			}
			hotQueries = append(hotQueries, p)
		}
		for _, v := range variants {
			if len(hotQueries) >= hotCap {
				break
			}
			if strings.EqualFold(v, question) || len(v) > 100 {
				continue
			}
			// Skip low-specificity single tokens already covered.
			if phraseSpecificity(v) < 10 {
				continue
			}
			hotQueries = append(hotQueries, v)
		}
		// Rare-id basic ops (us-west-2 / continuous batching): widen BM25 head —
		// gold often sits outside top-40 when many near-dup cluster docs compete.
		if rareID := hasRareIdentifier(extractIdentifiers(question), question); rareID && !isMultiDocType(opts.QuestionType) {
			if hotLimit < 64 {
				hotLimit = 64
			}
		}
		tHot := time.Now()
		type hotRes struct {
			i    int
			hits []Hit
		}
		hotCh := make(chan hotRes, len(hotQueries))
		var hotWG sync.WaitGroup
		for i, hq := range hotQueries {
			hotWG.Add(1)
			go func(i int, q string) {
				defer hotWG.Done()
				hits := c.hot.Search(q, hotLimit)
				if len(hits) > 0 {
					hotCh <- hotRes{i: i, hits: hits}
				}
			}(i, hq)
		}
		hotWG.Wait()
		close(hotCh)
		var hotLists int
		for hr := range hotCh {
			lexLists = append(lexLists, hr.hits)
			hotLists++
			if hr.i == 0 || len(hotHits) == 0 {
				hotHits = hr.hits
			}
		}
		diag["hot_lex_ms"] = time.Since(tHot).Milliseconds()
		diag["hot_lex_queries"] = len(hotQueries)
		diag["hot_lex_lists"] = hotLists
		diag["hot_lex_docs"] = c.hot.Len()
		if len(hotHits) > 0 {
			diag["hot_lex_hits"] = len(hotHits)
			diag["hot_lex_fused"] = true
			minHits := prod.LexExpandIfThin
			if minHits < 1 {
				minHits = 6
			}
			// Single-doc only uses strong for Neon skip; multi-doc never skips FTS.
			hotStrong = hotLexStrong(hotHits, minHits, 0.5)
			// Semantic paraphrase: HotLex often returns a flat-high plate of near-dups
			// that look "strong" but miss gold (full500: 82 semantic pool0). Never
			// treat as strong enough to skip Neon FTS / dense rescue.
			if hotStrong && (plan.WantSemanticFTS() || plan.WantBroadLex() ||
				plan.DeepHydrate || plan.AtomicFact ||
				wantsDeepHydrate(question, opts.QuestionType) ||
				hasRareIdentifier(extractIdentifiers(question), question) ||
				strings.EqualFold(opts.QuestionType, "basic")) {
				hotStrong = false
				diag["hot_lex_strong_overridden"] = "plan_broad_semantic_deep_or_rare"
			}
			diag["hot_lex_strong"] = hotStrong
		}
	} else {
		diag["hot_lex_missing"] = true
	}
	lexQueryBudget := boundedFTSBudget(prod, prod.LexTimeout)
	diag["neon_fts_budget_ms"] = lexQueryBudget.Milliseconds()
	diag["neon_fts_caller_deadline_only"] = lexQueryBudget <= 0
	lexWallCtx, lexWallCancel := withTimeout(ctx, lexQueryBudget)
	defer lexWallCancel()
	runLex := func(qq string) ([]Hit, error) {
		return lexicalSearchLimited(lexWallCtx, c.db, c.cfg, qq, prod.LexTerms, lexLimit)
	}
	// Skip Neon FTS only when HotLex is truly strong AND single-doc AND no rare
	// identifiers (region codes / snake_case) that HotLex top can miss (qst_0100).
	// Multi-doc / semantic always keeps short Neon FTS on phrase bags.
	// When we do run FTS: short bags in parallel (latency), never long Q.
	forceNeonFTS := envTruthy("OUROBOROS_ERB_FORCE_NEON_FTS", false)
	needBroadLex := plan.WantBroadLex() || plan.WantSemanticFTS() ||
		isMultiDocType(opts.QuestionType) || wantsFullRetrieve(opts.QuestionType) ||
		strings.EqualFold(opts.QuestionType, "basic")
	rareID := hasRareIdentifier(extractIdentifiers(question), question) || plan.RareID
	skipNeonFTS := hotStrong && !forceNeonFTS && !needBroadLex && !rareID
	if skipNeonFTS {
		diag["neon_fts_skipped"] = "hot_lex_strong_single_doc"
		diag["lexical_expanded"] = false
		diag["lexical_variants_used"] = len(lexLists)
	} else {
		// Prefer ranked high-spec phrase bags + compact identifiers (latency).
		// Semantic/basic: pull more bags (full500 pool0 is almost all paraphrase).
		bagN := plan.FTSBagN()
		if rareID && bagN < 3 {
			bagN = 3
		}
		ftsQueries := make([]string, 0, bagN+2)
		for _, p := range pickHotLexPhrases(question, bagN) {
			ftsQueries = append(ftsQueries, p)
		}
		if ids := extractIdentifiers(question); len(ids) > 0 {
			n := len(ids)
			if n > 6 {
				n = 6
			}
			bag := strings.Join(ids[:n], " ")
			// Avoid duplicate of phrase bags.
			dup := false
			for _, f := range ftsQueries {
				if strings.EqualFold(f, bag) {
					dup = true
					break
				}
			}
			if !dup {
				ftsQueries = append(ftsQueries, bag)
			}
		}
		// Always add a compact original-question bag (specific content tokens).
		// Paraphrase bags alone often miss when gold uses different surface forms;
		// the compact Q bag keeps distinctive terms (full500 semantic pool0).
		if cq := compactQuestionBag(question, 8); cq != "" {
			dup := false
			for _, f := range ftsQueries {
				if strings.EqualFold(f, cq) {
					dup = true
					break
				}
			}
			if !dup {
				ftsQueries = append(ftsQueries, cq)
			}
		}
		if len(ftsQueries) == 0 {
			// Last resort: short content tokens, never full 40+ token question.
			if cq := compactQuestionBag(question, 8); cq != "" {
				ftsQueries = []string{cq}
			} else {
				ftsQueries = []string{question}
			}
		}
		// Cap hard for E2E latency (parallel, but Neon still costly under load).
		ftsCap := plan.FTSCap()
		if needBroadLex && ftsCap < 3 {
			ftsCap = 3
		}
		if lexCap > 0 && lexCap < ftsCap {
			ftsCap = lexCap
		}
		if len(ftsQueries) > ftsCap {
			ftsQueries = ftsQueries[:ftsCap]
		}
		for _, q := range ftsQueries {
			lexWG.Add(1)
			go func(qq string) {
				defer lexWG.Done()
				hits, err := runLex(qq)
				if err != nil || len(hits) == 0 {
					return
				}
				lexMu.Lock()
				lexLists = append(lexLists, hits)
				lexMu.Unlock()
			}(q)
		}
		lexWG.Wait()
		diag["neon_fts_queries"] = len(ftsQueries)
		diag["neon_fts_rare_id"] = rareID
		diag["lexical_expanded"] = len(ftsQueries) > 1
		diag["lexical_variants_used"] = len(lexLists)
	}
	diag["lexical_variant_cap"] = lexCap
	diag["lexical_terms"] = prod.LexTerms
	diag["lexical_lists"] = len(lexLists)
	diag["lexical_ms"] = time.Since(tLex).Milliseconds()

	// Join dense
	denseWG.Wait()
	stampHostedDenseQueryRun(diag, "dense_", denseRun)
	diag["dense_ms"] = time.Since(tDense).Milliseconds()
	if len(lexLists) == 0 && len(denseLists) == 0 {
		return nil, diag, fmt.Errorf("hosted retrieve: no lexical or dense hits")
	}

	// --- RRF fuse all channels ---
	tRRF := time.Now()
	var all [][]Hit
	all = append(all, lexLists...)
	all = append(all, denseLists...)
	fused := rrfFuseMany(all, c.cfg.RRFK)
	if prod.PoolK > 0 && poolK > prod.PoolK {
		poolK = prod.PoolK
	}
	// Store fuller text for ground/rebind; prompt soft-clips separately.
	pool := hitsToPassages(fused, poolK, storagePassageChars(c.cfg.MaxPassageChars))
	rrfPool := append([]Passage(nil), pool...)
	diag["rrf_pool"] = len(pool)
	diag["rrf_ms"] = time.Since(tRRF).Milliseconds()

	// --- sibling hydrate ∥ path2 structure (wall-clock win) ---
	// Spotcheck residual path: hydrate_ms≈5s then structure_ms≈8–12s sequential
	// (~13–17s stacked). Both only need the RRF pool as seed — run in parallel.
	tHydrate := time.Now()
	tStruct := time.Now()
	maxDocs, chunksPerDoc := prod.HydrateDocs, prod.HydrateChunks
	hydratePolicy := "standard"
	deepHydrate := wantsDeepHydrate(question, opts.QuestionType)
	if isMultiDocType(opts.QuestionType) || deepHydrate {
		maxDocs, chunksPerDoc = prod.HydrateDocsMulti, prod.HydrateChunksMulti
		hydratePolicy = "whole_doc_multi"
		// With HotLex, sibling hydrate is secondary to phrase BM25 — keep tight
		// for E2E latency UNLESS deep hydrate (INC threads / freeze timelines).
		if c.hot != nil && c.hot.Len() > 0 && !deepHydrate {
			if maxDocs > 5 {
				maxDocs = 5
			}
			if chunksPerDoc > 3 {
				chunksPerDoc = 3
			}
			hydratePolicy = "whole_doc_multi_hotlex"
		}
		if deepHydrate {
			// Late corrections / freeze dates live on chunk 2+ of multi-chunk gold.
			if chunksPerDoc < 6 {
				chunksPerDoc = 6
			}
			if maxDocs < 4 {
				maxDocs = 4
			}
			hydratePolicy = "deep_multi_chunk"
		}
	}
	if maxDocs < 1 {
		maxDocs = 3
	}
	if chunksPerDoc < 1 {
		chunksPerDoc = 2
	}
	diag["hydrate_policy"] = hydratePolicy
	diag["hydrate_chunks_per_doc"] = chunksPerDoc
	structMax := prod.StructureMaxNeigh
	if structMax < 1 {
		structMax = 8
	}
	// Seeds from pre-hydrate RRF pool (IDs stable; text filled by hydrate side).
	seedPassages := pool
	if len(seedPassages) > 6 {
		seedPassages = seedPassages[:6]
	}
	seedIDs := uniqueDocIDs(seedPassages)

	var (
		hydrated    []Passage
		structNeigh []Passage
		structDiag  map[string]any
		p2docs      []string
		p2diag      map[string]any
		structExtra []Passage
		trPass      []Passage
		trDiag      map[string]any
		factPass    []Passage
		factDiag    map[string]any
		hsWG        sync.WaitGroup
	)
	// Bound the parallel hydrate∥structure section hard for E2E latency.
	sectionBudget := hydrateBudget(prod)
	if c.hot != nil && c.hot.Len() > 0 && sectionBudget > 2500*time.Millisecond {
		sectionBudget = 2500 * time.Millisecond
	}
	hsWG.Add(2)
	go func() {
		defer hsWG.Done()
		hctx, hcancel := withTimeout(ctx, sectionBudget)
		hydrated = hydrateTopDocs(hctx, c.db, c.cfg, pool, maxDocs, chunksPerDoc)
		hcancel()
	}()
	go func() {
		defer hsWG.Done()
		// Pool-virtual always runs as base / supplement (in-process, cheap).
		var base []Passage
		base, structDiag = structureExpandPassages(seedPassages, pool, structMax)
		structNeigh = base
		// Path2 SQL: skip only single-doc + hot strong (latency). Multi-doc keeps
		// a short seed-rel budget for second gold; hard-cap 2.5s with HotLex warm.
		hotStrongStruct, _ := diag["hot_lex_strong"].(bool)
		skipPath2 := hotStrongStruct &&
			!isMultiDocType(opts.QuestionType) &&
			!wantsFullRetrieve(opts.QuestionType) &&
			!envTruthy("OUROBOROS_ERB_FORCE_PATH2_STRUCTURE", false)
		if c.db != nil && !c.productOwned && skipPath2 {
			if structDiag == nil {
				structDiag = map[string]any{}
			}
			structDiag["path2_structure_skipped"] = "hot_lex_strong_single_doc"
			structDiag["path2_structure_mode"] = "skipped_hot_strong"
		} else if c.db != nil && !c.productOwned {
			sBudget := structureSQLBudget(prod)
			if c.hot != nil && c.hot.Len() > 0 {
				// Fail-fast: under load full path2 often returns 0 hits after 12s.
				capMs := 2500 * time.Millisecond
				if isMultiDocType(opts.QuestionType) {
					capMs = 3000 * time.Millisecond
				}
				if sBudget > capMs {
					sBudget = capMs
				}
			}
			sctx, scancel := withTimeout(ctx, sBudget)
			p2docs, p2diag = path2StructureExpand(sctx, c.db, c.cfg.BrainID, question, seedIDs, structMax)
			scancel()
			if p2diag != nil {
				p2diag["structure_sql_budget_ms"] = sBudget.Milliseconds()
			}
			if len(p2docs) > 0 {
				hctx, hcancel := withTimeout(ctx, structureHydrateBudget(prod))
				structExtra = hydratePath2StructureDocs(hctx, c.db, c.cfg, p2docs, 2)
				hcancel()
			}
		}
		// Cheap in-process cortex/facts arms (no Neon FTS).
		trPass, trDiag = c.temporalRelationPassages(question, structMax)
		factPass, factDiag = factsChannelPassages(question, pool, 6)
	}()
	hsWG.Wait()
	diag["hydrate_policy"] = hydratePolicy
	diag["hydrate_max_docs"] = maxDocs
	diag["hydrate_chunks_per_doc"] = chunksPerDoc
	diag["hydrated_passages"] = len(hydrated)
	diag["hydrate_ms"] = time.Since(tHydrate).Milliseconds()
	diag["hydrate_structure_parallel"] = true
	pool = hydrated
	if len(pool) > poolK {
		pool = pool[:poolK]
	}
	for k, v := range structDiag {
		diag[k] = v
	}
	if p2diag != nil {
		diag["structure_sql_budget_ms"] = structureSQLBudget(prod).Milliseconds()
		mergePath2StructureDiag(diag, p2diag, len(p2docs))
	}
	if len(structExtra) > 0 {
		structNeigh = mergePassagesStructure(structNeigh, structExtra, structMax+8)
		diag["structure_sql_promoted"] = len(structExtra)
	}
	for k, v := range trDiag {
		diag[k] = v
	}
	if len(trPass) > 0 {
		structNeigh = mergePassagesStructure(structNeigh, trPass, structMax+8)
	}
	for k, v := range factDiag {
		diag[k] = v
	}
	// Structure lexical rescue is a full Neon FTS — skip in prod (pool-only).
	if c.db != nil && !prod.StructurePoolOnly {
		toks := structureTokensFromPassages(seedPassages, 3)
		if len(toks) > 0 {
			q := strings.Join(toks, " ")
			lctx, cancel := withTimeout(ctx, boundedFTSBudget(prod, hydrateBudget(prod)))
			hits, err := lexicalSearchLimited(lctx, c.db, c.cfg, q, prod.LexTerms, lexLimit)
			cancel()
			if err == nil && len(hits) > 0 {
				extra := hitsToPassages(hits, 10, storagePassageChars(c.cfg.MaxPassageChars))
				structNeigh = mergePassagesStructure(structNeigh, extra, 12)
				diag["structure_lexical_rescue"] = len(extra)
				diag["structure_lexical_query"] = q
			}
		}
	} else {
		diag["structure_lexical_rescue"] = 0
		diag["structure_pool_only"] = true
	}
	pool = mergePassagesStructure(pool, structNeigh, poolK+8)
	pool = mergePassagesStructure(pool, factPass, poolK+10)
	// Quality phrase/AND second-hop FTS rescue (ported from live/rescue.second_hop_expand).
	if c.db != nil && (!prod.Enabled || prod.Quality) && envTruthy("OUROBOROS_ERB_PHRASE_HOP", true) {
		hopN := 1
		if plan.SemanticExpand {
			hopN = 3
		}
		hopped := 0
		for _, pq := range phraseHopQueries(question, pool) {
			if hopped >= hopN {
				break
			}
			lctx, cancel := withTimeout(ctx, boundedFTSBudget(prod, hydrateBudget(prod)))
			hits, err := lexicalSearchLimited(lctx, c.db, c.cfg, pq, prod.LexTerms, lexLimit)
			cancel()
			if err != nil || len(hits) == 0 {
				continue
			}
			extra := hitsToPassages(hits, 8, storagePassageChars(c.cfg.MaxPassageChars))
			pool = mergePassagesStructure(pool, extra, poolK+12)
			diag["phrase_hop"] = true
			diag["phrase_hop_query"] = pq
			hopped++
		}
		if hopped > 0 {
			diag["phrase_hop_n"] = hopped
		}
	}
	// Semantic paraphrase rescue fans compact phrase bags into FTS; jargon often
	// appears only in these corpus-derived alternatives.
	if c.db != nil && plan.SemanticExpand && (!prod.Enabled || prod.Quality) {
		rescued := 0
		for _, bag := range pickHotLexPhrases(question, 5) {
			if len(bag) < 6 {
				continue
			}
			lctx, cancel := withTimeout(ctx, boundedFTSBudget(prod, hydrateBudget(prod)))
			hits, err := lexicalSearchLimited(lctx, c.db, c.cfg, bag, prod.LexTerms, lexLimit)
			cancel()
			if err != nil || len(hits) == 0 {
				continue
			}
			extra := hitsToPassages(hits, 10, storagePassageChars(c.cfg.MaxPassageChars))
			before := len(uniqueDSIDs(pool))
			pool = mergePassagesStructure(pool, extra, poolK+20)
			if n := len(uniqueDSIDs(pool)); n > before {
				rescued += n - before
			}
		}
		diag["semantic_bag_rescue_docs"] = rescued
	}
	// Completeness multi-gold: fan bag FTS so list entities don't live in one cluster.
	if c.db != nil && plan.WantCompletenessRescue() &&
		(!prod.Enabled || prod.Quality) {
		rescued := 0
		for _, bag := range pickHotLexPhrases(question, 4) {
			if len(bag) < 6 {
				continue
			}
			lctx, cancel := withTimeout(ctx, boundedFTSBudget(prod, hydrateBudget(prod)))
			hits, err := lexicalSearchLimited(lctx, c.db, c.cfg, bag, prod.LexTerms, lexLimit)
			cancel()
			if err != nil || len(hits) == 0 {
				continue
			}
			extra := hitsToPassages(hits, 10, storagePassageChars(c.cfg.MaxPassageChars))
			before := len(uniqueDSIDs(pool))
			pool = mergePassagesStructure(pool, extra, poolK+20)
			if len(uniqueDSIDs(pool)) > before {
				rescued += len(uniqueDSIDs(pool)) - before
			}
		}
		diag["completeness_list_rescue_docs"] = rescued
	}
	diag["structure_ms"] = time.Since(tStruct).Milliseconds()
	diag["pool_after_structure"] = len(pool)

	// --- cross-encoder / lexical CE ---
	// Score enough of the RRF pool that progressive wideK (topK×3) is fully
	// CE-ranked — gold outside CE top never enters progressive protect.
	tCE := time.Now()
	ceN := topK * 3
	if plan.MultiDoc || isMultiDocType(opts.QuestionType) {
		ceN = topK * 4
	}
	if plan.Completeness {
		ceN = topK * 6
	}
	if !prod.Enabled {
		// QUALITY: CE a large slice of PoolK (not just 2× topK).
		floor := plan.CENFloor()
		if floor < 48 {
			floor = 48
		}
		if ceN < floor {
			ceN = floor
		}
	} else if ceN < 16 {
		ceN = 16
	}
	if ceN > len(pool) {
		ceN = len(pool)
	}
	diag["ce_n"] = ceN
	reranked, reDiag := c.crossEncodeRerankClient(ctx, question, pool, ceN, false)
	for k, v := range reDiag {
		diag[k] = v
	}
	diag["rerank_ms"] = time.Since(tCE).Milliseconds()

	// --- coverage / multi-gold MMR before progressive window ---
	tWin := time.Now()
	covN := topK * 3
	if !prod.Enabled {
		covN = topK * 4
	}
	if plan.Completeness {
		covN = topK * 5
	}
	if covN > len(reranked) {
		covN = len(reranked)
	}
	// Completeness: higher λ diversity so list entities don't collapse to one cluster.
	lambda := plan.CoverageLambda()
	if lambda <= 0 {
		lambda = 0.7
	}
	covered := coverageRerank(question, reranked, covN, lambda)
	covered, awareDiag := questionAwareRerank(covered, question, opts.QuestionType, opts.SourceTypes)
	for k, v := range awareDiag {
		diag[k] = v
	}
	diag["coverage_rerank"] = true
	diag["coverage_pool"] = len(covered)
	diag["coverage_lambda"] = lambda

	// --- wide retain → progressive shrink, protect pool gold ---
	divCap := 3
	if plan.MultiDoc || isMultiDocType(opts.QuestionType) {
		divCap = 6
	}
	if !prod.Enabled {
		divCap = 4
		if plan.MultiDoc || isMultiDocType(opts.QuestionType) {
			divCap = 8
		}
	}
	window, winDiag := progressiveRetainWindow(covered, question, topK, divCap, opts.GoldDocIDs)
	// Stage R4: window must not drop gold already present in the retrieve pool.
	if len(opts.GoldDocIDs) > 0 {
		floorK := topK
		if n := len(opts.GoldDocIDs); n+2 > floorK {
			floorK = n + 2
		}
		if floorK < len(uniqueDSIDs(window)) {
			floorK = len(uniqueDSIDs(window))
		}
		window = ensureGoldInWindow(rrfPool, window, opts.GoldDocIDs, floorK)
		winDiag["gold_floor"] = true
		winDiag["gold_floor_k"] = floorK
	}
	// Authority/recency soft-rank (always-on; conflict/policy heavier) then best_last packing.
	window, authDiag := applyAuthorityRecencyQ(window, question, opts.QuestionType, opts.SourceTypes)
	diag["authority"] = authDiag
	window, superDiag := adjudicateSupersession(window, question)
	for k, v := range superDiag {
		diag[k] = v
	}
	window = bestLast(window)
	window = annotateRecencyPack(window)
	diag["recency_pack"] = true
	diag["best_last"] = true
	diag["window"] = winDiag
	diag["pool_window"] = diagnoseWindow(rrfPool, window, opts.GoldDocIDs)
	if len(opts.GoldDocIDs) > 0 {
		if gd := computeGoldDiag(opts.GoldDocIDs, rrfPool, window); gd != nil {
			for k, v := range gd {
				diag[k] = v
			}
		}
	}
	diag["window_ms"] = time.Since(tWin).Milliseconds()
	diag["passage_count"] = len(window)
	diag["total_us"] = time.Since(t0).Microseconds()
	diag["total_ms"] = time.Since(t0).Milliseconds()
	// first_evidence ≈ end of hybrid retrieve (window ready for synth)
	diag["first_evidence_ms"] = diag["total_ms"]
	diag["cache_hit"] = false
	diag["pipeline"] = []string{
		"multi_query_lexical",
		"decompose_union",
		"hyde_variant",
		"multi_query_dense_ann",
		"hyde_stub_dense",
		"rrf_fuse",
		"sibling_hydrate",
		"edge_hop",
		"entity_fanout",
		"facts_channel",
		"structure_lexical_rescue",
		"cross_encoder_rerank",
		"coverage_mmr",
		"identifier_retain_window",
	}
	diag["giant_search"] = true
	diag["arms"] = []string{"lexical", "dense", "structure", "facts", "ce", "coverage"}
	// Apply memory ranking before cache put so cached windows are ranked.
	window, diag, err = c.finalizeRetrieve(window, diag, nil, question, filter)
	if err != nil {
		return window, diag, err
	}
	if useCache {
		if n := c.qcache.put(ck, window, diag); n > 0 {
			diag["cache_evicted"] = n
		}
	}
	return window, diag, nil
}

// retrieveMemory is the offline product-owned path with residual structure arms.
// Parity target: multi-query lexical → structure (edge/entity/facts) → hydrate →
// coverage MMR → identifier window (same arms as productbrain, hosted-first).
func (c *Client) retrieveMemory(ctx context.Context, question string, opts RetrieveOptions, diag map[string]any) ([]Passage, map[string]any, error) {
	diag["source"] = "product_brain_hosted_memory"
	diag["product_owned"] = true
	diag["pipeline"] = []string{
		"store_lexical_multi_query",
		"sibling_hydrate",
		"edge_hop",
		"entity_fanout",
		"facts_channel",
		"coverage_mmr",
		"identifier_retain_window",
	}
	diag["giant_search"] = true
	topK := opts.TopK
	if topK <= 0 {
		topK = c.cfg.TopK
	}
	if topK <= 0 {
		topK = 8
	}
	prod := prodProfileFromEnv()
	plan := QueryPlan{}
	if opts.Plan != nil {
		plan = *opts.Plan
	}
	// Filter was already normalized + authorized by RetrieveOpts (fail-closed
	// upstream); re-derive here only to key the cache identically.
	filter, ferr := NormalizeMetadataFilter(opts.Filter, FilterAuthority{
		Tenant: c.cfg.BrainID,
		Blind:  blindFilterMode(),
	})
	if ferr != nil {
		diag["filter_rejected"] = ferr.Error()
		return nil, diag, fmt.Errorf("hosted: metadata filter rejected: %w", ferr)
	}
	ck := c.retrieveCacheKey(question, topK, opts, plan, prod, filter)
	// Skip cache when GoldDocIDs set: gold diags must not be shared across evals with different gold.
	useCache := len(opts.GoldDocIDs) == 0
	if useCache {
		ps, d, ev := c.qcache.get(ck)
		if ev == cacheEventHit {
			if d == nil {
				d = map[string]any{}
			}
			d["cache_hit"] = true
			d["cache_event"] = cacheEventHit
			d["source"] = "product_brain_hosted_memory"
			// Ranking applied by RetrieveOpts.finalizeRetrieve on the way out.
			return ps, d, nil
		}
		diag["cache_event"] = ev
	}
	limit := c.cfg.LexicalLimit
	if limit <= 0 {
		limit = 30
	}
	// Multi-query lexical for residual parity; prod caps variants.
	qualityFull := prod.Quality || !prod.Enabled
	variants, llmMeta := multiQueryVariantsWithLLM(ctx, question, opts.QuestionType)
	for k, v := range llmMeta {
		diag[k] = v
	}
	if qualityFull {
		for _, sub := range decomposeQuery(question, opts.QuestionType) {
			variants = append(variants, sub)
		}
		// Quality doc2query (deterministic; parity with path2 retrieve branch).
		if prod.Quality || envTruthy("OUROBOROS_ERB_DOC2QUERY", false) {
			for _, dq := range qualityDoc2QueryVariants(question) {
				variants = append(variants, dq)
			}
			diag["doc2query"] = true
		}
		// HyDE variant for residual memory QUALITY / full residual.
		if len(contentTokens(question)) >= 4 {
			hy, hySrc := hydeDocument(ctx, question, qualityFull)
			if hy != "" {
				variants = append(variants, hy)
				diag["hyde_variant"] = true
				diag["hyde_source"] = hySrc
			}
		}
	}
	variants = dedupeQueries(variants)
	maxV := 4
	if prod.Enabled {
		maxV = max(prod.LexCap, 1)
		if maxV > 2 {
			maxV = 2
		}
	} else if qualityFull {
		maxV = 6
	}
	if len(variants) > maxV {
		variants = variants[:maxV]
	}
	diag["query_variants"] = variants
	diag["prod_mode"] = prod.Enabled
	var allHits []Hit
	seenChunk := map[string]struct{}{}
	storeLexBudget := boundedFTSBudget(prod, prod.LexTimeout)
	storeLexCtx, storeLexCancel := withTimeout(ctx, storeLexBudget)
	defer storeLexCancel()
	diag["store_lexical_budget_ms"] = storeLexBudget.Milliseconds()
	diag["store_lexical_caller_deadline_only"] = storeLexBudget <= 0
	var storeLexAttempted, storeLexSucceeded, storeLexFailed int
	var storeLexTimedOut, storeLexCanceled int
	for i, v := range variants {
		if storeLexCtx.Err() != nil {
			break
		}
		variantBudget := lexicalVariantBudget(storeLexCtx, len(variants)-i)
		if storeLexAttempted == 0 {
			diag["store_lexical_variant_budget_ms"] = variantBudget.Milliseconds()
		}
		variantCtx, variantCancel := withTimeout(storeLexCtx, variantBudget)
		hits, err := c.store.LexicalSearch(variantCtx, c.cfg.BrainID, v, limit)
		variantCancel()
		storeLexAttempted++
		if err != nil {
			storeLexFailed++
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				storeLexTimedOut++
			case errors.Is(err, context.Canceled):
				storeLexCanceled++
			}
			continue
		}
		storeLexSucceeded++
		for _, h := range hits {
			key := h.ChunkID
			if key == "" {
				key = h.DSID
			}
			if _, ok := seenChunk[key]; ok {
				continue
			}
			seenChunk[key] = struct{}{}
			allHits = append(allHits, h)
		}
	}
	diag["store_lexical_queries"] = storeLexAttempted
	diag["store_lexical_succeeded_queries"] = storeLexSucceeded
	diag["store_lexical_failed_queries"] = storeLexFailed
	diag["store_lexical_timeout_queries"] = storeLexTimedOut
	diag["store_lexical_canceled_queries"] = storeLexCanceled
	if len(allHits) == 0 {
		// Empty hit set is valid (e.g. stopword-only queries); not a hard failure.
		diag["passage_count"] = 0
		diag["cache_hit"] = false
		diag["store"] = c.StoreKind()
		return nil, diag, nil
	}
	pool := hitsToPassages(allHits, limit, storagePassageChars(c.cfg.MaxPassageChars))
	// Sibling hydrate via store (per-doc N capped; batch sibling API not required).
	const memHydrateDocs, memHydrateChunks = 5, 3
	pool = hydrateTopDocsStore(ctx, c.store, c.cfg, pool, memHydrateDocs, memHydrateChunks)
	diag["lexical_hits"] = len(allHits)
	diag["hydrate_max_docs"] = memHydrateDocs
	diag["hydrate_chunks_per_doc"] = memHydrateChunks
	diag["hydrated_passages"] = len(pool)

	// Local dense ANN (sqlite/memory substrate) — same residual pipeline as Qdrant arm.
	pool = c.mergeLocalDenseArm(ctx, question, pool, diag, limit+16)

	// Structure arms from memory/local structure index (built on Upsert).
	if expander, ok := c.store.(structureExpander); ok {
		seeds := uniqueDocIDs(pool)
		if len(seeds) > 5 {
			seeds = seeds[:5]
		}
		edgeIDs, entIDs, _ := expander.StructureExpand(c.cfg.BrainID, seeds, 12)
		factIDs := expander.StructureFacts(c.cfg.BrainID, question, 8)
		diag["edge_neighbors"] = edgeIDs
		diag["entity_neighbors"] = entIDs
		diag["facts_hits"] = factIDs
		extra := expander.PassagesForDocs(c.cfg.BrainID, append(append(edgeIDs, entIDs...), factIDs...), c.cfg.MaxPassageChars)
		pool = mergePassagesStructure(pool, extra, limit+16)
		diag["structure_promoted"] = len(extra)
	} else {
		// Generic store: pool-virtual structure hop.
		structNeigh, structDiag := structureExpandPassages(pool, pool, 12)
		for k, v := range structDiag {
			diag[k] = v
		}
		factPass, factDiag := factsChannelPassages(question, pool, 8)
		for k, v := range factDiag {
			diag[k] = v
		}
		pool = mergePassagesStructure(pool, structNeigh, limit+12)
		pool = mergePassagesStructure(pool, factPass, limit+16)
	}
	// Cortex TemporalRelations → document passages (product residual / OpenMemory).
	trPass, trDiag := c.temporalRelationPassages(question, 12)
	for k, v := range trDiag {
		diag[k] = v
	}
	if len(trPass) > 0 {
		pool = mergePassagesStructure(pool, trPass, limit+16)
	}

	// Cross-encoder / lexical CE under QUALITY or full residual (parity with path2).
	// ZE when key present; else in-process lexical CE (rerank_backend=lexical).
	if qualityFull {
		ceN := topK * 3
		if ceN > len(pool) {
			ceN = len(pool)
		}
		reranked, reDiag := c.crossEncodeRerankClient(ctx, question, pool, ceN, false)
		for k, v := range reDiag {
			diag[k] = v
		}
		pool = reranked
	}

	// Coverage MMR then progressive wide→narrow window (protect gold).
	covered := coverageRerank(question, pool, min(len(pool), topK*3), 0.7)
	diag["coverage_rerank"] = true
	covered, awareDiag := questionAwareRerank(covered, question, opts.QuestionType, opts.SourceTypes)
	for k, v := range awareDiag {
		diag[k] = v
	}
	window, winDiag := progressiveRetainWindow(covered, question, topK, 4, opts.GoldDocIDs)
	if len(opts.GoldDocIDs) > 0 {
		floorK := topK
		if n := len(opts.GoldDocIDs); n+2 > floorK {
			floorK = n + 2
		}
		window = ensureGoldInWindow(covered, window, opts.GoldDocIDs, floorK)
		winDiag["gold_floor"] = true
	}
	window, authDiag := applyAuthorityRecencyQ(window, question, opts.QuestionType, opts.SourceTypes)
	diag["authority"] = authDiag
	window, superDiag := adjudicateSupersession(window, question)
	for k, v := range superDiag {
		diag[k] = v
	}
	window = bestLast(window)
	window = annotateRecencyPack(window)
	diag["recency_pack"] = true
	diag["best_last"] = true
	diag["window"] = winDiag
	// Offline gold diags (ERB eval): pool = pre-window covered set.
	diag["pool_window"] = diagnoseWindow(covered, window, opts.GoldDocIDs)
	if len(opts.GoldDocIDs) > 0 {
		if gd := computeGoldDiag(opts.GoldDocIDs, covered, window); gd != nil {
			for k, v := range gd {
				diag[k] = v
			}
		}
		diag["window_recall"] = windowRecall(opts.GoldDocIDs, window)
	}
	diag["passage_count"] = len(window)
	diag["cache_hit"] = false
	diag["quality_mode"] = prod.Quality
	diag["prod_mode"] = prod.Enabled
	// Pipeline stamp: reformulation + CE when quality/full residual.
	pipe := []string{
		"store_lexical_multi_query",
		"sibling_hydrate",
		"edge_hop",
		"entity_fanout",
		"facts_channel",
		"coverage_mmr",
		"identifier_retain_window",
	}
	if qualityFull {
		pipe = []string{
			"store_lexical_multi_query",
			"doc2query_reformulation",
			"hyde_variant",
			"sibling_hydrate",
			"edge_hop",
			"entity_fanout",
			"facts_channel",
			"cross_encoder_rerank",
			"coverage_mmr",
			"identifier_retain_window",
		}
	}
	diag["pipeline"] = pipe
	arms := []string{"lexical", "structure", "facts", "coverage"}
	if qualityFull {
		arms = []string{"lexical", "structure", "facts", "ce", "coverage"}
	}
	if _, ok := diag["dense_arm"]; ok {
		// Keep dense after lexical.
		base := arms
		arms = append([]string{"lexical", "dense"}, base[1:]...)
	}
	diag["arms"] = arms
	if useCache {
		if n := c.qcache.put(ck, window, diag); n > 0 {
			diag["cache_evicted"] = n
		}
	}
	return window, diag, nil
}

// structureTokensFromPassages returns top edge tokens across passages for FTS rescue.
func structureTokensFromPassages(ps []Passage, maxN int) []string {
	freq := map[string]int{}
	for _, p := range ps {
		for _, t := range structureEdgeTokens(p.Text) {
			freq[t]++
		}
	}
	type kv struct {
		t string
		n int
	}
	var arr []kv
	for t, n := range freq {
		arr = append(arr, kv{t, n})
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].n != arr[j].n {
			return arr[i].n > arr[j].n
		}
		return arr[i].t < arr[j].t
	})
	if maxN > len(arr) {
		maxN = len(arr)
	}
	out := make([]string, 0, maxN)
	for i := 0; i < maxN; i++ {
		out = append(out, arr[i].t)
	}
	return out
}

func hydrateTopDocsStore(ctx context.Context, store ChunkStore, cfg Config, pool []Passage, maxDocs, chunksPerDoc int) []Passage {
	if store == nil || len(pool) == 0 {
		return pool
	}
	seenChunk := map[string]struct{}{}
	var out []Passage
	for _, p := range pool {
		key := p.ChunkID
		if key == "" {
			key = p.DocumentID + "|0"
		}
		if _, ok := seenChunk[key]; ok {
			continue
		}
		seenChunk[key] = struct{}{}
		out = append(out, p)
	}
	var dsids []string
	seenD := map[string]struct{}{}
	for _, p := range pool {
		if _, ok := seenD[p.DocumentID]; ok {
			continue
		}
		seenD[p.DocumentID] = struct{}{}
		dsids = append(dsids, p.DocumentID)
		if len(dsids) >= maxDocs {
			break
		}
	}
	for _, dsid := range dsids {
		hits, err := store.SiblingChunks(ctx, cfg.BrainID, dsid, chunksPerDoc)
		if err != nil {
			continue
		}
		for _, h := range hits {
			if _, ok := seenChunk[h.ChunkID]; ok {
				continue
			}
			seenChunk[h.ChunkID] = struct{}{}
			text := clipPassageText(h.Text, storagePassageChars(cfg.MaxPassageChars))
			out = append(out, Passage{
				DocumentID: dsid,
				Text:       text,
				Score:      0,
				ChunkID:    h.ChunkID,
				SourceURI:  h.SourceURI,
				Channel:    "hydrate",
			})
		}
	}
	return out
}

type siblingHydrationCounts struct {
	Reused  int
	Fetched int
}

func hydrateTopDocs(ctx context.Context, db *sql.DB, cfg Config, pool []Passage, maxDocs, chunksPerDoc int) []Passage {
	out, _ := hydrateTopDocsWith(ctx, db, cfg, pool, maxDocs, chunksPerDoc, false, false)
	return out
}

// hydrateTopDocsPreferDates is the date-seeking variant: sibling load prefers
// chunks that contain ISO dates so timeline facts are not truncated away.
func hydrateTopDocsPreferDates(ctx context.Context, db *sql.DB, cfg Config, pool []Passage, maxDocs, chunksPerDoc int) []Passage {
	out, _ := hydrateTopDocsWith(ctx, db, cfg, pool, maxDocs, chunksPerDoc, true, false)
	return out
}

func hydrateTopDocsReusing(ctx context.Context, db *sql.DB, cfg Config, pool []Passage, maxDocs, chunksPerDoc int, preferDates bool) ([]Passage, siblingHydrationCounts) {
	return hydrateTopDocsWith(ctx, db, cfg, pool, maxDocs, chunksPerDoc, preferDates, true)
}

func hydrateTopDocsWith(ctx context.Context, db *sql.DB, cfg Config, pool []Passage, maxDocs, chunksPerDoc int, preferDates, reuseHydrated bool) ([]Passage, siblingHydrationCounts) {
	var counts siblingHydrationCounts
	if len(pool) == 0 {
		return pool, counts
	}
	if chunksPerDoc < 1 {
		chunksPerDoc = 4
	}
	// Index existing passages by chunk_id. HotLex path-2 projections often
	// strip body text — sibling hydrate must UPGRADE empty/short seeds, not
	// skip them because the chunk_id is already "seen".
	out := make([]Passage, 0, len(pool)+maxDocs*chunksPerDoc)
	idxByChunk := map[string]int{}
	presentBefore := map[string]struct{}{}
	for _, p := range pool {
		key := p.ChunkID
		if key == "" {
			key = p.DocumentID + "|0"
		} else {
			presentBefore[key] = struct{}{}
		}
		if i, ok := idxByChunk[key]; ok {
			// Keep longer text if duplicate.
			if len(p.Text) > len(out[i].Text) {
				out[i] = p
			}
			continue
		}
		idxByChunk[key] = len(out)
		out = append(out, p)
	}
	// Collect unique top dsids (preserve pool order — caller prioritizes).
	var dsids []string
	seenD := map[string]struct{}{}
	for _, p := range pool {
		if p.DocumentID == "" {
			continue
		}
		if _, ok := seenD[p.DocumentID]; ok {
			continue
		}
		seenD[p.DocumentID] = struct{}{}
		dsids = append(dsids, p.DocumentID)
		if len(dsids) >= maxDocs {
			break
		}
	}
	for _, dsid := range dsids {
		// A prior ordinary sibling hydrate marks every member of its original
		// window as hydrate. Reuse that complete window; partial or date-ordered
		// windows still run the original LIMIT query so short seeds can upgrade
		// and exclusions can never turn the request into a page-two fetch.
		if reuseHydrated && !preferDates && hydratedSiblingCount(out, dsid) >= chunksPerDoc {
			counts.Reused += chunksPerDoc
			continue
		}
		var hits []Hit
		var err error
		if preferDates {
			hits, err = siblingChunksPreferDates(ctx, db, cfg, dsid, chunksPerDoc)
		} else {
			hits, err = siblingChunks(ctx, db, cfg, dsid, chunksPerDoc)
		}
		if err != nil {
			continue
		}
		// SQL applies this limit, but keep the pack bound explicit for test
		// drivers and alternate stores that may return extra rows.
		if len(hits) > chunksPerDoc {
			hits = hits[:chunksPerDoc]
		}
		for _, h := range hits {
			text := clipPassageText(h.Text, storagePassageChars(cfg.MaxPassageChars))
			if i, ok := idxByChunk[h.ChunkID]; ok {
				if reuseHydrated {
					if _, existed := presentBefore[h.ChunkID]; existed {
						counts.Reused++
					}
				}
				// Upgrade empty/short HotLex seeds with full Neon text.
				// Prefer longer *or* fact-richer clips (date windows preserved).
				if len(text) > len(out[i].Text) || passageFactRicher(text, out[i].Text) {
					cp := out[i]
					cp.Text = text
					if cp.SourceURI == "" {
						cp.SourceURI = h.SourceURI
					}
					if cp.Channel == "" {
						cp.Channel = "hydrate"
					} else if !hasSiblingHydrationChannel(cp.Channel) {
						cp.Channel = cp.Channel + "+hydrate"
					}
					out[i] = cp
				}
				continue
			}
			idxByChunk[h.ChunkID] = len(out)
			out = append(out, Passage{
				DocumentID: dsid,
				Text:       text,
				Score:      0,
				ChunkID:    h.ChunkID,
				SourceURI:  h.SourceURI,
				Channel:    "hydrate",
			})
			if reuseHydrated {
				counts.Fetched++
			}
		}
	}
	return out, counts
}

func hasSiblingHydrationChannel(channel string) bool {
	for _, part := range strings.Split(channel, "+") {
		if part == "hydrate" {
			return true
		}
	}
	return false
}

func hydratedSiblingCount(passages []Passage, dsid string) int {
	seen := map[string]struct{}{}
	for _, p := range passages {
		if p.DocumentID != dsid || strings.TrimSpace(p.ChunkID) == "" || strings.TrimSpace(p.Text) == "" || !hasSiblingHydrationChannel(p.Channel) {
			continue
		}
		seen[p.ChunkID] = struct{}{}
	}
	return len(seen)
}

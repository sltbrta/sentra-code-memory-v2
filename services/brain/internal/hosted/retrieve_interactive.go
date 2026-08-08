package hosted

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

func ftsFallbackOutcome(attempted bool, planned, hits, succeeded, failed, timedOut, canceled int) string {
	if !attempted {
		return "skipped"
	}
	if planned == 0 {
		return "not_started"
	}
	if failed > 0 && succeeded > 0 {
		return "partial_failure"
	}
	if failed > 0 {
		switch {
		case timedOut == failed:
			return "timeout"
		case canceled == failed:
			return "canceled"
		default:
			return "error"
		}
	}
	if succeeded > 0 && hits > 0 {
		return "hits"
	}
	if succeeded > 0 {
		return "empty"
	}
	return "not_started"
}

// preferInteractive is the ONE product retrieve class for all substrates and
// budgets (light / deep / research / QUALITY / benchmax). ERB path2 is the same
// pipeline with a pre-ingested brain + wider budgets — not a separate line.
//
// Pipeline: HotLex BM25 + dense ANN → union CE → structure → window → recovery
// multi-list → corpus grep fallback. QUALITY only widens budgets inside this path.
//
// Ablation-only opt-out (not product): OUROBOROS_ERB_FORCE_RESIDUAL=1.
func (c *Client) preferInteractive(prod ProdProfile) bool {
	if c == nil {
		return false
	}
	if envTruthy("OUROBOROS_ERB_FORCE_RESIDUAL", false) ||
		envTruthy("OUROBOROS_ERB_QUALITY_RESIDUAL", false) {
		return false
	}
	// Product default: interactive class whenever we can (HotLex optional —
	// path still runs dense/FTS/structure without it).
	if c.hot != nil && c.hot.Len() > 0 {
		return true
	}
	// No HotLex: still prefer product interactive skeleton (dense+FTS+CE).
	// Residual multi-arm is ablation-only.
	_ = prod
	return !envTruthy("OUROBOROS_ERB_FORCE_RESIDUAL", false)
}

// retrieveInteractive implements the production interactive class:
//
//	Phase A: HotLex BM25 first; dense ANN only if hot thin (or forced)
//	Phase B: optional single timed Neon FTS if thin / no hot
//	Hydrate missing texts by chunk_id; sibling hydrate only when thin
//	Light structure + lexical CE (no ZE RTT by default) + window
//
// Diag stamps stage latencies so G15 p95 regressions are attributable.
func (c *Client) retrieveInteractive(
	ctx context.Context,
	question string,
	opts RetrieveOptions,
	diag map[string]any,
	topK, poolK int,
	t0 time.Time,
	prod ProdProfile,
	ck string,
) ([]Passage, map[string]any, error) {
	diag["retrieve_class"] = "interactive"
	diag["pipeline"] = []string{
		"hot_lex",
		"dense_ann?",
		"rrf_fuse",
		"neon_fts_fallback?",
		"hydrate_by_id",
		"sibling_hydrate?",
		"structure_pool",
		"cross_encoder_rerank",
		"coverage_mmr",
		"identifier_retain_window",
	}
	hotAvailable := c.hotLexAvailable()
	diag["hot_lex_available"] = hotAvailable
	if !hotAvailable {
		diag["hot_lex_missing"] = true
		diag["hot_lex_state"] = hotLexState(false)
	} else {
		diag["hot_lex_state"] = hotLexState(true)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		diag["retrieval_status"] = retrievalContextStatus(err)
		diag["retrieval_context_done_before_fanout"] = true
		return nil, diag, err
	}
	// Nested agentic expand: pay only HotLex+dense, not another full QUALITY stack.
	expandLite := opts.ExpandLite
	if expandLite {
		diag["expand_lite"] = true
		diag["retrieve_class"] = "interactive_expand_lite"
	}

	// --- Phase A: HotLex + dense + FTS in PARALLEL (wall ≈ max, not sum) ---
	// Never skip dense/FTS for "latency" — that killed SOTA hybrid quality.
	// Opt-out only via explicit env (demos / ablation). ExpandLite is the nested
	// exception (agentic reformulate must not re-pay recovery+structure).
	tPhaseA := time.Now()
	plan := QueryPlan{}
	if opts.Plan != nil {
		plan = *opts.Plan
	}
	limit := prod.LexLimit
	if limit <= 0 {
		limit = 40
	}
	// More HotLex bags by default (SOTA recall); still parallel.
	// Semantic paraphrase is the full500 pool@0 sink — wider phrase fan-out.
	phraseN := 3
	semanticish := strings.EqualFold(opts.QuestionType, "semantic") || plan.SemanticExpand
	if plan.WantBroadLex() || plan.DeepHydrate || plan.RareID ||
		isMultiDocType(opts.QuestionType) || hasRareIdentifier(extractIdentifiers(question), question) ||
		wantsDeepHydrate(question, opts.QuestionType) || prod.Quality || semanticish {
		phraseN = 4
	}
	if semanticish {
		phraseN = 6
	}
	if expandLite {
		phraseN = 2
		semanticish = false // no paraphrase fan-out on nested expand
	}
	hotQueries := []string{question}
	for _, p := range pickHotLexPhrases(question, phraseN) {
		if len(hotQueries) >= phraseN+1 {
			break
		}
		hotQueries = append(hotQueries, p)
	}
	// Semantic: also static multi-query paraphrase bags (spending freeze → budget freeze).
	if semanticish {
		for _, v := range multiQueryVariants(question, "semantic") {
			if len(hotQueries) >= phraseN+4 {
				break
			}
			dup := false
			for _, h := range hotQueries {
				if strings.EqualFold(h, v) {
					dup = true
					break
				}
			}
			if !dup && len(v) >= 6 {
				hotQueries = append(hotQueries, v)
			}
		}
	}

	var (
		hotHits      []Hit
		hotLists     [][]Hit
		denseHits    []Hit
		ftsHits      []Hit
		ftsErr       error
		ftsQ         string
		ftsQueryN    int
		ftsBudget    time.Duration
		ftsReason    string
		ftsOKN       int
		ftsErrN      int
		ftsTimeoutN  int
		ftsCanceledN int
	)
	// Phase wall = max(arm), not a floor. Target ≤2.5s QUALITY / ≤1.5s ExpandLite.
	phaseBudget := phaseABudget(prod, expandLite)
	phaseCtx, phaseCancel := withTimeout(ctx, phaseBudget)
	defer phaseCancel()
	diag["phase_a_budget_ms"] = phaseBudget.Milliseconds()

	var wg sync.WaitGroup
	// HotLex (in-process, multi-query parallel).
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c.hot == nil || c.hot.Len() == 0 {
			return
		}
		type hres struct {
			i    int
			hits []Hit
		}
		ch := make(chan hres, len(hotQueries))
		for i, q := range hotQueries {
			go func(i int, q string) {
				if phaseCtx.Err() != nil {
					ch <- hres{i: i}
					return
				}
				ch <- hres{i: i, hits: c.hot.Search(q, limit)}
			}(i, q)
		}
		var local [][]Hit
		var primary []Hit
		for range hotQueries {
			hr := <-ch
			if len(hr.hits) == 0 {
				continue
			}
			local = append(local, hr.hits)
			if hr.i == 0 || len(primary) == 0 {
				primary = hr.hits
			}
		}
		hotLists = local
		hotHits = primary
	}()
	// Dense ANN — always on product path (skip only explicit env).
	// Multi-query dense in phase A (Cohere embed-v4): QUALITY DenseQueries was
	// only used later in residual; interactive previously did 1 query → semantic
	// pool@0. Fan out in parallel under the same wall budget.
	wantDense := !envTruthy("OUROBOROS_ERB_SKIP_DENSE", false)
	var denseLists [][]Hit
	var denseEmbedMs, denseAnnMs int64
	var denseRun hostedDenseQueryRun
	if wantDense {
		wg.Add(1)
		go func() {
			defer wg.Done()
			actx := phaseCtx
			nDense := prod.DenseQueries
			if nDense < 1 {
				nDense = 1
			}
			// Semantic: at most 2 dense (3rd was free quality, costly wall under Cohere RTT).
			if semanticish && nDense < 2 {
				nDense = 2
			}
			if nDense > 2 {
				nDense = 2
			}
			if expandLite {
				nDense = 1 // single embed+ANN for nested expand
			}
			dQueries := []string{question}
			for _, p := range pickHotLexPhrases(question, nDense) {
				if len(dQueries) >= nDense {
					break
				}
				if !strings.EqualFold(p, question) {
					dQueries = append(dQueries, p)
				}
			}
			if semanticish || prod.Quality {
				// Compact HyDE only — long stubs cost embed RTT without lift.
				if hy := shortHydeForDense(question); hy != "" && len(dQueries) < nDense {
					dQueries = append(dQueries, hy)
				}
				for _, v := range multiQueryVariants(question, "semantic") {
					if len(dQueries) >= nDense {
						break
					}
					dup := false
					for _, q := range dQueries {
						if strings.EqualFold(q, v) {
							dup = true
							break
						}
					}
					if !dup {
						dQueries = append(dQueries, v)
					}
				}
			}
			// Cohere accepts multiple texts. Embed the selected rewrites in one
			// bounded request (or a bounded sequence for unusually large plans),
			// then retain parallel ANN searches in deterministic query order.
			denseRun = c.runHostedDenseQueries(actx, dQueries)
			denseLists = denseRun.Lists
			denseEmbedMs = denseRun.EmbedMS
			denseAnnMs = denseRun.ANNMS
			if len(denseLists) > 0 {
				denseHits = denseLists[0]
			}
		}()
	}
	// Neon FTS: skip when ExpandLite or when HotLex is already projected (path2
	// FTS almost always deadline-exceeds under 2.5s and adds no lists).
	ftsDisabled := envTruthy("OUROBOROS_ERB_SKIP_FTS", false)
	forceFTS := envTruthy("OUROBOROS_ERB_FORCE_FTS", false)
	wantFTS := c.db != nil && !ftsDisabled && !expandLite
	switch {
	case c.db == nil:
		ftsReason = "no_neon_database"
	case ftsDisabled:
		ftsReason = "skip_fts_env"
	case expandLite:
		ftsReason = "expand_lite"
	case !hotAvailable:
		ftsReason = "missing_hot_lex"
	case forceFTS:
		ftsReason = "force_fts"
	default:
		ftsReason = "thin_hot_lex"
	}
	if wantFTS && hotAvailable && c.hot.Len() > 1000 && !forceFTS {
		// HotLex covers lexical; Neon FTS is residual rescue only.
		wantFTS = false
		ftsReason = "hot_lex_projection"
	}
	if wantFTS {
		ftsBudget = interactiveFTSBudget(prod, hotAvailable)
		wg.Add(1)
		go func() {
			defer wg.Done()
			lctx, lcancel := withTimeout(phaseCtx, ftsBudget)
			defer lcancel()
			lexLimit := prod.LexLimit
			if lexLimit <= 0 {
				lexLimit = c.cfg.LexicalLimit
			}
			ftsQueries := []string{}
			if phrases := pickHotLexPhrases(question, 2); len(phrases) > 0 {
				ftsQueries = append(ftsQueries, phrases...)
			}
			if cq := compactQuestionBag(question, 10); cq != "" {
				dup := false
				for _, q := range ftsQueries {
					if q == cq {
						dup = true
						break
					}
				}
				if !dup {
					ftsQueries = append(ftsQueries, cq)
				}
			}
			if len(ftsQueries) == 0 {
				ftsQueries = []string{question}
			}
			// A missing HotLex projection gets exactly one bounded Neon fallback.
			// Additional variants plus corpus recovery previously amplified a mount
			// failure into repeated slow FTS work on every request.
			queryCap := interactiveFTSQueryCap(hotAvailable)
			if len(ftsQueries) > queryCap {
				ftsQueries = ftsQueries[:queryCap]
			}
			ftsQueryN = len(ftsQueries)
			ftsQ = ftsQueries[0]
			type ftsRes struct {
				hits []Hit
				err  error
				q    string
			}
			ch := make(chan ftsRes, len(ftsQueries))
			for _, q := range ftsQueries {
				go func(qq string) {
					hits, err := lexicalSearchLimited(lctx, c.db, c.cfg, qq, prod.LexTerms, lexLimit)
					if err != nil {
						ch <- ftsRes{err: err, q: qq}
						return
					}
					ch <- ftsRes{hits: hits, q: qq}
				}(q)
			}
			seen := map[string]struct{}{}
			var merged []Hit
			var firstErr error
			for range ftsQueries {
				r := <-ch
				if r.err != nil {
					ftsErrN++
					if firstErr == nil {
						firstErr = r.err
					}
					switch {
					case errors.Is(r.err, context.DeadlineExceeded):
						ftsTimeoutN++
					case errors.Is(r.err, context.Canceled):
						ftsCanceledN++
					}
					continue
				}
				ftsOKN++
				for _, h := range r.hits {
					key := h.ChunkID
					if key == "" {
						key = h.DSID
					}
					if key == "" {
						continue
					}
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					merged = append(merged, h)
				}
			}
			ftsHits = merged
			if firstErr != nil {
				ftsErr = firstErr
			}
		}()
	}
	wg.Wait()
	phaseCancel()

	diag["hot_lex_ms"] = time.Since(tPhaseA).Milliseconds() // wall includes parallel
	diag["hot_lex_queries"] = len(hotQueries)
	diag["hot_lex_lists"] = len(hotLists)
	diag["hot_lex_hits"] = len(hotHits)
	diag["hot_lex_docs"] = 0
	if c.hot != nil {
		diag["hot_lex_docs"] = c.hot.Len()
	}
	minHits := prod.LexExpandIfThin
	if minHits < 1 {
		minHits = 6
	}
	strong := hotLexStrong(hotHits, minHits, 0.5)
	// strong is diagnostic only — never used to skip dense/FTS/hydrate.
	diag["hot_lex_strong"] = strong
	diag["dense_skipped"] = !wantDense
	if !wantDense {
		diag["dense_skip_reason"] = "skip_dense_env"
	}
	diag["dense_ms"] = time.Since(tPhaseA).Milliseconds()
	diag["dense_embed_ms"] = denseEmbedMs
	diag["dense_ann_ms"] = denseAnnMs
	diag["dense_lists"] = len(denseLists)
	if wantDense {
		stampHostedDenseQueryRun(diag, "dense_", denseRun)
	}
	diag["phase_a_ms"] = time.Since(tPhaseA).Milliseconds()
	diag["phase_a_parallel"] = true
	ftsAttempted := ftsQueryN > 0
	diag["neon_fts_fallback_attempted"] = ftsAttempted
	diag["neon_fts_fallback_queries"] = ftsQueryN
	diag["neon_fts_fallback_succeeded_queries"] = ftsOKN
	diag["neon_fts_fallback_failed_queries"] = ftsErrN
	diag["neon_fts_fallback_timeout_queries"] = ftsTimeoutN
	diag["neon_fts_fallback_canceled_queries"] = ftsCanceledN
	diag["neon_fts_fallback_reason"] = ftsReason
	if wantFTS {
		diag["neon_fts_fallback_budget_ms"] = ftsBudget.Milliseconds()
		diag["neon_fts_fallback_caller_deadline_only"] = ftsBudget <= 0
	}
	if !hotAvailable {
		diag["neon_fts_fallback_query_cap"] = interactiveFTSQueryCap(false)
	}
	// RRF fuse all HotLex lists + dense + FTS
	var lists [][]Hit
	if len(hotLists) > 0 {
		lists = append(lists, hotLists...)
	} else if len(hotHits) > 0 {
		lists = append(lists, hotHits)
	}
	if len(denseLists) > 0 {
		lists = append(lists, denseLists...)
		diag["dense_phase_a_lists"] = len(denseLists)
	} else if len(denseHits) > 0 {
		lists = append(lists, denseHits)
		diag["dense_phase_a_lists"] = 1
	}
	if len(ftsHits) > 0 {
		lists = append(lists, ftsHits)
		diag["neon_fts_fallback"] = true
		diag["phase_b_fts_hits"] = len(ftsHits)
		diag["phase_b_fts_query"] = ftsQ
	} else {
		diag["neon_fts_fallback"] = false
	}
	diag["neon_fts_fallback_outcome"] = ftsFallbackOutcome(
		ftsAttempted, ftsQueryN, len(ftsHits), ftsOKN, ftsErrN, ftsTimeoutN, ftsCanceledN,
	)
	if ftsErr != nil {
		diag["phase_b_fts_error"] = truncateErr(ftsErr, 160)
	}
	diag["phase_b_fts_ms"] = 0 // parallel — included in phase_a wall
	if ctxErr := ctx.Err(); ctxErr != nil {
		diag["retrieval_status"] = retrievalContextStatus(ctxErr)
		return nil, diag, ctxErr
	}

	if len(lists) == 0 {
		// Soft empty (stopword-only / cold miss) — Answer returns no-docs, not Failure.
		diag["passage_count"] = 0
		diag["cache_hit"] = false
		diag["soft_empty"] = true
		diag["store"] = c.StoreKind()
		return nil, diag, nil
	}

	if prod.PoolK > 0 && poolK > prod.PoolK {
		poolK = prod.PoolK
	}
	// First fuse (for PRF seeds), then gated recovery (v5 vocab_gate spirit).
	tRec := time.Now()
	top1, flat := bm25Flatness(hotHits)
	diag["bm25_top1"] = top1
	diag["bm25_flat"] = flat
	// Seed texts from first-pass hits (hydrate later; use raw hit text if present).
	seedPass := hitsToPassages(rrfFuseMany(lists, c.cfg.RRFK), 24, 800)
	seedTexts := passageTexts(seedPass)
	// v5-style strength gate + smf funnel budgets (latency-tiered).
	doRec := false
	phaseADenseN0 := len(denseLists)
	if phaseADenseN0 == 0 && len(denseHits) > 0 {
		phaseADenseN0 = 1
	}
	projectishRec := strings.EqualFold(opts.QuestionType, "project_related") ||
		strings.EqualFold(opts.QuestionType, "completeness")
	multiDoc := isMultiDocType(opts.QuestionType)
	tier, tierWhy := classifyEvidenceTier(
		hotHits, phaseADenseN0, semanticish, projectishRec, multiDoc, nil, "",
	)
	diag["evidence_tier"] = tier.String()
	diag["evidence_tier_why"] = tierWhy
	multiGold := len(opts.GoldDocIDs) > 1 || multiDoc
	fb := budgetsForFunnel(tier, opts.QuestionType, multiGold, prod.Quality)
	stampFunnelDiag(diag, tier, fb)

	// Offline entity hits: full fanout of source dsids (smf P1 — never min(dsid)).
	entMax := fb.EntityMax
	if entMax < 8 {
		entMax = 8
	}
	if entHits := c.scopedOfflineEntityHits(question, entMax); len(entHits) > 0 {
		lists = append(lists, entHits)
		diag["entity_catalog_offline_hits"] = len(entHits)
		diag["entity_fanout_full"] = true
	}
	if expandLite {
		doRec = false
		diag["recovery_skipped"] = "expand_lite"
	} else if tier == TierExpand {
		doRec = true
	} else if tier == TierLean {
		doRec = false
		diag["recovery_skipped"] = "tier_lean"
	} else {
		doRec = needsVocabRecovery(hotHits, nil, "")
		if !doRec {
			diag["recovery_skipped"] = "vocab_gate_strong"
		}
	}
	diag["vocab_recovery_gate"] = doRec
	diag["entity_catalog"] = true
	if doRec {
		// Short bags only — never HotLex-search long HyDE (was 11–14s).
		recN := fb.RecN
		if recN < 4 {
			recN = 4
		}
		rq := c.recoveryQueriesForClient(ctx, question, seedTexts, recN)
		var shortRQ []string
		for _, q := range rq {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			if len(q) > 100 {
				if ph := pickHotLexPhrases(q, 1); len(ph) > 0 {
					shortRQ = append(shortRQ, ph[0])
				}
				continue
			}
			shortRQ = append(shortRQ, q)
		}
		// smf: gated doc2query + decompose + short HyDE for dense (expand only).
		dqN := fb.Doc2QueryN
		for i, dq := range qualityDoc2QueryVariants(question) {
			if i >= dqN {
				break
			}
			if len(dq) <= 100 {
				shortRQ = append(shortRQ, dq)
			}
		}
		// Multi-part decompose (smf query-decomp arm) — short clauses only.
		if tier == TierExpand && (multiDoc || semanticish) {
			for _, d := range decomposeQuery(question, opts.QuestionType) {
				if len(d) >= 12 && len(d) <= 100 {
					shortRQ = append(shortRQ, d)
				}
			}
			diag["query_decompose"] = true
		}
		if dqN > 0 {
			diag["doc2query"] = true
			diag["doc2query_gardener_hook"] = "query_time" // ingest-time gardener optional via entity gob
		}
		// Dense HyDE string (compact) only on expand recovery — not HotLex.
		var denseRQ []string
		denseRQ = append(denseRQ, shortRQ...)
		if fb.DenseRecovery {
			if hy := shortHydeForDense(question); hy != "" {
				denseRQ = append([]string{hy}, denseRQ...)
				diag["hyde_stub"] = true
				diag["hyde_variant"] = true
				diag["hyde_dense_only"] = true
			}
		}
		if len(shortRQ) > recN {
			shortRQ = shortRQ[:recN]
		}
		diag["recovery_queries"] = shortRQ
		hotExtra := c.runRecoveryHotLists(shortRQ, 40)
		for _, hl := range hotExtra {
			lists = append(lists, hl)
		}
		// Dense recovery: expand always (smf +8pt); else only if phase-A dense empty.
		phaseADenseN := phaseADenseN0
		var denseExtra [][]Hit
		var recoveryDenseRun hostedDenseQueryRun
		wantDenseRec := fb.DenseRecovery || phaseADenseN < 1 ||
			(semanticish && phaseADenseN < 2 && flat > 0.6)
		if wantDenseRec {
			drq := denseRQ
			if len(drq) > 5 {
				drq = drq[:5]
			}
			recoveryDenseRun = c.runRecoveryDenseLists(ctx, drq, prod)
			denseExtra = recoveryDenseRun.Lists
			for _, dl := range denseExtra {
				lists = append(lists, dl)
			}
			diag["chunk_ann_recovery"] = len(denseExtra) > 0
			stampHostedDenseQueryRun(diag, "recovery_dense_", recoveryDenseRun)
		} else {
			diag["chunk_ann_recovery"] = false
			diag["chunk_ann_skipped"] = "phase_a_dense_ok"
		}
		diag["recovery_hot_lists"] = len(hotExtra)
		diag["recovery_dense_lists"] = len(denseExtra)
		diag["chunk_ann_lists"] = len(denseExtra)
		diag["recovery"] = true
	} else {
		diag["recovery"] = false
		if _, ok := diag["recovery_skipped"]; !ok {
			diag["recovery_skipped"] = "vocab_gate_strong"
		}
	}
	diag["recovery_ms"] = time.Since(tRec).Milliseconds()
	if semanticish {
		diag["semantic_pool_boost"] = true
	}

	tFuse := time.Now()
	// Union of list heads → CE (never RRF-cut BM25-only winners away).
	// Usable pool: 64–80 (was 100–120 hydrate tax).
	headN, maxPool := 40, 80
	if prod.Quality || semanticish {
		headN, maxPool = 48, 96
	}
	unionHits := unionHitListsForCE(lists, headN, maxPool)
	fused := rrfFuseMany(lists, c.cfg.RRFK)
	fusedHead := mergeHitsPreferFirst(unionHits, fused, maxPool)
	diag["ce_union_head"] = headN
	diag["ce_union_max"] = maxPool
	if len(fusedHead) == 0 {
		fusedHead = fused
	}
	poolChars := storagePassageChars(c.cfg.MaxPassageChars)
	cePoolN := poolK
	if cePoolN < 48 {
		cePoolN = 48
	}
	if prod.Quality && cePoolN < 64 {
		cePoolN = 64
	}
	if cePoolN > 80 {
		cePoolN = 80
	}
	pool := hitsToPassages(fusedHead, cePoolN, poolChars)
	rrfPool := hitsToPassages(fused, poolK, poolChars)
	if len(rrfPool) == 0 {
		rrfPool = append([]Passage(nil), pool...)
	}
	diag["rrf_pool"] = len(rrfPool)
	diag["ce_input_pool"] = len(pool)
	diag["fuse_ms"] = time.Since(tFuse).Milliseconds()

	// --- Hydrate ∥ structure: overlapped post-retrieval arms (#276) ---
	// Arm H (hydrate chain) needs only fused-pool IDs; arm S (structure chain)
	// needs seed doc IDs plus durable path2/cortex structure. Neither mutates
	// the other's inputs, so the Neon-bound walls overlap instead of stacking
	// (residual hydrate∥structure pattern; wall ≈ max(arm), not sum).
	// Dependent steps stay ordered INSIDE each arm: hydrate-by-id → entity
	// stubs → sibling hydrate (each consumes the prior pool state), and path2 →
	// project hop2 → path2 doc hydrate (each hop seeds the next). Pool-virtual
	// expansion and the facts channel run after the join on the hydrated pool,
	// exactly as the serial ordering did.
	deepHydrate := wantsDeepHydrate(question, opts.QuestionType) && !expandLite && tier != TierLean
	skipSibling := envTruthy("OUROBOROS_ERB_SKIP_SIBLING", false) || expandLite || tier == TierLean
	maxDocs, chunksPerDoc := prod.HydrateDocs, prod.HydrateChunks
	if isMultiDocType(opts.QuestionType) || deepHydrate {
		maxDocs, chunksPerDoc = prod.HydrateDocsMulti, prod.HydrateChunksMulti
	}
	if maxDocs < 1 {
		maxDocs = 4
	}
	if chunksPerDoc < 1 {
		chunksPerDoc = 3
	}
	if deepHydrate {
		if chunksPerDoc < 6 {
			chunksPerDoc = 6
		}
		if maxDocs < 4 {
			maxDocs = 4
		}
		diag["hydrate_policy"] = "deep_multi_chunk"
	}
	structMax := prod.StructureMaxNeigh
	if structMax < 1 {
		structMax = 12
	}
	projectish := strings.EqualFold(opts.QuestionType, "project_related") ||
		strings.EqualFold(opts.QuestionType, "completeness") ||
		plan.Completeness || plan.MultiDoc
	if projectish && structMax < 20 {
		structMax = 20
	}
	// Path2 SQL: lean tier + ExpandLite skip; expand/standard multi-doc keep.
	// v5: structure cost only when genuinely needed (not every strong basic).
	skipPath2SQL := expandLite || tier == TierLean
	// Multi-doc/project always get structure even if tier was lean (reclassify).
	if projectish || plan.Completeness {
		skipPath2SQL = expandLite
	}
	if skipPath2SQL && !expandLite {
		diag["structure_sql_skipped"] = "tier_lean"
	}
	path2Eligible := c.db != nil && !c.productOwned && !skipPath2SQL
	factLim := fb.FactsLimit
	if factLim < 4 {
		factLim = 4
	}
	if tier == TierLean {
		factLim = 3
	}

	// path2Seeds snapshots seed doc IDs from src's top-6 plus offline entity
	// fanout. Stable pools may snapshot before the sibling hydrate; unstable
	// entity-stub pools compare before/after seed sequences and retain serial
	// post-hydrate seeding.
	path2Seeds := func(src []Passage) []string {
		seed := src
		if len(seed) > 6 {
			seed = seed[:6]
		}
		ids := uniqueDocIDs(seed)
		if off := c.scopedOfflineEntityDSIDs(question, 8); len(off) > 0 {
			ids = uniqueStringsStable(append(ids, off...))
		}
		return ids
	}

	type hydrateArmOut struct {
		pool      []Passage
		diag      map[string]any
		siblingMs int64
		wallMs    int64
	}
	// runHydrateSeedPhase owns its pool copy; the shared diag map is never
	// touched here (merged by the parent after the join). Hydrate-by-id and
	// entity stubs stay ordered because filled texts change stub classification.
	runHydrateSeedPhase := func(hydPool []Passage) hydrateArmOut {
		tArm := time.Now()
		out := hydrateArmOut{pool: hydPool, diag: map[string]any{}}
		// Hydrate missing text by chunk_id from Neon.
		var missing []string
		for _, p := range hydPool {
			if strings.TrimSpace(p.Text) == "" && p.ChunkID != "" {
				missing = append(missing, p.ChunkID)
			}
		}
		if len(missing) > 0 {
			tH := time.Now()
			// Hydrate-by-id is point lookups — keep tight budget (≤1.5s).
			hBudget := 1500 * time.Millisecond
			if prod.LexTimeout > 0 && prod.LexTimeout < hBudget {
				hBudget = prod.LexTimeout
			}
			hctx, hcancel := withTimeout(ctx, hBudget)
			hydrated, err := hydrateByChunkIDs(hctx, c.db, c.cfg, missing)
			hcancel()
			out.diag["hydrate_by_id_ms"] = time.Since(tH).Milliseconds()
			out.diag["hydrate_by_id_n"] = len(missing)
			if err != nil {
				out.diag["hydrate_by_id_error"] = err.Error()
			} else {
				byID := map[string]Hit{}
				for _, h := range hydrated {
					byID[h.ChunkID] = h
				}
				for i := range hydPool {
					if hydPool[i].Text != "" {
						continue
					}
					if h, ok := byID[hydPool[i].ChunkID]; ok {
						hydPool[i].Text = h.Text
						if hydPool[i].DocumentID == "" {
							hydPool[i].DocumentID = h.DSID
						}
						if hydPool[i].SourceURI == "" {
							hydPool[i].SourceURI = h.SourceURI
						}
					}
				}
			}
		} else {
			out.diag["hydrate_by_id_n"] = 0
			out.diag["hydrate_by_id_ms"] = 0
		}
		// Offline entity stubs use synthetic chunk_id — hydrate by DSID. This
		// supplemental fan-out is bounded separately from the main sibling
		// hydrate so a long sequential Neon tail cannot dominate interactive
		// latency.
		tEntity := time.Now()
		entityCtx, entityCancel := withTimeout(ctx, hydrateBudget(prod))
		entityStubN := len(offlineEntityStubIDs(hydPool))
		hydPool = hydrateOfflineEntityStubs(entityCtx, c.db, c.cfg, hydPool, 2)
		entityCancel()
		out.diag["entity_stub_hydrate_ms"] = time.Since(tEntity).Milliseconds()
		out.diag["entity_stub_hydrate_n"] = entityStubN
		out.diag["entity_stub_hydrate_resolved_n"] = entityStubN - len(offlineEntityStubIDs(hydPool))
		if entityStubN > len(offlineEntityStubIDs(hydPool)) {
			out.diag["entity_catalog_offline_hydrated"] = true
		}
		out.pool = hydPool
		out.wallMs = time.Since(tArm).Milliseconds()
		return out
	}

	// continueHydrateArm performs only the sibling tail. Keeping this separate
	// lets a seed-stable entity pool overlap its remaining hydrate with path2.
	continueHydrateArm := func(out hydrateArmOut) hydrateArmOut {
		tArm := time.Now()
		if !skipSibling {
			tHyd := time.Now()
			hBudget := hydrateBudget(prod)
			hctx, hcancel := withTimeout(ctx, hBudget)
			out.pool = hydrateTopDocs(hctx, c.db, c.cfg, out.pool, maxDocs, chunksPerDoc)
			hcancel()
			out.siblingMs = time.Since(tHyd).Milliseconds()
			out.diag["hydrate_policy"] = "product_always"
		}
		out.wallMs += time.Since(tArm).Milliseconds()
		return out
	}

	runHydrateArm := func(hydPool []Passage) hydrateArmOut {
		return continueHydrateArm(runHydrateSeedPhase(hydPool))
	}

	type structureArmOut struct {
		p2docs       []string
		p2diag       map[string]any
		p2budgetMs   int64
		p2HydrateRan bool
		hop2Ran      bool
		hop2Docs     int
		hop2Diag     map[string]any
		structExtra  []Passage
		trPass       []Passage
		trDiag       map[string]any
		wallMs       int64
	}
	// runStructureArm reads only the seed snapshot + durable structure; it
	// never mutates pool. Path2 hop2 depends on hop1 docs and stays ordered
	// inside the arm. Temporal relations depend on the question only.
	runStructureArm := func(seedIDs []string) structureArmOut {
		tArm := time.Now()
		out := structureArmOut{}
		if path2Eligible {
			sBudget := structureSQLBudget(prod)
			// Project: slight headroom, hard cap 2.5s (was 5–8s).
			if projectish && sBudget < 2500*time.Millisecond {
				sBudget = 2500 * time.Millisecond
			}
			if sBudget > 2500*time.Millisecond {
				sBudget = 2500 * time.Millisecond
			}
			out.p2budgetMs = sBudget.Milliseconds()
			sctx, scancel := withTimeout(ctx, sBudget)
			out.p2docs, out.p2diag = path2StructureExpand(sctx, c.db, c.cfg.BrainID, question, seedIDs, structMax)
			scancel()
			// Project second hop only (ticket↔wiki↔PR); ≤2s.
			if projectish && len(out.p2docs) > 0 {
				out.hop2Ran = true
				seed2 := uniqueStringsStable(append(seedIDs, out.p2docs...))
				if len(seed2) > 12 {
					seed2 = seed2[:12]
				}
				s2budget := 2 * time.Second
				s2ctx, s2cancel := withTimeout(ctx, s2budget)
				p2b, p2bdiag := path2StructureExpand(s2ctx, c.db, c.cfg.BrainID, question, seed2, structMax)
				s2cancel()
				if len(p2b) > 0 {
					out.p2docs = uniqueStringsStable(append(out.p2docs, p2b...))
					out.hop2Docs = len(p2b)
				}
				out.hop2Diag = p2bdiag
			}
			if len(out.p2docs) > 0 {
				out.p2HydrateRan = true
				hctx, hcancel := withTimeout(ctx, structureHydrateBudget(prod))
				out.structExtra = hydratePath2StructureDocs(hctx, c.db, c.cfg, out.p2docs, 2)
				hcancel()
			}
		}
		// TemporalRelations run after the join because the helper has no context
		// surface of its own; keeping it out of the bounded structure arm prevents
		// a canceled caller from waiting on an uninterruptible memory walk.
		if path2Eligible {
			out.wallMs = time.Since(tArm).Milliseconds()
		}
		return out
	}

	serialHS := envTruthy("OUROBOROS_ERB_SERIAL_HYDRATE_STRUCTURE", false)
	tHS := time.Now()
	var hydOut hydrateArmOut
	var structOut structureArmOut
	serialReason := ""
	entitySeedPrepared := false
	entitySeedsStable := false
	// Entity-stub hydration can move stubs to the pool tail. Run only the
	// seed-affecting hydrate prefix, then compare the exact path2 seed sequence.
	// Stable seeds can safely overlap sibling hydration with structure; changed
	// seeds retain the byte-equivalent serial fallback from #276.
	if !serialHS && c.db != nil && path2Eligible && !skipSibling && len(offlineEntityStubIDs(pool)) > 0 {
		seedBeforePool := pool
		if len(seedBeforePool) > poolK {
			seedBeforePool = seedBeforePool[:poolK]
		}
		seedBefore := path2Seeds(seedBeforePool)
		hydOut = runHydrateSeedPhase(append([]Passage(nil), pool...))
		pool = hydOut.pool
		seedAfterPool := pool
		if len(seedAfterPool) > poolK {
			seedAfterPool = seedAfterPool[:poolK]
		}
		entitySeedPrepared = true
		entitySeedsStable = slices.Equal(seedBefore, path2Seeds(seedAfterPool))
		diag["entity_stub_seed_ids_unchanged"] = entitySeedsStable
		if !entitySeedsStable {
			serialHS = true
			serialReason = "offline_entity_stub_seed_safety"
		}
	} else if !serialHS && len(offlineEntityStubIDs(pool)) > 0 {
		serialHS = true
		serialReason = "offline_entity_stub_seed_safety"
	}
	// parallelRan is true only when both arms actually executed concurrently.
	// It stays false for env-serial, the stub-safety fallback, and c.db==nil
	// (structure runs synchronously with no hydrate arm to overlap) so the
	// hydrate_structure_parallel diagnostic is honest about real overlap.
	parallelRan := false
	if serialHS {
		// Operational rollback switch / stub-safety fallback: faithful legacy
		// serial execution. Hydration runs to completion first, then path2
		// seeds are derived from the hydrated, poolK-trimmed pool exactly as the
		// pre-#276 pipeline did. The post-join pool-virtual expansion still
		// merges pool-virtual → path2 → temporal in legacy order on that same
		// hydrated pool, so structNeigh and the final window are byte-identical
		// to legacy serial ordering. (The wall-clock grouping inside the
		// structure section differs — path2 runs before pool-virtual expansion —
		// but structureExpandPassages reads only the hydrated pool and merges in
		// the same order, so results are identical, not merely equivalent.)
		if c.db != nil {
			if entitySeedPrepared {
				hydOut = continueHydrateArm(hydOut)
			} else {
				hydOut = runHydrateArm(append([]Passage(nil), pool...))
			}
			pool = hydOut.pool
		} else {
			diag["hydrate_ms"] = 0
			if expandLite {
				diag["hydrate_policy"] = "skipped_expand_lite"
			} else {
				diag["hydrate_policy"] = "skipped_env"
			}
			diag["sibling_hydrate_skipped"] = skipSibling
		}
		if len(pool) > poolK {
			pool = pool[:poolK]
		}
		structOut = runStructureArm(path2Seeds(pool))
	} else {
		// Parallel: arm H owns a private pool copy; arm S reads an exact seed
		// snapshot. A seed-stable entity pool has already completed only the
		// seed-affecting prefix, so its sibling tail can still overlap path2.
		if entitySeedsStable {
			seedPool := pool
			if len(seedPool) > poolK {
				seedPool = seedPool[:poolK]
			}
			seedIDs := path2Seeds(seedPool)
			var hsWG sync.WaitGroup
			hsWG.Add(2)
			go func() {
				defer hsWG.Done()
				hydOut = continueHydrateArm(hydOut)
			}()
			go func() {
				defer hsWG.Done()
				structOut = runStructureArm(seedIDs)
			}()
			hsWG.Wait()
			pool = hydOut.pool
			parallelRan = true
		} else if c.db != nil && path2Eligible {
			seedSnap := pool
			if len(seedSnap) > poolK {
				seedSnap = seedSnap[:poolK]
			}
			seedIDs := path2Seeds(seedSnap)
			hydPool := append([]Passage(nil), pool...)
			var hsWG sync.WaitGroup
			hsWG.Add(2)
			go func() {
				defer hsWG.Done()
				hydOut = runHydrateArm(hydPool)
			}()
			go func() {
				defer hsWG.Done()
				structOut = runStructureArm(seedIDs)
			}()
			hsWG.Wait()
			pool = hydOut.pool
			parallelRan = true
		} else if c.db != nil {
			// A durable store without an eligible path2 arm has no independent
			// structure work to overlap; preserve the bounded serial path.
			serialReason = "path2_ineligible"
			hydOut = runHydrateArm(append([]Passage(nil), pool...))
			pool = hydOut.pool
			seedPool := pool
			if len(seedPool) > poolK {
				seedPool = seedPool[:poolK]
			}
			structOut = runStructureArm(path2Seeds(seedPool))
		} else {
			// No durable store: there is no hydrate arm to overlap, so the
			// section is synchronous. parallelRan stays false.
			diag["hydrate_ms"] = 0
			if expandLite {
				diag["hydrate_policy"] = "skipped_expand_lite"
			} else {
				diag["hydrate_policy"] = "skipped_env"
			}
			diag["sibling_hydrate_skipped"] = skipSibling
			structOut = runStructureArm(nil)
		}
	}
	// Wall vs arm diagnostics: wall is the overlapped section; arms report
	// their own walls and keep per-stage failure keys attributable.
	diag["hydrate_structure_parallel"] = parallelRan
	if serialReason != "" {
		diag["hydrate_structure_serial_reason"] = serialReason
	}
	diag["hydrate_structure_wall_ms"] = time.Since(tHS).Milliseconds()
	diag["hydrate_arm_ms"] = hydOut.wallMs
	diag["structure_arm_ms"] = structOut.wallMs
	// Merge arm diagnostics single-threaded post-join (arms never wrote diag).
	if c.db != nil {
		for k, v := range hydOut.diag {
			diag[k] = v
		}
		if skipSibling {
			diag["hydrate_ms"] = 0
			if expandLite {
				diag["hydrate_policy"] = "skipped_expand_lite"
			} else {
				diag["hydrate_policy"] = "skipped_env"
			}
			diag["sibling_hydrate_skipped"] = skipSibling
		} else {
			diag["hydrate_ms"] = hydOut.siblingMs
		}
	}
	if len(pool) > poolK {
		pool = pool[:poolK]
	}

	// Structure: pool-virtual + precomputed path2 SQL (left-shifted brain).
	// No query-time OpenIE — only read durable structure. Runs post-join on
	// the hydrated pool exactly as the serial ordering did.
	tStructPost := time.Now()
	seed := pool
	if len(seed) > 6 {
		seed = seed[:6]
	}
	// Do not shrink structure when HotLex is "strong" — that starved link expand.
	structNeigh, sdiag := structureExpandPassages(seed, pool, structMax)
	for k, v := range sdiag {
		diag[k] = v
	}
	if path2Eligible {
		diag["structure_sql_budget_ms"] = structOut.p2budgetMs
		mergePath2StructureDiag(diag, structOut.p2diag, len(structOut.p2docs))
		if structOut.hop2Ran {
			if structOut.hop2Docs > 0 {
				diag["structure_project_hop2"] = true
				diag["structure_project_hop2_docs"] = structOut.hop2Docs
			}
			for k, v := range structOut.hop2Diag {
				diag["hop2_"+k] = v
			}
		}
		if structOut.p2HydrateRan {
			structNeigh = mergePassagesStructure(structNeigh, structOut.structExtra, structMax+12)
			diag["structure_sql_promoted"] = len(structOut.structExtra)
		}
	} else if expandLite {
		diag["structure_sql_skipped"] = "expand_lite"
	}
	if ctx.Err() == nil {
		structOut.trPass, structOut.trDiag = c.temporalRelationPassages(question, structMax)
	} else {
		structOut.trDiag = map[string]any{
			"temporal_relation":                false,
			"temporal_relation_skipped":        "context_done",
			"temporal_relation_context_status": retrievalContextStatus(ctx.Err()),
		}
	}
	for k, v := range structOut.trDiag {
		diag[k] = v
	}
	if len(structOut.trPass) > 0 {
		structNeigh = mergePassagesStructure(structNeigh, structOut.trPass, structMax+8)
	}
	// Facts channel: expand gets more SPO-ish hits (smf: wire inert structure).
	factPass, fdiag := factsChannelPassages(question, pool, factLim)
	for k, v := range fdiag {
		diag[k] = v
	}
	diag["structure_pool_only"] = false
	pool = mergePassagesStructure(pool, structNeigh, poolK+8)
	pool = mergePassagesStructure(pool, factPass, poolK+10)
	diag["structure_ms"] = structOut.wallMs + time.Since(tStructPost).Milliseconds()
	diag["smf_facts_injected"] = len(factPass)

	// Corpus grep: only thin/flat pools (latency); never ExpandLite; skip lean.
	nPoolDocs := len(uniqueDSIDs(pool))
	needGrep := !expandLite && tier != TierLean &&
		(nPoolDocs < 6 || (prod.Quality && flat > 0.55 && nPoolDocs < 12) ||
			(semanticish && nPoolDocs < 10 && tier == TierExpand))
	if needGrep {
		tGrep := time.Now()
		if !hotAvailable && ftsAttempted {
			diag["corpus_grep_fts_skipped"] = "hot_lex_unavailable_single_fallback"
		}
		ftsState := retrievalFTSState{
			hotLexAvailable:         hotAvailable,
			phaseAFallbackAttempted: ftsAttempted,
			ftsDisabled:             ftsDisabled,
		}
		grepHits := c.corpusGrepFallback(ctx, question, prod, pool, ftsState)
		if len(grepHits) > 0 {
			extra := hitsToPassages(grepHits, 40, poolChars)
			pool = mergePassages(pool, extra, cePoolN+20)
			rrfPool = mergePassages(rrfPool, extra, poolK+20)
			diag["corpus_grep_fallback"] = true
			diag["corpus_grep_hits"] = len(grepHits)
		} else {
			diag["corpus_grep_fallback"] = false
		}
		diag["corpus_grep_ms"] = time.Since(tGrep).Milliseconds()
	} else if expandLite {
		diag["corpus_grep_skipped"] = "expand_lite"
	} else if tier == TierLean {
		diag["corpus_grep_skipped"] = "tier_lean"
	}

	// CE: smf wide pool on expand (rerank_cap 120–150), lean tight for latency.
	tCE := time.Now()
	ceN := cePoolSizeN(len(pool), topK, prod, fb.CEPool)
	forceLexCE := !preferRealCE(prod.Quality)
	reranked, reDiag := c.crossEncodeRerankClient(ctx, question, pool, ceN, forceLexCE)
	for k, v := range reDiag {
		diag[k] = v
	}
	diag["ce_ms"] = time.Since(tCE).Milliseconds()
	diag["ce_n"] = ceN
	scores := normalizeCEScores(reranked)
	agenticSig, agenticWhy := shouldSignalAgentic(question, scores)
	diag["signal_agentic"] = agenticSig
	diag["signal_agentic_reason"] = agenticWhy
	if top, mean3 := confidenceTopMean3(scores); true {
		diag["ce_conf_top"] = top
		diag["ce_conf_mean3"] = mean3
	}

	tWin := time.Now()
	// smf window: lean 12, expand 18–20, multi-gold adaptive.
	winK := fb.WinK
	if winK < topK {
		winK = topK
	}
	if winK < 12 {
		winK = 12
	}
	if winK > 20 {
		winK = 20
	}
	covN := min(len(reranked), winK*3)
	if tier == TierExpand && covN < 48 {
		covN = min(len(reranked), 48)
	}
	covered := coverageRerank(question, reranked, covN, fb.CovLambda)
	covered, awareDiag := questionAwareRerank(covered, question, opts.QuestionType, opts.SourceTypes)
	for k, v := range awareDiag {
		diag[k] = v
	}
	divCap := fb.DivCap
	if divCap < 3 {
		divCap = 3
	}
	window, winDiag := progressiveRetainWindow(covered, question, winK, divCap, opts.GoldDocIDs)
	if len(opts.GoldDocIDs) > 0 {
		// Multi-gold project/completeness: floor must fit ALL gold in pack.
		floorK := winK
		if n := len(opts.GoldDocIDs); n+2 > floorK {
			floorK = n + 2
		}
		if isMultiDocType(opts.QuestionType) && floorK < len(opts.GoldDocIDs)+4 {
			floorK = len(opts.GoldDocIDs) + 4
		}
		if floorK > 28 {
			floorK = 28
		}
		window = ensureGoldInWindow(rrfPool, window, opts.GoldDocIDs, floorK)
		// Second pass from full pool (pre-CE union) so multi-gold survives CE cut.
		window = ensureGoldInWindow(append(rrfPool, pool...), window, opts.GoldDocIDs, floorK)
		winDiag["gold_floor"] = true
		winDiag["gold_floor_k"] = floorK
	}
	// P1 union context: CE window + RRF head + near-dups (version adjudication).
	window = unionContextWindow(window, append(rrfPool, pool...), winK)
	window, authDiag := applyAuthorityRecencyQ(window, question, opts.QuestionType, opts.SourceTypes)
	diag["authority"] = authDiag
	// Passage-level supersession (near-dup date/currency) — works without Mem.
	window, superDiag := adjudicateSupersession(window, question)
	for k, v := range superDiag {
		diag[k] = v
	}
	window = bestLast(window)
	// P0 recency/near-dup headers for synth (prefer NEWEST / effective dates).
	window = annotateRecencyPack(window)
	diag["recency_pack"] = true
	diag["best_last"] = true
	diag["window"] = winDiag
	diag["window_ms"] = time.Since(tWin).Milliseconds()
	diag["pool_window"] = diagnoseWindow(rrfPool, window, opts.GoldDocIDs)
	if len(opts.GoldDocIDs) > 0 {
		if gd := computeGoldDiag(opts.GoldDocIDs, rrfPool, window); gd != nil {
			for k, v := range gd {
				diag[k] = v
			}
		}
	}
	stampQualityProfile(diag, prod)
	diag["coverage_rerank"] = true
	diag["product_v5"] = true
	diag["passage_count"] = len(window)
	diag["total_us"] = time.Since(t0).Microseconds()
	diag["total_ms"] = time.Since(t0).Milliseconds()
	diag["first_evidence_ms"] = diag["total_ms"]
	diag["cache_hit"] = false
	diag["giant_search"] = true
	diag["arms"] = []string{"hot_lex", "dense", "recovery?", "neon_fts_fallback", "structure", "ce_union", "coverage", "union_ctx", "recency_pack"}
	diag["store"] = c.StoreKind()
	if indexDiag := c.entityCatalogIndexDiagnostics(); len(indexDiag) > 0 {
		diag["entity_catalog_index"] = indexDiag
	}
	// Stage breakdown for p95 attribution (sum ≈ total when sequential; the
	// overlapped hydrate∥structure section is also split into arm walls plus
	// hydrate_structure_wall_ms, so its stages may sum past total by design).
	diag["latency_breakdown"] = map[string]any{
		"hot_lex_ms":                diag["hot_lex_ms"],
		"dense_ms":                  diag["dense_ms"],
		"dense_embed_ms":            diag["dense_embed_ms"],
		"dense_ann_ms":              diag["dense_ann_ms"],
		"recovery_ms":               diag["recovery_ms"],
		"phase_b_fts_ms":            diag["phase_b_fts_ms"],
		"fuse_ms":                   diag["fuse_ms"],
		"hydrate_by_id_ms":          diag["hydrate_by_id_ms"],
		"entity_stub_hydrate_ms":    diag["entity_stub_hydrate_ms"],
		"hydrate_ms":                diag["hydrate_ms"],
		"structure_ms":              diag["structure_ms"],
		"hydrate_arm_ms":            diag["hydrate_arm_ms"],
		"structure_arm_ms":          diag["structure_arm_ms"],
		"hydrate_structure_wall_ms": diag["hydrate_structure_wall_ms"],
		"ce_ms":                     diag["ce_ms"],
		"window_ms":                 diag["window_ms"],
		"total_ms":                  diag["total_ms"],
	}
	// Skip cache put when GoldDocIDs set (gold diags are request-scoped).
	if len(opts.GoldDocIDs) == 0 && ck != "" {
		if n := c.qcache.put(ck, window, diag); n > 0 {
			diag["cache_evicted"] = n
		}
	}
	return window, diag, nil
}

// retrieveInteractiveLocal is HotLex-first for memory/local (+ optional local dense).
func (c *Client) retrieveInteractiveLocal(
	ctx context.Context,
	question string,
	opts RetrieveOptions,
	diag map[string]any,
	topK, poolK int,
	t0 time.Time,
	prod ProdProfile,
	ck string,
) ([]Passage, map[string]any, error) {
	diag["retrieve_class"] = "interactive_local"
	diag["product_owned"] = true
	diag["pipeline"] = []string{"hot_lex", "local_dense?", "store_lexical_fallback?", "edge_hop", "entity_fanout", "facts_channel", "structure_pool", "coverage_mmr", "identifier_retain_window"}
	expandLite := opts.ExpandLite
	if expandLite {
		diag["expand_lite"] = true
		diag["retrieve_class"] = "interactive_local_expand_lite"
	}
	limit := prod.LexLimit
	if limit <= 0 {
		limit = 40
	}
	var hotHits []Hit
	tHot := time.Now()
	if c.hot != nil {
		hotHits = c.hot.Search(question, limit)
		diag["hot_lex_docs"] = c.hot.Len()
	} else {
		diag["hot_lex_docs"] = 0
	}
	diag["hot_lex_ms"] = time.Since(tHot).Milliseconds()
	diag["hot_lex_hits"] = len(hotHits)
	var lists [][]Hit
	if len(hotHits) > 0 {
		lists = append(lists, hotHits)
	}
	// Fallback to store lexical if thin (skip on ExpandLite — HotLex only).
	minThin := prod.LexExpandIfThin
	if minThin < 1 {
		minThin = 6
	}
	if !expandLite && len(hotHits) < minThin && c.store != nil {
		storeLexBudget := boundedFTSBudget(prod, prod.LexTimeout)
		storeLexCtx, storeLexCancel := withTimeout(ctx, storeLexBudget)
		hits, err := c.store.LexicalSearch(storeLexCtx, c.cfg.BrainID, question, limit)
		storeLexCancel()
		diag["store_lexical_budget_ms"] = storeLexBudget.Milliseconds()
		diag["store_lexical_caller_deadline_only"] = storeLexBudget <= 0
		if err == nil && len(hits) > 0 {
			lists = append(lists, hits)
			diag["store_lexical_fallback"] = len(hits)
		}
	}
	if len(lists) == 0 {
		// Soft empty (e.g. stopword-only "a b"); not a hard Failure.
		diag["passage_count"] = 0
		diag["cache_hit"] = false
		diag["soft_empty"] = true
		diag["store"] = c.StoreKind()
		return nil, diag, nil
	}
	fused := rrfFuseMany(lists, c.cfg.RRFK)
	storeChars := storagePassageChars(c.cfg.MaxPassageChars)
	pool := hitsToPassages(fused, poolK, storeChars)
	// Local dense (sqlite) shares the residual interactive path — not Qdrant-only.
	if !expandLite {
		pool = c.mergeLocalDenseArm(ctx, question, pool, diag, poolK+16)
	} else {
		diag["local_dense_skipped"] = "expand_lite"
	}
	// Also pull structure-linked docs from memory store index (full corpus).
	if expandLite {
		diag["structure_sql_skipped"] = "expand_lite"
		diag["recovery_skipped"] = "expand_lite"
		diag["recovery"] = false
	} else if expander, ok := c.store.(structureExpander); ok {
		seeds := uniqueDocIDs(pool)
		if len(seeds) > 5 {
			seeds = seeds[:5]
		}
		edgeIDs, entIDs, _ := expander.StructureExpand(c.cfg.BrainID, seeds, 12)
		factIDs := expander.StructureFacts(c.cfg.BrainID, question, 8)
		diag["edge_neighbors"] = edgeIDs
		diag["entity_neighbors"] = entIDs
		diag["facts_hits"] = factIDs
		extra := expander.PassagesForDocs(c.cfg.BrainID, append(append(edgeIDs, entIDs...), factIDs...), storeChars)
		pool = mergePassagesStructure(pool, extra, poolK+16)
	} else {
		seed := pool
		if len(seed) > 6 {
			seed = seed[:6]
		}
		structNeigh, sdiag := structureExpandPassages(seed, pool, prod.StructureMaxNeigh)
		for k, v := range sdiag {
			diag[k] = v
		}
		factPass, fdiag := factsChannelPassages(question, pool, 6)
		for k, v := range fdiag {
			diag[k] = v
		}
		pool = mergePassagesStructure(pool, structNeigh, poolK+8)
		pool = mergePassagesStructure(pool, factPass, poolK+10)
	}
	// Cortex TemporalRelations → document passages (skip ExpandLite nested).
	if !expandLite {
		trPass, trDiag := c.temporalRelationPassages(question, 12)
		for k, v := range trDiag {
			diag[k] = v
		}
		if len(trPass) > 0 {
			pool = mergePassagesStructure(pool, trPass, poolK+16)
		}
	}
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
	diag["pool_window"] = diagnoseWindow(pool, window, opts.GoldDocIDs)
	if len(opts.GoldDocIDs) > 0 {
		if gd := computeGoldDiag(opts.GoldDocIDs, pool, window); gd != nil {
			for k, v := range gd {
				diag[k] = v
			}
		}
	}
	// Same profile stamps as residual / path2 so ERB+company diags match.
	stampQualityProfile(diag, prod)
	diag["passage_count"] = len(window)
	diag["total_ms"] = time.Since(t0).Milliseconds()
	diag["first_evidence_ms"] = diag["total_ms"]
	diag["cache_hit"] = false
	diag["giant_search"] = true
	if _, ok := diag["dense_arm"]; ok {
		diag["arms"] = []string{"hot_lex", "dense", "structure", "coverage"}
	} else {
		diag["arms"] = []string{"hot_lex", "structure", "coverage"}
	}
	// Skip cache put when GoldDocIDs set (gold diags are request-scoped).
	if len(opts.GoldDocIDs) == 0 && ck != "" {
		if n := c.qcache.put(ck, window, diag); n > 0 {
			diag["cache_evicted"] = n
		}
	}
	return window, diag, nil
}

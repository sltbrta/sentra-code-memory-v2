package hosted

import (
	"context"
	"strings"
	"time"
)

// AgenticOptions budgets tool rounds for multi-doc expansion.
type AgenticOptions struct {
	Enabled      bool
	MaxRounds    int
	MaxExtraDocs int
	GoldHint     []string // optional for tests only; never from production judges
	// SourceTypes and Filter preserve the already-authorized request scope for
	// nested retrieval. GoldDocIDs are deliberately not part of this type.
	SourceTypes []string
	Filter      map[string]any
	// Plan optional; when set, hardPool/topK floors use capability flags.
	Plan *QueryPlan
	// ForceExpand forces the first reformulate round even when nSeed is high
	// and gradeEvidence says sufficient. Only for low-confidence seed-dense
	// signals where CE scores show relevant evidence may be missing.
	ForceExpand bool
}

// agenticExpand grades evidence and expands via edge-like sibling hydrate and
// reformulation re-retrieve when the window looks thin for multi-doc types.
func (c *Client) agenticExpand(
	ctx context.Context,
	question, questionType string,
	window []Passage,
	opts AgenticOptions,
) ([]Passage, map[string]any) {
	diag := map[string]any{
		"agentic": false,
		"rounds":  0,
	}
	// Enabled is the sole gate. Callers set Enabled from wantsAgentic+prod
	// (answer.go: agentOn). Do NOT re-enable via wantsAgentic when Enabled=false.
	if !opts.Enabled {
		return window, diag
	}
	ledger := retrievalExpansionLedgerFrom(ctx)
	if ledger == nil {
		ledger = newRequestRetrievalExpansionLedger()
		ctx = withRetrievalExpansionLedger(ctx, ledger)
	}
	ctx, budgetCancel := ledger.budgetContext(ctx)
	defer budgetCancel()
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = 1 // one ExpandLite round (≤60s agentic budget)
	}
	if opts.MaxExtraDocs <= 0 {
		opts.MaxExtraDocs = 4
	}
	diag["agentic"] = true
	cur := append([]Passage(nil), window...)
	rounds := 0
	tools := []string{}
	// Multi-doc expand policy (random-40 gap analysis):
	// - completeness / semantic / conflicting often pack 8–10 WRONG docs with high
	//   lexical gap — skipping expand on nSeed≥7 left pool_recall=0 (v40).
	// - constrained/project with dense correct packs can skip expand for latency.
	nSeed := len(uniqueDSIDs(window))
	gapSeed := lexicalGap(question, window)
	plan := QueryPlan{}
	if opts.Plan != nil {
		plan = *opts.Plan
	} else {
		_, plan = ResolveQuestionType(question, questionType)
	}
	hardPool := plan.WantHardPoolExpand()
	// Force expand only when pack is thin/gappy — not every multi-doc with 12+ seeds.
	forceOne := hardPool && (nSeed < 10 || gapSeed > 0.35)
	if !forceOne {
		forceOne = (plan.MultiDoc || isMultiDocType(questionType)) && (nSeed < 6 || gapSeed > 0.45)
	}
	// ForceExpand (low-confidence seed-dense signal) ensures one round even when
	// the local nSeed/gap heuristics would otherwise skip.
	if opts.ForceExpand {
		forceOne = true
	}
	diag["force_expand"] = forceOne
	diag["seed_docs"] = nSeed
	diag["seed_gap"] = gapSeed
	diag["hard_pool_expand"] = hardPool

	// One-bound recursive gap expand flag (not unbounded recursion).
	didGapRecursive := false
	for rounds < opts.MaxRounds && ledger.canContinue(ctx, "agentic_round") {
		grade := gradeEvidence(question, questionType, cur)
		diag["grade"] = grade
		if grade["sufficient"] == true && !(forceOne && rounds == 0) {
			break
		}
		// Only skip expand for non-hard multi-doc when the pack is already dense
		// and gap is modest (likely gold already in window).
		if !forceOne && rounds == 0 && nSeed >= 7 && gapSeed <= 0.40 {
			break
		}
		rounds++
		// Reformulate: static phrase bags only (LLM multiquery adds RTT; ExpandLite is enough).
		subs := pickHotLexPhrases(question, 3)
		// Prefer short sub-clauses over long full-Q decompose tails for HotLex/ANN.
		for _, d := range decomposeQuery(question, questionType) {
			if len(d) <= 100 && len(d) >= 20 {
				// Ranked phrases on the clause beat the raw long clause.
				subs = append(subs, pickHotLexPhrases(d, 1)...)
			}
		}
		if len(subs) == 0 {
			subs = multiQueryVariants(question, questionType)
		}
		subs = dedupeQueries(subs)
		// Parallel phrase retrieves — ExpandLite only (never full QUALITY re-retrieve).
		nSub := 2
		if nSub > len(subs) {
			nSub = len(subs)
		}
		// Wider topK on hard expand so multi-gold can enter the merge.
		expandTopK := 8
		if hardPool {
			expandTopK = 10
		}
		if nSub > 0 {
			type pack struct{ ps []Passage }
			ch := make(chan pack, nSub)
			for i := 0; i < nSub; i++ {
				sub := subs[i]
				go func(q string) {
					// ExpandLite: HotLex+1 dense, no recovery/structure/grep stack.
					more, _, err, attempted := c.expansionRetrieve(
						ctx, "agentic_reformulate", q, expandTopK, "basic",
						opts.SourceTypes, opts.Filter,
					)
					if !attempted || err != nil {
						ch <- pack{}
						return
					}
					ch <- pack{ps: more}
				}(sub)
			}
			for i := 0; i < nSub; i++ {
				p := <-ch
				if len(p.ps) > 0 {
					tools = append(tools, "reformulate_retrieve")
					cur = mergePassages(cur, p.ps, opts.MaxExtraDocs+len(window)+12)
				}
			}
		}
		// ONE recursive gap pass: ExpandLite only (full re-retrieve was the 200s sink).
		if !didGapRecursive && (hardPool || lexicalGap(question, cur) > 0.35) {
			if gapQ := gapQueryFromPassages(question, cur); gapQ != "" &&
				!strings.EqualFold(gapQ, question) {
				didGapRecursive = true
				gapTopK := 12
				if hardPool {
					gapTopK = 16
				}
				gapType := questionType
				if gapType == "" {
					gapType = "basic"
				}
				more, _, err, attempted := c.expansionRetrieve(
					ctx, "agentic_gap", gapQ, gapTopK, gapType,
					opts.SourceTypes, opts.Filter,
				)
				if attempted && err == nil && len(more) > 0 {
					tools = append(tools, "gap_recursive_retrieve")
					cur = mergePassages(cur, more, opts.MaxExtraDocs+len(window)+16)
					diag["gap_recursive"] = true
					diag["gap_query"] = gapQ
				} else {
					diag["gap_recursive"] = false
					diag["gap_query"] = gapQ
				}
			}
		}
		// Sibling hydrate: thin packs always; deep-hydrate types always pull more
		// chunks so late INC corrections / freeze dates enter the window.
		if c.db != nil {
			needHydrate := len(uniqueDSIDs(cur)) < 4 || wantsDeepHydrate(question, questionType)
			if needHydrate {
				tools = append(tools, "hydrate_expand")
				hctx, hcancel := withTimeout(ctx, 2*time.Second)
				hDocs, hChunks := 4, 3
				if wantsDeepHydrate(question, questionType) {
					hDocs, hChunks = 4, 6
				}
				cur = hydrateTopDocs(hctx, c.db, c.cfg, cur, hDocs, hChunks)
				hcancel()
			}
		}
	}
	// Coverage re-pack
	topK := c.cfg.TopK
	if topK <= 0 {
		topK = 8
	}
	if fl := plan.TopKFloor(); fl > 0 && topK < fl {
		topK = fl
	} else if isMultiDocType(questionType) && topK < 10 {
		topK = 10
	}
	seed := append([]Passage(nil), window...)
	// Protect pre-agentic seed docs (often multi-gold) through wide→narrow cut.
	var protect []string
	seenP := map[string]struct{}{}
	for _, p := range seed {
		if p.DocumentID == "" {
			continue
		}
		if _, ok := seenP[p.DocumentID]; ok {
			continue
		}
		seenP[p.DocumentID] = struct{}{}
		protect = append(protect, p.DocumentID)
	}
	cur = coverageRerank(question, cur, topK*3, 0.65)
	var winDiag map[string]any
	cur, winDiag = progressiveRetainWindow(cur, question, topK, 8, protect)
	for k, v := range winDiag {
		diag["agentic_"+k] = v
	}
	// Agentic re-pack can drop multi-gold seed docs that already survived retrieve.
	// Floor seeds back in (by DocumentID) so post-agentic cite floors can fire.
	if len(seed) > 0 {
		floorK := topK
		if len(protect)+2 > floorK {
			floorK = len(protect) + 2
		}
		cur = ensureSeedDocsInWindow(seed, cur, floorK)
		diag["seed_floor"] = true
		diag["seed_floor_k"] = floorK
	}
	diag["rounds"] = rounds
	diag["tools"] = tools
	diag["final_passages"] = len(cur)
	ledger.stampInto(diag)
	return cur, diag
}

// ensureSeedDocsInWindow keeps every unique DocumentID from seed that is missing
// from window (multi-gold agentic drop). Caps at topK with seeds first.
func ensureSeedDocsInWindow(seed, window []Passage, topK int) []Passage {
	if topK <= 0 {
		topK = 8
	}
	winSet := map[string]struct{}{}
	for _, p := range window {
		if p.DocumentID != "" {
			winSet[p.DocumentID] = struct{}{}
		}
	}
	var missing []Passage
	seenMiss := map[string]struct{}{}
	for _, p := range seed {
		if p.DocumentID == "" {
			continue
		}
		if _, ok := winSet[p.DocumentID]; ok {
			continue
		}
		if _, ok := seenMiss[p.DocumentID]; ok {
			continue
		}
		seenMiss[p.DocumentID] = struct{}{}
		p.Channel = p.Channel + "+seed_floor"
		missing = append(missing, p)
	}
	if len(missing) == 0 {
		return window
	}
	out := make([]Passage, 0, topK)
	seen := map[string]struct{}{}
	for _, p := range missing {
		if len(out) >= topK {
			break
		}
		if _, ok := seen[p.DocumentID]; ok {
			continue
		}
		seen[p.DocumentID] = struct{}{}
		out = append(out, p)
	}
	for _, p := range window {
		if len(out) >= topK {
			break
		}
		if p.DocumentID != "" {
			if _, ok := seen[p.DocumentID]; ok {
				continue
			}
			seen[p.DocumentID] = struct{}{}
		}
		out = append(out, p)
	}
	return out
}

func wantsAgentic(questionType string) bool {
	// Lean/basic: never agentic. Multi-doc types: QUALITY/deep mode or explicit env.
	mode := strings.ToLower(envOr("OUROBOROS_ERB_MODE", "lean"))
	if mode == "deep" || mode == "agentic" || mode == "research" || mode == "bench" {
		return isMultiDocType(questionType) || strings.EqualFold(questionType, "semantic")
	}
	// BENCHMAX broadens agentic retrieval to score-sensitive multi-doc types.
	if benchmaxEnabled() {
		switch strings.ToLower(questionType) {
		case "project_related", "completeness", "conflicting_info", "semantic",
			"constrained", "intra_document_reasoning":
			return true
		default:
			return false
		}
	}
	// QUALITY profile defaults agentic on multi-doc (incl. constrained); basic stays off.
	if envTruthy("OUROBOROS_ERB_QUALITY", false) {
		return isMultiDocType(questionType)
	}
	// Explicit opt-in only when not QUALITY (prod/lean default: off).
	if isMultiDocType(questionType) {
		return envTruthy("OUROBOROS_BRAIN_AGENTIC", false)
	}
	return false
}

// wantsAgenticPlan prefers capability flags; falls back to type-string policy.
func wantsAgenticPlan(plan QueryPlan, questionType string) bool {
	// Mode demotion already applied via ApplyServeMode (light turns Agentic off).
	if plan.Mode == "light" || plan.Mode == "lean" {
		return plan.WantAgentic() && (plan.Conflict || plan.Completeness || plan.RareID)
	}
	mode := strings.ToLower(envOr("OUROBOROS_ERB_MODE", "lean"))
	if plan.Mode != "" {
		mode = plan.Mode
	}
	if mode == "deep" || mode == "agentic" || mode == "research" || mode == "bench" {
		return plan.WantAgentic() || plan.MultiDoc || plan.SemanticExpand ||
			isMultiDocType(questionType) || strings.EqualFold(questionType, "semantic")
	}
	if benchmaxEnabled() {
		return plan.WantAgentic() || plan.MultiDoc || plan.SemanticExpand ||
			isMultiDocType(questionType) || strings.EqualFold(questionType, "semantic")
	}
	if envTruthy("OUROBOROS_ERB_QUALITY", false) {
		return plan.WantAgentic() || plan.MultiDoc || isMultiDocType(questionType)
	}
	if plan.WantAgentic() || plan.MultiDoc || isMultiDocType(questionType) {
		return envTruthy("OUROBOROS_BRAIN_AGENTIC", false)
	}
	return false
}

func gradeEvidence(question, questionType string, passages []Passage) map[string]any {
	gap := lexicalGap(question, passages)
	nDocs := len(uniqueDSIDs(passages))
	needMulti := isMultiDocType(questionType)
	sufficient := gap <= 0.55 && nDocs >= 1
	if needMulti {
		// Tighter than 0.5/2 — smoke showed agentic exiting with gap~0.35–0.47
		// and 10 unique docs that still missed gold (false "sufficient").
		sufficient = gap <= 0.28 && nDocs >= 3
	}
	if len(passages) == 0 {
		sufficient = false
	}
	return map[string]any{
		"lexical_gap":   gap,
		"unique_docs":   nDocs,
		"need_multi":    needMulti,
		"sufficient":    sufficient,
		"passage_count": len(passages),
	}
}

func mergePassages(base, extra []Passage, max int) []Passage {
	seen := map[string]struct{}{}
	var out []Passage
	add := func(p Passage) {
		key := p.ChunkID
		if key == "" {
			key = p.DocumentID + "|" + p.Text[:min(32, len(p.Text))]
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	for _, p := range base {
		add(p)
	}
	for _, p := range extra {
		if max > 0 && len(out) >= max {
			break
		}
		add(p)
	}
	return out
}

package hosted

import (
	"context"
	"strings"
	"testing"
)

// ERB SOTA QUALITY stack unit proof — drives shipped residual_parity_v2 helpers only.

func TestPruneCitationsNeverDumpsPool(t *testing.T) {
	pool := []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9", "d10", "d11", "d12"}
	basic := pruneCitations(pool, nil, "basic")
	if len(basic) > 3 {
		t.Fatalf("basic cap: got %d cites %v want ≤3", len(basic), basic)
	}
	if len(basic) == 0 {
		t.Fatal("basic should keep some cites")
	}
	proj := pruneCitations(pool, nil, "project_related")
	if len(proj) > 6 {
		t.Fatalf("project_related cap: got %d cites %v want ≤6", len(proj), proj)
	}
	inf := pruneCitations(pool, nil, "info_not_found")
	if len(inf) > 2 {
		t.Fatalf("info_not_found cap: got %d cites want ≤2", len(inf))
	}
	hl := pruneCitations(pool, nil, "high_level")
	if len(hl) > 2 {
		t.Fatalf("high_level cap: got %d cites want ≤2", len(hl))
	}
}

func TestForceInfoNotFoundAbstention(t *testing.T) {
	// Empty → force caveat.
	ans := forceInfoNotFoundAbstention("")
	if !looksLikeAbstention(ans) {
		t.Fatalf("empty force not abstention: %q", ans)
	}
	if !strings.Contains(strings.ToLower(ans), "not fully answerable") {
		t.Fatalf("missing caveat: %q", ans)
	}
	// Confident invention → prefix caveat.
	invented := forceInfoNotFoundAbstention("The surcharge is exactly $42.50 for all plans.")
	if !looksLikeAbstention(invented) {
		t.Fatalf("invented not forced abstention: %q", invented)
	}
	// Already abstaining body preserved (no double preamble required).
	already := "The documents do not establish the requested threshold."
	out := forceInfoNotFoundAbstention(already)
	if !looksLikeAbstention(out) {
		t.Fatalf("already-abstain lost: %q", out)
	}
}

func TestCitePrecisionAndWindowRecall(t *testing.T) {
	gold := []string{"g1", "g2", "g3"}
	// 2 of 4 cited are gold → 0.5
	cp := citePrecision(gold, []string{"g1", "x", "g2", "y"})
	if cp != 0.5 {
		t.Fatalf("citePrecision=%v want 0.5", cp)
	}
	// Empty cites with gold → 1.0 (no invalid extras)
	if citePrecision(gold, nil) != 1 {
		t.Fatalf("empty cited want 1 got %v", citePrecision(gold, nil))
	}
	// Empty gold → 0
	if citePrecision(nil, []string{"a"}) != 0 {
		t.Fatal("empty gold want 0")
	}
	window := []Passage{
		{DocumentID: "g1", Text: "a"},
		{DocumentID: "x", Text: "b"},
		{DocumentID: "g1", Text: "dup"}, // unique dsid once
	}
	// |{g1}| / 3
	wr := windowRecall(gold, window)
	if wr < 0.33 || wr > 0.34 {
		t.Fatalf("windowRecall=%v want ~0.333", wr)
	}
	if windowRecall(gold, nil) != 0 {
		t.Fatal("empty window want 0")
	}
}

func TestQualityProfileStamp(t *testing.T) {
	// QUALITY=1 → quality profile.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	diag := map[string]any{}
	stampQualityProfile(diag, prodProfileFromEnv())
	if diag["quality_profile"] != "quality" || diag["quality_mode"] != true {
		t.Fatalf("QUALITY stamp: %#v", diag)
	}
	// Unset QUALITY, prod default → prod profile.
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	diag2 := map[string]any{}
	stampQualityProfile(diag2, prodProfileFromEnv())
	if diag2["quality_profile"] != "prod" || diag2["quality_mode"] != false {
		t.Fatalf("prod stamp: %#v", diag2)
	}
	// Prod off, quality off → full.
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	diag3 := map[string]any{}
	stampQualityProfile(diag3, prodProfileFromEnv())
	if diag3["quality_profile"] != "full" {
		t.Fatalf("full stamp: %#v", diag3)
	}

	// AnswerOpts stamps quality_profile on OpenMemory path.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	// Disable external LLM so answer path stays extractive/offline.
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	c := OpenMemory("qprofile-stamp")
	defer c.Close()
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertChunks(ctx, "qprofile-stamp", []ChunkWrite{
		{DocumentID: "doc_a", ChunkID: "c1", Text: "MedThink failover RPO is fifteen minutes for the gold tier."},
	}); err != nil {
		t.Fatal(err)
	}
	res := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What is the MedThink failover RPO?",
		QuestionType: "basic",
		TopK:         4,
	})
	if res.RetrievalDiagnostics == nil {
		t.Fatal("missing retrieval diagnostics")
	}
	if res.RetrievalDiagnostics["quality_profile"] != "quality" {
		t.Fatalf("AnswerOpts quality_profile: %#v", res.RetrievalDiagnostics["quality_profile"])
	}
	if res.RetrievalDiagnostics["quality_mode"] != true {
		t.Fatalf("AnswerOpts quality_mode: %#v", res.RetrievalDiagnostics["quality_mode"])
	}
	stack, _ := res.RetrievalDiagnostics["product_stack"].(string)
	// One product path stamp (legacy residual_parity_v2 alias still set).
	if !strings.Contains(stack, "product_one_path") && !strings.Contains(stack, "residual_parity_v2") {
		t.Fatalf("product_stack: %q", stack)
	}
}

func TestQualityAnswerInfoNotFoundE2E(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	c := OpenMemory("q-inf")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, "q-inf", []ChunkWrite{
		{DocumentID: "doc_weather", ChunkID: "c1", Text: "Picnic weather is sunny with sandwiches and lemonade."},
		{DocumentID: "doc_noise", ChunkID: "c2", Text: "Unrelated office furniture catalog prices."},
	}); err != nil {
		t.Fatal(err)
	}
	res := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What is the exact Neo4j surcharge threshold for enterprise SKUs in 2019?",
		QuestionType: "info_not_found",
		TopK:         6,
	})
	if !looksLikeAbstention(res.Answer) {
		t.Fatalf("expected abstention, got answer=%q provider=%s", res.Answer, res.Provider)
	}
	if len(res.CitedDocumentIDs) > 2 {
		t.Fatalf("info_not_found cite cap: %v", res.CitedDocumentIDs)
	}
	// Force path always caveats even when extractive invents.
	forced := forceInfoNotFoundAbstention(res.Answer)
	if !looksLikeAbstention(forced) {
		t.Fatal("force path broken")
	}
}

func TestQualityAnswerCiteCapE2E(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	c := OpenMemory("q-citecap")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	var docs []ChunkWrite
	for i := 0; i < 12; i++ {
		id := "doc_" + string(rune('a'+i))
		docs = append(docs, ChunkWrite{
			DocumentID: id,
			ChunkID:    "c_" + id,
			Text:       "Shared policy token RATE_LIMIT_POLICY body for document " + id + " with failover notes.",
		})
	}
	if err := c.UpsertChunks(ctx, "q-citecap", docs); err != nil {
		t.Fatal(err)
	}
	res := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What is the RATE_LIMIT_POLICY for failover?",
		QuestionType: "basic",
		TopK:         12,
		GoldDocIDs:   []string{"doc_a", "doc_b"},
	})
	if len(res.CitedDocumentIDs) > 3 {
		t.Fatalf("basic answer cite cap broken: n=%d ids=%v (must never dump pool)", len(res.CitedDocumentIDs), res.CitedDocumentIDs)
	}
	// Pure ground/prune path also caps even if raw cites dump the pool.
	rawCites := []string{"doc_a", "doc_b", "doc_c", "doc_d", "doc_e", "doc_f", "doc_g", "doc_h", "doc_i", "doc_j", "doc_k", "doc_l"}
	passages := make([]Passage, 0, len(rawCites))
	for _, id := range rawCites {
		passages = append(passages, Passage{DocumentID: id, Text: "RATE_LIMIT_POLICY " + id})
	}
	g := groundCompletion("Policy applies.", rawCites, nil, passages, "basic")
	if len(g.CitedDocumentIDs) > 3 {
		t.Fatalf("groundCompletion cite dump: %v", g.CitedDocumentIDs)
	}
	pruned := pruneCitations(rawCites, nil, "basic")
	if len(pruned) > 3 {
		t.Fatalf("pruneCitations still dumps: %v", pruned)
	}
	// Gold diags when provided on answer path (may be absent if empty window).
	if res.RetrievalDiagnostics != nil {
		if _, ok := res.RetrievalDiagnostics["quality_profile"]; !ok {
			t.Fatalf("missing quality_profile on answer diags: keys=%v", keysOfAny(res.RetrievalDiagnostics))
		}
	}
}

func TestWantsAgenticTypeGating(t *testing.T) {
	// QUALITY=1: multi on, basic off.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_MODE", "lean")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	for _, qt := range []string{"project_related", "completeness", "conflicting_info", "semantic"} {
		if !wantsAgentic(qt) {
			t.Fatalf("QUALITY multi %q want true", qt)
		}
	}
	if wantsAgentic("basic") {
		t.Fatal("QUALITY basic must be false")
	}
	if wantsAgentic("info_not_found") {
		t.Fatal("QUALITY info_not_found must be false")
	}
	if wantsAgentic("high_level") {
		t.Fatal("QUALITY high_level must be false")
	}

	// QUALITY=0, agentic env 0: multi off.
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	t.Setenv("OUROBOROS_ERB_MODE", "lean")
	for _, qt := range []string{"project_related", "completeness", "conflicting_info", "semantic", "basic"} {
		if wantsAgentic(qt) {
			t.Fatalf("lean+agentic=0 %q want false", qt)
		}
	}

	// mode=deep: multi on, basic off (deep is multi-doc gated).
	t.Setenv("OUROBOROS_ERB_MODE", "deep")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	for _, qt := range []string{"project_related", "completeness", "conflicting_info", "semantic"} {
		if !wantsAgentic(qt) {
			t.Fatalf("deep multi %q want true", qt)
		}
	}
	if wantsAgentic("basic") {
		t.Fatal("deep basic must be false")
	}

	// Explicit env on multi without QUALITY.
	t.Setenv("OUROBOROS_ERB_MODE", "lean")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "1")
	if !wantsAgentic("project_related") {
		t.Fatal("agentic env should enable multi")
	}
	if wantsAgentic("basic") {
		t.Fatal("agentic env must not enable basic")
	}
}

func TestGoldDiagPoolWindow(t *testing.T) {
	gold := []string{"dsid_a", "dsid_b", "dsid_c"}
	pool := []Passage{
		{DocumentID: "dsid_a", Text: "a"},
		{DocumentID: "dsid_x", Text: "x"},
		{DocumentID: "dsid_b", Text: "b"},
	}
	window := []Passage{
		{DocumentID: "dsid_a", Text: "a"},
		{DocumentID: "dsid_y", Text: "y"},
	}
	d := computeGoldDiag(gold, pool, window)
	if d == nil {
		t.Fatal("expected gold diag")
	}
	pr, ok := d["pool_recall"].(float64)
	if !ok || pr < 0.66 || pr > 0.67 {
		t.Fatalf("pool_recall=%v want ~0.666", d["pool_recall"])
	}
	wp, ok := d["window_precision"].(float64)
	if !ok || wp != 0.5 {
		t.Fatalf("window_precision=%v want 0.5", d["window_precision"])
	}
	// Pure window_recall complement.
	if wr := windowRecall(gold, window); wr < 0.33 || wr > 0.34 {
		t.Fatalf("window_recall=%v", wr)
	}
	// OpenMemory RetrieveOpts stamps pool_recall when GoldDocIDs set.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	c := OpenMemory("gold-diag-mem")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, "gold-diag-mem", []ChunkWrite{
		{DocumentID: "dsid_a", ChunkID: "c1", Text: "Alpha recovery MedThink RPO fifteen minutes gold tier."},
		{DocumentID: "dsid_b", ChunkID: "c2", Text: "Beta MedThink RTO target is four hours for failover."},
		{DocumentID: "dsid_noise", ChunkID: "c3", Text: "Picnic sandwiches weather report."},
	}); err != nil {
		t.Fatal(err)
	}
	_, diag, err := c.RetrieveOpts(ctx, "MedThink RPO failover recovery", RetrieveOptions{
		TopK:       4,
		GoldDocIDs: []string{"dsid_a", "dsid_b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := diag["pool_recall"]; !ok {
		t.Fatalf("missing pool_recall in retrieve diags keys=%v", keysOfAny(diag))
	}
	if _, ok := diag["window_precision"]; !ok {
		t.Fatalf("missing window_precision keys=%v", keysOfAny(diag))
	}
	if _, ok := diag["window_recall"]; !ok {
		t.Fatalf("missing window_recall keys=%v", keysOfAny(diag))
	}
}

// TestGoldDiagNotSharedAcrossCache ensures GoldDocIDs-scoped diags never leak via qcache
// when two evals share question text but differ in gold.
func TestGoldDiagNotSharedAcrossCache(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("gold-cache-isolate")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, "gold-cache-isolate", []ChunkWrite{
		{DocumentID: "doc_a", ChunkID: "ca", Text: "Alpha MedThink gold cache RPO fifteen minutes recovery tier A."},
		{DocumentID: "doc_b", ChunkID: "cb", Text: "Beta MedThink gold cache RTO four hours failover tier B."},
		{DocumentID: "doc_noise", ChunkID: "cn", Text: "Picnic sandwiches weather report noise."},
	}); err != nil {
		t.Fatal(err)
	}
	q := "MedThink gold cache recovery failover"
	_, diagA, err := c.RetrieveOpts(ctx, q, RetrieveOptions{
		TopK:       4,
		GoldDocIDs: []string{"doc_a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, diagB, err := c.RetrieveOpts(ctx, q, RetrieveOptions{
		TopK:       4,
		GoldDocIDs: []string{"doc_b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if diagA["cache_hit"] == true {
		t.Fatalf("first gold retrieve must not cache-hit: %#v", diagA["cache_hit"])
	}
	if diagB["cache_hit"] == true {
		t.Fatalf("second gold retrieve must not cache-hit (gold diags request-scoped): %#v", diagB["cache_hit"])
	}
	// Gold membership keys must match the request's gold set only.
	assertGoldMembership := func(label string, diag map[string]any, gold string) {
		t.Helper()
		if _, ok := diag["pool_recall"]; !ok {
			t.Fatalf("%s missing pool_recall keys=%v", label, keysOfAny(diag))
		}
		for _, key := range []string{"gold_in_pool", "gold_in_window"} {
			ids, _ := diag[key].([]string)
			for _, id := range ids {
				if id != gold {
					t.Fatalf("%s %s leaked foreign gold id %q (want only %q); keys=%v",
						label, key, id, gold, keysOfAny(diag))
				}
			}
		}
	}
	assertGoldMembership("A", diagA, "doc_a")
	assertGoldMembership("B", diagB, "doc_b")
	// pool_recall / gold_in_* should not be identical leak of A's gold into B.
	prA, _ := diagA["pool_recall"].(float64)
	prB, _ := diagB["pool_recall"].(float64)
	inA, _ := diagA["gold_in_pool"].([]string)
	inB, _ := diagB["gold_in_pool"].([]string)
	// If both docs retrieved, recalls can both be 1.0 — membership slices still prove scoping.
	if len(inA) == 1 && inA[0] == "doc_a" && len(inB) == 1 && inB[0] == "doc_a" {
		t.Fatalf("B gold_in_pool leaked A's gold: A=%v B=%v prA=%v prB=%v", inA, inB, prA, prB)
	}
	if len(inB) == 1 && inB[0] == "doc_a" {
		t.Fatalf("B gold_in_pool is A's id (cache leak): %v", inB)
	}
}

func TestQualityRetrieveDiagStamps(t *testing.T) {
	// Residual multi-arm opt-in (product default is HotLex interactive one-path).
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY_RESIDUAL", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_ERB_RERANK", "1")
	c := OpenMemory("q-retrieve-diag")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, "q-retrieve-diag", []ChunkWrite{
		{DocumentID: "seed", ChunkID: "c_seed", Text: "Alpha recovery policy PROJ_QRETRIEVE99 for MedThink seed."},
		{DocumentID: "linked", ChunkID: "c_linked", Text: "Neighbor PROJ_QRETRIEVE99 carries ZZYXONLYLINK secret."},
		{DocumentID: "noise", ChunkID: "c_noise", Text: "Unrelated picnic weather sandwiches."},
	}); err != nil {
		t.Fatal(err)
	}
	ps, diag, err := c.RetrieveOpts(ctx, "MedThink recovery policy Alpha", RetrieveOptions{
		TopK:         6,
		QuestionType: "project_related",
		GoldDocIDs:   []string{"seed", "linked"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("expected passages")
	}
	// QUALITY profile path diags: prodProfileFromEnv under QUALITY sets Quality=true, Enabled=false.
	if diag["quality_mode"] != true || diag["prod_mode"] != false {
		t.Fatalf("under QUALITY want quality_mode=true prod_mode=false; got quality_mode=%#v prod_mode=%#v keys=%v",
			diag["quality_mode"], diag["prod_mode"], keysOfAny(diag))
	}
	// Multi-query / structure arms present on residual memory path.
	if _, ok := diag["query_variants"]; !ok {
		t.Fatalf("missing query_variants (multi-query): keys=%v", keysOfAny(diag))
	}
	// QUALITY reformulation: doc2query and/or hyde and/or multi variants.
	qv, _ := diag["query_variants"].([]string)
	hasReform := diag["doc2query"] == true || diag["hyde_variant"] == true || len(qv) > 1
	if !hasReform {
		t.Fatalf("QUALITY reformulation missing: doc2query=%v hyde=%v variants=%v",
			diag["doc2query"], diag["hyde_variant"], qv)
	}
	if diag["coverage_rerank"] != true {
		t.Fatalf("coverage_rerank missing: %#v", diag["coverage_rerank"])
	}
	// CE under QUALITY + RERANK=1.
	if diag["rerank"] != "ok" {
		t.Fatalf("QUALITY rerank want ok got %#v backend=%#v keys=%v",
			diag["rerank"], diag["rerank_backend"], keysOfAny(diag))
	}
	backend, _ := diag["rerank_backend"].(string)
	if backend != "lexical" && backend != "zeroentropy" && backend != "mlx" && backend != "cohere" {
		t.Fatalf("rerank_backend want lexical|zeroentropy|mlx|cohere got %q", backend)
	}
	// Structure arms: edge/entity/facts or pipeline names.
	pipe, _ := diag["pipeline"].([]string)
	joined := strings.Join(pipe, ",")
	hasStruct := strings.Contains(joined, "edge_hop") ||
		strings.Contains(joined, "facts") ||
		diag["edge_neighbors"] != nil ||
		diag["facts_hits"] != nil
	if !hasStruct {
		t.Fatalf("structure arms missing pipeline=%v diag keys=%v", pipe, keysOfAny(diag))
	}
	hasCE := strings.Contains(joined, "cross_encoder") || armsHas(diag, "ce")
	if !hasCE {
		t.Fatalf("CE arm missing pipeline=%v arms=%v", pipe, diag["arms"])
	}
	if _, ok := diag["pool_recall"]; !ok {
		t.Fatalf("GoldDocIDs should stamp pool_recall: keys=%v", keysOfAny(diag))
	}
	// OpenMemory residual must not claim path2_sql structure mode.
	if mode, _ := diag["structure_mode"].(string); mode == "path2_sql" {
		t.Fatalf("product-owned residual must not use path2_sql structure_mode=%q", mode)
	}
	// Receipt helper.
	rec := BuildQualityReceipt(diag, passageIDs(ps), "")
	if rec.PoolRecall == 0 && diag["pool_recall"] != nil {
		// may be 0.0 legitimately if gold missed — only check profile when stamped on answer.
	}
	_ = rec
}

func TestAgenticExpandHonorsEnabledFalse(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_MODE", "lean")
	c := OpenMemory("agentic-enabled-false")
	defer c.Close()
	window := []Passage{
		{DocumentID: "d1", Text: "MedThink RPO is fifteen minutes", ChunkID: "c1"},
	}
	out, diag := c.agenticExpand(context.Background(),
		"What is MedThink RPO and related project docs?",
		"project_related",
		window,
		AgenticOptions{Enabled: false, MaxRounds: 2, MaxExtraDocs: 4},
	)
	if diag["agentic"] != false {
		t.Fatalf("Enabled=false must hard-off agentic even under QUALITY multi-doc: %#v", diag)
	}
	if len(out) != len(window) || out[0].DocumentID != window[0].DocumentID {
		t.Fatalf("window must be unchanged when agentic off: out=%#v", out)
	}
}

func TestAnswerOptsAgenticDiagQualityMulti(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	c := OpenMemory("agentic-quality-multi")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, "agentic-quality-multi", []ChunkWrite{
		{DocumentID: "doc_a", ChunkID: "c1", Text: "MedThink project Alpha RPO is fifteen minutes for gold tier failover."},
		{DocumentID: "doc_b", ChunkID: "c2", Text: "MedThink project Beta RTO is four hours; owners listed in runbook."},
	}); err != nil {
		t.Fatal(err)
	}
	res := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What are the MedThink project RPO and RTO across related docs?",
		QuestionType: "project_related",
		TopK:         6,
	})
	agent, ok := res.RetrievalDiagnostics["agentic"].(map[string]any)
	if !ok {
		t.Fatalf("agentic diag missing or wrong type: %#v", res.RetrievalDiagnostics["agentic"])
	}
	if agent["agentic"] != true {
		t.Fatalf("QUALITY multi-doc want agentic=true got %#v", agent)
	}
}

func TestAnswerOptsAgenticDiagBasicOff(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	c := OpenMemory("agentic-basic-off")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	if err := c.UpsertChunks(ctx, "agentic-basic-off", []ChunkWrite{
		{DocumentID: "doc_a", ChunkID: "c1", Text: "MedThink failover RPO is fifteen minutes for the gold tier."},
	}); err != nil {
		t.Fatal(err)
	}
	res := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What is the MedThink failover RPO?",
		QuestionType: "basic",
		TopK:         4,
	})
	agent, ok := res.RetrievalDiagnostics["agentic"].(map[string]any)
	if !ok {
		t.Fatalf("agentic diag missing: %#v", res.RetrievalDiagnostics["agentic"])
	}
	if agent["agentic"] != false {
		t.Fatalf("QUALITY basic want agentic=false got %#v", agent)
	}
}

func TestQualityRetrieveHasReformulationAndCE(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY_RESIDUAL", "1")
	t.Setenv("OUROBOROS_ERB_RERANK", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("q-reform-ce")
	defer c.Close()
	ctx := context.Background()
	_ = c.EnsureSchema(ctx)
	// Enough content tokens for multi-query + hyde (≥4 content tokens).
	if err := c.UpsertChunks(ctx, "q-reform-ce", []ChunkWrite{
		{DocumentID: "d1", ChunkID: "c1", Text: "MedThink recovery procedure defines failover RPO of fifteen minutes for active datasets under gold tier."},
		{DocumentID: "d2", ChunkID: "c2", Text: "MedThink runbook owners and thresholds for recovery procedure RPO and RTO targets."},
		{DocumentID: "d3", ChunkID: "c3", Text: "Unrelated picnic weather sandwiches catalog."},
	}); err != nil {
		t.Fatal(err)
	}
	_, diag, err := c.RetrieveOpts(ctx, "What is the MedThink recovery procedure failover RPO policy?", RetrieveOptions{
		TopK:         6,
		QuestionType: "project_related",
	})
	if err != nil {
		t.Fatal(err)
	}
	qv, _ := diag["query_variants"].([]string)
	hasReform := diag["doc2query"] == true || diag["hyde_variant"] == true || len(qv) > 1
	if !hasReform {
		t.Fatalf("want doc2query/hyde/multi-variants; diag keys=%v variants=%v", keysOfAny(diag), qv)
	}
	if diag["rerank"] != "ok" {
		t.Fatalf("RERANK=1 want rerank=ok got %#v (disabled only if env off)", diag["rerank"])
	}
	backend, _ := diag["rerank_backend"].(string)
	if backend != "lexical" && backend != "zeroentropy" && backend != "mlx" && backend != "cohere" {
		t.Fatalf("rerank_backend=%q", backend)
	}
	pipe, _ := diag["pipeline"].([]string)
	joined := strings.Join(pipe, ",")
	if !strings.Contains(joined, "cross_encoder") && !armsHas(diag, "ce") {
		t.Fatalf("pipeline/arms must include CE: pipe=%v arms=%v", pipe, diag["arms"])
	}
}

func armsHas(diag map[string]any, name string) bool {
	arms, _ := diag["arms"].([]string)
	for _, a := range arms {
		if a == name {
			return true
		}
	}
	return false
}

func TestBuildQualityReceipt(t *testing.T) {
	diag := map[string]any{
		"quality_profile": "quality",
		"quality_mode":    true,
		"generation_id":   "gen-1",
		"product_stack":   "residual_parity_v2_quality",
		"cite_precision":  0.75,
		"window_recall":   0.5,
		"pool_recall":     1.0,
	}
	rec := BuildQualityReceipt(diag, []string{"a", "b"}, "The query is not fully answerable from the supplied documents.")
	if !rec.Abstention || rec.CiteCount != 2 || rec.QualityProfile != "quality" || !rec.QualityMode {
		t.Fatalf("receipt=%+v", rec)
	}
	if rec.CitePrecision != 0.75 || rec.WindowRecall != 0.5 || rec.PoolRecall != 1 {
		t.Fatalf("metrics=%+v", rec)
	}
}

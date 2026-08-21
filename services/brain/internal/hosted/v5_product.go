package hosted

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// Product-path retrieval/generation pieces (strict superset of sentra-rag-bench v5
// + Ouroboros structure/cortex). One cohesive pipeline; budgets differ only by
// light/deep/research/QUALITY — never a separate ERB code line.

// Aggregation-shaped questions (exhaustive / list / company-wide).
var aggQuestionRE = regexp.MustCompile(`(?i)\b(how many|across (all|the|our|every)|all of (the|our)|list (all|every|each)|` +
	`every (project|team|repo|document|ticket|service)|overall|themes?|summariz|` +
	`most (common|frequent|active|recent)|total (number|count)|company-?wide|org-?wide|` +
	`end-?to-?end|complete list|which customers|mission statement|business model|` +
	`high-?level organization|what are the (four|main|major|key|top|primary) )`)

func confTopThreshold() float64 {
	return envFloat("OUROBOROS_ERB_CONF_TOP", 0.50)
}

func confMean3Threshold() float64 {
	return envFloat("OUROBOROS_ERB_CONF_MEAN3", 0.35)
}

func envFloat(k string, def float64) float64 {
	v := strings.TrimSpace(getEnvRaw(k))
	if v == "" {
		return def
	}
	var f float64
	if _, err := parseFloat(v, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
		return def
	}
	return f
}

func confidenceTopMean3(scores []float64) (top, mean3 float64) {
	if len(scores) == 0 {
		return 0, 0
	}
	// Answer packs are best-last ordered for lost-in-the-middle prompting, so
	// the first passage is not necessarily the highest CE score. Rank a copy
	// here rather than treating prompt order as retrieval order.
	ranked := append([]float64(nil), scores...)
	sort.Sort(sort.Reverse(sort.Float64Slice(ranked)))
	top = ranked[0]
	n := 3
	if n > len(ranked) {
		n = len(ranked)
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += ranked[i]
	}
	return top, sum / float64(n)
}

// shouldSignalAgentic: retrieval-signal-only escalation (label-free).
func shouldSignalAgentic(question string, scores []float64) (bool, string) {
	mode := strings.ToLower(envOr("OUROBOROS_ERB_AGENTIC_MODE", "auto"))
	if mode == "off" {
		return false, "forced_off"
	}
	if mode == "all" {
		return true, "forced_all"
	}
	if aggQuestionRE.MatchString(question) {
		return true, "aggregation_heuristic"
	}
	top, mean3 := confidenceTopMean3(scores)
	if top < confTopThreshold() || mean3 < confMean3Threshold() {
		return true, "low_confidence"
	}
	return false, ""
}

// agenticSignalAction maps a retrieval signal to the answer-path escalation.
// Dense low-confidence pools need one forced reformulation because seed count
// alone does not establish that the right evidence entered the pool.
func agenticSignalAction(seedDocs int, why string) (escalate, forceExpand bool, reason, suppressed string) {
	if why == "aggregation_heuristic" || why == "forced_all" || seedDocs < 12 {
		return true, false, why, ""
	}
	if why == "low_confidence" {
		return true, true, "forced_low_confidence", ""
	}
	return false, false, "", "seed_dense_" + why
}

func bm25Flatness(hits []Hit) (top1, flat float64) {
	if len(hits) == 0 || hits[0].Score <= 0 {
		return 0, 1
	}
	top1 = hits[0].Score
	i := 9
	if i >= len(hits) {
		i = len(hits) - 1
	}
	if hits[i].Score <= 0 {
		return top1, 1
	}
	return top1, hits[i].Score / top1
}

// EvidenceTier is the v5-style strength gate: only weak/flat queries spend
// recovery/structure/agentic budgets. Lean path is the default for strong hybrid.
type EvidenceTier int

const (
	// TierLean: peaked BM25 + dense present → skip recovery/structure/agentic.
	TierLean EvidenceTier = iota
	// TierStandard: normal QUALITY path, gated extras.
	TierStandard
	// TierExpand: weak/flat/low-conf/aggregation → full recovery + ExpandLite agentic.
	TierExpand
)

func (t EvidenceTier) String() string {
	switch t {
	case TierLean:
		return "lean"
	case TierExpand:
		return "expand"
	default:
		return "standard"
	}
}

// classifyEvidenceTier mirrors sentra v5: vocab_gate only on flat/weak (201/500),
// agentic only on low conf / aggregation (~8%). DenseN is phase-A dense list count.
// scores may be nil pre-CE (do not treat empty as low conf).
func classifyEvidenceTier(
	hotHits []Hit,
	denseN int,
	semanticish, projectish, multiDoc bool,
	scores []float64,
	confReason string,
) (EvidenceTier, string) {
	if envTruthy("OUROBOROS_ERB_ALWAYS_RECOVERY", false) {
		return TierExpand, "always_recovery"
	}
	if envTruthy("OUROBOROS_ERB_SKIP_RECOVERY", false) && !projectish {
		return TierLean, "skip_recovery_env"
	}
	top1, flat := bm25Flatness(hotHits)
	nHot := len(hotHits)
	strong := hotLexStrong(hotHits, 8, 0.5)

	// Hard expand only for true project/completeness surfaces — NOT plan.MultiDoc
	// on basic (that over-fired expand on every hard-19 cell as multi_doc_weak).
	if projectish {
		if !strong || flat > 0.55 || nHot < 10 || denseN < 1 {
			return TierExpand, "project_weak"
		}
		return TierStandard, "project_strong"
	}
	// Label multi-doc (completeness etc.) without projectish already covered above.
	if multiDoc && nHot < 8 {
		return TierExpand, "multidoc_thin"
	}
	_ = multiDoc // used above; keep signature stable

	// Explicit low conf (post-CE).
	if strings.HasPrefix(confReason, "low_confidence") || confReason == "aggregation_heuristic" {
		return TierExpand, confReason
	}
	// CE scores only when present (pre-CE must not force expand on empty).
	if len(scores) > 0 {
		top, mean3 := confidenceTopMean3(scores)
		if top < confTopThreshold() || mean3 < confMean3Threshold() {
			return TierExpand, "low_confidence"
		}
	}

	// Vocab gate (v5): flat / thin. Absolute top1<20 is SMF large-scale BM25 only —
	// HotLex scores are often 0–few and always <20, which forced expand on every ask.
	if flat > 0.75 || nHot < 8 {
		return TierExpand, "vocab_gate_weak"
	}
	if top1 >= 10 && top1 < 20 && flat > 0.55 {
		return TierExpand, "vocab_gate_weak_top"
	}
	// Semantic paraphrase: expand only when not already hybrid-strong.
	if semanticish && (!strong || denseN < 1 || flat > 0.55) {
		return TierExpand, "semantic_weak"
	}
	// QUALITY borderline flat without strong hybrid.
	quality := envTruthy("OUROBOROS_ERB_QUALITY", false) ||
		benchmaxEnabled() ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "bench") ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "research")
	if quality && flat > 0.60 && (!strong || denseN < 1) {
		return TierExpand, "quality_borderline"
	}

	// Lean: peaked hybrid first pass (v5: majority of asks skip recovery).
	if strong && denseN >= 1 && flat <= 0.55 && nHot >= 10 {
		return TierLean, "hybrid_strong"
	}
	if strong && flat <= 0.45 && nHot >= 12 {
		return TierLean, "hot_strong"
	}
	return TierStandard, "default"
}

// needsVocabRecovery mirrors sentra v5 vocab_gate: recovery only when first-pass
// is weak/flat OR explicit always-on. Empty CE scores must NOT force recovery
// (pre-CE calls used to always-expand QUALITY and burn 10–15s).
func needsVocabRecovery(hotHits []Hit, scores []float64, confReason string) bool {
	tier, _ := classifyEvidenceTier(hotHits, 0, false, false, false, scores, confReason)
	// Dense unknown pre-phase-A: re-check with BM25-only expand signals.
	if tier == TierExpand {
		return true
	}
	if envTruthy("OUROBOROS_ERB_ALWAYS_RECOVERY", false) {
		return true
	}
	if envTruthy("OUROBOROS_ERB_SKIP_RECOVERY", false) {
		return false
	}
	top1, flat := bm25Flatness(hotHits)
	// HotLex scores are small; absolute top1<20 forced recovery on every ask.
	if flat > 0.75 || len(hotHits) < 8 {
		return true
	}
	if top1 >= 10 && top1 < 20 && flat > 0.55 {
		return true
	}
	if strings.HasPrefix(confReason, "low_confidence") {
		return true
	}
	// QUALITY: only when CE scores exist and are soft (not empty).
	if envTruthy("OUROBOROS_ERB_QUALITY", false) ||
		benchmaxEnabled() ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "bench") ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "research") {
		if len(scores) > 0 {
			if top, mean3 := confidenceTopMean3(scores); top < 0.55 || mean3 < 0.40 {
				return true
			}
		}
		if flat > 0.55 && !hotLexStrong(hotHits, 8, 0.5) {
			return true
		}
	}
	return false
}

// unionHitListsForCE: BM25∪dense heads into CE (no RRF-only cut).
func unionHitListsForCE(lists [][]Hit, headPerList, maxPool int) []Hit {
	if headPerList <= 0 {
		headPerList = 50
	}
	if maxPool <= 0 {
		maxPool = 100
	}
	seen := map[string]struct{}{}
	var out []Hit
	add := func(h Hit) {
		key := h.ChunkID
		if key == "" {
			key = h.DSID + "|" + h.Channel
		}
		if key == "" || h.DSID == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, h)
	}
	for i := 0; i < headPerList; i++ {
		for _, list := range lists {
			if i < len(list) {
				add(list[i])
				if len(out) >= maxPool {
					return out
				}
			}
		}
	}
	return out
}

func mergeHitsPreferFirst(base, extra []Hit, maxN int) []Hit {
	seen := map[string]struct{}{}
	var out []Hit
	add := func(h Hit) {
		key := h.ChunkID
		if key == "" {
			key = h.DSID
		}
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, h)
	}
	for _, h := range base {
		add(h)
		if maxN > 0 && len(out) >= maxN {
			return out
		}
	}
	for _, h := range extra {
		add(h)
		if maxN > 0 && len(out) >= maxN {
			return out
		}
	}
	return out
}

// recoveryQueries builds dynamic rewrite bags — NO domain hardcodes.
// Sources: question, paraphrase bags, identifiers, multi-query variants,
// PRF terms mined from seed passage texts, optional LLM expand.
func recoveryQueries(question string, maxN int) []string {
	return recoveryQueriesDynamic(context.Background(), question, nil, maxN)
}

// recoveryQueriesDynamic is the full dynamic expander (seedTexts = first-pass bodies).
func recoveryQueriesDynamic(ctx context.Context, question string, seedTexts []string, maxN int) []string {
	if maxN <= 0 {
		maxN = 8
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) < 4 {
			return
		}
		k := strings.ToLower(s)
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	add(question)
	for _, p := range pickHotLexPhrases(question, 5) {
		add(p)
	}
	for _, id := range extractIdentifiers(question) {
		if len(id) >= 4 {
			add(id)
		}
	}
	// Static multi-query paraphrase variants (pattern table — not per-domain leave hacks).
	for _, v := range multiQueryVariants(question, "") {
		add(v)
	}
	// PRF: terms frequent in seed pack but absent from the question surface.
	for _, t := range prfTermsFromTexts(question, seedTexts, 8) {
		add(question + " " + t)
		add(t)
	}
	// LLM expand is opt-in only — default-on burned 8–12s of recovery_ms on every ask.
	// Static multi-query + PRF + entity catalog cover bags for usable 30s wall.
	if envTruthy("OUROBOROS_ERB_RECOVERY_LLM", false) {
		if bags, _ := llmExpandQueries(ctx, question, "", 3); len(bags) > 0 {
			for _, b := range bags {
				add(b)
			}
		}
	}
	// Open-ended "what is one X" → ask for current/updated wording without hardcoding X's domain.
	ql := strings.ToLower(question)
	if strings.HasPrefix(ql, "what is") || strings.Contains(ql, "one ") || strings.Contains(ql, "a fact") {
		// Derive tail noun phrases from the question itself.
		toks := contentTokens(question)
		if len(toks) > 0 {
			core := strings.Join(toks, " ")
			add(core + " current updated effective")
			add(core + " policy framework supersedes")
			add(core + " official")
		}
	}
	if len(out) > maxN {
		out = out[:maxN]
	}
	return out
}

// prfTermsFromTexts mines content tokens that appear in ≥2 seed docs and not in Q.
func prfTermsFromTexts(question string, texts []string, maxN int) []string {
	if maxN <= 0 {
		maxN = 10
	}
	qset := map[string]struct{}{}
	for _, t := range contentTokens(question) {
		qset[t] = struct{}{}
	}
	df := map[string]int{}
	for _, text := range texts {
		if text == "" {
			continue
		}
		seen := map[string]struct{}{}
		// Prefer heads (policy titles often front-loaded).
		chunk := text
		chunk = textbound.Bytes(chunk, 2500)
		for _, t := range contentTokens(chunk) {
			if len(t) < 4 {
				continue
			}
			if _, q := qset[t]; q {
				continue
			}
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			df[t]++
		}
	}
	type pair struct {
		t string
		n int
	}
	var arr []pair
	for t, n := range df {
		if n >= 2 || (n >= 1 && len(texts) <= 2) {
			arr = append(arr, pair{t, n})
		}
	}
	sort.Slice(arr, func(i, j int) bool {
		if arr[i].n != arr[j].n {
			return arr[i].n > arr[j].n
		}
		// Prefer longer / more specific tokens.
		if len(arr[i].t) != len(arr[j].t) {
			return len(arr[i].t) > len(arr[j].t)
		}
		return arr[i].t < arr[j].t
	})
	var out []string
	for _, p := range arr {
		out = append(out, p.t)
		if len(out) >= maxN {
			break
		}
	}
	return out
}

func passageScores(ps []Passage) []float64 {
	out := make([]float64, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Score)
	}
	allZero := true
	for _, s := range out {
		if s > 0 {
			allZero = false
			break
		}
	}
	if allZero && len(out) > 0 {
		for i := range out {
			out[i] = 1.0 / float64(i+1)
		}
	}
	return out
}

func keepNearDuplicatePassages(window, pool []Passage, maxAdd int) []Passage {
	if maxAdd <= 0 {
		maxAdd = 4
	}
	inWin := map[string]struct{}{}
	for _, p := range window {
		inWin[p.DocumentID] = struct{}{}
	}
	heads := map[string]string{}
	for _, p := range window {
		heads[p.DocumentID] = bodyHead(p.Text, 280)
	}
	added := 0
	var extra []Passage
	for _, p := range pool {
		if _, ok := inWin[p.DocumentID]; ok {
			continue
		}
		ph := bodyHead(p.Text, 280)
		if ph == "" {
			continue
		}
		for _, wh := range heads {
			if nearDupHeads(wh, ph) {
				extra = append(extra, p)
				inWin[p.DocumentID] = struct{}{}
				added++
				break
			}
		}
		if added >= maxAdd {
			break
		}
	}
	if len(extra) == 0 {
		return window
	}
	return append(append([]Passage(nil), window...), extra...)
}

func bodyHead(text string, n int) string {
	t := stripRecencyHeaders(text)
	t = strings.Join(strings.Fields(t), " ")
	if len(t) > n {
		t = t[:n]
	}
	return strings.ToLower(t)
}

func nearDupHeads(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ta := strings.Fields(a)
	tb := strings.Fields(b)
	if len(ta) < 8 || len(tb) < 8 {
		return false
	}
	set := map[string]struct{}{}
	for _, t := range ta {
		if len(t) >= 4 {
			set[t] = struct{}{}
		}
	}
	inter := 0
	for _, t := range tb {
		if len(t) < 4 {
			continue
		}
		if _, ok := set[t]; ok {
			inter++
		}
	}
	union := len(set)
	for _, t := range tb {
		if len(t) >= 4 {
			if _, ok := set[t]; !ok {
				union++
			}
		}
	}
	if union == 0 {
		return false
	}
	return float64(inter)/float64(union) >= 0.55
}

func annotateRecencyPack(ps []Passage) []Passage {
	if len(ps) < 1 {
		return ps
	}
	type meta struct {
		date string
		idx  int
	}
	dates := make([]string, len(ps))
	for i, p := range ps {
		dates[i] = passageDocDate(p)
	}
	groups := nearDupGroups(ps)
	marks := map[int]string{}
	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		var dated []meta
		for _, i := range g {
			if dates[i] != "" {
				dated = append(dated, meta{date: dates[i], idx: i})
			}
		}
		if len(dated) < 2 {
			continue
		}
		sort.Slice(dated, func(i, j int) bool { return dated[i].date < dated[j].date })
		oldest, newest := dated[0].date, dated[len(dated)-1].date
		if oldest == newest {
			continue
		}
		for _, d := range dated {
			if d.date == newest {
				marks[d.idx] = "[the NEWEST version among near-duplicates]"
			} else {
				marks[d.idx] = "[an OLDER version of another provided document]"
			}
		}
	}
	out := make([]Passage, len(ps))
	for i, p := range ps {
		body := stripRecencyHeaders(p.Text)
		var hdr []string
		if m, ok := marks[i]; ok {
			hdr = append(hdr, m)
		}
		if dates[i] != "" {
			hdr = append(hdr, "[document date: "+dates[i]+"]")
		}
		if len(hdr) > 0 {
			p.Text = strings.Join(hdr, "\n") + "\n" + body
		} else {
			p.Text = body
		}
		out[i] = p
	}
	return out
}

func passageDocDate(p Passage) string {
	text := p.Text
	text = textbound.Bytes(text, 4000)
	if m := effectiveDateRE.FindStringSubmatch(text); len(m) > 1 {
		return m[1]
	}
	head := text
	head = textbound.Bytes(head, 800)
	if m := isoDateRE.FindString(head); m != "" {
		return m
	}
	return ""
}

var effectiveDateRE = regexp.MustCompile(`(?i)(?:effective|updated|goes live|published)[^\d]{0,24}(20\d{2}-\d{2}-\d{2})`)

var recencyHeaderRE = regexp.MustCompile(`(?m)^\[(?:document date: [^\]]+|an OLDER version[^\]]*|the NEWEST version[^\]]*)\]\s*`)

func stripRecencyHeaders(text string) string {
	prev := ""
	for text != prev {
		prev = text
		text = recencyHeaderRE.ReplaceAllString(text, "")
	}
	return strings.TrimSpace(text)
}

func nearDupGroups(ps []Passage) [][]int {
	heads := make([]string, len(ps))
	for i, p := range ps {
		heads[i] = bodyHead(p.Text, 280)
	}
	var groups [][]int
	used := make([]bool, len(ps))
	for i := range ps {
		if used[i] || heads[i] == "" {
			continue
		}
		g := []int{i}
		used[i] = true
		for j := i + 1; j < len(ps); j++ {
			if used[j] || heads[j] == "" {
				continue
			}
			if nearDupHeads(heads[i], heads[j]) {
				g = append(g, j)
				used[j] = true
			}
		}
		if len(g) > 1 {
			groups = append(groups, g)
		}
	}
	return groups
}

var notFoundAnswerRE = regexp.MustCompile(`(?i)\b(not found|no information|do(es)? not (contain|mention|specify|establish)|` +
	`couldn'?t find|could not find|not (mentioned|specified|established)|` +
	`no supporting documents|documents do not establish|not fully answerable)\b`)

func shouldClearCitesOnAbstain(answer string) bool {
	return notFoundAnswerRE.MatchString(answer)
}

func unionContextWindow(window, rrfPool []Passage, topK int) []Passage {
	if topK <= 0 {
		topK = 12
	}
	out := append([]Passage(nil), window...)
	seen := map[string]struct{}{}
	for _, p := range out {
		seen[p.DocumentID] = struct{}{}
	}
	headN := 10
	if headN > len(rrfPool) {
		headN = len(rrfPool)
	}
	for _, p := range rrfPool[:headN] {
		if _, ok := seen[p.DocumentID]; ok {
			continue
		}
		seen[p.DocumentID] = struct{}{}
		out = append(out, p)
	}
	out = keepNearDuplicatePassages(out, rrfPool, 4)
	capN := topK + 8
	if len(out) > capN {
		out = out[:capN]
	}
	return out
}

// preferRealCE: prefer Cohere/ZE CE whenever keys exist. QUALITY always wants real CE;
// light interactive may still force lexical for latency (caller passes forceLexical).
func preferRealCE(prodQuality bool) bool {
	if envTruthy("OUROBOROS_ERB_FORCE_LEXICAL_CE", false) {
		return false
	}
	_ = prodQuality
	// Default: use ZE/Cohere/MLX whenever available — never force lexical on product.
	if zeKey() != "" {
		return true
	}
	// Cohere CE (rerank-v3.5) must not be silently bypassed when only its key
	// is present — previously required ZE and fell back to lexical.
	if cohereKey() != "" {
		return true
	}
	ranker := strings.ToLower(envOr("OUROBOROS_BRAIN_RANKER", ""))
	if ranker == SubstrateAPIMLX || ranker == "local" {
		return true
	}
	// No remote CE: lexical is last resort (still better than skip).
	return false
}

func cePoolSize(poolLen, topK int, prod ProdProfile) int {
	return cePoolSizeN(poolLen, topK, prod, 0)
}

// cePoolSizeN allows smf expand-tier wide CE (rerank_cap spirit, 120–150).
func cePoolSizeN(poolLen, topK int, prod ProdProfile, wantN int) int {
	n := 48
	if prod.Quality || benchmaxEnabled() {
		n = 100
	}
	if wantN > n {
		n = wantN
	}
	if topK*5 > n {
		n = topK * 5
	}
	// Hard cap: CE latency grows with n; 150 matches smf rerank_cap keep-band.
	if n > 150 {
		n = 150
	}
	if n > poolLen {
		n = poolLen
	}
	if n < topK {
		n = topK
	}
	return n
}

func normalizeCEScores(ps []Passage) []float64 {
	raw := passageScores(ps)
	if len(raw) == 0 {
		return raw
	}
	max := 0.0
	for _, s := range raw {
		if s > max {
			max = s
		}
	}
	if max <= 0 {
		return raw
	}
	if max > 1.5 {
		out := make([]float64, len(raw))
		for i, s := range raw {
			out[i] = s / max
		}
		return out
	}
	return raw
}

func selfConsistencyWanted(question string) int {
	// Multi-sample synth is expensive (~2×). Auto-enable only on QUALITY/bench
	// budgets or explicit env — not every light leave ask (Modal ~200s wall).
	n := envInt("OUROBOROS_ERB_SELF_CONSISTENCY", 0)
	auto := seeksAtomicDate(question) || seeksAtomicQuantity(question) ||
		seeksMoneyQuantity(question) || prefersSupersedingEvidence(question, "")
	qualityBudget := envTruthy("OUROBOROS_ERB_QUALITY", false) ||
		benchmaxEnabled() ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "bench") ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "research")
	if envTruthy("OUROBOROS_ERB_SELF_CONSISTENCY_OFF", false) {
		return 1
	}
	if n <= 0 {
		// Default single-shot: multi-sample doubles wall (~220s mean on harden20b)
		// and caused timeouts. Opt in via OUROBOROS_ERB_SELF_CONSISTENCY=2+.
		// Pack contest + quant rebind + gpt-5.4 cover atomic fidelity cheaper.
		if auto && qualityBudget && envTruthy("OUROBOROS_ERB_SELF_CONSISTENCY_AUTO", false) {
			return 2
		}
		return 1
	}
	if n == 1 {
		return 1
	}
	if n > 4 {
		n = 4
	}
	if auto || qualityBudget || envTruthy("OUROBOROS_ERB_SELF_CONSISTENCY_ALWAYS", false) {
		return n
	}
	return 1
}

func pickBestGrounded(samples []synthRaw, passages []Passage) synthRaw {
	if len(samples) == 0 {
		return synthRaw{}
	}
	if len(samples) == 1 {
		return samples[0]
	}
	// Pack blob for atom-overlap scoring (prefer answers that copy pack quantities).
	var pack strings.Builder
	for _, p := range passages {
		pack.WriteString(strings.ToLower(p.Text))
		pack.WriteByte(' ')
	}
	packLow := pack.String()
	best := samples[0]
	bestScore := -1
	for _, s := range samples {
		sc := 0
		if s.Answer != "" {
			sc++
		}
		if shouldClearCitesOnAbstain(s.Answer) {
			sc -= 3
		}
		// Reward answer atoms that appear in pack (numbers, $ amounts, short ids).
		ansLow := strings.ToLower(s.Answer)
		for _, m := range durationAtomRE.FindAllString(ansLow, -1) {
			if strings.Contains(packLow, strings.ToLower(m)) {
				sc += 3
			}
		}
		for _, m := range moneyAtomRE.FindAllString(s.Answer, -1) {
			if strings.Contains(packLow, strings.ToLower(m)) {
				sc += 3
			}
		}
		// Identifier / metric-like tokens (≥6 alnum with . or _)
		for _, t := range contentTokens(s.Answer) {
			if len(t) >= 6 && (strings.Contains(t, ".") || strings.Contains(t, "_")) {
				if strings.Contains(packLow, t) {
					sc += 2
				}
			}
		}
		allowed := map[string]struct{}{}
		for _, p := range passages {
			allowed[p.DocumentID] = struct{}{}
		}
		for _, c := range s.Cited {
			if _, ok := allowed[c]; ok {
				sc += 2
			}
		}
		al := strings.ToLower(s.Answer)
		if strings.Contains(al, "supersed") || strings.Contains(al, "effective") ||
			strings.Contains(al, "current") || strings.Contains(al, "updated") {
			sc++
		}
		if sc > bestScore {
			bestScore = sc
			best = s
		}
	}
	return best
}

// runRecoveryDenseLists is the production chunk-ANN recovery leg (sentra v5
// FAISS chunk index equivalent). Path2 already stores chunk vectors in Qdrant
// (or local dense) — multi-query parallel ANN is more prod-ready than a second
// 4.6M offline FAISS file: shared infra, multi-tenant, no dual index drift.
func (c *Client) runRecoveryDenseLists(ctx context.Context, queries []string, prod ProdProfile) hostedDenseQueryRun {
	if c == nil || len(queries) == 0 {
		return hostedDenseQueryRun{}
	}
	// Bounded batch embed + parallel ANN under denseBudget — wall share ≤2.5s
	// for the 30s total target.
	budget := denseBudget(prod)
	if budget <= 0 {
		budget = 2500 * time.Millisecond
	}
	if budget > 2500*time.Millisecond {
		budget = 2500 * time.Millisecond
	}
	actx, cancel := withTimeout(ctx, budget)
	defer cancel()
	// Cap recovery dense (phase-A already multi-query).
	maxQ := 3
	if prod.Quality {
		maxQ = 3
	}
	if len(queries) > maxQ {
		queries = queries[:maxQ]
	}
	// Query embeddings share bounded Cohere requests. ANN remains parallel and
	// stable-ordered; keeping it separate preserves Qdrant brain scoping.
	run := c.runHostedDenseQueries(actx, queries)
	for i := range run.Lists {
		// Head 50 into CE union (sentra fuse_n / chunk recover).
		if len(run.Lists[i]) > 50 {
			run.Lists[i] = run.Lists[i][:50]
		}
	}
	return run
}

func (c *Client) runRecoveryHotLists(queries []string, limit int) [][]Hit {
	if c == nil || c.hot == nil || c.hot.Len() == 0 {
		return nil
	}
	if limit <= 0 {
		limit = 50
	}
	if len(queries) > 5 {
		queries = queries[:5]
	}
	// Parallel HotLex (CPU) — long queries are already short-capped by caller.
	type hres struct {
		hits []Hit
	}
	ch := make(chan hres, len(queries))
	for _, q := range queries {
		go func(qq string) {
			qq = textbound.Bytes(qq, 120)
			hits := c.hot.Search(qq, limit)
			ch <- hres{hits: hits}
		}(q)
	}
	var lists [][]Hit
	for range queries {
		r := <-ch
		if len(r.hits) > 0 {
			lists = append(lists, r.hits)
		}
	}
	return lists
}

// corpusGrepFallback: HotLex phrase + Neon FTS on dynamic bags when the window
// is thin or soft-empty (turn_grep is conversation-only; this is corpus).
func (c *Client) corpusGrepFallback(ctx context.Context, question string, prod ProdProfile, existing []Passage, ftsState retrievalFTSState) []Hit {
	if c == nil {
		return nil
	}
	bags := c.corpusGrepBags(ctx, question, existing, 14)
	var lists [][]Hit
	if c.hot != nil && c.hot.Len() > 0 {
		for _, q := range bags {
			hits := c.hot.Search(q, 40)
			if len(hits) > 0 {
				lists = append(lists, hits)
			}
		}
	}
	// Neon FTS bags only when a usable HotLex projection was present but thin.
	// When HotLex is unavailable, phase A may already have spent the request's
	// one bounded Neon fallback; corpusGrepFTSAllowed skips this tail only when
	// that fallback was actually attempted, never for an availability failure.
	hotListN := len(lists)
	if c.corpusGrepFTSAllowed(ftsState, hotListN) {
		ftsBudget := boundedFTSBudget(prod, prod.LexTimeout)
		lctx, cancel := withTimeout(ctx, ftsBudget)
		defer cancel()
		maxFTS := 3
		if !ftsState.hotLexAvailable {
			maxFTS = missingHotLexFTSQueryCap
		}
		for i, q := range bags {
			if i >= maxFTS {
				break
			}
			hits, err := lexicalSearchLimited(lctx, c.db, c.cfg, q, prod.LexTerms, 40)
			if err == nil && len(hits) > 0 {
				lists = append(lists, hits)
			}
		}
	}
	if len(lists) == 0 {
		return nil
	}
	fused := rrfFuseMany(lists, c.cfg.RRFK)
	if len(fused) > 40 {
		fused = fused[:40]
	}
	return fused
}

func (c *Client) corpusGrepBags(ctx context.Context, question string, existing []Passage, maxN int) []string {
	if c == nil || maxN < 1 {
		return nil
	}
	// Identifiers first (exact term win for pool@0 names/metrics/INC).
	base := extractIdentifiers(question)
	for _, t := range contentTokens(question) {
		if len(t) >= 6 {
			base = append(base, t)
		}
	}
	base = append(base, c.recoveryQueriesForClient(ctx, question, passageTexts(existing), 6)...)
	// Use only the client's brain/generation-compatible catalog. Reserve a
	// bounded share before identifier/content bags are capped so the live grep
	// path cannot both cross tenant scope and truncate every catalog alias.
	entityTerms := c.scopedOfflineEntityTerms(question, 8)
	reserve := minInt(len(entityTerms), minInt(4, maxN))
	var bags []string
	seen := map[string]struct{}{}
	add := func(s string) bool {
		s = strings.TrimSpace(s)
		if s == "" || len(bags) >= maxN {
			return false
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			return false
		}
		seen[key] = struct{}{}
		bags = append(bags, s)
		return true
	}
	basePos := 0
	for basePos < len(base) && len(bags) < maxN-reserve {
		add(base[basePos])
		basePos++
	}
	for i := 0; i < reserve; i++ {
		add(entityTerms[i])
	}
	for basePos < len(base) && len(bags) < maxN {
		add(base[basePos])
		basePos++
	}
	for i := reserve; i < len(entityTerms) && len(bags) < maxN; i++ {
		add(entityTerms[i])
	}
	return bags
}

// retrievalFTSState is captured once for a retrieve request. Recovery gates use
// it instead of re-reading the mutable HotLex pointer or environment after
// phase A, avoiding TOCTOU route changes within one request.
type retrievalFTSState struct {
	hotLexAvailable         bool
	phaseAFallbackAttempted bool
	ftsDisabled             bool
}

// corpusGrepFTSAllowed prevents recovery from becoming a second Neon fallback.
// If phase A could not attempt the sole missing-HotLex fallback, recovery may
// spend it once; otherwise missing projection never amplifies into more queries.
func (c *Client) corpusGrepFTSAllowed(state retrievalFTSState, hotListN int) bool {
	return c != nil && c.db != nil && !state.ftsDisabled && hotListN < 3 &&
		(state.hotLexAvailable || !state.phaseAFallbackAttempted)
}

func passageTexts(ps []Passage) []string {
	var out []string
	for _, p := range ps {
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	return out
}

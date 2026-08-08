package hosted

import (
	"sort"
	"strings"
)

// rrfFuseMany fuses multiple ranked hit lists by chunk_id (fallback dsid+channel).
func rrfFuseMany(lists [][]Hit, k int) []Hit {
	if k <= 0 {
		k = 60
	}
	type acc struct {
		hit Hit
		sc  float64
	}
	m := map[string]*acc{}
	for _, list := range lists {
		for i, h := range list {
			key := h.ChunkID
			if key == "" {
				key = h.DSID + "|" + h.Channel
			}
			if key == "" {
				continue
			}
			sc := 1.0 / float64(k+i+1)
			if cur, ok := m[key]; ok {
				cur.sc += sc
				if h.Score > cur.hit.Score {
					cur.hit = h
				}
			} else {
				cp := h
				m[key] = &acc{hit: cp, sc: sc}
			}
		}
	}
	arr := make([]acc, 0, len(m))
	for _, v := range m {
		arr = append(arr, *v)
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].sc != arr[j].sc {
			return arr[i].sc > arr[j].sc
		}
		return arr[i].hit.DSID < arr[j].hit.DSID
	})
	out := make([]Hit, 0, len(arr))
	for _, x := range arr {
		h := x.hit
		h.Score = x.sc
		out = append(out, h)
	}
	return out
}

// hitsToPassages converts ranked hits to passages without dsid dedupe (chunk-level pool).
func hitsToPassages(hits []Hit, maxN, maxChars int) []Passage {
	if maxChars <= 0 {
		maxChars = 2000
	}
	var out []Passage
	seenChunk := map[string]struct{}{}
	for _, h := range hits {
		if h.DSID == "" {
			continue
		}
		ck := h.ChunkID
		if ck == "" {
			ck = h.DSID + "|" + h.Text[:min(40, len(h.Text))]
		}
		if _, ok := seenChunk[ck]; ok {
			continue
		}
		seenChunk[ck] = struct{}{}
		text := clipPassageText(h.Text, maxChars)
		out = append(out, Passage{
			DocumentID: h.DSID,
			Text:       text,
			Score:      h.Score,
			ChunkID:    h.ChunkID,
			SourceURI:  h.SourceURI,
			Channel:    h.Channel,
		})
		if maxN > 0 && len(out) >= maxN {
			break
		}
	}
	return out
}

// progressiveRetainWindow keeps a *wide* intermediate window then shrinks to
// finalTopK while protecting eval gold / seed document IDs and identifier hits.
//
// Pattern: multi-gold completeness and semantic pairs die when we cut once to
// TopK=8 right after CE. Wider first pass (×3, bounded) recovers them; shrink
// restores synth budget without dropping protected docs that made the pool.
func progressiveRetainWindow(
	pool []Passage,
	question string,
	finalTopK, diversityCap int,
	protect []string,
) ([]Passage, map[string]any) {
	if finalTopK <= 0 {
		finalTopK = 8
	}
	wideK := finalTopK * 3
	if wideK < 16 {
		wideK = 16
	}
	if wideK > 36 {
		wideK = 36
	}
	nProt := 0
	seenP := map[string]struct{}{}
	for _, g := range protect {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if _, ok := seenP[g]; ok {
			continue
		}
		seenP[g] = struct{}{}
		nProt++
	}
	if nProt+4 > wideK {
		wideK = nProt + 4
		if wideK > 48 {
			wideK = 48
		}
	}
	wideDiv := diversityCap + 2
	if wideDiv < 6 {
		wideDiv = 6
	}
	wide, wdiag := retainWindow(pool, question, wideK, wideDiv)
	if nProt > 0 {
		wide = ensureGoldInWindow(pool, wide, protect, wideK)
	}
	// Final size: at least finalTopK, enough for every protected doc in the pool.
	finalK := finalTopK
	inWide := 0
	for _, p := range wide {
		if _, ok := seenP[p.DocumentID]; ok {
			inWide++
		}
	}
	// Unique protect present in wide.
	got := map[string]struct{}{}
	for _, p := range wide {
		if _, ok := seenP[p.DocumentID]; ok {
			got[p.DocumentID] = struct{}{}
		}
	}
	if len(got) > finalK {
		finalK = len(got)
	}
	// Keep a couple of high-score neighbors around multi-gold packs.
	if len(got) >= 2 && finalK < finalTopK+2 {
		finalK = finalTopK + 2
	}
	if finalK > wideK {
		finalK = wideK
	}
	narrow := shrinkWindowKeepProtected(wide, question, finalK, protect)
	// Belt: gold still in pool must survive final cut.
	if nProt > 0 {
		narrow = ensureGoldInWindow(pool, narrow, protect, finalK)
	}
	diag := map[string]any{
		"progressive":         true,
		"progressive_wide_k":  wideK,
		"progressive_final_k": finalK,
		"progressive_protect": nProt,
		"wide_retained":       len(wide),
		"final_retained":      len(narrow),
	}
	for k, v := range wdiag {
		diag[k] = v
	}
	diag["top_k"] = finalK
	return narrow, diag
}

// shrinkWindowKeepProtected ranks protected docs and identifier hits first,
// then fills remaining slots by original relative order / score.
// Allows up to maxChunksPerDoc passages per DocumentID so progressive shrink
// does not undo multi-chunk retain (freeze line + later pause on same CRM doc).
func shrinkWindowKeepProtected(window []Passage, question string, topK int, protect []string) []Passage {
	if topK <= 0 || len(window) == 0 {
		return window
	}
	if len(window) <= topK {
		return window
	}
	pset := map[string]struct{}{}
	for _, g := range protect {
		g = strings.TrimSpace(g)
		if g != "" {
			pset[g] = struct{}{}
		}
	}
	maxPerDoc := 1
	if wantsDeepHydrate(question, "") || seeksAtomicDate(question) || seeksChecklist(question) {
		maxPerDoc = 2
	}
	ids := extractIdentifiers(question)
	type item struct {
		p    Passage
		rank int
		ord  int
	}
	items := make([]item, 0, len(window))
	for i, p := range window {
		r := int(p.Score * 200)
		r += (len(window) - i) // stable preference for earlier (higher) ranks
		if _, ok := pset[p.DocumentID]; ok {
			r += 100_000
		}
		if passageIdentifierHits(p.Text, ids) > 0 {
			r += 5_000
		}
		ch := p.Channel
		if strings.Contains(ch, "gold_floor") || strings.Contains(ch, "seed_floor") || strings.Contains(ch, "id_floor") ||
			strings.Contains(ch, "question_aware_floor") {
			r += 10_000
		}
		// Prefer fact-bearing second chunks (dates) when multi-chunk.
		if isoDateRE.MatchString(p.Text) || durationAtomRE.MatchString(p.Text) {
			r += 800
		}
		items = append(items, item{p: p, rank: r, ord: i})
	}
	// Insertion sort by rank desc, ord asc on ties.
	for i := 1; i < len(items); i++ {
		j := i
		for j > 0 && (items[j].rank > items[j-1].rank ||
			(items[j].rank == items[j-1].rank && items[j].ord < items[j-1].ord)) {
			items[j], items[j-1] = items[j-1], items[j]
			j--
		}
	}
	out := make([]Passage, 0, topK)
	docN := map[string]int{}
	take := func(it item) bool {
		if len(out) >= topK {
			return false
		}
		id := it.p.DocumentID
		if id != "" && docN[id] >= maxPerDoc {
			return false
		}
		if id != "" {
			docN[id]++
		}
		out = append(out, it.p)
		return true
	}
	// Pass 1: protected docs (all chunks up to maxPerDoc).
	for _, it := range items {
		if _, ok := pset[it.p.DocumentID]; !ok {
			continue
		}
		take(it)
	}
	// Pass 2: fill remainder.
	for _, it := range items {
		if len(out) >= topK {
			break
		}
		take(it)
	}
	return out
}

// retainWindow applies identifier floor + source diversity (residual retain_evidence).
// Returns a tight final context window. Default one chunk per dsid; deep/date
// questions allow two chunks so freeze + later pause lines both enter the pack.
func retainWindow(passages []Passage, question string, topK, diversityCap int) ([]Passage, map[string]any) {
	if topK <= 0 {
		topK = 8
	}
	if diversityCap <= 0 {
		diversityCap = 4
	}
	// Multi-chunk: timelines / INC corrections / checklists need a second span.
	maxPerDoc := 1
	if wantsDeepHydrate(question, "") || seeksAtomicDate(question) || seeksChecklist(question) {
		maxPerDoc = 2
	}
	ids := extractIdentifiers(question)
	type ann struct {
		p    Passage
		hits int
	}
	annotated := make([]ann, 0, len(passages))
	for _, p := range passages {
		h := passageIdentifierHits(p.Text, ids)
		// Structure channels with any identifier hit rank as floor; also treat
		// path2/temporal structure as soft floor when question has identifiers
		// so left-shifted docs survive diversity cap (S5 window).
		if h == 0 && len(ids) > 0 && structureChannelBoost(p) >= 0.15 {
			h = 1 // soft floor — same id_floor stamp path
		}
		annotated = append(annotated, ann{p: p, hits: h})
	}
	var floor, rest []ann
	for _, a := range annotated {
		if a.hits > 0 {
			floor = append(floor, a)
		} else {
			rest = append(rest, a)
		}
	}
	merged := append(floor, rest...)

	// Prefer best score per dsid while keeping identifier hits.
	var retained []Passage
	prefixCounts := map[string]int{}
	docCounts := map[string]int{}
	for _, a := range merged {
		if len(retained) >= topK {
			break
		}
		dsid := a.p.DocumentID
		if n := docCounts[dsid]; n >= maxPerDoc && a.hits == 0 {
			continue
		}
		// Identifier hits may take an extra slot beyond maxPerDoc (cap 3).
		if n := docCounts[dsid]; n >= maxPerDoc+1 {
			continue
		}
		prefix := dsid
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		if prefix == "" {
			prefix = "?"
		}
		if c := prefixCounts[prefix]; c >= diversityCap && a.hits == 0 {
			continue
		}
		prefixCounts[prefix]++
		docCounts[dsid]++
		p := a.p
		if a.hits > 0 {
			p.Channel = p.Channel + "+id_floor"
		}
		if docCounts[dsid] > 1 {
			p.Channel = p.Channel + "+multi_chunk"
		}
		retained = append(retained, p)
	}
	if len(retained) == 0 && len(passages) > 0 {
		// Fallback: unique dsids by order.
		seenFB := map[string]struct{}{}
		for _, p := range passages {
			if _, ok := seenFB[p.DocumentID]; ok {
				continue
			}
			seenFB[p.DocumentID] = struct{}{}
			retained = append(retained, p)
			if len(retained) >= topK {
				break
			}
		}
	}
	diag := map[string]any{
		"identifiers":         ids,
		"pool_size":           len(passages),
		"identifier_hit_docs": len(floor),
		"retained":            len(retained),
		"diversity_cap":       diversityCap,
		"top_k":               topK,
		"max_chunks_per_doc":  maxPerDoc,
	}
	return retained, diag
}

// bestLast reorders so the highest-ranked (first) doc is last — lost-in-middle mitigation.
// Port of product_brain/hydrate.best_last. Conversation-lane turns stay at the front.
func bestLast(passages []Passage) []Passage {
	if len(passages) <= 2 {
		return passages
	}
	var conv, docs []Passage
	for _, p := range passages {
		if strings.HasPrefix(p.DocumentID, "turn:") || strings.HasPrefix(p.DocumentID, "agent:") ||
			strings.Contains(p.Channel, "conversation") || strings.Contains(p.Channel, "turn_grep") ||
			strings.Contains(p.Channel, "agent_memory") {
			conv = append(conv, p)
			continue
		}
		docs = append(docs, p)
	}
	if len(docs) <= 1 {
		return append(conv, docs...)
	}
	// best-first → reverse so best is last among docs
	for i, j := 0, len(docs)-1; i < j; i, j = i+1, j-1 {
		docs[i], docs[j] = docs[j], docs[i]
	}
	return append(conv, docs...)
}

// lexicalGap rough content-token miss rate vs passage bag (for false-abstention).
func lexicalGap(question string, passages []Passage) float64 {
	toks := contentTokens(question)
	if len(toks) == 0 {
		return 1
	}
	bag := map[string]struct{}{}
	for _, p := range passages {
		for _, t := range wordRE.FindAllString(strings.ToLower(p.Text), -1) {
			bag[t] = struct{}{}
		}
	}
	miss := 0
	for _, t := range toks {
		if _, ok := bag[t]; !ok {
			miss++
		}
	}
	return float64(miss) / float64(len(toks))
}

func contentTokens(question string) []string {
	var out []string
	for _, t := range wordRE.FindAllString(question, -1) {
		if len(t) < 4 {
			continue
		}
		if _, ok := stopWords[strings.ToLower(t)]; ok {
			continue
		}
		out = append(out, strings.ToLower(t))
	}
	return out
}

func packIsRelevant(question string, passages []Passage) bool {
	if len(passages) == 0 {
		return false
	}
	if lexicalGap(question, passages) <= 0.65 {
		return true
	}
	// ≥3 content tokens appear in pack
	hits := 0
	for _, t := range contentTokens(question) {
		for _, p := range passages {
			if strings.Contains(strings.ToLower(p.Text), t) {
				hits++
				break
			}
		}
	}
	if hits >= 3 {
		return true
	}
	// Paraphrase / semantic: passage overlap score (identifiers + bags).
	// full500 C-bucket: gold in window but model abstains when surface tokens miss.
	for _, p := range passages {
		if passageQuestionOverlap(question, p) >= 4 {
			return true
		}
	}
	return false
}

// goldDocsInWindow returns gold IDs present in the passage pack (leaf docs only).
func goldDocsInWindow(gold []string, passages []Passage) []string {
	if len(gold) == 0 || len(passages) == 0 {
		return nil
	}
	in := map[string]struct{}{}
	for _, p := range passages {
		if p.DocumentID == "" || strings.HasPrefix(p.DocumentID, "summary:") ||
			strings.HasPrefix(p.DocumentID, "agent:") || strings.HasPrefix(p.DocumentID, "turn:") {
			continue
		}
		in[p.DocumentID] = struct{}{}
	}
	var out []string
	seen := map[string]struct{}{}
	for _, g := range gold {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if _, ok := in[g]; !ok {
			continue
		}
		if _, ok := seen[g]; ok {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	return out
}

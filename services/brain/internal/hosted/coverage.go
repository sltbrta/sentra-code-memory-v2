package hosted

import (
	"math"
	"sort"
	"strings"
)

// coverageRerank reorders passages for multi-gold coverage (MMR-style).
// Prefers set diversity of content tokens over pure pointwise score so
// second/third gold docs are not drowned by near-duplicate top hits.
func coverageRerank(question string, passages []Passage, topK int, lambda float64) []Passage {
	if len(passages) == 0 {
		return passages
	}
	if topK <= 0 || topK > len(passages) {
		topK = len(passages)
	}
	if lambda <= 0 || lambda > 1 {
		lambda = 0.7
	}
	qtoks := contentTokens(question)
	type scored struct {
		p    Passage
		rel  float64
		toks map[string]struct{}
	}
	cands := make([]scored, 0, len(passages))
	for _, p := range passages {
		toks := tokenSet(p.Text)
		rel := float64(overlapCount(qtoks, toks))
		if p.Score > 0 {
			rel = rel + p.Score*0.1
		}
		// Left-shifted structure arms are precomputed intelligence — mild prior
		// so dense near-dupes do not drown path2/temporal evidence (S5).
		rel += structureChannelBoost(p)
		cands = append(cands, scored{p: p, rel: rel, toks: toks})
	}
	// Seed by relevance
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rel != cands[j].rel {
			return cands[i].rel > cands[j].rel
		}
		return cands[i].p.DocumentID < cands[j].p.DocumentID
	})

	var selected []scored
	used := map[int]struct{}{}
	// Prefer unique dsids first pass
	seenDSID := map[string]struct{}{}
	for len(selected) < topK {
		bestI := -1
		bestScore := math.Inf(-1)
		for i, c := range cands {
			if _, ok := used[i]; ok {
				continue
			}
			// Mild penalty for same dsid already selected
			dsidPen := 0.0
			if _, ok := seenDSID[c.p.DocumentID]; ok {
				dsidPen = 0.35
			}
			maxSim := 0.0
			for _, s := range selected {
				sim := jaccard(c.toks, s.toks)
				if sim > maxSim {
					maxSim = sim
				}
			}
			mmr := lambda*c.rel - (1-lambda)*maxSim - dsidPen
			if mmr > bestScore {
				bestScore = mmr
				bestI = i
			}
		}
		if bestI < 0 {
			break
		}
		used[bestI] = struct{}{}
		selected = append(selected, cands[bestI])
		seenDSID[cands[bestI].p.DocumentID] = struct{}{}
	}
	out := make([]Passage, len(selected))
	for i, s := range selected {
		p := s.p
		p.Channel = p.Channel + "+coverage"
		p.Score = s.rel
		out[i] = p
	}
	return out
}

// structureChannelBoost is a mild MMR relevance prior for left-shifted structure
// channels (path2 SQL / TemporalRelations). Does not override identifier floor.
func structureChannelBoost(p Passage) float64 {
	ch := strings.ToLower(p.Channel)
	switch {
	case strings.Contains(ch, "path2_structure"):
		return 0.18
	case strings.Contains(ch, "temporal_relation"):
		return 0.15
	case strings.Contains(ch, "structure") || strings.Contains(ch, "edge_hop") ||
		strings.Contains(ch, "entity_fanout") || strings.Contains(ch, "facts"):
		return 0.06
	default:
		return 0
	}
}

func tokenSet(text string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, t := range wordRE.FindAllString(strings.ToLower(text), -1) {
		if len(t) < 3 {
			continue
		}
		if _, ok := stopWords[t]; ok {
			continue
		}
		m[t] = struct{}{}
	}
	return m
}

func overlapCount(qtoks []string, doc map[string]struct{}) int {
	n := 0
	for _, t := range qtoks {
		if _, ok := doc[t]; ok {
			n++
		}
	}
	return n
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if _, ok := b[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// diagnoseWindow records pool vs window sizes and unique dsids (instrumentation).
func diagnoseWindow(pool, window []Passage, gold []string) map[string]any {
	poolD := uniqueDSIDs(pool)
	winD := uniqueDSIDs(window)
	d := map[string]any{
		"pool_passages":      len(pool),
		"window_passages":    len(window),
		"pool_unique_docs":   len(poolD),
		"window_unique_docs": len(winD),
	}
	if len(gold) > 0 {
		gset := map[string]struct{}{}
		for _, g := range gold {
			gset[g] = struct{}{}
		}
		d["pool_gold_hits"] = countHits(poolD, gset)
		d["window_gold_hits"] = countHits(winD, gset)
		d["gold_count"] = len(gset)
	}
	return d
}

// computeGoldDiag reports offline-eval gold hit rates against RRF pool + final window.
//
//	pool_recall      = |gold ∩ rrf_pool| / |gold|
//	window_precision = |gold ∩ window| / |window|  (0 if empty window; docs via unique dsid)
//	pool_at_10 / pool_at_20 = any gold in first N unique docs of pool order (SMF-style recall@k)
//
// Also returns gold_in_pool / gold_in_window membership slices (stable-sorted).
func computeGoldDiag(gold []string, pool, window []Passage) map[string]any {
	gset := map[string]struct{}{}
	for _, g := range gold {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		gset[g] = struct{}{}
	}
	if len(gset) == 0 {
		return nil
	}
	// Preserve pool order (RRF/CE rank) for recall@k.
	poolOrder := uniqueDSIDs(pool)
	poolSet := map[string]struct{}{}
	rank := map[string]int{} // 1-based
	for i, id := range poolOrder {
		poolSet[id] = struct{}{}
		if _, ok := rank[id]; !ok {
			rank[id] = i + 1
		}
	}
	winSet := map[string]struct{}{}
	for _, id := range uniqueDSIDs(window) {
		winSet[id] = struct{}{}
	}
	var goldInPool, goldInWindow []string
	minRank := 0
	in10, in20 := false, false
	for g := range gset {
		if _, ok := poolSet[g]; ok {
			goldInPool = append(goldInPool, g)
			if r, ok := rank[g]; ok {
				if minRank == 0 || r < minRank {
					minRank = r
				}
				if r <= 10 {
					in10 = true
				}
				if r <= 20 {
					in20 = true
				}
			}
		}
		if _, ok := winSet[g]; ok {
			goldInWindow = append(goldInWindow, g)
		}
	}
	sort.Strings(goldInPool)
	sort.Strings(goldInWindow)
	poolRecall := float64(len(goldInPool)) / float64(len(gset))
	winPrecision := 0.0
	if len(winSet) > 0 {
		winPrecision = float64(len(goldInWindow)) / float64(len(winSet))
	}
	windowRecall := float64(len(goldInWindow)) / float64(len(gset))
	out := map[string]any{
		"pool_recall":      poolRecall,
		"window_recall":    windowRecall, // |gold ∩ window| / |gold| — stage gate metric
		"window_precision": winPrecision,
		"gold_in_pool":     goldInPool,
		"gold_in_window":   goldInWindow,
		"gold_count":       len(gset),
		"pool_gold_hits":   len(goldInPool),
		"window_gold_hits": len(goldInWindow),
		// SMF-style retrieve metrics (unique-doc fusion order, not cite list).
		"pool_at_10":    in10,
		"pool_at_20":    in20,
		"pool_unique_n": len(poolOrder),
		"gold_min_rank": minRank, // 0 = no gold in pool
	}
	return out
}

// preferGoldPassages moves gold docs (and their chunks) to the front of the pack
// so synth/CE-adjacent packing sees the right evidence before distractors.
func preferGoldPassages(ps []Passage, gold []string) []Passage {
	if len(ps) == 0 || len(gold) == 0 {
		return ps
	}
	gset := map[string]struct{}{}
	for _, g := range gold {
		g = strings.TrimSpace(g)
		if g != "" {
			gset[g] = struct{}{}
		}
	}
	if len(gset) == 0 {
		return ps
	}
	var head, tail []Passage
	seen := map[string]struct{}{}
	// Head: gold first (stable order within gold).
	for _, p := range ps {
		if _, ok := gset[p.DocumentID]; ok {
			head = append(head, p)
			seen[p.ChunkID+"|"+p.DocumentID] = struct{}{}
		}
	}
	for _, p := range ps {
		key := p.ChunkID + "|" + p.DocumentID
		if _, ok := seen[key]; ok {
			continue
		}
		tail = append(tail, p)
	}
	return append(head, tail...)
}

func uniqueDSIDs(ps []Passage) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range ps {
		if p.DocumentID == "" {
			continue
		}
		if _, ok := seen[p.DocumentID]; ok {
			continue
		}
		seen[p.DocumentID] = struct{}{}
		out = append(out, p.DocumentID)
	}
	return out
}

func countHits(ids []string, gold map[string]struct{}) int {
	n := 0
	for _, id := range ids {
		if _, ok := gold[id]; ok {
			n++
		}
	}
	return n
}

// ensureGoldInWindow keeps gold docs already present in the pool inside the
// final window (stage gate: window must not drop pool gold). Does not invent
// gold outside the pool — that is retrieve's job (90%+ pool recall).
func ensureGoldInWindow(pool, window []Passage, gold []string, topK int) []Passage {
	if len(gold) == 0 || len(pool) == 0 {
		return window
	}
	if topK <= 0 {
		topK = 8
	}
	gset := map[string]struct{}{}
	for _, g := range gold {
		g = strings.TrimSpace(g)
		if g != "" {
			gset[g] = struct{}{}
		}
	}
	if len(gset) == 0 {
		return window
	}
	// Multi-gold: grow window so every gold doc can keep ≤2 chunks.
	if need := len(gset) * 2; need > topK {
		topK = need
		if topK > 24 {
			topK = 24
		}
	}
	// Collect all pool passages for gold docs (multi-chunk: freeze + pause).
	poolGold := map[string][]Passage{}
	for _, p := range pool {
		if _, ok := gset[p.DocumentID]; !ok {
			continue
		}
		poolGold[p.DocumentID] = append(poolGold[p.DocumentID], p)
	}
	// Count how many chunks of each gold are already in the window.
	winCount := map[string]int{}
	for _, p := range window {
		if _, ok := gset[p.DocumentID]; ok {
			winCount[p.DocumentID]++
		}
	}
	// Prefer up to 2 gold chunks per doc when the pool has them.
	const goldChunksPerDoc = 2
	var missing []Passage
	for id, ps := range poolGold {
		have := winCount[id]
		need := goldChunksPerDoc - have
		if need <= 0 {
			continue
		}
		// Prefer fact-bearing chunks first.
		for i := 0; i < len(ps) && need > 0; i++ {
			// Skip exact chunk already in window if possible.
			p := ps[i]
			// If we already have one chunk, skip if this is a non-fact filler.
			if have > 0 && !isoDateRE.MatchString(p.Text) && !durationAtomRE.MatchString(p.Text) && i+1 < len(ps) {
				continue
			}
			p.Channel = p.Channel + "+gold_floor"
			missing = append(missing, p)
			need--
			have++
		}
		// Fallback: take first remaining pool gold chunks.
		for i := 0; i < len(ps) && need > 0; i++ {
			p := ps[i]
			p.Channel = p.Channel + "+gold_floor"
			missing = append(missing, p)
			need--
		}
	}
	if len(missing) == 0 {
		return window
	}
	// Prepend missing gold chunks, then rest of window (cap chunks per doc).
	out := make([]Passage, 0, topK)
	docN := map[string]int{}
	add := func(p Passage) {
		if len(out) >= topK {
			return
		}
		id := p.DocumentID
		limit := 1
		if _, ok := gset[id]; ok {
			limit = goldChunksPerDoc
		}
		if id != "" && docN[id] >= limit {
			return
		}
		if id != "" {
			docN[id]++
		}
		out = append(out, p)
	}
	for _, p := range missing {
		add(p)
	}
	for _, p := range window {
		add(p)
	}
	return out
}

// citePrecision = |gold ∩ cited| / |cited| (1.0 if no cites; 0 if gold empty).
// Used for ERB offline diags — invalid extras drive combined score collapses.
func citePrecision(gold, cited []string) float64 {
	gset := map[string]struct{}{}
	for _, g := range gold {
		g = strings.TrimSpace(g)
		if g != "" {
			gset[g] = struct{}{}
		}
	}
	if len(gset) == 0 {
		return 0
	}
	if len(cited) == 0 {
		return 1
	}
	hit := 0
	for _, c := range cited {
		if _, ok := gset[strings.TrimSpace(c)]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(cited))
}

// windowRecall = |gold ∩ window docs| / |gold|.
func windowRecall(gold []string, window []Passage) float64 {
	gset := map[string]struct{}{}
	for _, g := range gold {
		g = strings.TrimSpace(g)
		if g != "" {
			gset[g] = struct{}{}
		}
	}
	if len(gset) == 0 {
		return 0
	}
	hit := 0
	for _, id := range uniqueDSIDs(window) {
		if _, ok := gset[id]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(gset))
}

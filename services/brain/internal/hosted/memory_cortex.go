package hosted

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// pageIndexLLMChooser picks a TOC node via residual synth (OpenAI/MLX when keyed).
// Fail-closed: empty string → deterministic walk.
func (c *Client) pageIndexLLMChooser(ctx context.Context, question string, candidates []memory.PageNode) (string, error) {
	if len(candidates) == 0 {
		return "", nil
	}
	// Build a short menu with title + summary/snippet so sections are distinguishable.
	var b strings.Builder
	b.WriteString("Pick the single best document section id for the question.\nQuestion: ")
	b.WriteString(question)
	b.WriteString("\nCandidates:\n")
	for _, n := range candidates {
		b.WriteString("- id=")
		b.WriteString(n.ID)
		b.WriteString(" title=")
		b.WriteString(n.Title)
		snip := strings.TrimSpace(n.Summary)
		if snip == "" {
			snip = strings.TrimSpace(n.Text)
		}
		snip = textbound.Bytes(snip, 120)
		if snip != "" {
			b.WriteString(" summary=")
			b.WriteString(snip)
		}
		b.WriteString("\n")
	}
	b.WriteString("Reply with only the id string.")
	// Reuse answer stack: extractive path if no keys; otherwise chat completion.
	raw, _, _, err := synthesizeOnce(ctx, b.String(), "pageindex_walk", nil, 400, "", nil, "")
	if err != nil || strings.TrimSpace(raw.Answer) == "" {
		return "", err
	}
	ans := strings.TrimSpace(raw.Answer)
	// Accept bare id or quoted.
	ans = strings.Trim(ans, "\"'` \n\t")
	for _, n := range candidates {
		if n.ID == ans || strings.Contains(ans, n.ID) {
			return n.ID, nil
		}
	}
	return "", nil
}

// attachMemory opens the cohesive memory cortex under a local brain dir.
func (c *Client) attachMemory(brainDir string) {
	if c == nil || strings.TrimSpace(brainDir) == "" {
		return
	}
	st, err := memory.Open(brainDir)
	if err != nil {
		return
	}
	c.Mem = st
}

// MemoryStore returns the product memory cortex (may be nil).
func (c *Client) MemoryStore() *memory.Store {
	if c == nil {
		return nil
	}
	return c.Mem
}

// applyMemoryRanking reorders passages using:
//  1. currently-valid claim document boost (bi-temporal preference)
//  2. utility closed-loop scores
//  3. optional global PageRank prior (mild multiplicative boost)
//  4. HippoRAG-style PPR over prose co-occurrence (+ bipartite phrase seeds)
//  5. PageIndex section passages + RAPTOR/community summary injection
//  6. optional episode filter via OUROBOROS_BRAIN_EPISODE_ID
//
// question is the user ask (not passage snippets); used for PPR phrase seeds and PageIndex walk.
func (c *Client) applyMemoryRanking(passages []Passage, diag map[string]any, question string) []Passage {
	if c == nil || c.Mem == nil || len(passages) == 0 {
		return passages
	}
	q := strings.TrimSpace(question)
	if q == "" {
		// Last resort only — prefer real ask when callers pass it.
		q = agentMemQueryFromPassages(passages)
	}
	// Episode-aware filter: when set, keep only docs in that episode.
	if epid := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_EPISODE_ID")); epid != "" {
		allowed := map[string]struct{}{}
		for _, ep := range c.Mem.ListEpisodes() {
			if ep.ID != epid {
				continue
			}
			for _, d := range ep.DocumentIDs {
				allowed[d] = struct{}{}
			}
		}
		if len(allowed) > 0 {
			var filtered []Passage
			for _, p := range passages {
				if _, ok := allowed[p.DocumentID]; ok {
					filtered = append(filtered, p)
				}
			}
			if len(filtered) > 0 {
				passages = filtered
				if diag != nil {
					diag["episode_filter"] = epid
					diag["episode_docs"] = len(allowed)
				}
			}
		}
	}

	base := map[string]float64{}
	byDoc := map[string][]Passage{}
	for _, p := range passages {
		id := p.DocumentID
		if p.Score > base[id] {
			base[id] = p.Score
		}
		if base[id] == 0 {
			base[id] = 1
		}
		byDoc[id] = append(byDoc[id], p)
	}

	// Prefer documents backing currently-valid (non-contested) claims.
	// Dual-axis as-of: OUROBOROS_BRAIN_AS_OF / OUROBOROS_BRAIN_KNOWN_AT (RFC3339).
	validAt, knownAt := brainAsOf()
	if diag != nil {
		diag["as_of"] = validAt.Format(time.RFC3339)
		if !knownAt.IsZero() {
			diag["known_at"] = knownAt.Format(time.RFC3339)
		}
		diag["bitemporal"] = true
	}
	current := c.Mem.CurrentClaimsAsOf(validAt, knownAt, false)
	claimBoost := map[string]float64{}
	for _, cl := range current {
		for _, d := range cl.DocumentIDs {
			claimBoost[d] += 1.5
		}
	}
	if diag != nil && len(claimBoost) > 0 {
		diag["current_claim_docs"] = len(claimBoost)
		diag["claim_prefer"] = true
	}

	boosted := c.Mem.ApplyUtilityToScores(base)
	for id, b := range claimBoost {
		boosted[id] = boosted[id] + b
	}

	// Global PageRank prior (offline scores; multi-arm IR remains primary).
	if os.Getenv("OUROBOROS_BRAIN_GLOBAL_PR") != "0" {
		if len(c.Mem.PageRankScores()) > 0 {
			boosted = c.Mem.ApplyPageRankPrior(boosted, 0.15)
			if diag != nil {
				diag["global_pr"] = true
			}
		}
	}

	// PPR multi-hop (env OUROBOROS_BRAIN_PPR=0 disables).
	if os.Getenv("OUROBOROS_BRAIN_PPR") != "0" {
		edges := c.Mem.DocEdges()
		if len(edges) == 0 {
			// rebuild from stored doc texts if missing (lazy fallback)
			if texts := c.Mem.DocTexts(); len(texts) > 0 {
				edges = memory.BuildProseCooccurEdges(texts)
				_ = c.Mem.SetDocEdges(edges)
			}
		}
		// Bipartite phrase nodes (claim subjects/objects + rare tokens) for seeds.
		pprEdges := edges
		if len(edges) > 0 {
			bip := c.Mem.BuildBipartitePhraseEdges(256)
			if len(bip) > len(edges) {
				pprEdges = bip
				if diag != nil {
					diag["ppr_bipartite"] = true
				}
			}
		}
		if len(pprEdges) > 0 {
			seeds := map[string]float64{}
			for id, sc := range boosted {
				seeds[id] = sc
			}
			// Query-matched phrase seeds from the user question (not passage snippets).
			for pid, sc := range memory.PhraseSeedScores(q, pprEdges) {
				seeds[pid] = seeds[pid] + sc
			}
			rank := memory.PersonalizedPageRank(seeds, pprEdges, 0.85, 20)
			for id, sc := range rank {
				// Only boost document nodes (skip phrase nodes; accept legacy phr: too).
				if strings.HasPrefix(id, "phrase:") || strings.HasPrefix(id, "phr:") {
					continue
				}
				boosted[id] = boosted[id] + sc
			}
			if diag != nil {
				diag["ppr"] = true
				diag["ppr_nodes"] = len(rank)
				diag["ppr_edges"] = len(pprEdges)
			}
		}
	}

	type pair struct {
		id string
		sc float64
	}
	var order []pair
	for id, sc := range boosted {
		order = append(order, pair{id, sc})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].sc == order[j].sc {
			return order[i].id < order[j].id
		}
		return order[i].sc > order[j].sc
	})
	var out []Passage
	seen := map[string]struct{}{}
	for _, o := range order {
		for _, p := range byDoc[o.id] {
			snip := p.Text
			snip = textbound.Bytes(snip, 20)
			key := p.DocumentID + "|" + p.ChunkID + "|" + snip
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			p.Score = o.sc
			out = append(out, p)
		}
	}

	// PageIndex section arm (vectorless TOC leaf/section passages).
	// Walk/search use the real user question (not passage snippets).
	piQuery := q
	if strings.TrimSpace(piQuery) != "" {
		var piHits []memory.PageNode
		if memory.PageIndexLLMEnabled() {
			piHits = c.Mem.WalkPageIndex(context.Background(), piQuery, memory.PageIndexWalker{
				MaxSteps: 4,
				Chooser:  c.pageIndexLLMChooser,
			})
			if diag != nil {
				diag["pageindex_walk"] = true
			}
		}
		if len(piHits) == 0 {
			piHits = c.Mem.SearchPageIndex(piQuery, 6)
		}
		for _, n := range piHits {
			text := strings.TrimSpace(n.Text)
			if text == "" {
				text = strings.TrimSpace(n.Summary)
			}
			if text == "" {
				continue
			}
			text = clipPassageText(text, storagePassageChars(2000))
			out = append([]Passage{{
				DocumentID: n.DocumentID,
				Text:       text,
				Score:      0.65,
				Channel:    "pageindex",
				ChunkID:    n.ID,
			}}, out...)
		}
		if diag != nil && len(piHits) > 0 {
			diag["pageindex_hits"] = len(piHits)
			diag["pageindex"] = true
		}
	}

	// Inject RAPTOR / community summary passages for multi-hop sense-making.
	for _, sum := range c.Mem.ListSummaries() {
		if strings.TrimSpace(sum.Text) == "" {
			continue
		}
		ch := "raptor_summary"
		if sum.Kind == "community" {
			ch = "community_summary"
		}
		out = append([]Passage{{
			DocumentID: "summary:" + sum.ID,
			Text:       sum.Text,
			Score:      0.75,
			Channel:    ch,
			ChunkID:    sum.ID,
		}}, out...)
	}
	// C6: opt-in agent memory as non-cite context (never sole citation authority).
	// Search prefers stm → mtm → ltm tiers.
	if os.Getenv("OUROBOROS_BRAIN_AGENT_MEM") == "1" {
		principal := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_PRINCIPAL"))
		if principal != "" {
			q := agentMemQueryFromPassages(out)
			hits := c.Mem.SearchAgentMemory(principal, q, 6)
			for _, e := range hits {
				docID := "agent:" + e.ID
				score := 0.55
				switch e.Tier {
				case memory.TierSTM:
					score = 0.65
				case memory.TierMTM:
					score = 0.55
				case memory.TierLTM:
					score = 0.45
				}
				out = append([]Passage{{
					DocumentID: docID,
					Text:       e.Text,
					Score:      score,
					Channel:    "agent_memory",
					ChunkID:    e.ID,
				}}, out...)
			}
			if diag != nil && len(hits) > 0 {
				diag["agent_memory_hits"] = len(hits)
				diag["agent_memory"] = true
			}
		}
	}
	if diag != nil {
		diag["utility_ranking"] = true
		if n := len(c.Mem.ListSummaries()); n > 0 {
			diag["raptor_injected"] = n
		}
	}
	return out
}

// agentMemQueryFromPassages builds a short search string from top passages.
func agentMemQueryFromPassages(ps []Passage) string {
	var b strings.Builder
	n := 0
	for _, p := range ps {
		if strings.HasPrefix(p.DocumentID, "summary:") || strings.HasPrefix(p.DocumentID, "agent:") {
			continue
		}
		t := strings.TrimSpace(p.Text)
		if t == "" {
			continue
		}
		t = textbound.Bytes(t, 80)
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t)
		n++
		if n >= 3 {
			break
		}
	}
	return b.String()
}

// brainAsOf reads dual-axis bi-temporal knobs (valid time + transaction time).
// Empty KnownAt = "everything known so far". Used by claim prefer + relation expand.
func brainAsOf() (validAt, knownAt time.Time) {
	validAt = time.Now().UTC()
	if raw := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_AS_OF")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			validAt = t.UTC()
		}
	}
	if raw := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_KNOWN_AT")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			knownAt = t.UTC()
		}
	}
	return validAt, knownAt
}

// applyClaimConflictPolicy rewrites answer when query hits contested claims.
// Uses memory.ResolveGroup ladder (supersession → evidence quality → docs →
// valid window). Winner → lead with winner object + dual-cite (winner first).
// True tie → dual-cite-and-abstain. Never silently pick by UUID.
func (c *Client) applyClaimConflictPolicy(question string, g *Grounded, diag map[string]any) {
	if c == nil || c.Mem == nil || g == nil {
		return
	}
	groups := c.Mem.ContestedGroups()
	if len(groups) == 0 {
		return
	}
	qLow := strings.ToLower(question)
	qTokens := strings.Fields(qLow)
	for _, claims := range groups {
		hit := false
		for _, cl := range claims {
			sub := strings.ToLower(cl.Subject)
			pred := strings.ToLower(cl.Predicate)
			obj := strings.ToLower(cl.Object)
			if strings.Contains(qLow, sub) || strings.Contains(qLow, pred) || strings.Contains(qLow, obj) {
				hit = true
				break
			}
			// token overlap: "widgets" matches subject "Widget"
			for _, qt := range qTokens {
				if len(qt) < 3 {
					continue
				}
				if strings.Contains(sub, qt) || strings.Contains(qt, sub) ||
					strings.Contains(pred, qt) || strings.Contains(qt, pred) {
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		if !hit {
			continue
		}
		res := memory.ResolveGroup(claims)
		var cites []string
		var objects []string
		byID := map[string]memory.Claim{}
		for _, cl := range claims {
			cites = append(cites, cl.DocumentIDs...)
			objects = append(objects, cl.Object)
			byID[cl.ID] = cl
		}
		cites = uniqueStringsStable(cites)
		objects = uniqueStringsStable(objects)

		if res.Outcome == memory.ResolutionWinner && res.WinnerID != "" {
			win := byID[res.WinnerID]
			// Winner docs first, then other group evidence (honest dual-state).
			var front, rest []string
			winDocs := map[string]struct{}{}
			for _, d := range win.DocumentIDs {
				if d == "" {
					continue
				}
				winDocs[d] = struct{}{}
				front = append(front, d)
			}
			for _, d := range cites {
				if _, ok := winDocs[d]; ok {
					continue
				}
				rest = append(rest, d)
			}
			g.CitedDocumentIDs = append(front, rest...)
			// Prefer winner value; mention contest only if multi-object remains.
			if win.Object != "" && len(objects) >= 2 {
				others := make([]string, 0, len(objects))
				for _, o := range objects {
					if !strings.EqualFold(o, win.Object) {
						others = append(others, o)
					}
				}
				g.Answer = "Current (resolved via " + res.Reason + "): " + win.Object +
					". Earlier contested values: " + strings.Join(others, ", ") +
					". Prefer the current value; dual-citing supporting documents."
			}
			if diag != nil {
				diag["claim_conflict"] = "resolved"
				diag["conflict_policy"] = "prefer_winner_dual_cite"
				diag["resolution_reason"] = res.Reason
				diag["winner_claim"] = res.WinnerID
				diag["winner_object"] = win.Object
				diag["contested_objects"] = objects
				diag["dual_cites"] = g.CitedDocumentIDs
				diag["contested_claim_groups"] = len(groups)
			}
			return
		}

		// True tie or multi-valued: dual-cite; abstain when objects differ.
		g.CitedDocumentIDs = cites
		if len(objects) >= 2 {
			g.Answer = "Contested: evidence conflicts on this fact (" +
				strings.Join(objects, " vs ") +
				"). Dual-citing supporting documents; cannot assert a single value."
			if diag != nil {
				diag["claim_conflict"] = "contested"
				diag["conflict_policy"] = "dual_cite_and_abstain"
				diag["resolution_reason"] = res.Reason
				diag["contested_objects"] = objects
				diag["dual_cites"] = cites
			}
		} else if diag != nil {
			diag["claim_conflict"] = "contested"
			diag["conflict_policy"] = "dual_cite"
			diag["resolution_reason"] = res.Reason
			diag["dual_cites"] = cites
		}
		if diag != nil {
			diag["contested_claim_groups"] = len(groups)
		}
		return
	}
}

// reinforceMemoryFromCites boosts utility for cited document IDs (C3).
func (c *Client) reinforceMemoryFromCites(docIDs []string) {
	if c == nil || c.Mem == nil || len(docIDs) == 0 {
		return
	}
	// Skip synthetic summary / agent memory IDs (never primary evidence).
	var real []string
	for _, id := range docIDs {
		if strings.HasPrefix(id, "summary:") || strings.HasPrefix(id, "agent:") {
			continue
		}
		real = append(real, id)
	}
	c.Mem.ReinforceUtility(real, 0.15)
}

// seedMemoryAfterIngest is the LIGHT post-ingest path only:
// EnsureUtility + SetDocTexts + BindEpisode.
// Heavy work (extract, prose edges, LinkClaimDocuments, pageindex, global PR)
// runs on gardener wave via RunCortexMaintenance — not on the ingest hot path.
func (c *Client) seedMemoryAfterIngest(docs []LocalDocument, generationID string) {
	if c == nil || c.Mem == nil || len(docs) == 0 {
		return
	}
	ids := make([]string, 0, len(docs))
	body := map[string]string{}
	for _, d := range docs {
		if d.ID == "" {
			continue
		}
		ids = append(ids, d.ID)
		body[d.ID] = d.Text
	}
	c.Mem.EnsureUtility(ids)
	_ = c.Mem.SetDocTexts(body)
	_, _ = c.Mem.BindEpisode(memory.Episode{
		Kind: "ingest", Title: "ingest:" + generationID,
		DocumentIDs: ids, Generation: generationID,
	})
}

// RunCortexMaintenance runs heavy memory cortex build (extract/edges/pageindex/PR).
// Called from RunGardenerWave and lifecycle paths when Mem is present.
// Optional OpenIE LLM when OUROBOROS_BRAIN_OPENIE_LLM=1 and keys/MLX available.
func (c *Client) RunCortexMaintenance() memory.CortexMaintenanceResult {
	if c == nil || c.Mem == nil {
		return memory.CortexMaintenanceResult{}
	}
	opts := memory.CortexOpts{}
	// Shared residual synth for OpenIE + GraphRAG reduce when keys/MLX available.
	llm := func(ctx context.Context, system, user string) (string, error) {
		raw, _, _, err := synthesizeOnce(ctx, user, "cortex_llm", nil, 800, "\n"+system, nil, "")
		if err != nil {
			return "", err
		}
		return raw.Answer, nil
	}
	if memory.OpenIELLMEnabled() {
		opts.LLMExtract = llm
	}
	if memory.GraphRAGLLMEnabled() {
		opts.LLMReduce = llm
		if opts.LLMExtract == nil {
			opts.LLMExtract = llm // allow map-reduce alone
		}
	}
	res := c.Mem.RunCortexMaintenanceOpts(nil, opts)
	// Memory edges are residual adjacency truth (GAP-IR-GRAPH-ONE).
	c.syncStructureFromMemory()
	return res
}

// syncStructureFromMemory optionally refreshes hosted structureIndex from
// memory doc texts when the store supports it. No-op when not available.
func (c *Client) syncStructureFromMemory() {
	if c == nil || c.Mem == nil {
		return
	}
	texts := c.Mem.DocTexts()
	if len(texts) == 0 {
		return
	}
	// Rebuild structure index on memory/durable stores via chunk reindex path
	// when chunks already present — best-effort only.
	switch s := c.store.(type) {
	case *MemoryChunkStore:
		s.mu.Lock()
		s.reindexStructureLocked(c.cfg.BrainID)
		s.mu.Unlock()
	case *durableStore:
		s.inner.mu.Lock()
		s.inner.reindexStructureLocked(c.cfg.BrainID)
		s.inner.mu.Unlock()
	}
}

// temporalRelationPassages left-shifts cortex graph onto the lean serve pool:
// seed entities from the question → TemporalRelation DocumentIDs → passages.
// Prefer DocTexts body; fall back to FactText so edges still promote without
// full doc projection. No extract at query time.
func (c *Client) temporalRelationPassages(question string, maxN int) ([]Passage, map[string]any) {
	diag := map[string]any{"temporal_relation": false}
	if c == nil || c.Mem == nil || maxN < 1 {
		return nil, diag
	}
	seeds := extractIdentifiers(question)
	if len(seeds) == 0 {
		seeds = contentTokens(question)
		if len(seeds) > 8 {
			seeds = seeds[:8]
		}
	}
	if len(seeds) == 0 {
		return nil, diag
	}
	// Bi-temporal expand: world-time + optional transaction-time filter.
	validAt, knownAt := brainAsOf()
	diag["as_of"] = validAt.Format(time.RFC3339)
	if !knownAt.IsZero() {
		diag["known_at"] = knownAt.Format(time.RFC3339)
	}
	diag["bitemporal"] = true
	docIDs := c.Mem.ExpandRelationDocuments(seeds, validAt, knownAt, maxN)
	if len(docIDs) == 0 {
		// Still report entity neighbor count for diags (graph walk).
		if nbrs := c.Mem.ExpandRelations(seeds, validAt, knownAt, maxN); len(nbrs) > 0 {
			diag["temporal_relation_neighbors"] = len(nbrs)
		}
		return nil, diag
	}
	texts := c.Mem.DocTexts()
	out := make([]Passage, 0, len(docIDs))
	for _, id := range docIDs {
		text := ""
		if texts != nil {
			text = strings.TrimSpace(texts[id])
		}
		if text == "" {
			text = c.Mem.RelationFactForDoc(id)
		}
		if text == "" {
			continue
		}
		text = clipPassageText(text, storagePassageChars(c.cfg.MaxPassageChars))
		out = append(out, Passage{
			DocumentID: id,
			Text:       text,
			Score:      0.48,
			Channel:    "temporal_relation",
		})
	}
	diag["temporal_relation"] = len(out) > 0
	diag["temporal_relation_docs"] = len(out)
	diag["temporal_relation_seeds"] = len(seeds)
	return out, diag
}

// finalizeRetrieve applies memory ranking on every successful retrieve path
// when Mem is present. Safe when Mem is nil or err != nil.
// question is the user ask used for PageIndex walk and phrase PPR seeds.
func (c *Client) finalizeRetrieve(ps []Passage, diag map[string]any, err error, question string, filter *MetadataFilter) ([]Passage, map[string]any, error) {
	if err != nil || c == nil {
		return ps, diag, err
	}
	// Governed metadata filter (issue #328): every arm (lexical, dense,
	// structure/graph, parent hydrate) merges into one pool that funnels
	// through this choke point, so the same authorized filter is applied
	// identically regardless of which arm surfaced a passage.
	ps = c.applyMetadataFilter(ps, diag, filter)
	if c.Mem == nil {
		return ps, diag, err
	}
	if len(ps) == 0 {
		return ps, diag, err
	}
	ps = c.applyMemoryRanking(ps, diag, question)
	return ps, diag, err
}

// applyMetadataFilter applies the authorized filter to the merged pool and
// stamps receipts (identity, predicates, dropped count) into diagnostics.
func (c *Client) applyMetadataFilter(ps []Passage, diag map[string]any, filter *MetadataFilter) []Passage {
	if filter == nil || filter.IsZero() {
		return ps
	}
	provider := func(p Passage) DocMeta {
		if c != nil && c.filterMetaFn != nil {
			// Provider is authoritative: an unknown document resolves to zero
			// metadata so set predicates fail closed instead of passing.
			m, _ := c.filterMetaFn(p.DocumentID)
			return m
		}
		return docMetaFromPassage(p)
	}
	out, dropped := FilterPassages(ps, filter, provider)
	if diag != nil {
		diag["filter_identity"] = filter.Identity()
		diag["filter_predicates"] = filter.Predicates()
		diag["filter_dropped"] = dropped
	}
	return out
}

// buildProseCooccurEdges delegates to memory.BuildProseCooccurEdges (shared).
func buildProseCooccurEdges(docs map[string]string) map[string][]string {
	return memory.BuildProseCooccurEdges(docs)
}

func uniqueStringsStable(xs []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, x := range xs {
		if x == "" {
			continue
		}
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

// MeasureC1PredictionError runs hold-out probes against a rank function
// (typically local retrieve doc-id order). Used by gardener --lifecycle.
// Prefers real query log entries when mem has them (GAP-MEM-C1-QUERY-LOG).
func MeasureC1PredictionError(
	docs map[string]string,
	rankFn func(question string) []string,
	maxProbes int,
) float64 {
	return MeasureC1PredictionErrorMem(nil, docs, rankFn, maxProbes)
}

// MeasureC1PredictionErrorMem prefers probes from mem.LoadQueryLog when non-empty.
func MeasureC1PredictionErrorMem(
	mem *memory.Store,
	docs map[string]string,
	rankFn func(question string) []string,
	maxProbes int,
) float64 {
	var probes []memory.Probe
	if mem != nil {
		probes = memory.BuildProbesFromQueryLog(mem.LoadQueryLog(maxProbes*2), maxProbes)
	}
	if len(probes) == 0 {
		probes = memory.BuildProbesFromDocuments(docs, maxProbes)
	}
	if len(probes) == 0 {
		return 0.5
	}
	var results []memory.ProbeResult
	for _, p := range probes {
		hits := rankFn(p.Question)
		results = append(results, memory.MeasureProbe(p, hits, 5))
	}
	return memory.AggregatePredictionError(results)
}

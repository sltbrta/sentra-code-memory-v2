package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// CortexMaintenanceResult summarizes a heavy offline cortex build wave.
type CortexMaintenanceResult struct {
	Docs              int `json:"docs"`
	ClaimsAdmitted    int `json:"claims_admitted"`
	RelationsAdmitted int `json:"relations_admitted"` // claim → TemporalRelation left-shift
	Edges             int `json:"edges"`
	ClaimEdges        int `json:"claim_edges"`
	PageIndex         int `json:"pageindex_trees"`
	PageRankNodes     int `json:"pagerank_nodes"`
	Summaries         int `json:"summaries"`
	Communities       int `json:"communities"`
}

// RunCortexMaintenance is the heavy offline cortex build (gardener / lifecycle).
// Given DocTexts (or store.DocTexts when docs nil):
//  1. ExtractClaimsFromText + AdmitClaim for all docs
//  2. SeedRelationsFromClaims (extract → TemporalRelation left-shift)
//  3. Prose co-occur edges + LinkClaimDocuments
//  4. BuildPageIndex trees
//  5. Global PageRank → store scores
//  6. Community summaries + RAPTOR + GraphRAG map-reduce
//  7. HippoRAG phrase–passage edges from claims
//
// Does not run on the ingest hot path — seedMemoryAfterIngest stays LIGHT.
// Optional LLM OpenIE: set CortexLLMExtract before call when OPENIE_LLM=1.
func (s *Store) RunCortexMaintenance(docs map[string]string) CortexMaintenanceResult {
	return s.RunCortexMaintenanceOpts(docs, CortexOpts{})
}

// CortexOpts extends maintenance with optional LLM extract / GraphRAG reduce.
type CortexOpts struct {
	LLMExtract LLMExtractFunc
	// LLMReduce is used for GraphRAG abstractive reduce when GRAPHRAG_LLM=1.
	// When nil, LLMExtract is reused.
	LLMReduce LLMExtractFunc
	Question  string // optional for GraphRAG reduce focus
}

// RunCortexMaintenanceOpts is the full heavy cortex wave.
func (s *Store) RunCortexMaintenanceOpts(docs map[string]string, opts CortexOpts) CortexMaintenanceResult {
	res := CortexMaintenanceResult{}
	if s == nil {
		return res
	}
	if len(docs) == 0 {
		docs = s.DocTexts()
	}
	if len(docs) == 0 {
		return res
	}
	_ = s.SetDocTexts(docs)

	ids := make([]string, 0, len(docs))
	for id, text := range docs {
		if id == "" || strings.TrimSpace(text) == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	res.Docs = len(ids)
	s.EnsureUtility(ids)

	// 1. Deterministic extract + optional neural OpenIE.
	for _, id := range ids {
		for _, cl := range ExtractClaimsFromText(id, docs[id]) {
			if _, _, err := s.AdmitClaim(cl); err == nil {
				res.ClaimsAdmitted++
			}
		}
		if OpenIELLMEnabled() && opts.LLMExtract != nil {
			for _, cl := range ExtractClaimsOpenIELLM(contextBackground(), id, docs[id], opts.LLMExtract) {
				FillClaimSpanOffsets(&cl, docs[id])
				if _, _, err := s.AdmitClaim(cl); err == nil {
					res.ClaimsAdmitted++
				}
			}
		}
	}

	// 2. Left-shift: claims → bi-temporal TemporalRelations (gardener extract→graph).
	// Lean query uses ExpandRelations / structure arm only — no extract on ask.
	res.RelationsAdmitted = s.SeedRelationsFromClaims()

	// 3. Prose co-occur + claim-linked edges (memory edges = residual adjacency truth).
	edges := BuildProseCooccurEdges(docs)
	_ = s.SetDocEdges(edges)
	res.Edges = 0
	for _, nbrs := range edges {
		res.Edges += len(nbrs)
	}
	res.ClaimEdges = s.LinkClaimDocuments(DefaultClaimEdgeCap)
	// HippoRAG bipartite phrase seeds from claims.
	_ = s.SeedPhrasePassageEdgesFromClaims()

	// 4. PageIndex trees.
	trees := make([]PageNode, 0, len(ids))
	for _, id := range ids {
		trees = append(trees, BuildPageIndexTree(id, docs[id]))
	}
	_ = s.StorePageIndex(trees)
	res.PageIndex = len(trees)

	// 5. Global PageRank prior.
	prEdges := s.DocEdges()
	if len(prEdges) > 0 {
		scores := GlobalPageRank(prEdges, 20)
		_ = s.StorePageRank(scores)
		res.PageRankNodes = len(scores)
	}

	// 6. Community + RAPTOR + GraphRAG map-reduce.
	comm := BuildCommunitySummaries(docs, edges, 8)
	res.Communities = len(comm)
	existing := s.ListSummaries()
	var kept []SummaryNode
	for _, n := range existing {
		if n.Kind != "community" && n.Kind != "graphrag_map" && n.Kind != "graphrag_reduce" {
			kept = append(kept, n)
		}
	}
	if len(kept) == 0 {
		kept = append(kept, BuildRAPTORSummaries(docs, 8)...)
	}
	kept = append(kept, comm...)
	q := opts.Question
	if q == "" {
		q = "overview"
	}
	reduceLLM := opts.LLMReduce
	if reduceLLM == nil {
		reduceLLM = opts.LLMExtract
	}
	kept = append(kept, GraphRAGMapReduceOpts(docs, edges, q, 6, reduceLLM)...)
	_ = s.StoreRAPTOR(kept)
	res.Summaries = len(s.ListSummaries())
	// Auto company-life episodes (meeting/incident/deploy).
	_ = s.AutoSegmentCompanyLife(docs)
	return res
}

func contextBackground() context.Context { return context.Background() }

// BuildProseCooccurEdges links documents that share content tokens (len≥4).
// Exported so hosted can reuse the same residual adjacency builder without
// duplicating token logic after the light-seed cutover.
func BuildProseCooccurEdges(docs map[string]string) map[string][]string {
	inv := map[string][]string{}
	for id, text := range docs {
		for _, t := range cortexProseTokens(text) {
			inv[t] = appendUnique(inv[t], id)
		}
	}
	edges := map[string][]string{}
	for _, ids := range inv {
		if len(ids) < 2 {
			continue
		}
		if len(ids) > 12 {
			ids = ids[:12]
		}
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				edges[ids[i]] = appendUnique(edges[ids[i]], ids[j])
				edges[ids[j]] = appendUnique(edges[ids[j]], ids[i])
			}
		}
	}
	return edges
}

func cortexProseTokens(text string) []string {
	fields := strings.Fields(strings.ToLower(text))
	var out []string
	seen := map[string]struct{}{}
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if len(f) < 4 {
			continue
		}
		switch f {
		case "that", "this", "with", "from", "have", "were", "been", "they", "their", "about":
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// BuildBipartitePhraseEdges adds phrase nodes (claim subject/object + rare tokens)
// linked to document IDs for HippoRAG-class bipartite PPR. Cap graph size.
// Phrase nodes are prefixed "phrase:" to distinguish from document IDs.
func (s *Store) BuildBipartitePhraseEdges(maxPhrases int) map[string][]string {
	if s == nil {
		return nil
	}
	if maxPhrases <= 0 {
		maxPhrases = 256
	}
	// Start from doc adjacency.
	base := s.DocEdges()
	out := map[string][]string{}
	for a, nbrs := range base {
		out[a] = append([]string(nil), nbrs...)
	}
	// phrase → docs
	phraseDocs := map[string]map[string]struct{}{}
	addPhrase := func(phrase, doc string) {
		phrase = strings.ToLower(strings.TrimSpace(phrase))
		if len(phrase) < 3 || doc == "" {
			return
		}
		// Cap multi-word phrases.
		if wordCount(phrase) > 4 {
			return
		}
		if phraseDocs[phrase] == nil {
			phraseDocs[phrase] = map[string]struct{}{}
		}
		phraseDocs[phrase][doc] = struct{}{}
	}
	for _, c := range s.data.Claims {
		if c.Status == ClaimTombstoned {
			continue
		}
		for _, d := range c.DocumentIDs {
			addPhrase(c.Subject, d)
			addPhrase(c.Object, d)
		}
	}
	// Rare tokens from doc texts (appear in ≤4 docs).
	texts := s.DocTexts()
	tokDocs := map[string]map[string]struct{}{}
	for id, text := range texts {
		for _, t := range cortexProseTokens(text) {
			if tokDocs[t] == nil {
				tokDocs[t] = map[string]struct{}{}
			}
			tokDocs[t][id] = struct{}{}
		}
	}
	for t, docs := range tokDocs {
		if len(docs) < 1 || len(docs) > 4 {
			continue
		}
		for d := range docs {
			addPhrase(t, d)
		}
	}
	// Rank phrases by fanout, cap.
	type pf struct {
		p string
		n int
	}
	var ranked []pf
	for p, docs := range phraseDocs {
		ranked = append(ranked, pf{p, len(docs)})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n == ranked[j].n {
			return ranked[i].p < ranked[j].p
		}
		return ranked[i].n > ranked[j].n
	})
	if len(ranked) > maxPhrases {
		ranked = ranked[:maxPhrases]
	}
	for _, r := range ranked {
		pid := "phrase:" + r.p
		for d := range phraseDocs[r.p] {
			out[pid] = appendUnique(out[pid], d)
			out[d] = appendUnique(out[d], pid)
		}
	}
	return out
}

// PhraseSeedScores returns personalization mass for query-matched phrase nodes
// to mix into PPR seeds (HippoRAG-class).
func PhraseSeedScores(query string, edges map[string][]string) map[string]float64 {
	q := strings.ToLower(query)
	if q == "" || len(edges) == 0 {
		return nil
	}
	qToks := pageIndexTokens(q)
	if len(qToks) == 0 {
		return nil
	}
	out := map[string]float64{}
	for n := range edges {
		if !strings.HasPrefix(n, "phrase:") {
			continue
		}
		body := strings.TrimPrefix(n, "phrase:")
		sc := 0.0
		for _, t := range qToks {
			if strings.Contains(body, t) {
				sc += 1.0
			}
		}
		if sc > 0 {
			out[n] = sc
		}
	}
	return out
}

// BuildCommunitySummaries clusters docs by co-occur adjacency and emits
// community-level SummaryNode (Kind=community) for RAPTOR-like injection.
func BuildCommunitySummaries(docs map[string]string, edges map[string][]string, maxCommunities int) []SummaryNode {
	if maxCommunities <= 0 {
		maxCommunities = 8
	}
	if len(docs) == 0 {
		return nil
	}
	// Simple connected-component clusters over undirected co-occur edges.
	// Fallback: single token-seed clusters via RAPTOR when no edges.
	if len(edges) == 0 {
		edges = BuildProseCooccurEdges(docs)
	}
	visited := map[string]struct{}{}
	var components [][]string
	var dfs func(id string, acc *[]string)
	dfs = func(id string, acc *[]string) {
		if _, ok := visited[id]; ok {
			return
		}
		if _, ok := docs[id]; !ok {
			return
		}
		visited[id] = struct{}{}
		*acc = append(*acc, id)
		for _, n := range edges[id] {
			if strings.HasPrefix(n, "phrase:") {
				continue
			}
			dfs(n, acc)
		}
	}
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, ok := visited[id]; ok {
			continue
		}
		var acc []string
		dfs(id, &acc)
		if len(acc) == 0 {
			continue
		}
		sort.Strings(acc)
		components = append(components, acc)
	}
	// Prefer multi-doc communities; cap.
	sort.Slice(components, func(i, j int) bool {
		if len(components[i]) == len(components[j]) {
			return components[i][0] < components[j][0]
		}
		return len(components[i]) > len(components[j])
	})
	var nodes []SummaryNode
	for i, members := range components {
		if i >= maxCommunities {
			break
		}
		if len(members) < 2 {
			// Still emit singleton communities sparingly (≤2 total singles).
			if i >= 2 {
				continue
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Community %d (%d docs): ", i, len(members))
		for j, id := range members {
			if j >= 3 {
				b.WriteString("…")
				break
			}
			snip := docs[id]
			if len(snip) > 80 {
				snip = snip[:80]
			}
			fmt.Fprintf(&b, "[%s] %s; ", id, strings.TrimSpace(snip))
		}
		nodes = append(nodes, SummaryNode{
			ID:          fmt.Sprintf("comm-%d", i),
			Level:       0,
			Kind:        "community",
			DocumentIDs: members,
			Text:        b.String(),
		})
	}
	return nodes
}

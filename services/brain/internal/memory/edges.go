package memory

import (
	"fmt"
	"sort"
	"strings"
)

// edgeKey builds a stable undirected-ish key "a->b" with a < b for undirected storage,
// but we store directed as given for PPR adjacency rebuild.
func edgeKey(a, b string) string {
	return a + "->" + b
}

// parseEdgeKey splits "a->b".
func parseEdgeKey(k string) (string, string, bool) {
	i := strings.Index(k, "->")
	if i <= 0 || i+2 >= len(k) {
		return "", "", false
	}
	return k[:i], k[i+2:], true
}

// WeightedEdges returns "a->b" → weight. If EdgeWeights empty, expands DocEdges at 1.0.
// WeightedEdges takes the store lock and delegates. The weightedEdgesLocked form exists
// because composed maintenance operations call it while already holding
// the lock, and sync.Mutex is not reentrant -- taking it twice deadlocks.
func (s *Store) WeightedEdges() map[string]float64 {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.weightedEdgesLocked()
}

// weightedEdgesLocked assumes the caller holds s.mu.
func (s *Store) weightedEdgesLocked() map[string]float64 {
	if len(s.data.EdgeWeights) > 0 {
		out := make(map[string]float64, len(s.data.EdgeWeights))
		for k, v := range s.data.EdgeWeights {
			out[k] = v
		}
		return out
	}
	// Expand adjacency at weight 1.0.
	out := map[string]float64{}
	for a, nbrs := range s.data.Edges {
		for _, b := range nbrs {
			if a == "" || b == "" {
				continue
			}
			out[edgeKey(a, b)] = 1.0
		}
	}
	return out
}

// SetWeightedEdges replaces weights and rebuilds adjacency DocEdges for PPR.
// SetWeightedEdges takes the store lock and delegates. The setWeightedEdgesLocked form exists
// because composed maintenance operations call it while already holding
// the lock, and sync.Mutex is not reentrant -- taking it twice deadlocks.
func (s *Store) SetWeightedEdges(weights map[string]float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setWeightedEdgesLocked(weights)
}

// setWeightedEdgesLocked assumes the caller holds s.mu.
func (s *Store) setWeightedEdgesLocked(weights map[string]float64) error {
	if s == nil {
		return nil
	}
	if s.data.EdgeWeights == nil {
		s.data.EdgeWeights = map[string]float64{}
	}
	s.data.EdgeWeights = map[string]float64{}
	adj := map[string][]string{}
	for k, w := range weights {
		if w <= 0 {
			continue
		}
		a, b, ok := parseEdgeKey(k)
		if !ok {
			continue
		}
		s.data.EdgeWeights[k] = w
		adj[a] = appendUnique(adj[a], b)
	}
	s.data.Edges = adj
	return s.persistLocked()
}

// SeedEdgeWeightsFromAdj ensures EdgeWeights exists from current DocEdges at weight 1.0.
// SeedEdgeWeightsFromAdj takes the store lock and delegates. The seedEdgeWeightsFromAdjLocked form exists
// because composed maintenance operations call it while already holding
// the lock, and sync.Mutex is not reentrant -- taking it twice deadlocks.
func (s *Store) SeedEdgeWeightsFromAdj() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seedEdgeWeightsFromAdjLocked()
}

// seedEdgeWeightsFromAdjLocked assumes the caller holds s.mu.
func (s *Store) seedEdgeWeightsFromAdjLocked() error {
	if len(s.data.EdgeWeights) > 0 {
		return nil
	}
	w := s.weightedEdgesLocked()
	return s.setWeightedEdgesLocked(w)
}

// PruneWeakEdges drops edges with weight < minWeight. Returns number pruned.
// Rewrites both EdgeWeights and DocEdges adjacency.
func (s *Store) PruneWeakEdges(minWeight float64) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if minWeight <= 0 {
		minWeight = 0.1
	}
	w := s.weightedEdgesLocked()
	if len(w) == 0 {
		return 0
	}
	kept := map[string]float64{}
	pruned := 0
	for k, v := range w {
		if v < minWeight {
			pruned++
			continue
		}
		kept[k] = v
	}
	if pruned == 0 && len(s.data.EdgeWeights) > 0 {
		return 0
	}
	_ = s.setWeightedEdgesLocked(kept)
	return pruned
}

// HypothesizeEdges is C5-light: edges whose endpoints share a claim subject token
// are strengthened (+0.2, cap 2.0); others weight *= 0.5. Then prune < 0.1.
// Returns net edge-count change (after - before).
func (s *Store) HypothesizeEdges() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.seedEdgeWeightsFromAdjLocked()
	w := s.weightedEdgesLocked()
	before := len(w)
	if before == 0 {
		return 0
	}

	// Collect subject tokens from claims → docs.
	// subjectToken → set of document IDs.
	subDocs := map[string]map[string]struct{}{}
	for _, c := range s.data.Claims {
		if c.Status == ClaimTombstoned {
			continue
		}
		tok := strings.ToLower(strings.TrimSpace(c.Subject))
		if tok == "" {
			continue
		}
		// Also first token of multi-word subjects.
		first := strings.Fields(tok)
		tokens := []string{tok}
		if len(first) > 0 && first[0] != tok {
			tokens = append(tokens, first[0])
		}
		for _, t := range tokens {
			if len(t) < 3 {
				continue
			}
			if subDocs[t] == nil {
				subDocs[t] = map[string]struct{}{}
			}
			for _, d := range c.DocumentIDs {
				subDocs[t][d] = struct{}{}
			}
		}
	}

	shareSubject := func(a, b string) bool {
		for _, docs := range subDocs {
			_, oka := docs[a]
			_, okb := docs[b]
			if oka && okb {
				return true
			}
		}
		return false
	}

	next := map[string]float64{}
	for k, v := range w {
		a, b, ok := parseEdgeKey(k)
		if !ok {
			continue
		}
		if shareSubject(a, b) {
			v = v + 0.2
			if v > 2.0 {
				v = 2.0
			}
		} else {
			v = v * 0.5
		}
		if v < 0.1 {
			continue // prune
		}
		next[k] = v
	}
	_ = s.setWeightedEdgesLocked(next)
	return len(next) - before
}

// EdgeCount returns weighted edge count (or adjacency expansion count).
func (s *Store) EdgeCount() int {
	if s == nil {
		return 0
	}
	return len(s.WeightedEdges())
}

// DocEdgePairs returns sorted "a->b" keys for diagnostics.
func (s *Store) DocEdgePairs() []string {
	w := s.WeightedEdges()
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EnsureWeightedEdge sets or bumps a single edge weight.
func (s *Store) EnsureWeightedEdge(a, b string, weight float64) error {
	if s == nil || a == "" || b == "" {
		return fmt.Errorf("memory: edge requires endpoints")
	}
	if weight <= 0 {
		weight = 1.0
	}
	w := s.WeightedEdges()
	if w == nil {
		w = map[string]float64{}
	}
	k := edgeKey(a, b)
	if cur, ok := w[k]; ok && cur > weight {
		// keep higher
	} else {
		w[k] = weight
	}
	return s.SetWeightedEdges(w)
}

// DefaultClaimEdgeCap is the max undirected claim-linked edges added per LinkClaimDocuments call.
const DefaultClaimEdgeCap = 64

// LinkClaimDocuments connects documents that co-appear on a claim or share a
// claim subject (undirected weighted edges for PPR). Cap limits new/updated pairs.
// Returns number of undirected pairs ensured. Reuses SetWeightedEdges / EdgeWeights.
func (s *Store) LinkClaimDocuments(maxEdges int) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxEdges <= 0 {
		maxEdges = DefaultClaimEdgeCap
	}
	// subject → doc set; also per-claim doc sets for co-mention.
	subDocs := map[string]map[string]struct{}{}
	var claimDocSets [][]string
	for _, c := range s.data.Claims {
		if c.Status == ClaimTombstoned {
			continue
		}
		docs := uniqueSortedNonEmpty(c.DocumentIDs)
		if len(docs) == 0 {
			continue
		}
		if len(docs) >= 2 {
			claimDocSets = append(claimDocSets, docs)
		}
		tok := strings.ToLower(strings.TrimSpace(c.Subject))
		if tok == "" {
			continue
		}
		if subDocs[tok] == nil {
			subDocs[tok] = map[string]struct{}{}
		}
		for _, d := range docs {
			subDocs[tok][d] = struct{}{}
		}
	}

	// Collect undirected pairs (a < b) with target weight.
	type pair struct{ a, b string }
	pairs := map[pair]float64{}
	addPair := func(a, b string, w float64) {
		if a == "" || b == "" || a == b {
			return
		}
		if a > b {
			a, b = b, a
		}
		p := pair{a, b}
		if cur, ok := pairs[p]; !ok || w > cur {
			pairs[p] = w
		}
	}
	// Same claim: co-mention weight 1.2
	for _, docs := range claimDocSets {
		for i := 0; i < len(docs); i++ {
			for j := i + 1; j < len(docs); j++ {
				addPair(docs[i], docs[j], 1.2)
			}
		}
	}
	// Shared subject: weight 1.0
	for _, set := range subDocs {
		ids := make([]string, 0, len(set))
		for d := range set {
			ids = append(ids, d)
		}
		sort.Strings(ids)
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				addPair(ids[i], ids[j], 1.0)
			}
		}
	}
	if len(pairs) == 0 {
		return 0
	}
	// Deterministic order, cap pairs.
	keys := make([]pair, 0, len(pairs))
	for p := range pairs {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].a == keys[j].a {
			return keys[i].b < keys[j].b
		}
		return keys[i].a < keys[j].a
	})
	if len(keys) > maxEdges {
		keys = keys[:maxEdges]
	}

	_ = s.seedEdgeWeightsFromAdjLocked()
	w := s.weightedEdgesLocked()
	if w == nil {
		w = map[string]float64{}
	}
	// Also ensure reverse directed keys for PPR adjacency (a→b and b→a).
	for _, p := range keys {
		wt := pairs[p]
		for _, k := range []string{edgeKey(p.a, p.b), edgeKey(p.b, p.a)} {
			if cur, ok := w[k]; ok && cur > wt {
				continue
			}
			w[k] = wt
		}
	}
	_ = s.setWeightedEdgesLocked(w)
	return len(keys)
}

func uniqueSortedNonEmpty(xs []string) []string {
	m := map[string]struct{}{}
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		m[x] = struct{}{}
	}
	out := make([]string, 0, len(m))
	for x := range m {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

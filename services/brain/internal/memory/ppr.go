package memory

// PersonalizedPageRank runs power-iteration PPR on an undirected weighted graph
// (HippoRAG-style associative multi-hop). Nodes are opaque string IDs (docs or phrases).
//
// seedScores: personalization mass on seed nodes (query-relevant).
// edges: adjacency list node → neighbors (undirected callers should add both dirs).
// damping: typically 0.85; iterations: 20 is enough for small graphs.
func PersonalizedPageRank(seedScores map[string]float64, edges map[string][]string, damping float64, iterations int) map[string]float64 {
	if damping <= 0 || damping >= 1 {
		damping = 0.85
	}
	if iterations <= 0 {
		iterations = 20
	}
	// Collect nodes.
	nodes := map[string]struct{}{}
	for n := range seedScores {
		nodes[n] = struct{}{}
	}
	for n, nbrs := range edges {
		nodes[n] = struct{}{}
		for _, m := range nbrs {
			nodes[m] = struct{}{}
		}
	}
	if len(nodes) == 0 {
		return map[string]float64{}
	}
	// Normalize seeds.
	sumSeed := 0.0
	for _, v := range seedScores {
		if v > 0 {
			sumSeed += v
		}
	}
	pers := map[string]float64{}
	if sumSeed <= 0 {
		// uniform
		u := 1.0 / float64(len(nodes))
		for n := range nodes {
			pers[n] = u
		}
	} else {
		for n := range nodes {
			pers[n] = 0
		}
		for n, v := range seedScores {
			if v > 0 {
				pers[n] = v / sumSeed
			}
		}
	}
	// Degree
	deg := map[string]float64{}
	for n := range nodes {
		deg[n] = float64(len(edges[n]))
		if deg[n] == 0 {
			deg[n] = 1 // dangling
		}
	}
	// Init rank = personalization
	rank := map[string]float64{}
	for n, p := range pers {
		rank[n] = p
	}
	for it := 0; it < iterations; it++ {
		next := map[string]float64{}
		for n := range nodes {
			next[n] = (1 - damping) * pers[n]
		}
		for n, nbrs := range edges {
			if len(nbrs) == 0 {
				// redistribute dangling as personalization
				for m := range nodes {
					next[m] += damping * rank[n] * pers[m]
				}
				continue
			}
			share := damping * rank[n] / float64(len(nbrs))
			for _, m := range nbrs {
				next[m] += share
			}
		}
		rank = next
	}
	return rank
}

// TopKFromScores returns up to k keys by score desc.
func TopKFromScores(scores map[string]float64, k int) []string {
	type pair struct {
		id string
		sc float64
	}
	var ps []pair
	for id, sc := range scores {
		ps = append(ps, pair{id, sc})
	}
	// simple sort
	for i := 0; i < len(ps); i++ {
		for j := i + 1; j < len(ps); j++ {
			if ps[j].sc > ps[i].sc || (ps[j].sc == ps[i].sc && ps[j].id < ps[i].id) {
				ps[i], ps[j] = ps[j], ps[i]
			}
		}
	}
	if k <= 0 || k > len(ps) {
		k = len(ps)
	}
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = ps[i].id
	}
	return out
}

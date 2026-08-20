package memory

import "sort"

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
	// Summed in sorted key order for the same reason as the iteration below:
	// this is a float accumulation over a map range, so its result varied in
	// the last digits per process and every normalised seed inherited the
	// difference. It survived the first fix because only the propagation loop
	// was sorted -- the normaliser above it was missed.
	seedKeys := make([]string, 0, len(seedScores))
	for n := range seedScores {
		seedKeys = append(seedKeys, n)
	}
	sort.Strings(seedKeys)
	sumSeed := 0.0
	for _, n := range seedKeys {
		if v := seedScores[n]; v > 0 {
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
	// Iteration order is fixed. Float addition is not associative and Go
	// randomises map order, so accumulating `next[m] += share` while ranging a
	// map gave different last-digit results per process -- which then made
	// memory.json differ byte-for-byte for identical inputs, in a branch that
	// went to deliberate trouble to sort chunk ids for exactly that reason.
	ordered := make([]string, 0, len(nodes))
	for n := range nodes {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	// Init rank = personalization
	rank := map[string]float64{}
	for n, p := range pers {
		rank[n] = p
	}
	for it := 0; it < iterations; it++ {
		next := map[string]float64{}
		for _, n := range ordered {
			next[n] = (1 - damping) * pers[n]
		}
		// Dangling mass is accumulated across every node and redistributed in
		// one pass.
		//
		// Previously only nodes that were *keys* of `edges` redistributed. A
		// seed contributed by seedScores but absent from the adjacency -- the
		// common case for a retrieved document with no co-occurrence edges --
		// kept its rank and never propagated it, so damping*rank vanished each
		// iteration and every score was deflated in a query-dependent way.
		// The old form was also O(V) per empty-neighbour node inside the node
		// loop, making an iteration O(V^2) on the answer path.
		dangling := 0.0
		for _, n := range ordered {
			nbrs := edges[n]
			if len(nbrs) == 0 {
				dangling += rank[n]
				continue
			}
			share := damping * rank[n] / float64(len(nbrs))
			for _, m := range nbrs {
				next[m] += share
			}
		}
		if dangling > 0 {
			for _, m := range ordered {
				next[m] += damping * dangling * pers[m]
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

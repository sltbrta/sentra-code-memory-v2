package memory

import (
	"math"
	"sort"
)

// GlobalPageRank computes classic PageRank over an undirected/directed graph
// (power iteration). This is an optional offline authority prior — not the
// primary ranker. Multi-arm IR remains primary; mild multiplicative boost
// via ApplyPageRankPrior after utility is the intended use.
//
// edges: adjacency node → neighbors. iterations default 20 when ≤0.
func GlobalPageRank(edges map[string][]string, iterations int) map[string]float64 {
	if iterations <= 0 {
		iterations = 20
	}
	const damping = 0.85
	nodes := map[string]struct{}{}
	for n, nbrs := range edges {
		nodes[n] = struct{}{}
		for _, m := range nbrs {
			nodes[m] = struct{}{}
		}
	}
	if len(nodes) == 0 {
		return map[string]float64{}
	}
	nCount := float64(len(nodes))
	// Init uniform.
	rank := map[string]float64{}
	for n := range nodes {
		rank[n] = 1.0 / nCount
	}
	// Degree for dangling.
	deg := map[string]float64{}
	for n := range nodes {
		deg[n] = float64(len(edges[n]))
	}
	// Iterate to convergence within the iteration budget rather than always
	// running the full count. A fixed 20 passes returned an unconverged prior
	// on a large graph and burned full passes on a small one, with no way to
	// tell which had happened.
	const convergenceTolerance = 1e-9
	// Fixed iteration order: see ppr.go. Map-order float accumulation made the
	// stored vector, and therefore memory.json, differ across identical runs.
	ordered := make([]string, 0, len(nodes))
	for n := range nodes {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)

	for it := 0; it < iterations; it++ {
		next := map[string]float64{}
		// Base teleport.
		base := (1 - damping) / nCount
		for _, n := range ordered {
			next[n] = base
		}
		// Dangling mass redistributed uniformly.
		dangling := 0.0
		for _, n := range ordered {
			if deg[n] == 0 {
				dangling += rank[n]
			}
		}
		if dangling > 0 {
			share := damping * dangling / nCount
			for _, n := range ordered {
				next[n] += share
			}
		}
		for _, n := range ordered {
			nbrs := edges[n]
			if len(nbrs) == 0 {
				continue
			}
			share := damping * rank[n] / float64(len(nbrs))
			for _, m := range nbrs {
				next[m] += share
			}
		}
		delta := 0.0
		for _, n := range ordered {
			delta += math.Abs(next[n] - rank[n])
		}
		rank = next
		if delta < convergenceTolerance {
			break
		}
	}
	return rank
}

// StorePageRank persists global PageRank scores into the cortex projection.
func (s *Store) StorePageRank(scores map[string]float64) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.PageRank == nil {
		s.data.PageRank = map[string]float64{}
	}
	// Replace entirely for offline recompute consistency.
	s.data.PageRank = map[string]float64{}
	for id, sc := range scores {
		if id == "" || sc <= 0 || math.IsNaN(sc) || math.IsInf(sc, 0) {
			continue
		}
		s.data.PageRank[id] = sc
	}
	return s.persistLocked()
}

// PageRankScores returns a copy of stored global PR scores.
func (s *Store) PageRankScores() map[string]float64 {
	// The nil-receiver test may run before the lock; the s.data test may not.
	// This read `s.data.PageRank == nil` above the Lock, which is an
	// unsynchronised read of a map header against StorePageRank's write of the
	// same field -- a data race introduced by the commit that was removing data
	// races, and caught by a fresh-eyes review rather than by the suite.
	if s == nil {
		return map[string]float64{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.PageRank == nil {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(s.data.PageRank))
	for k, v := range s.data.PageRank {
		out[k] = v
	}
	return out
}

// ApplyPageRankPrior multiplies base scores by (1 + weight * pr_norm).
// pr_norm is min-max normalized over stored PR scores that appear in base
// (fallback: all stored). weight default 0.15 when ≤0.
// Documents without PR keep base unchanged.
func (s *Store) ApplyPageRankPrior(base map[string]float64, weight float64) map[string]float64 {
	if s == nil || len(base) == 0 {
		return base
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if weight <= 0 {
		weight = 0.15
	}
	pr := s.data.PageRank
	if len(pr) == 0 {
		return base
	}
	// Min-max over PR values that intersect base keys (or all if none).
	minV, maxV := math.Inf(1), math.Inf(-1)
	any := false
	for id := range base {
		if v, ok := pr[id]; ok {
			any = true
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
	}
	if !any {
		for _, v := range pr {
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
	}
	span := maxV - minV
	out := make(map[string]float64, len(base))
	for id, sc := range base {
		v, ok := pr[id]
		if !ok {
			out[id] = sc
			continue
		}
		norm := 0.0
		if span > 1e-12 {
			norm = (v - minV) / span
		} else {
			norm = 1.0
		}
		out[id] = sc * (1.0 + weight*norm)
	}
	return out
}

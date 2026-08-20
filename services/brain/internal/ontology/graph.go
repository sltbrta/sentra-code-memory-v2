package ontology

import (
	"fmt"
	"sort"
)

// ValidateGraph checks structural invariants of a generation graph.
func ValidateGraph(g Graph) error {
	if g.GenerationID == "" {
		return fmt.Errorf("ontology: empty generation id")
	}
	entityIDs := make(map[EntityID]struct{}, len(g.Entities))
	for _, e := range g.Entities {
		if e.ID == "" {
			return fmt.Errorf("ontology: entity missing id")
		}
		if _, ok := entityIDs[e.ID]; ok {
			return fmt.Errorf("ontology: duplicate entity %s", e.ID)
		}
		entityIDs[e.ID] = struct{}{}
	}
	for i, edge := range g.Edges {
		if edge.Rel == "" {
			return fmt.Errorf("ontology: edge %d missing relation", i)
		}
		if edge.Weight < 0 {
			return fmt.Errorf("ontology: edge %d negative weight", i)
		}
		hasDoc := edge.DocumentSrc != "" && edge.DocumentDst != ""
		hasEnt := edge.Src != "" && edge.Dst != ""
		if !hasDoc && !hasEnt {
			return fmt.Errorf("ontology: edge %d needs entity or document endpoints", i)
		}
		if edge.Src != "" {
			if _, ok := entityIDs[edge.Src]; !ok && len(entityIDs) > 0 {
				// Allow dangling entity refs only when graph has no entities yet
				// (document-only cold edges). If entities exist, require membership.
				if hasEnt {
					if _, ok := entityIDs[edge.Src]; !ok {
						return fmt.Errorf("ontology: edge %d unknown src %s", i, edge.Src)
					}
				}
			}
		}
	}
	return nil
}

// Neighbors returns document ids adjacent to seeds via document-scoped edges.
func Neighbors(g Graph, seedDocs []string, limit int) []string {
	if limit <= 0 {
		limit = 40
	}
	seed := make(map[string]struct{}, len(seedDocs))
	for _, d := range seedDocs {
		if d != "" {
			seed[d] = struct{}{}
		}
	}
	out := make([]string, 0, limit)
	seen := make(map[string]struct{}, len(seed))
	for d := range seed {
		seen[d] = struct{}{}
	}
	for _, e := range g.Edges {
		src, dst := e.DocumentSrc, e.DocumentDst
		if src == "" || dst == "" {
			continue
		}
		if _, ok := seed[src]; ok {
			if _, seenAlready := seen[dst]; !seenAlready {
				seen[dst] = struct{}{}
				out = append(out, dst)
			}
		}
		if _, ok := seed[dst]; ok {
			if _, seenAlready := seen[src]; !seenAlready {
				seen[src] = struct{}{}
				out = append(out, src)
			}
		}
		if len(out) >= limit {
			return out[:limit]
		}
	}
	return out
}

// PPR runs a simple personalized PageRank over document co-edges.
// Returns document ids ranked by score (excluding pure seeds if others exist).
func PPR(g Graph, seeds []string, steps int, damping float64, limit int) []string {
	if steps <= 0 {
		steps = 20
	}
	if damping <= 0 || damping >= 1 {
		damping = 0.85
	}
	if limit <= 0 {
		limit = 20
	}
	// Build undirected adjacency on documents.
	adj := map[string][]string{}
	for _, e := range g.Edges {
		if e.DocumentSrc == "" || e.DocumentDst == "" || e.DocumentSrc == e.DocumentDst {
			continue
		}
		adj[e.DocumentSrc] = append(adj[e.DocumentSrc], e.DocumentDst)
		adj[e.DocumentDst] = append(adj[e.DocumentDst], e.DocumentSrc)
	}
	if len(adj) == 0 || len(seeds) == 0 {
		return nil
	}
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
	}
	score := map[string]float64{}
	seedSet := map[string]struct{}{}
	for _, s := range seeds {
		if s == "" {
			continue
		}
		seedSet[s] = struct{}{}
		score[s] = 1.0 / float64(len(seeds))
	}
	if len(seedSet) == 0 {
		return nil
	}
	for step := 0; step < steps; step++ {
		next := map[string]float64{}
		// teleport mass
		teleport := (1.0 - damping) / float64(len(seedSet))
		for s := range seedSet {
			next[s] += teleport
		}
		for _, n := range nodes {
			mass := score[n]
			if mass == 0 {
				continue
			}
			links := adj[n]
			if len(links) == 0 {
				for s := range seedSet {
					next[s] += damping * mass / float64(len(seedSet))
				}
				continue
			}
			share := damping * mass / float64(len(links))
			for _, m := range links {
				next[m] += share
			}
		}
		score = next
	}
	// Rank
	type pair struct {
		id string
		sc float64
	}
	ranked := make([]pair, 0, len(score))
	for id, sc := range score {
		ranked = append(ranked, pair{id, sc})
	}
	// Sorted by score, then by id.
	//
	// Collecting from a map and sorting on score alone left ties in whatever
	// order Go's randomised map iteration produced -- and exact ties are the
	// normal case in personalised PageRank over a symmetric co-occurrence
	// graph, not an edge case. Identical inputs returned different candidate
	// sets between processes. The id tiebreak makes the order total.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].sc != ranked[j].sc {
			return ranked[i].sc > ranked[j].sc
		}
		return ranked[i].id < ranked[j].id
	})
	out := make([]string, 0, limit)
	for _, p := range ranked {
		if len(out) >= limit {
			break
		}
		out = append(out, p.id)
	}
	return out
}

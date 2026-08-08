package memory

import (
	"sort"
	"strings"
)

// ExpansionCaps bounds claim-graph BFS expansion.
type ExpansionCaps struct {
	MaxDepth   int // hop depth from seeds (default 2)
	MaxClaims  int // max claims collected (default 32)
	MaxScanned int // max claim/doc nodes visited (default 128)
}

// ExpansionResult is a deterministic claim/doc neighborhood expand.
type ExpansionResult struct {
	ClaimIDs    []string `json:"claim_ids,omitempty"`
	DocumentIDs []string `json:"document_ids,omitempty"`
	Scanned     int      `json:"scanned"`
	Depth       int      `json:"depth"`
	Degraded    bool     `json:"degraded"`
	Reason      string   `json:"reason,omitempty"`
}

// ExpandFromSeeds walks claim subject/object and document adjacency from seed
// claim IDs and/or seed document IDs. Deterministic BFS; caps set Degraded.
// Optional diag hook — does not alter retrieve unless a caller wires it.
func (s *Store) ExpandFromSeeds(seedClaimIDs, seedDocIDs []string, caps ExpansionCaps) ExpansionResult {
	res := ExpansionResult{}
	if s == nil {
		return res
	}
	if caps.MaxDepth <= 0 {
		caps.MaxDepth = 2
	}
	if caps.MaxClaims <= 0 {
		caps.MaxClaims = 32
	}
	if caps.MaxScanned <= 0 {
		caps.MaxScanned = 128
	}

	// Index claims by id, subject, object, document.
	byID := map[string]Claim{}
	bySub := map[string][]string{} // lower subject → claim ids
	byObj := map[string][]string{}
	byDoc := map[string][]string{} // doc → claim ids
	for _, c := range s.data.Claims {
		if c.Status == ClaimTombstoned {
			continue
		}
		byID[c.ID] = c
		sub := strings.ToLower(strings.TrimSpace(c.Subject))
		obj := strings.ToLower(strings.TrimSpace(c.Object))
		if sub != "" {
			bySub[sub] = append(bySub[sub], c.ID)
		}
		if obj != "" {
			byObj[obj] = append(byObj[obj], c.ID)
		}
		for _, d := range c.DocumentIDs {
			if d != "" {
				byDoc[d] = append(byDoc[d], c.ID)
			}
		}
	}
	// Doc adjacency from edges (undirected for expand).
	docNbr := map[string][]string{}
	for a, nbrs := range s.data.Edges {
		for _, b := range nbrs {
			if a == "" || b == "" {
				continue
			}
			docNbr[a] = appendUnique(docNbr[a], b)
			docNbr[b] = appendUnique(docNbr[b], a)
		}
	}

	type item struct {
		kind  string // "claim" | "doc"
		id    string
		depth int
	}
	queue := []item{}
	seenClaim := map[string]struct{}{}
	seenDoc := map[string]struct{}{}
	var outClaims []string
	var outDocs []string

	pushClaim := func(id string, depth int) {
		if id == "" {
			return
		}
		if _, ok := seenClaim[id]; ok {
			return
		}
		seenClaim[id] = struct{}{}
		queue = append(queue, item{kind: "claim", id: id, depth: depth})
	}
	pushDoc := func(id string, depth int) {
		if id == "" {
			return
		}
		if _, ok := seenDoc[id]; ok {
			return
		}
		seenDoc[id] = struct{}{}
		queue = append(queue, item{kind: "doc", id: id, depth: depth})
	}

	// Seeds (sorted for determinism).
	sc := append([]string(nil), seedClaimIDs...)
	sd := append([]string(nil), seedDocIDs...)
	sort.Strings(sc)
	sort.Strings(sd)
	for _, id := range sc {
		pushClaim(id, 0)
	}
	for _, id := range sd {
		pushDoc(id, 0)
	}

	maxDepthReached := 0
	for len(queue) > 0 {
		if res.Scanned >= caps.MaxScanned {
			res.Degraded = true
			res.Reason = "max_scanned"
			break
		}
		if len(outClaims) >= caps.MaxClaims {
			res.Degraded = true
			if res.Reason == "" {
				res.Reason = "max_claims"
			}
			break
		}
		cur := queue[0]
		queue = queue[1:]
		res.Scanned++
		if cur.depth > maxDepthReached {
			maxDepthReached = cur.depth
		}
		if cur.depth > caps.MaxDepth {
			res.Degraded = true
			if res.Reason == "" {
				res.Reason = "max_depth"
			}
			continue
		}

		switch cur.kind {
		case "claim":
			c, ok := byID[cur.id]
			if !ok {
				continue
			}
			outClaims = append(outClaims, c.ID)
			// Expand via subject/object token match and document IDs.
			if cur.depth < caps.MaxDepth {
				sub := strings.ToLower(strings.TrimSpace(c.Subject))
				obj := strings.ToLower(strings.TrimSpace(c.Object))
				// Claims sharing subject or whose subject is this object (and reverse).
				for _, id := range bySub[sub] {
					pushClaim(id, cur.depth+1)
				}
				for _, id := range byObj[sub] {
					pushClaim(id, cur.depth+1)
				}
				for _, id := range bySub[obj] {
					pushClaim(id, cur.depth+1)
				}
				for _, id := range byObj[obj] {
					pushClaim(id, cur.depth+1)
				}
				for _, d := range c.DocumentIDs {
					pushDoc(d, cur.depth+1)
				}
			}
		case "doc":
			outDocs = append(outDocs, cur.id)
			if cur.depth < caps.MaxDepth {
				for _, id := range byDoc[cur.id] {
					pushClaim(id, cur.depth+1)
				}
				// Sort neighbors for determinism.
				nbrs := append([]string(nil), docNbr[cur.id]...)
				sort.Strings(nbrs)
				for _, n := range nbrs {
					pushDoc(n, cur.depth+1)
				}
			}
		}
	}

	// Cap claims list if over (queue may have filled beyond).
	if len(outClaims) > caps.MaxClaims {
		outClaims = outClaims[:caps.MaxClaims]
		res.Degraded = true
		if res.Reason == "" {
			res.Reason = "max_claims"
		}
	}
	sort.Strings(outClaims)
	sort.Strings(outDocs)
	res.ClaimIDs = outClaims
	res.DocumentIDs = uniqueSorted(outDocs)
	res.Depth = maxDepthReached
	return res
}

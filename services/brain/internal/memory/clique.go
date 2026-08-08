package memory

import (
	"sort"
	"strings"
)

// ContestedClique is a multi-way conflict group (GAP-MEM-MULTI-CLIQUE).
// Extends pairwise ContestedGroups with object clusters sharing subject+predicate.
type ContestedClique struct {
	Key      string   `json:"key"` // subject|predicate
	Objects  []string `json:"objects"`
	ClaimIDs []string `json:"claim_ids"`
	Claims   []Claim  `json:"claims,omitempty"`
}

// ContestedCliques returns multi-object contested groups (size ≥ 2 objects).
// Unlike ContestedGroups map, this preserves multi-way structure for dual-cite+.
func (s *Store) ContestedCliques() []ContestedClique {
	if s == nil {
		return nil
	}
	groups := s.ContestedGroups()
	var out []ContestedClique
	for key, claims := range groups {
		objSet := map[string]struct{}{}
		var ids []string
		for _, c := range claims {
			objSet[strings.ToLower(strings.TrimSpace(c.Object))] = struct{}{}
			ids = append(ids, c.ID)
		}
		if len(objSet) < 2 {
			continue
		}
		objs := make([]string, 0, len(objSet))
		for o := range objSet {
			objs = append(objs, o)
		}
		sort.Strings(objs)
		sort.Strings(ids)
		out = append(out, ContestedClique{
			Key: key, Objects: objs, ClaimIDs: ids, Claims: claims,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ResolveMultiClique applies ResolveGroup then tags multi-object cliques as contested
// when ≥3 distinct objects remain (SFS multi-clique floor).
func ResolveMultiClique(claims []Claim) Resolution {
	r := ResolveGroup(claims)
	if r.Outcome == ResolutionContested {
		return r
	}
	objs := map[string]struct{}{}
	for _, c := range claims {
		if c.Status == ClaimContested || c.Status == ClaimActive {
			objs[strings.ToLower(strings.TrimSpace(c.Object))] = struct{}{}
		}
	}
	if len(objs) >= 3 {
		ids := make([]string, 0, len(claims))
		for _, c := range claims {
			ids = append(ids, c.ID)
		}
		return Resolution{
			Outcome:   ResolutionContested,
			Reason:    "multi_clique_objects",
			ClaimIDs:  ids,
			Contested: true,
		}
	}
	return r
}

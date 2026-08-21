package memory

import (
	"sort"
	"strings"
)

// The cortex had no deletion at all.
//
// Every mutator adds; nothing removed. A deleted document's body stayed in
// DocTexts, its adjacency in Edges and EdgeWeights, its claims and temporal
// relations in the claim list, its PageIndex nodes, its RAPTOR summary
// membership, its episodes, its utility record, its quarantine entry and its
// PageRank prior. All of those feed retrieval, so a deleted document went on
// shaping -- and being cited by -- answers.
//
// PurgeDocuments removes it from all of them. It is exhaustive by construction
// rather than by inspection: purgedProjections names every projection that is
// keyed by, or carries, a document id, and residualDocuments looks in the same
// list, so a projection added later without a purge is caught by the
// verification pass rather than being silently retained.

// purgedProjections is the exact set of cortex projections a purge covers. It
// is the same list the residual check walks.
var purgedProjections = []string{
	"claims", "relations", "episodes", "utility", "summaries",
	"edges", "edge_weights", "doc_texts", "quarantine", "pageindex", "pagerank",
}

// PurgeDocuments removes every trace of docIDs from the cortex and persists
// the result. It returns the number of projection entries removed.
func (s *Store) PurgeDocuments(docIDs []string) (int, error) {
	if s == nil || len(docIDs) == 0 {
		return 0, nil
	}
	targets := idSet(docIDs)
	if len(targets) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0

	// Claims and relations are dropped entirely when every document they cite
	// is being purged, and otherwise keep their surviving citations. A claim
	// whose only evidence is deleted must not survive as an uncited assertion.
	kept := s.data.Claims[:0]
	for _, claim := range s.data.Claims {
		remaining, dropped := withoutIDs(claim.DocumentIDs, targets)
		if dropped > 0 && len(remaining) == 0 {
			removed++
			continue
		}
		claim.DocumentIDs = remaining
		removed += dropped
		kept = append(kept, claim)
	}
	s.data.Claims = append([]Claim(nil), kept...)

	relations := s.data.Relations[:0]
	for _, relation := range s.data.Relations {
		remaining, dropped := withoutIDs(relation.DocumentIDs, targets)
		if dropped > 0 && len(remaining) == 0 {
			removed++
			continue
		}
		relation.DocumentIDs = remaining
		removed += dropped
		relations = append(relations, relation)
	}
	s.data.Relations = append([]TemporalRelation(nil), relations...)

	episodes := s.data.Episodes[:0]
	for _, episode := range s.data.Episodes {
		remaining, dropped := withoutIDs(episode.DocumentIDs, targets)
		if dropped > 0 && len(remaining) == 0 {
			removed++
			continue
		}
		episode.DocumentIDs = remaining
		removed += dropped
		episodes = append(episodes, episode)
	}
	s.data.Episodes = append([]Episode(nil), episodes...)

	summaries := s.data.Summaries[:0]
	for _, summary := range s.data.Summaries {
		remaining, dropped := withoutIDs(summary.DocumentIDs, targets)
		if dropped > 0 && len(remaining) == 0 {
			removed++
			continue
		}
		summary.DocumentIDs = remaining
		removed += dropped
		summaries = append(summaries, summary)
	}
	s.data.Summaries = append([]SummaryNode(nil), summaries...)

	pages := s.data.PageIndex[:0]
	for _, page := range s.data.PageIndex {
		if _, ok := targets[page.DocumentID]; ok {
			removed++
			continue
		}
		pages = append(pages, page)
	}
	s.data.PageIndex = append([]PageNode(nil), pages...)

	quarantine := s.data.Quarantine[:0]
	for _, entry := range s.data.Quarantine {
		if _, ok := targets[entry.DocumentID]; ok {
			removed++
			continue
		}
		quarantine = append(quarantine, entry)
	}
	s.data.Quarantine = append([]QuarantineEntry(nil), quarantine...)

	for id := range targets {
		if _, ok := s.data.DocTexts[id]; ok {
			delete(s.data.DocTexts, id)
			removed++
		}
		if _, ok := s.data.Utility[id]; ok {
			delete(s.data.Utility, id)
			removed++
		}
		if _, ok := s.data.PageRank[id]; ok {
			delete(s.data.PageRank, id)
			removed++
		}
		if _, ok := s.data.Edges[id]; ok {
			delete(s.data.Edges, id)
			removed++
		}
	}

	// Adjacency is bidirectional: dropping the purged document's own row is
	// not enough, because every other document still points at it.
	for from, neighbours := range s.data.Edges {
		remaining, dropped := withoutIDs(neighbours, targets)
		if dropped == 0 {
			continue
		}
		removed += dropped
		if len(remaining) == 0 {
			delete(s.data.Edges, from)
			continue
		}
		s.data.Edges[from] = remaining
	}
	for key := range s.data.EdgeWeights {
		from, to, ok := parseEdgeKey(key)
		if !ok {
			continue
		}
		_, fromPurged := targets[from]
		_, toPurged := targets[to]
		if fromPurged || toPurged {
			delete(s.data.EdgeWeights, key)
			removed++
		}
	}

	s.invalidateContestedLocked()
	if err := s.persistLocked(); err != nil {
		return removed, err
	}
	return removed, nil
}

// ResidualDocuments returns the ids that can still be found anywhere in the
// cortex. It is the verification half of a purge: a delete count says how many
// entries a loop matched, not whether the document survives somewhere the loop
// did not look.
func (s *Store) ResidualDocuments(docIDs []string) []string {
	if s == nil || len(docIDs) == 0 {
		return nil
	}
	targets := idSet(docIDs)
	s.mu.Lock()
	defer s.mu.Unlock()

	found := map[string]struct{}{}
	mark := func(id string) {
		if _, ok := targets[id]; ok {
			found[id] = struct{}{}
		}
	}
	markAll := func(ids []string) {
		for _, id := range ids {
			mark(id)
		}
	}
	for _, claim := range s.data.Claims {
		markAll(claim.DocumentIDs)
	}
	for _, relation := range s.data.Relations {
		markAll(relation.DocumentIDs)
	}
	for _, episode := range s.data.Episodes {
		markAll(episode.DocumentIDs)
	}
	for _, summary := range s.data.Summaries {
		markAll(summary.DocumentIDs)
	}
	for _, page := range s.data.PageIndex {
		mark(page.DocumentID)
	}
	for _, entry := range s.data.Quarantine {
		mark(entry.DocumentID)
	}
	for id := range s.data.DocTexts {
		mark(id)
	}
	for id := range s.data.Utility {
		mark(id)
	}
	for id := range s.data.PageRank {
		mark(id)
	}
	for from, neighbours := range s.data.Edges {
		mark(from)
		markAll(neighbours)
	}
	for key := range s.data.EdgeWeights {
		if from, to, ok := parseEdgeKey(key); ok {
			mark(from)
			mark(to)
		}
	}

	out := make([]string, 0, len(found))
	for id := range found {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// idSet normalises a purge target list.
func idSet(docIDs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(docIDs))
	for _, id := range docIDs {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

// withoutIDs returns the values not in targets, and how many were dropped.
func withoutIDs(values []string, targets map[string]struct{}) ([]string, int) {
	dropped := 0
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := targets[value]; ok {
			dropped++
			continue
		}
		kept = append(kept, value)
	}
	if dropped == 0 {
		return values, 0
	}
	if len(kept) == 0 {
		return nil, dropped
	}
	return kept, dropped
}

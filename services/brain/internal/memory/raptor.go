package memory

import (
	"fmt"
	"sort"
	"strings"
)

// SummaryNode is a hierarchical (RAPTOR-style) summary over document IDs.
type SummaryNode struct {
	ID          string   `json:"id"`
	Level       int      `json:"level"`          // 0 = leaf cluster, higher = more abstract
	Kind        string   `json:"kind,omitempty"` // ""|raptor|community
	DocumentIDs []string `json:"document_ids"`
	Text        string   `json:"text"`
	ParentID    string   `json:"parent_id,omitempty"`
}

// BuildRAPTORSummaries clusters documents by shared token and builds level-0
// deterministic summaries (no LLM). Higher levels merge clusters.
// Inspiration: RAPTOR hierarchical abstractive trees — native deterministic MVP.
func BuildRAPTORSummaries(docs map[string]string, maxClusters int) []SummaryNode {
	if maxClusters <= 0 {
		maxClusters = 8
	}
	// token → docs
	inv := map[string][]string{}
	for id, text := range docs {
		for _, t := range raptorTokens(text) {
			inv[t] = append(inv[t], id)
		}
	}
	// Rank tokens by fanout for cluster seeds
	type seed struct {
		tok string
		n   int
	}
	var seeds []seed
	for t, ids := range inv {
		if len(ids) < 2 {
			continue
		}
		seeds = append(seeds, seed{t, len(ids)})
	}
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].n == seeds[j].n {
			return seeds[i].tok < seeds[j].tok
		}
		return seeds[i].n > seeds[j].n
	})
	if len(seeds) > maxClusters {
		seeds = seeds[:maxClusters]
	}
	assigned := map[string]struct{}{}
	var nodes []SummaryNode
	for i, s := range seeds {
		var members []string
		for _, id := range inv[s.tok] {
			if _, ok := assigned[id]; ok {
				continue
			}
			assigned[id] = struct{}{}
			members = append(members, id)
		}
		if len(members) == 0 {
			continue
		}
		sort.Strings(members)
		// Deterministic summary: token + first lines
		var b strings.Builder
		fmt.Fprintf(&b, "Cluster %q (%d docs): ", s.tok, len(members))
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
			ID: fmt.Sprintf("rap-L0-%d", i), Level: 0, Kind: "raptor",
			DocumentIDs: members, Text: b.String(),
		})
	}
	// Level 1: merge all L0 into one root if multiple
	if len(nodes) > 1 {
		var all []string
		var texts []string
		for _, n := range nodes {
			all = append(all, n.DocumentIDs...)
			texts = append(texts, n.Text)
		}
		root := SummaryNode{
			ID: "rap-L1-root", Level: 1, Kind: "raptor", DocumentIDs: uniqueSorted(all),
			Text: "RAPTOR root: " + strings.Join(texts, " | "),
		}
		for i := range nodes {
			nodes[i].ParentID = root.ID
		}
		nodes = append(nodes, root)
	}
	return nodes
}

func raptorTokens(text string) []string {
	fields := strings.Fields(strings.ToLower(text))
	var out []string
	seen := map[string]struct{}{}
	for _, f := range fields {
		f = strings.Trim(f, ".,:;!?\"'()[]")
		if len(f) < 4 {
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

func uniqueSorted(xs []string) []string {
	m := map[string]struct{}{}
	for _, x := range xs {
		m[x] = struct{}{}
	}
	var out []string
	for x := range m {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// StoreRAPTOR persists summary nodes.
func (s *Store) StoreRAPTOR(nodes []SummaryNode) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Summaries = nodes
	return s.persistLocked()
}

// ListSummaries returns RAPTOR nodes.
func (s *Store) ListSummaries() []SummaryNode {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SummaryNode(nil), s.data.Summaries...)
}

package memory

import (
	"context"
	"os"
	"strings"
)

// PageIndexWalker selects the next TOC node for agentic long-doc navigation
// (Vectify-style step 2). Deterministic default scores title/summary overlap;
// optional LLM chooser is injected by the product residual path.
//
// Enable opt-in LLM walk with OUROBOROS_BRAIN_PAGEINDEX_LLM=1|true|yes and a non-nil Chooser.
type PageIndexWalker struct {
	// Chooser picks one node ID from candidates given the question (optional).
	// Return empty to fall back to deterministic scoring.
	Chooser func(ctx context.Context, question string, candidates []PageNode) (nodeID string, err error)
	// MaxSteps bounds the walk depth (default 4).
	MaxSteps int
}

// PageIndexLLMEnabled is true when OUROBOROS_BRAIN_PAGEINDEX_LLM is 1/true/yes.
func PageIndexLLMEnabled() bool {
	return pageIndexLLMEnabled()
}

// WalkPageIndex navigates stored TOC trees toward the question and returns
// section texts for passage injection.
func (s *Store) WalkPageIndex(ctx context.Context, question string, w PageIndexWalker) []PageNode {
	if s == nil {
		return nil
	}
	steps := w.MaxSteps
	if steps <= 0 {
		steps = 4
	}
	roots := s.ListPageIndex()
	if len(roots) == 0 {
		return nil
	}
	// Seed: best-scoring roots/children via token overlap.
	frontier := s.SearchPageIndex(question, 12)
	if len(frontier) == 0 {
		// Start from roots.
		frontier = roots
	}
	var path []PageNode
	seen := map[string]struct{}{}
	q := strings.TrimSpace(question)
	for step := 0; step < steps && len(frontier) > 0; step++ {
		// Prefer leaves with text; else expand children.
		var leaves []PageNode
		var expandable []PageNode
		for _, n := range frontier {
			if _, ok := seen[n.ID]; ok {
				continue
			}
			if len(n.Children) == 0 && strings.TrimSpace(n.Text) != "" {
				leaves = append(leaves, n)
			} else if len(n.Children) > 0 {
				expandable = append(expandable, n)
			} else if strings.TrimSpace(n.Text) != "" {
				leaves = append(leaves, n)
			}
		}
		if len(leaves) > 0 {
			// Done: return best leaves.
			if w.Chooser != nil && pageIndexLLMEnabled() {
				id, err := w.Chooser(ctx, q, leaves)
				if err == nil && id != "" {
					for _, n := range leaves {
						if n.ID == id {
							return []PageNode{n}
						}
					}
				}
			}
			// Deterministic quality rank: token overlap on title+text+summary.
			rankPageLeaves(q, leaves)
			limit := 3
			if limit > len(leaves) {
				limit = len(leaves)
			}
			return leaves[:limit]
		}
		// Expand one node (LLM or best title match).
		var next PageNode
		if w.Chooser != nil && pageIndexLLMEnabled() && len(expandable) > 0 {
			id, err := w.Chooser(ctx, q, expandable)
			if err == nil && id != "" {
				for _, n := range expandable {
					if n.ID == id {
						next = n
						break
					}
				}
			}
		}
		if next.ID == "" && len(expandable) > 0 {
			// Deterministic: score expandable nodes, not frontier order.
			rankPageLeaves(q, expandable)
			next = expandable[0]
		}
		if next.ID == "" {
			break
		}
		seen[next.ID] = struct{}{}
		path = append(path, next)
		frontier = next.Children
		if len(frontier) == 0 && strings.TrimSpace(next.Text) != "" {
			return []PageNode{next}
		}
	}
	if len(path) > 0 {
		return path[len(path)-1:]
	}
	return nil
}

func pageIndexLLMEnabled() bool {
	v := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_PAGEINDEX_LLM"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// rankPageLeaves sorts leaves by question token overlap (title > summary > text).
func rankPageLeaves(question string, leaves []PageNode) {
	if len(leaves) < 2 {
		return
	}
	qToks := pageIndexTokens(question)
	if len(qToks) == 0 {
		return
	}
	type scored struct {
		i  int
		sc float64
	}
	scores := make([]scored, len(leaves))
	for i, n := range leaves {
		sc := 0.0
		title := strings.ToLower(n.Title)
		sum := strings.ToLower(n.Summary)
		body := strings.ToLower(n.Text)
		for _, t := range qToks {
			if strings.Contains(title, t) {
				sc += 3
			}
			if strings.Contains(sum, t) {
				sc += 2
			}
			if strings.Contains(body, t) {
				sc += 1
			}
		}
		// Prefer longer evidence when scores tie (more useful passage).
		sc += 0.001 * float64(len(n.Text))
		scores[i] = scored{i: i, sc: sc}
	}
	// Sort leaves in-place by score desc.
	for i := 0; i < len(scores); i++ {
		for j := i + 1; j < len(scores); j++ {
			if scores[j].sc > scores[i].sc {
				scores[i], scores[j] = scores[j], scores[i]
			}
		}
	}
	ordered := make([]PageNode, len(leaves))
	for i, s := range scores {
		ordered[i] = leaves[s.i]
	}
	copy(leaves, ordered)
}

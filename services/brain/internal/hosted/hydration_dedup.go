package hosted

import "strings"

// HydrationReuseDiag holds answer-time hydration counters.
type HydrationReuseDiag struct {
	Reused         int
	Skipped        int
	Fetched        int
	SiblingReused  int
	SiblingFetched int
	SkipReason     string
}

type HydrationReuse struct {
	Reused   int
	Skipped  int
	FetchIDs []string
}

// classifyHydrationReuse records the pre-hydrate pool and returns only chunk
// IDs that have no usable text on any duplicate passage.
func classifyHydrationReuse(passages []Passage) HydrationReuse {
	withText := map[string]struct{}{}
	for _, p := range passages {
		if strings.TrimSpace(p.ChunkID) != "" && strings.TrimSpace(p.Text) != "" {
			withText[p.ChunkID] = struct{}{}
		}
	}

	out := HydrationReuse{}
	seenFetch := map[string]struct{}{}
	for _, p := range passages {
		hasText := strings.TrimSpace(p.Text) != ""
		hasChunk := strings.TrimSpace(p.ChunkID) != ""
		switch {
		case hasText && hasChunk:
			out.Reused++
		case !hasChunk:
			out.Skipped++
		default:
			if _, ok := withText[p.ChunkID]; ok {
				continue
			}
			if _, ok := seenFetch[p.ChunkID]; ok {
				continue
			}
			seenFetch[p.ChunkID] = struct{}{}
			out.FetchIDs = append(out.FetchIDs, p.ChunkID)
		}
	}
	return out
}

func countAppliedHydrationHits(fetchIDs []string, hits []Hit) int {
	wanted := make(map[string]struct{}, len(fetchIDs))
	for _, id := range fetchIDs {
		wanted[id] = struct{}{}
	}
	applied := map[string]struct{}{}
	for _, h := range hits {
		if strings.TrimSpace(h.Text) == "" {
			continue
		}
		if _, ok := wanted[h.ChunkID]; ok {
			applied[h.ChunkID] = struct{}{}
		}
	}
	return len(applied)
}

// stampHydrationReuseDiag stamps counts captured before hydration. It never
// reclassifies the final pool, which may contain newly hydrated passages.
func stampHydrationReuseDiag(diag map[string]any, d HydrationReuseDiag) {
	if diag == nil {
		return
	}
	diag["answer_hydrate_reused_n"] = d.Reused
	diag["answer_hydrate_skipped_n"] = d.Skipped
	diag["answer_hydrate_fetched_n"] = d.Fetched
	diag["answer_hydrate_sibling_reused_n"] = d.SiblingReused
	diag["answer_hydrate_sibling_fetched_n"] = d.SiblingFetched
	if d.SkipReason != "" {
		diag["answer_hydrate_skip_reason"] = d.SkipReason
	}
}

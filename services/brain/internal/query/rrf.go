package query

import "sort"

// defaultRRFK is the classic Cormack et al. reciprocal-rank constant.
const defaultRRFK = 60

// rrfFuse merges ranked document-id lists with classic reciprocal rank
// fusion: score(d) = Σ 1/(k + rank_i(d)) over lists that contain d, with
// 1-based ranks. Ties break by first-seen order across lists (stable,
// deterministic). Returns at most topN ids; topN <= 0 means unbounded.
func rrfFuse(rankedLists [][]string, k, topN int) []string {
	if k <= 0 {
		k = defaultRRFK
	}
	scores := make(map[string]float64)
	firstSeen := make(map[string]int)
	order := 0
	for _, list := range rankedLists {
		for rank, id := range list {
			if id == "" {
				continue
			}
			if _, ok := firstSeen[id]; !ok {
				firstSeen[id] = order
				order++
			}
			// rank is 0-based; classic RRF uses 1-based rank → k+rank+1.
			scores[id] += 1.0 / float64(k+rank+1)
		}
	}
	if len(scores) == 0 {
		return nil
	}
	type scored struct {
		id    string
		score float64
		seen  int
	}
	items := make([]scored, 0, len(scores))
	for id, score := range scores {
		items = append(items, scored{id: id, score: score, seen: firstSeen[id]})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		if items[i].seen != items[j].seen {
			return items[i].seen < items[j].seen
		}
		return items[i].id < items[j].id
	})
	if topN <= 0 || topN > len(items) {
		topN = len(items)
	}
	out := make([]string, 0, topN)
	for i := 0; i < topN; i++ {
		out = append(out, items[i].id)
	}
	return out
}

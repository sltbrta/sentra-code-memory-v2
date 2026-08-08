package hosted

import (
	"math"
	"sort"
	"unicode"
)

// HotLexSearchOptions tunes query-time behavior. The zero value reproduces
// the legacy full-sort semantics exactly; every deviation is opt-in and
// reported back through HotLexSearchStats.
type HotLexSearchOptions struct {
	// MaxDocumentFrequency excludes terms whose document frequency strictly
	// exceeds this count from BM25 scoring. 0 disables pruning (legacy
	// behavior). Identifier-bearing terms (digits or word underscores, e.g.
	// error codes, versions, config keys, code symbols) are never pruned:
	// they are precisely the high-precision evidence a high-DF cut would
	// otherwise silently destroy.
	MaxDocumentFrequency int
}

// HotLexSearchStats makes query-time selection and pruning observable.
type HotLexSearchStats struct {
	// MatchedDocs is the number of documents scored before top-k truncation.
	MatchedDocs int
	// PrunedTerms lists high-DF terms excluded from scoring (explicit opt-in).
	PrunedTerms []string
	// ProtectedTerms lists high-DF identifier terms kept despite the cut.
	ProtectedTerms []string
}

// hotScored is one scored candidate document index.
type hotScored struct {
	i int
	s float64
}

// hotTermIsIdentifier reports whether a token carries identifier evidence.
// hotTokenize keeps letters, Unicode numbers, and '_', so Unicode numbers and
// underscores are the complete identifier signal (error codes, versions, code
// symbols).
func hotTermIsIdentifier(term string) bool {
	for _, r := range term {
		if r == '_' || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

// hotPruneStats applies the high-DF cut for one term. It reports whether the
// term must be skipped and records the decision in stats. Identifier-bearing
// terms are recorded as protected and never skipped.
func hotPruneStats(stats *HotLexSearchStats, term string, df, maxDF int) bool {
	if maxDF <= 0 || df <= maxDF {
		return false
	}
	if hotTermIsIdentifier(term) {
		stats.ProtectedTerms = append(stats.ProtectedTerms, term)
		return false
	}
	stats.PrunedTerms = append(stats.PrunedTerms, term)
	return true
}

// hotScoreCompare orders finite scores above NaN, then descending score. It
// keeps heap selection deterministic even if defensive callers provide a NaN.
func hotScoreCompare(a, b float64) int {
	aNaN, bNaN := math.IsNaN(a), math.IsNaN(b)
	if aNaN != bNaN {
		if aNaN {
			return -1
		}
		return 1
	}
	if a > b {
		return 1
	}
	if a < b {
		return -1
	}
	return 0
}

// hotTopK returns the best limit candidates under better, exactly matching a
// full sort of arr followed by truncation. better must be a strict total
// order over the candidates (HotLex guarantees unique chunk ids, so score
// desc + chunk-id asc is total). With a total order the top-k set is unique,
// so the min-heap of size limit yields the identical ranking as the full
// sort at O(n log k) comparisons instead of O(n log n).
func hotTopK(arr []hotScored, limit int, better func(a, b hotScored) bool) []hotScored {
	if limit <= 0 {
		return nil
	}
	if limit >= len(arr) {
		sort.Slice(arr, func(a, b int) bool { return better(arr[a], arr[b]) })
		return arr
	}
	heap := make([]hotScored, 0, limit)
	for _, c := range arr {
		if len(heap) < limit {
			heap = append(heap, c)
			hotSiftUp(heap, len(heap)-1, better)
			continue
		}
		if better(c, heap[0]) {
			heap[0] = c
			hotSiftDown(heap, 0, better)
		}
	}
	sort.Slice(heap, func(a, b int) bool { return better(heap[a], heap[b]) })
	return heap
}

// hotTopKMap avoids materializing a full candidate slice when limit is smaller
// than the number of scored documents, which is the common bounded-search case.
func hotTopKMap(scores map[int]float64, limit int, better func(a, b hotScored) bool) []hotScored {
	if limit <= 0 {
		return nil
	}
	if limit >= len(scores) {
		arr := make([]hotScored, 0, len(scores))
		for i, score := range scores {
			arr = append(arr, hotScored{i: i, s: score})
		}
		return hotTopK(arr, limit, better)
	}
	heap := make([]hotScored, 0, limit)
	for i, score := range scores {
		candidate := hotScored{i: i, s: score}
		if len(heap) < limit {
			heap = append(heap, candidate)
			hotSiftUp(heap, len(heap)-1, better)
			continue
		}
		if better(candidate, heap[0]) {
			heap[0] = candidate
			hotSiftDown(heap, 0, better)
		}
	}
	sort.Slice(heap, func(a, b int) bool { return better(heap[a], heap[b]) })
	return heap
}

// hotSiftUp/hotSiftDown maintain a worst-at-root heap: the root always ranks
// last under better, so a better candidate can evict it in O(log k).
func hotSiftUp(heap []hotScored, i int, better func(a, b hotScored) bool) {
	for i > 0 {
		p := (i - 1) / 2
		if !better(heap[p], heap[i]) {
			return
		}
		heap[p], heap[i] = heap[i], heap[p]
		i = p
	}
}

func hotSiftDown(heap []hotScored, i int, better func(a, b hotScored) bool) {
	n := len(heap)
	for {
		w := i
		if l := 2*i + 1; l < n && better(heap[w], heap[l]) {
			w = l
		}
		if r := 2*i + 2; r < n && better(heap[w], heap[r]) {
			w = r
		}
		if w == i {
			return
		}
		heap[w], heap[i] = heap[i], heap[w]
		i = w
	}
}

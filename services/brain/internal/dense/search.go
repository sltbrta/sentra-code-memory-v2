package dense

import (
	"math"
	"sort"
)

// Search returns the topK nearest neighbors of query by cosine similarity.
// topK <= 0 returns all documents ranked descending by score.
// A zero or empty query, dim mismatch, or empty store yields nil.
// Ties break by DocumentID ascending for stable ordering.
func (s *MemoryStore) Search(query []float32, topK int) []Hit {
	if s == nil || len(query) == 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dim == 0 || len(s.docs) == 0 || len(query) != s.dim {
		return nil
	}
	qNorm := l2Norm(query)
	if qNorm == 0 {
		return nil
	}
	hits := make([]Hit, 0, len(s.docs))
	for id, vec := range s.docs {
		score := cosine(query, vec, qNorm)
		hits = append(hits, Hit{DocumentID: id, Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].DocumentID < hits[j].DocumentID
	})
	if topK <= 0 || topK > len(hits) {
		topK = len(hits)
	}
	return hits[:topK]
}

// Cosine returns cosine similarity of a and b. Unequal length or zero vectors yield 0.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	return cosine(a, b, l2Norm(a))
}

func cosine(a, b []float32, aNorm float64) float64 {
	if aNorm == 0 || len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	bNorm := l2Norm(b)
	if bNorm == 0 {
		return 0
	}
	return dot / (aNorm * bNorm)
}

func l2Norm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return 0
	}
	return math.Sqrt(sum)
}

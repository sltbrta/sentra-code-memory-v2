package query

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"
)

// BagOfWordsDense implements DenseSearcher with bag-of-words cosine similarity
// over generation-scoped document bodies. No external embedder or dense store
// is required: suitable for unit tests and offline hybrid fusion until a real
// dense projection is wired. Bodies maps generationID → docID → text.
type BagOfWordsDense struct {
	Bodies map[string]map[string]string
}

// NewBagOfWordsDense returns a DenseSearcher for one generation's body map.
// bodies is copied by reference (callers must not mutate after construction).
func NewBagOfWordsDense(generationID string, bodies map[string]string) *BagOfWordsDense {
	if generationID == "" || len(bodies) == 0 {
		return &BagOfWordsDense{Bodies: map[string]map[string]string{}}
	}
	return &BagOfWordsDense{
		Bodies: map[string]map[string]string{generationID: bodies},
	}
}

// Search ranks generation docs by bag-of-words cosine with the query string.
// Returns at most topK document ids (paths), highest similarity first.
// Unknown generation, empty query, or empty corpus yields nil.
func (b *BagOfWordsDense) Search(ctx context.Context, generationID, query string, topK int) []string {
	if b == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	docs := b.Bodies[generationID]
	if len(docs) == 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	qBag := bagVector(query)
	if len(qBag) == 0 {
		return nil
	}
	type scored struct {
		id    string
		score float64
	}
	hits := make([]scored, 0, len(docs))
	for id, text := range docs {
		if id == "" {
			continue
		}
		dBag := bagVector(text)
		score := bagCosine(qBag, dBag)
		if score <= 0 {
			continue
		}
		hits = append(hits, scored{id: id, score: score})
	}
	if len(hits) == 0 {
		return nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})
	if topK <= 0 || topK > len(hits) {
		topK = len(hits)
	}
	out := make([]string, 0, topK)
	for i := 0; i < topK; i++ {
		out = append(out, hits[i].id)
	}
	return out
}

// bagVector builds a term-frequency bag from text (lowercase alphanumeric tokens).
func bagVector(text string) map[string]float32 {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]float32, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		out[f]++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func bagL2(v map[string]float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return 0
	}
	return math.Sqrt(sum)
}

// bagCosine is cosine similarity of two sparse term-frequency bags.
func bagCosine(a, b map[string]float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Iterate the smaller map for fewer lookups; cosine is symmetric.
	small, large := a, b
	if len(a) > len(b) {
		small, large = b, a
	}
	var dot float64
	for t, av := range small {
		if bv, ok := large[t]; ok {
			dot += float64(av) * float64(bv)
		}
	}
	if dot == 0 {
		return 0
	}
	aNorm := bagL2(a)
	bNorm := bagL2(b)
	if aNorm == 0 || bNorm == 0 {
		return 0
	}
	return dot / (aNorm * bNorm)
}

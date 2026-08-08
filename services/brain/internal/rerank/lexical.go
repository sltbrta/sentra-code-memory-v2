package rerank

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// LexicalReranker scores documents by token overlap with the query.
// Always available; no network. Score is |query ∩ doc| / |query| in [0, 1]
// (Jaccard-like recall over query tokens). Empty query yields empty results.
type LexicalReranker struct{}

// NewLexicalReranker returns a LexicalReranker.
func NewLexicalReranker() *LexicalReranker {
	return &LexicalReranker{}
}

// Rerank ranks docs by token overlap with query, highest first.
// topN <= 0 returns all non-zero (or all if every score is zero) ranked.
func (r *LexicalReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]Ranked, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	qTokens := tokenize(query)
	if len(qTokens) == 0 || len(docs) == 0 {
		return nil, nil
	}
	ranked := make([]Ranked, 0, len(docs))
	for i, doc := range docs {
		score := overlapScore(qTokens, tokenize(doc))
		ranked = append(ranked, Ranked{Index: i, Score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Index < ranked[j].Index
	})
	if topN <= 0 || topN > len(ranked) {
		topN = len(ranked)
	}
	return ranked[:topN], nil
}

func tokenize(s string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	out := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		out[f] = struct{}{}
	}
	return out
}

// overlapScore is |q ∩ d| / |q| (query-token recall). Empty q yields 0.
func overlapScore(q, d map[string]struct{}) float64 {
	if len(q) == 0 {
		return 0
	}
	var hit int
	for t := range q {
		if _, ok := d[t]; ok {
			hit++
		}
	}
	return float64(hit) / float64(len(q))
}

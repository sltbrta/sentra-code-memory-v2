package query

import (
	"context"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

// HTTPCandidateReranker uses a live cross-encoder when available, else lexical.
type HTTPCandidateReranker struct {
	Inner    rerank.Reranker
	Fallback *LexicalCandidateReranker
}

// NewHTTPCandidateRerankerFromEnv builds ZE HTTP reranker or lexical fallback.
func NewHTTPCandidateRerankerFromEnv() CandidateReranker {
	if r, err := rerank.NewHTTPRerankerFromEnv(); err == nil && r != nil {
		return &HTTPCandidateReranker{
			Inner:    r,
			Fallback: NewLexicalCandidateReranker(),
		}
	}
	return NewLexicalCandidateReranker()
}

// Rerank implements CandidateReranker.
func (h *HTTPCandidateReranker) Rerank(ctx context.Context, query string, paths []string, bodies map[string]string, topN int) []string {
	if h == nil || len(paths) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	docs := make([]string, len(paths))
	for i, p := range paths {
		docs[i] = bodies[p]
	}
	inner := h.Inner
	if inner == nil {
		if h.Fallback != nil {
			return h.Fallback.Rerank(ctx, query, paths, bodies, topN)
		}
		return NewLexicalCandidateReranker().Rerank(ctx, query, paths, bodies, topN)
	}
	ranked, err := inner.Rerank(ctx, query, docs, topN)
	if err != nil || len(ranked) == 0 {
		if h.Fallback != nil {
			return h.Fallback.Rerank(ctx, query, paths, bodies, topN)
		}
		return nil
	}
	out := make([]string, 0, len(ranked))
	for _, r := range ranked {
		if r.Index >= 0 && r.Index < len(paths) {
			out = append(out, paths[r.Index])
		}
	}
	return out
}

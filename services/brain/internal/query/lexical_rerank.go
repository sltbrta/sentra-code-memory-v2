package query

import (
	"context"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

// LexicalCandidateReranker implements CandidateReranker by scoring candidate
// path bodies with rerank.LexicalReranker (token-overlap, no network).
type LexicalCandidateReranker struct {
	// Inner is the path-agnostic lexical scorer. Nil uses NewLexicalReranker.
	Inner rerank.Reranker
}

// NewLexicalCandidateReranker returns a CandidateReranker backed by
// rerank.NewLexicalReranker.
func NewLexicalCandidateReranker() *LexicalCandidateReranker {
	return &LexicalCandidateReranker{Inner: rerank.NewLexicalReranker()}
}

// Rerank reorders paths by lexical overlap of query against each path's body.
// Paths missing from bodies are scored as empty strings (lowest non-error).
// Returns at most topN paths; empty query or paths yields nil.
func (r *LexicalCandidateReranker) Rerank(ctx context.Context, query string, paths []string, bodies map[string]string, topN int) []string {
	if r == nil || len(paths) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	inner := r.Inner
	if inner == nil {
		inner = rerank.NewLexicalReranker()
	}
	docs := make([]string, len(paths))
	for i, path := range paths {
		docs[i] = bodies[path]
	}
	ranked, err := inner.Rerank(ctx, query, docs, topN)
	if err != nil || len(ranked) == 0 {
		return nil
	}
	out := make([]string, 0, len(ranked))
	seen := make(map[string]bool, len(ranked))
	for _, hit := range ranked {
		if hit.Index < 0 || hit.Index >= len(paths) {
			continue
		}
		path := paths[hit.Index]
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

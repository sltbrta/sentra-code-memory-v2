package rerank_test

import (
	"context"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
)

func TestLexicalRerankerTokenOverlap(t *testing.T) {
	r := rerank.NewLexicalReranker()
	docs := []string{
		"unrelated gardening tips",
		"the quick brown fox jumps",
		"quick fox tutorial for engineers",
	}
	ranked, err := r.Rerank(context.Background(), "quick fox", docs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 2 {
		t.Fatalf("len=%d want 2", len(ranked))
	}
	// Both docs[1] and docs[2] match both tokens; higher or equal score.
	// docs[1] has both; docs[2] has both — either order OK if scores equal,
	// but both must beat docs[0].
	if ranked[0].Index == 0 || ranked[1].Index == 0 {
		t.Fatalf("unexpected zero-score doc in top-2: %+v", ranked)
	}
	if ranked[0].Score < 1.0 {
		// full query-token coverage
		t.Fatalf("top score=%v want 1.0", ranked[0].Score)
	}

	// Exact-match doc should rank first over partial.
	docs2 := []string{
		"alpha only",
		"alpha beta gamma",
		"beta only",
	}
	ranked, err = r.Rerank(context.Background(), "alpha beta", docs2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 3 {
		t.Fatalf("len=%d want 3", len(ranked))
	}
	if ranked[0].Index != 1 {
		t.Fatalf("top index=%d want 1 (full overlap)", ranked[0].Index)
	}
	if ranked[0].Score != 1.0 {
		t.Fatalf("top score=%v want 1", ranked[0].Score)
	}
	// Partial matches score 0.5 each; stable by index.
	if ranked[1].Score != 0.5 || ranked[2].Score != 0.5 {
		t.Fatalf("partial scores: %+v", ranked)
	}
}

func TestLexicalRerankerEmpty(t *testing.T) {
	r := rerank.NewLexicalReranker()
	ranked, err := r.Rerank(context.Background(), "", []string{"a"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ranked != nil {
		t.Fatalf("empty query: got %+v", ranked)
	}
	ranked, err = r.Rerank(context.Background(), "hello", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ranked != nil {
		t.Fatalf("empty docs: got %+v", ranked)
	}
}

func TestLexicalRerankerCanceledContext(t *testing.T) {
	r := rerank.NewLexicalReranker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Rerank(ctx, "q", []string{"d"}, 1)
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestCachedEmbedderHits(t *testing.T) {
	stub := &countingEmbedder{dim: 3}
	cached, err := rerank.NewCachedEmbedder(stub)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v1, err := cached.Embed(ctx, []string{"hello", "world"}, "document")
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("calls=%d want 1", stub.calls)
	}
	v2, err := cached.Embed(ctx, []string{"hello", "world"}, "document")
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("cache miss: calls=%d want 1", stub.calls)
	}
	if len(v1) != 2 || len(v2) != 2 {
		t.Fatalf("lens %d %d", len(v1), len(v2))
	}
	for i := range v1 {
		if len(v1[i]) != 3 || v1[i][0] != v2[i][0] {
			t.Fatalf("vectors differ: %v vs %v", v1[i], v2[i])
		}
	}
	// Different inputType is a different cache key.
	_, err = cached.Embed(ctx, []string{"hello"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 2 {
		t.Fatalf("inputType key: calls=%d want 2", stub.calls)
	}
	// Partial hit: only "new" should embed.
	_, err = cached.Embed(ctx, []string{"hello", "new"}, "document")
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 3 {
		t.Fatalf("partial miss: calls=%d want 3", stub.calls)
	}
	if cached.Len() < 3 {
		t.Fatalf("cache len=%d want >=3", cached.Len())
	}
}

func TestNewCachedEmbedderNil(t *testing.T) {
	if _, err := rerank.NewCachedEmbedder(nil); err == nil {
		t.Fatal("expected error for nil embedder")
	}
}

// countingEmbedder is a hermetic Embedder stub for cache tests.
type countingEmbedder struct {
	calls int
	dim   int
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string, _ string) ([][]float32, error) {
	c.calls++
	out := make([][]float32, len(texts))
	for i := range texts {
		vec := make([]float32, c.dim)
		// Deterministic non-zero vector from text length.
		vec[0] = float32(len(texts[i]) + 1)
		out[i] = vec
	}
	return out, nil
}

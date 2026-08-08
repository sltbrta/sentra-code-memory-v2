package rerank

import "context"

// Embedder maps texts to dense vectors. inputType is typically "query" or
// "document" for asymmetric models; OpenAI-style models may ignore it.
type Embedder interface {
	Embed(ctx context.Context, texts []string, inputType string) ([][]float32, error)
}

// Reranker scores documents against a query and returns the top-N ranked.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string, topN int) ([]Ranked, error)
}

// Ranked is one document score from a Reranker. Index is the position in the
// input docs slice; Score is higher-is-better relevance.
type Ranked struct {
	Index int
	Score float64
}

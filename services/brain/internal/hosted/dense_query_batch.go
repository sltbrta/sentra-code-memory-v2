package hosted

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// hostedDenseQueryRun is one request-scoped embedding batch followed by ANN
// searches. Lists and errors are compacted in query order, independent of ANN
// completion order, so RRF tie behavior remains deterministic.
type hostedDenseQueryRun struct {
	Lists       [][]Hit
	Embedding   QueryEmbeddingDiagnostics
	EmbedMS     int64
	ANNMS       int64
	ANNRequests int
	ANNOK       int
	ANNFailed   int
	Errors      []string
}

type hostedQueryEmbedBatchFunc func(context.Context, []string, string, int) ([][]float32, QueryEmbeddingDiagnostics, error)
type hostedDenseSearchFunc func(context.Context, Config, []float32) ([]Hit, error)

func (c *Client) runHostedDenseQueries(ctx context.Context, queries []string) hostedDenseQueryRun {
	return c.runHostedDenseQueriesWith(ctx, queries, EmbedQueries, denseSearch)
}

func (c *Client) runHostedDenseQueriesWith(
	ctx context.Context,
	queries []string,
	embed hostedQueryEmbedBatchFunc,
	search hostedDenseSearchFunc,
) hostedDenseQueryRun {
	run := hostedDenseQueryRun{}
	if c == nil || len(queries) == 0 || embed == nil || search == nil {
		return run
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tEmbed := time.Now()
	vectors, embedDiag, embedErr := embed(ctx, queries, c.cfg.CohereModel, c.cfg.CohereDim)
	run.EmbedMS = time.Since(tEmbed).Milliseconds()
	run.Embedding = embedDiag
	if embedErr != nil {
		run.Errors = append(run.Errors, "dense embedding: "+hostedDenseErrorKind(embedErr))
	}

	type annResult struct {
		hits []Hit
		err  error
		ms   int64
	}
	results := make([]annResult, len(vectors))
	var wg sync.WaitGroup
	for i, vector := range vectors {
		if len(vector) == 0 {
			continue
		}
		run.ANNRequests++
		wg.Add(1)
		go func(i int, vector []float32) {
			defer wg.Done()
			t0 := time.Now()
			hits, err := search(ctx, c.cfg, vector)
			results[i] = annResult{hits: hits, err: err, ms: time.Since(t0).Milliseconds()}
		}(i, vector)
	}
	wg.Wait()
	for i, result := range results {
		if result.ms > run.ANNMS {
			run.ANNMS = result.ms
		}
		if len(vectors[i]) == 0 {
			continue
		}
		if result.err != nil {
			run.ANNFailed++
			run.Errors = append(run.Errors, fmt.Sprintf("dense ANN query %d: %s", i, hostedDenseErrorKind(result.err)))
			continue
		}
		run.ANNOK++
		if len(result.hits) > 0 {
			run.Lists = append(run.Lists, result.hits)
		}
	}
	return run
}

func hostedDenseErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case err != nil:
		// Provider error bodies may echo request content. Diagnostics expose the
		// stage/index and explicit failure state, never the raw provider string.
		return "provider_error"
	default:
		return "unknown"
	}
}

func stampHostedDenseQueryRun(diag map[string]any, prefix string, run hostedDenseQueryRun) {
	if diag == nil {
		return
	}
	put := func(name string, value any) { diag[prefix+name] = value }
	put("query_count", run.Embedding.Queries)
	put("embedding_requests", run.Embedding.Requests)
	put("embedding_succeeded_queries", run.Embedding.SucceededQueries)
	put("embedding_failed_queries", run.Embedding.FailedQueries)
	put("embedding_max_batch_texts", run.Embedding.MaxBatchTexts)
	put("embedding_max_batch_bytes", run.Embedding.MaxBatchBytes)
	put("ann_requests", run.ANNRequests)
	put("ann_succeeded_requests", run.ANNOK)
	put("ann_failed_requests", run.ANNFailed)
	put("embed_ms", run.EmbedMS)
	put("ann_ms", run.ANNMS)
	put("lists", len(run.Lists))
	if len(run.Errors) > 0 {
		put("errors", append([]string(nil), run.Errors...))
	}
	status := "ok"
	switch {
	case run.Embedding.Queries == 0:
		status = "skipped"
	case len(run.Lists) == 0 && (run.Embedding.FailedQueries > 0 || run.ANNFailed > 0):
		status = "error"
	case run.Embedding.FailedQueries > 0 || run.ANNFailed > 0:
		status = "partial_failure"
	case len(run.Lists) == 0:
		status = "empty"
	}
	put("status", status)
}

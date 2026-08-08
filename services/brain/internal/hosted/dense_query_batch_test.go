package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRunHostedDenseQueriesPreservesScopeOrderAndCitationMetadata(t *testing.T) {
	c := &Client{cfg: Config{
		BrainID: "brain-scope", CohereModel: "embed-model", CohereDim: 2, DenseLimit: 4,
	}}
	queries := []string{"primary", "rewrite", "hyde"}
	embed := func(_ context.Context, got []string, model string, dim int) ([][]float32, QueryEmbeddingDiagnostics, error) {
		if strings.Join(got, "|") != strings.Join(queries, "|") || model != "embed-model" || dim != 2 {
			t.Fatalf("embed inputs changed: queries=%v model=%q dim=%d", got, model, dim)
		}
		return [][]float32{{0, 1}, {1, 1}, {2, 1}}, QueryEmbeddingDiagnostics{
			Queries: 3, Requests: 1, SucceededQueries: 3, MaxBatchTexts: 3, MaxBatchBytes: 19,
		}, nil
	}
	// Complete ANN calls in reverse order. The returned list order must still
	// match the embedding/query order because RRF tie order is observable.
	releasePrimary := make(chan struct{})
	releaseRewrite := make(chan struct{})
	search := func(_ context.Context, cfg Config, vector []float32) ([]Hit, error) {
		if cfg.BrainID != "brain-scope" {
			t.Fatalf("ANN scope=%q", cfg.BrainID)
		}
		index := int(vector[0])
		switch index {
		case 0:
			<-releasePrimary
		case 1:
			<-releaseRewrite
		case 2:
			close(releaseRewrite)
			close(releasePrimary)
		}
		return []Hit{{
			ChunkID:   fmt.Sprintf("chunk-%d", index),
			DSID:      fmt.Sprintf("doc-%d", index),
			SourceURI: fmt.Sprintf("slack://channel/%d", index),
			Text:      fmt.Sprintf("authorized evidence %d", index),
			Score:     1 - float64(index)/10,
			Channel:   "dense",
		}}, nil
	}
	run := c.runHostedDenseQueriesWith(context.Background(), queries, embed, search)
	if len(run.Lists) != 3 || run.Embedding.Requests != 1 || run.ANNRequests != 3 || run.ANNOK != 3 {
		t.Fatalf("run=%+v", run)
	}
	for i, list := range run.Lists {
		if len(list) != 1 || list[0].DSID != fmt.Sprintf("doc-%d", i) ||
			list[0].ChunkID != fmt.Sprintf("chunk-%d", i) ||
			list[0].SourceURI != fmt.Sprintf("slack://channel/%d", i) {
			t.Fatalf("ordered evidence list %d=%+v", i, list)
		}
		passages := hitsToPassages(list, 1, 1_000)
		if len(passages) != 1 || passages[0].DocumentID != list[0].DSID ||
			passages[0].ChunkID != list[0].ChunkID || passages[0].SourceURI != list[0].SourceURI {
			t.Fatalf("citation metadata changed: hit=%+v passage=%+v", list[0], passages)
		}
	}
	diag := map[string]any{}
	stampHostedDenseQueryRun(diag, "dense_", run)
	if diag["dense_status"] != "ok" || diag["dense_embedding_requests"] != 1 || diag["dense_lists"] != 3 {
		t.Fatalf("diagnostics=%+v", diag)
	}
	raw, err := json.Marshal(diag)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(append([]string(nil), queries...), "brain-scope", "doc-0", "slack://channel/0") {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("sanitized diagnostics leaked %q: %s", secret, raw)
		}
	}
}

func TestRunHostedDenseQueriesKeepsPartialEmbeddingFallbackExplicit(t *testing.T) {
	c := &Client{cfg: Config{BrainID: "brain", CohereModel: "embed-model", CohereDim: 2}}
	embed := func(context.Context, []string, string, int) ([][]float32, QueryEmbeddingDiagnostics, error) {
		return [][]float32{{1, 0}, nil, {3, 0}}, QueryEmbeddingDiagnostics{
			Queries: 3, Requests: 2, SucceededQueries: 2, FailedQueries: 1, MaxBatchTexts: 2, MaxBatchBytes: 16,
		}, errors.New("bounded batch q2-secret failed")
	}
	search := func(_ context.Context, _ Config, vector []float32) ([]Hit, error) {
		index := int(vector[0])
		return []Hit{{DSID: fmt.Sprintf("doc-%d", index), ChunkID: fmt.Sprintf("chunk-%d", index)}}, nil
	}
	run := c.runHostedDenseQueriesWith(context.Background(), []string{"q1", "q2", "q3"}, embed, search)
	if len(run.Lists) != 2 || run.Lists[0][0].DSID != "doc-1" || run.Lists[1][0].DSID != "doc-3" ||
		run.ANNRequests != 2 || len(run.Errors) != 1 {
		t.Fatalf("partial run=%+v", run)
	}
	diag := map[string]any{}
	stampHostedDenseQueryRun(diag, "dense_", run)
	if diag["dense_status"] != "partial_failure" || diag["dense_embedding_failed_queries"] != 1 || diag["dense_ann_requests"] != 2 {
		t.Fatalf("partial diagnostics=%+v", diag)
	}
	raw, err := json.Marshal(diag)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "q2-secret") || !strings.Contains(string(raw), "provider_error") {
		t.Fatalf("partial diagnostics are not sanitized/explicit: %s", raw)
	}
}

func TestRunHostedDenseQueriesReportsTotalANNFailure(t *testing.T) {
	c := &Client{cfg: Config{BrainID: "brain", CohereModel: "embed-model", CohereDim: 1}}
	embed := func(context.Context, []string, string, int) ([][]float32, QueryEmbeddingDiagnostics, error) {
		return [][]float32{{1}}, QueryEmbeddingDiagnostics{Queries: 1, Requests: 1, SucceededQueries: 1}, nil
	}
	search := func(context.Context, Config, []float32) ([]Hit, error) {
		return nil, errors.New("qdrant unavailable")
	}
	run := c.runHostedDenseQueriesWith(context.Background(), []string{"query"}, embed, search)
	diag := map[string]any{}
	stampHostedDenseQueryRun(diag, "dense_", run)
	if diag["dense_status"] != "error" || diag["dense_ann_failed_requests"] != 1 || len(run.Errors) != 1 {
		t.Fatalf("failure run=%+v diagnostics=%+v", run, diag)
	}
}

package hosted

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

type queryEmbedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f queryEmbedRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type cohereEmbedRequest struct {
	Model           string   `json:"model"`
	Texts           []string `json:"texts"`
	InputType       string   `json:"input_type"`
	OutputDimension int      `json:"output_dimension"`
}

func cohereEmbeddingResponse(rows [][]float64, tokens int) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"embeddings": map[string]any{"float": rows},
		"meta":       map[string]any{"billed_units": map[string]int{"input_tokens": tokens}},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

func TestEmbedQueriesCohereBatchesWithinBothBoundsAndPreservesOrder(t *testing.T) {
	texts := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	wantIndex := make(map[string]int, len(texts))
	for i, text := range texts {
		wantIndex[text] = i
	}
	var mu sync.Mutex
	var requests []cohereEmbedRequest
	client := &http.Client{Transport: queryEmbedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization header = %q", got)
		}
		var body cohereEmbedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		rows := make([][]float64, len(body.Texts))
		for i, text := range body.Texts {
			index := wantIndex[text]
			rows[i] = []float64{float64(index), float64(index) + 0.5}
		}
		return cohereEmbeddingResponse(rows, len(body.Texts)*3), nil
	})}

	vectors, diag, err := embedQueriesCohere(context.Background(), texts, cohereQueryEmbedConfig{
		url: "https://cohere.invalid/v2/embed", apiKey: "test-key", model: "embed-test", dim: 2,
		maxBatchTexts: 3, maxBatchBytes: 12, httpClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || diag.Requests != 3 {
		t.Fatalf("requests=%d diagnostics=%+v", len(requests), diag)
	}
	for requestIndex, request := range requests {
		inputBytes := 0
		for _, text := range request.Texts {
			inputBytes += len(text)
		}
		if len(request.Texts) > 3 || inputBytes > 12 {
			t.Fatalf("request %d exceeded bounds: texts=%d bytes=%d body=%+v", requestIndex, len(request.Texts), inputBytes, request)
		}
		if request.Model != "embed-test" || request.InputType != "search_query" || request.OutputDimension != 2 {
			t.Fatalf("request shape changed: %+v", request)
		}
	}
	for i, vector := range vectors {
		if len(vector) != 2 || vector[0] != float32(i) || vector[1] != float32(i)+0.5 {
			t.Fatalf("vector %d = %v; input order lost", i, vector)
		}
	}
	if diag.Queries != len(texts) || diag.SucceededQueries != len(texts) || diag.FailedQueries != 0 ||
		diag.MaxBatchTexts > 3 || diag.MaxBatchBytes > 12 {
		t.Fatalf("diagnostics=%+v", diag)
	}
}

func TestEmbedQueriesCohereDefaultBoundsLongFixedWorkload(t *testing.T) {
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = strings.Repeat(string(rune('a'+i)), maxCohereQueryTextBytes+31)
	}
	var requestCount int
	client := &http.Client{Transport: queryEmbedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body cohereEmbedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		requestCount++
		inputBytes := 0
		for _, text := range body.Texts {
			inputBytes += len(text)
			if len(text) > maxCohereQueryTextBytes {
				t.Fatalf("query text bytes=%d exceeds %d", len(text), maxCohereQueryTextBytes)
			}
		}
		if len(body.Texts) > maxCohereQueryEmbedTexts || inputBytes > maxCohereQueryEmbedBytes {
			t.Fatalf("provider request exceeded bounds: texts=%d bytes=%d", len(body.Texts), inputBytes)
		}
		rows := make([][]float64, len(body.Texts))
		for i := range rows {
			rows[i] = []float64{float64(i)}
		}
		return cohereEmbeddingResponse(rows, len(rows)), nil
	})}
	vectors, diag, err := embedQueriesCohere(context.Background(), texts, cohereQueryEmbedConfig{
		url: "https://cohere.invalid/v2/embed", apiKey: "test-key", model: "embed-test", dim: 1,
		maxBatchTexts: maxCohereQueryEmbedTexts, maxBatchBytes: maxCohereQueryEmbedBytes, httpClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestCount != 5 || diag.Requests != 5 || len(vectors) != len(texts) {
		t.Fatalf("fixed workload requests=%d vectors=%d diagnostics=%+v", requestCount, len(vectors), diag)
	}
}

func TestEmbedQueriesCohereSingleTextCannotExceedAggregateUTF8Bound(t *testing.T) {
	client := &http.Client{Transport: queryEmbedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body cohereEmbedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if len(body.Texts) != 1 || len(body.Texts[0]) > 11 || !utf8.ValidString(body.Texts[0]) {
			t.Fatalf("bounded UTF-8 text=%q bytes=%d", body.Texts, len(body.Texts[0]))
		}
		return cohereEmbeddingResponse([][]float64{{1}}, 1), nil
	})}
	_, diag, err := embedQueriesCohere(context.Background(), []string{strings.Repeat("界", 20)}, cohereQueryEmbedConfig{
		url: "https://cohere.invalid/v2/embed", apiKey: "test-key", model: "embed-test", dim: 1,
		maxBatchTexts: 16, maxBatchBytes: 11, httpClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if diag.MaxBatchBytes != 9 {
		t.Fatalf("UTF-8 diagnostics=%+v", diag)
	}
}

func TestEmbedQueriesCoherePartialBatchFailureIsExplicitAndKeepsSuccessfulOrder(t *testing.T) {
	texts := []string{"q0", "q1", "q2", "q3", "q4"}
	var call atomic.Int32
	client := &http.Client{Transport: queryEmbedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestNumber := call.Add(1)
		var body cohereEmbedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if requestNumber == 2 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("provider unavailable")),
			}, nil
		}
		rows := make([][]float64, len(body.Texts))
		for i, text := range body.Texts {
			rows[i] = []float64{float64(text[1] - '0')}
		}
		return cohereEmbeddingResponse(rows, len(rows)), nil
	})}

	vectors, diag, err := embedQueriesCohere(context.Background(), texts, cohereQueryEmbedConfig{
		url: "https://cohere.invalid/v2/embed", apiKey: "test-key", model: "embed-test", dim: 1,
		maxBatchTexts: 2, maxBatchBytes: 100, httpClient: client,
	})
	if err == nil || !strings.Contains(err.Error(), "batch 2 queries [2,4)") || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("error=%v", err)
	}
	for _, index := range []int{0, 1} {
		if len(vectors[index]) != 1 || vectors[index][0] != float32(index) {
			t.Fatalf("successful vector %d=%v", index, vectors[index])
		}
	}
	if vectors[2] != nil || vectors[3] != nil || vectors[4] != nil {
		t.Fatalf("failed batch/suffix returned vectors: %v %v %v", vectors[2], vectors[3], vectors[4])
	}
	if diag.Requests != 2 || diag.SucceededQueries != 2 || diag.FailedQueries != 3 || call.Load() != 2 {
		t.Fatalf("diagnostics=%+v", diag)
	}
}

func TestEmbedQueriesCohereRejectsAmbiguousRows(t *testing.T) {
	tests := []struct {
		name string
		rows [][]float64
		want string
	}{
		{name: "missing row", rows: [][]float64{{1, 2}}, want: "row count 1, want 2"},
		{name: "empty row", rows: [][]float64{{1, 2}, {}}, want: "row 1 is empty"},
		{name: "wrong dimension", rows: [][]float64{{1, 2}, {3}}, want: "row 1 dimension 1, want 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: queryEmbedRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return cohereEmbeddingResponse(test.rows, 2), nil
			})}
			vectors, diag, err := embedQueriesCohere(context.Background(), []string{"first", "second"}, cohereQueryEmbedConfig{
				url: "https://cohere.invalid/v2/embed", apiKey: "test-key", model: "embed-test", dim: 2,
				maxBatchTexts: 2, maxBatchBytes: 100, httpClient: client,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
			if vectors[0] != nil || vectors[1] != nil || diag.FailedQueries != 2 || diag.SucceededQueries != 0 {
				t.Fatalf("ambiguous response was partially accepted: vectors=%v diagnostics=%+v", vectors, diag)
			}
		})
	}
}

func TestEmbedQueriesCohereIsRequestScopedAndHonorsPreCanceledContext(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: queryEmbedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		var body cohereEmbedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		rows := make([][]float64, len(body.Texts))
		for i := range rows {
			rows[i] = []float64{1}
		}
		return cohereEmbeddingResponse(rows, len(rows)), nil
	})}
	cfg := cohereQueryEmbedConfig{
		url: "https://cohere.invalid/v2/embed", apiKey: "test-key", model: "embed-test", dim: 1,
		maxBatchTexts: 16, maxBatchBytes: 100, httpClient: client,
	}
	for _, requestQueries := range [][]string{{"tenant-a-primary", "tenant-a-rewrite"}, {"tenant-b-primary"}} {
		if _, _, err := embedQueriesCohere(context.Background(), requestQueries, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("separate retrieval requests were coalesced: provider calls=%d", calls.Load())
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, diag, err := embedQueriesCohere(canceled, []string{"must-not-start"}, cfg)
	if err == nil || calls.Load() != 2 || diag.Requests != 0 || diag.FailedQueries != 1 {
		t.Fatalf("pre-canceled call started provider work: calls=%d diagnostics=%+v error=%v", calls.Load(), diag, err)
	}
}

func BenchmarkCohereQueryEmbeddingFixedWorkload(b *testing.B) {
	texts := make([]string, 8)
	for i := range texts {
		texts[i] = fmt.Sprintf("query-%02d %s", i, strings.Repeat("semantic-rewrite ", 8))
	}
	client := &http.Client{Transport: queryEmbedRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body cohereEmbedRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		rows := make([][]float64, len(body.Texts))
		for i := range rows {
			rows[i] = make([]float64, 64)
		}
		return cohereEmbeddingResponse(rows, len(rows)*16), nil
	})}
	inputBytes := 0
	for _, text := range texts {
		inputBytes += len(text)
	}
	for _, test := range []struct {
		name     string
		maxTexts int
	}{
		{name: "scalar_8_requests", maxTexts: 1},
		{name: "batched_1_request", maxTexts: 16},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(inputBytes))
			cfg := cohereQueryEmbedConfig{
				url: "https://cohere.invalid/v2/embed", apiKey: "test-key", model: "embed-test", dim: 64,
				maxBatchTexts: test.maxTexts, maxBatchBytes: maxCohereQueryEmbedBytes, httpClient: client,
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				vectors, _, err := embedQueriesCohere(context.Background(), texts, cfg)
				if err != nil || len(vectors) != len(texts) {
					b.Fatalf("vectors=%d error=%v", len(vectors), err)
				}
			}
		})
	}
}

package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	cohereEmbedURL = "https://api.cohere.com/v2/embed"

	// Query embedding batches are deliberately below Cohere's provider limit.
	// The count bound controls response/vector allocation while the byte bound
	// prevents a handful of long rewrites from creating an oversized request.
	// These are per retrieval request: batches never mix tenants or principals.
	maxCohereQueryEmbedTexts = 16
	maxCohereQueryEmbedBytes = 32 << 10
	maxCohereQueryTextBytes  = 8_000
)

// QueryEmbeddingDiagnostics is sanitized request-shape accounting for one
// request-scoped query embedding operation. It intentionally contains no query
// text, tenant/principal identity, document IDs, or citation data.
type QueryEmbeddingDiagnostics struct {
	Queries          int
	Requests         int
	SucceededQueries int
	FailedQueries    int
	MaxBatchTexts    int
	MaxBatchBytes    int
}

type cohereQueryEmbedConfig struct {
	url           string
	apiKey        string
	model         string
	dim           int
	maxBatchTexts int
	maxBatchBytes int
	httpClient    *http.Client
}

type cohereEmbedResponse struct {
	Embeddings struct {
		Float [][]float64 `json:"float"`
	} `json:"embeddings"`
	Meta struct {
		BilledUnits struct {
			InputTokens int `json:"input_tokens"`
		} `json:"billed_units"`
	} `json:"meta"`
}

// EmbedQuery preserves the legacy single-query API. Hosted retrieval fanout
// should call EmbedQueries so rewrites share bounded provider requests.
func EmbedQuery(ctx context.Context, text string, model string, dim int) ([]float32, error) {
	vectors, _, err := EmbedQueries(ctx, []string{text}, model, dim)
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return nil, fmt.Errorf("cohere embed: missing single-query vector")
	}
	return vectors[0], nil
}

// EmbedQueries embeds search queries in stable input order. Calls are split by
// both item count and aggregate input bytes. A failed bounded batch leaves nil
// vectors for exactly that range and returns an explicit joined error, allowing
// callers to keep successful batches while retaining lexical fallback.
func EmbedQueries(ctx context.Context, texts []string, model string, dim int) ([][]float32, QueryEmbeddingDiagnostics, error) {
	key := strings.TrimSpace(os.Getenv("COHERE_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("CO_API_KEY"))
	}
	return embedQueriesCohere(ctx, texts, cohereQueryEmbedConfig{
		url:           cohereEmbedURL,
		apiKey:        key,
		model:         model,
		dim:           dim,
		maxBatchTexts: maxCohereQueryEmbedTexts,
		maxBatchBytes: maxCohereQueryEmbedBytes,
	})
}

func embedQueriesCohere(ctx context.Context, texts []string, cfg cohereQueryEmbedConfig) ([][]float32, QueryEmbeddingDiagnostics, error) {
	diag := QueryEmbeddingDiagnostics{Queries: len(texts)}
	vectors := make([][]float32, len(texts))
	if len(texts) == 0 {
		return vectors, diag, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		diag.FailedQueries = len(texts)
		return vectors, diag, err
	}
	if strings.TrimSpace(cfg.apiKey) == "" {
		diag.FailedQueries = len(texts)
		return vectors, diag, fmt.Errorf("COHERE_API_KEY missing")
	}
	if strings.TrimSpace(cfg.url) == "" {
		cfg.url = cohereEmbedURL
	}
	if strings.TrimSpace(cfg.model) == "" {
		cfg.model = "embed-v4.0"
	}
	if cfg.dim <= 0 {
		cfg.dim = 1536
	}
	if cfg.maxBatchTexts <= 0 || cfg.maxBatchTexts > maxCohereQueryEmbedTexts {
		cfg.maxBatchTexts = maxCohereQueryEmbedTexts
	}
	if cfg.maxBatchBytes <= 0 || cfg.maxBatchBytes > maxCohereQueryEmbedBytes {
		cfg.maxBatchBytes = maxCohereQueryEmbedBytes
	}

	prepared := make([]string, len(texts))
	textLimit := maxCohereQueryTextBytes
	if cfg.maxBatchBytes < textLimit {
		textLimit = cfg.maxBatchBytes
	}
	for i, text := range texts {
		text = truncateUTF8Bytes(text, textLimit)
		if strings.TrimSpace(text) == "" {
			diag.FailedQueries = len(texts)
			return vectors, diag, fmt.Errorf("cohere embed: query %d is empty", i)
		}
		prepared[i] = text
	}

	var errs []error
	for start := 0; start < len(prepared); {
		end, batchBytes := boundedQueryEmbedBatch(prepared, start, cfg.maxBatchTexts, cfg.maxBatchBytes)
		batch := prepared[start:end]
		diag.Requests++
		if len(batch) > diag.MaxBatchTexts {
			diag.MaxBatchTexts = len(batch)
		}
		if batchBytes > diag.MaxBatchBytes {
			diag.MaxBatchBytes = batchBytes
		}
		rows, inputTokens, err := embedCohereBatch(ctx, batch, cfg)
		if err != nil {
			diag.FailedQueries += len(batch)
			errs = append(errs, fmt.Errorf("cohere embed batch %d queries [%d,%d): %w", diag.Requests, start, end, err))
			start = end
			// Do not amplify an auth/provider/deadline failure into more requests.
			// Earlier bounded batches remain usable; the suffix falls back to the
			// lexical arms and is explicitly accounted as unstarted.
			remaining := len(prepared) - start
			diag.FailedQueries += remaining
			if remaining > 0 {
				cause := err
				if ctx.Err() != nil {
					cause = ctx.Err()
				}
				errs = append(errs, fmt.Errorf("cohere embed unstarted queries [%d,%d) after batch failure: %w", start, len(prepared), cause))
			}
			break
		}
		for i, row := range rows {
			vectors[start+i] = row
		}
		diag.SucceededQueries += len(rows)
		ledgerFrom(ctx).recordUsage("embed", "cohere", cfg.model, inputTokens, 0, inputTokens)
		start = end
	}
	return vectors, diag, errors.Join(errs...)
}

func boundedQueryEmbedBatch(texts []string, start, maxTexts, maxBytes int) (end, batchBytes int) {
	end = start
	for end < len(texts) && end-start < maxTexts {
		n := len(texts[end])
		if end > start && batchBytes+n > maxBytes {
			break
		}
		batchBytes += n
		end++
	}
	// Each prepared text is independently capped at the aggregate bound, so this
	// is defensive only and guarantees forward progress.
	if end == start && start < len(texts) {
		end++
		batchBytes = len(texts[start])
	}
	return end, batchBytes
}

func embedCohereBatch(ctx context.Context, texts []string, cfg cohereQueryEmbedConfig) ([][]float32, int, error) {
	body, err := json.Marshal(map[string]any{
		"model":            cfg.model,
		"texts":            texts,
		"input_type":       "search_query",
		"embedding_types":  []string{"float"},
		"output_dimension": cfg.dim,
	})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ouroboros-product-brain-hosted/0.1")

	client := cfg.httpClient
	if client == nil {
		// Honor the caller deadline tightly (phase-A is normally <=2.5s).
		httpTO := 8 * time.Second
		if dl, ok := ctx.Deadline(); ok {
			if rem := time.Until(dl); rem > 0 && rem < httpTO {
				httpTO = rem + 50*time.Millisecond
			}
		}
		client = providerHTTPClient(httpTO)
	}
	ledgerFrom(ctx).attempt("embed", "cohere", cfg.model)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("cohere embed HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed cohereEmbedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, 0, err
	}
	if len(parsed.Embeddings.Float) != len(texts) {
		return nil, 0, fmt.Errorf("cohere embed row count %d, want %d", len(parsed.Embeddings.Float), len(texts))
	}
	rows := make([][]float32, len(parsed.Embeddings.Float))
	for rowIndex, values := range parsed.Embeddings.Float {
		if len(values) == 0 {
			return nil, 0, fmt.Errorf("cohere embed row %d is empty", rowIndex)
		}
		if len(values) != cfg.dim {
			return nil, 0, fmt.Errorf("cohere embed row %d dimension %d, want %d", rowIndex, len(values), cfg.dim)
		}
		row := make([]float32, len(values))
		for i, value := range values {
			row[i] = float32(value)
		}
		rows[rowIndex] = row
	}
	return rows, parsed.Meta.BilledUnits.InputTokens, nil
}

func truncateUTF8Bytes(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

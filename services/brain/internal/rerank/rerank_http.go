package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	zeroEntropyKeyEnv         = "ZEROENTROPY_API_KEY"
	zeroEntropyBaseURLEnv     = "OUROBOROS_ZE_BASE"
	zeroEntropyRerankModelEnv = "OUROBOROS_ZE_RERANK_MODEL"
	zeroEntropyDefaultBase    = "https://api.zeroentropy.dev/v1"
	zeroEntropyDefaultModel   = "zerank-2"
	zeroEntropyMaxDocChars    = 1500
	zeroEntropyMaxRespBytes   = 8 << 20
)

// HTTPReranker calls a ZeroEntropy-style POST /models/rerank endpoint.
type HTTPReranker struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// HTTPRerankerConfig configures HTTPReranker.
type HTTPRerankerConfig struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// NewHTTPReranker validates config and returns an HTTPReranker.
func NewHTTPReranker(cfg HTTPRerankerConfig) (*HTTPReranker, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, fmt.Errorf("rerank: empty ZeroEntropy API key")
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = zeroEntropyDefaultBase
	}
	base = strings.TrimRight(base, "/")
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = zeroEntropyDefaultModel
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &HTTPReranker{
		baseURL: base,
		apiKey:  key,
		model:   model,
		http:    client,
	}, nil
}

// NewHTTPRerankerFromEnv builds an HTTPReranker from ZEROENTROPY_API_KEY and
// optional OUROBOROS_ZE_BASE / OUROBOROS_ZE_RERANK_MODEL. Returns an error when
// the key is unset (callers should fall back to LexicalReranker).
func NewHTTPRerankerFromEnv() (*HTTPReranker, error) {
	key := firstNonEmpty(
		os.Getenv(zeroEntropyKeyEnv),
		os.Getenv("SENTRA_ZEROENTROPY_API_KEY"),
		os.Getenv("ZE_API_KEY"),
	)
	return NewHTTPReranker(HTTPRerankerConfig{
		APIKey:  key,
		BaseURL: os.Getenv(zeroEntropyBaseURLEnv),
		Model:   os.Getenv(zeroEntropyRerankModelEnv),
	})
}

type zeRerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      *int     `json:"top_n,omitempty"`
}

type zeRerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank posts query+docs to /models/rerank and returns ranked results.
func (r *HTTPReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]Ranked, error) {
	if r == nil || r.http == nil {
		return nil, fmt.Errorf("rerank: nil HTTP reranker")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	clipped := make([]string, len(docs))
	for i, d := range docs {
		if len(d) > zeroEntropyMaxDocChars {
			clipped[i] = d[:zeroEntropyMaxDocChars]
		} else {
			clipped[i] = d
		}
	}
	payloadBody := zeRerankRequest{
		Model:     r.model,
		Query:     query,
		Documents: clipped,
	}
	if topN > 0 {
		n := topN
		payloadBody.TopN = &n
	}
	payload, err := json.Marshal(payloadBody)
	if err != nil {
		return nil, fmt.Errorf("rerank: encode rerank request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.baseURL+"/models/rerank", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("rerank: build rerank request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ouroboros-brain-rerank/0.1")

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: rerank request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, zeroEntropyMaxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("rerank: read rerank response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rerank: rerank HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed zeRerankResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("rerank: decode rerank response: %w", err)
	}
	out := make([]Ranked, 0, len(parsed.Results))
	for _, row := range parsed.Results {
		if row.Index < 0 || row.Index >= len(docs) {
			return nil, fmt.Errorf("rerank: result index out of range: %d", row.Index)
		}
		out = append(out, Ranked{Index: row.Index, Score: row.RelevanceScore})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Index < out[j].Index
	})
	if topN > 0 && topN < len(out) {
		out = out[:topN]
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

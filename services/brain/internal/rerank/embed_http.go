package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	openAIKeyEnv            = "OPENAI_API_KEY"
	openAIBaseURLEnv        = "OUROBOROS_OPENAI_BASE_URL"
	openAIEmbedModelEnv     = "OUROBOROS_OPENAI_EMBED_MODEL"
	openAIDefaultBaseURL    = "https://api.openai.com"
	openAIDefaultEmbedModel = "text-embedding-3-small"
	openAIMaxResponseBytes  = 8 << 20
)

// HTTPEmbedder calls an OpenAI-compatible embeddings API.
type HTTPEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// HTTPEmbedderConfig configures HTTPEmbedder.
type HTTPEmbedderConfig struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// NewHTTPEmbedder validates config and returns an HTTPEmbedder.
func NewHTTPEmbedder(cfg HTTPEmbedderConfig) (*HTTPEmbedder, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, fmt.Errorf("rerank: empty OpenAI API key")
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = openAIDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = openAIDefaultEmbedModel
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &HTTPEmbedder{
		baseURL: base,
		apiKey:  key,
		model:   model,
		http:    client,
	}, nil
}

// EmbedIdentity returns the embedding identity (model, default dimension,
// normalization) so a bounded cache can scope entries to this provider's
// model identity. Unknown models report dimension 0; the cache treats a
// zero dimension as "unknown" and never reuses vectors across unknown
// embedders that happen to share the same text.
func (e *HTTPEmbedder) EmbedIdentity() Identity {
	if e == nil {
		return Identity{}
	}
	return Identity{
		Model:         e.model,
		Dimension:     openAIModelDimension(e.model),
		Normalization: "l2", // OpenAI embeddings are returned normalized in cosine use.
	}
}

// openAIModelDimension maps known OpenAI embedding model names to their
// canonical output dimension. Unknown models report 0 so the cache keys
// remain unique per model name without hard-coding future releases.
func openAIModelDimension(model string) int {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "text-embedding-3-small", "text-embedding-3-small-512":
		if strings.HasSuffix(strings.ToLower(model), "-512") {
			return 512
		}
		return 1536
	case "text-embedding-3-large":
		return 3072
	case "text-embedding-ada-002":
		return 1536
	default:
		return 0
	}
}

// NewHTTPEmbedderFromEnv builds an HTTPEmbedder from OPENAI_API_KEY and
// optional OUROBOROS_OPENAI_BASE_URL / OUROBOROS_OPENAI_EMBED_MODEL.
func NewHTTPEmbedderFromEnv() (*HTTPEmbedder, error) {
	return NewHTTPEmbedder(HTTPEmbedderConfig{
		APIKey:  os.Getenv(openAIKeyEnv),
		BaseURL: os.Getenv(openAIBaseURLEnv),
		Model:   os.Getenv(openAIEmbedModelEnv),
	})
}

type openAIEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed posts texts to /v1/embeddings. inputType is accepted for interface
// compatibility but ignored by the OpenAI embeddings shape.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string, inputType string) ([][]float32, error) {
	_ = inputType
	if e == nil || e.http == nil {
		return nil, fmt.Errorf("rerank: nil HTTP embedder")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(openAIEmbedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("rerank: encode embed request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.baseURL+"/v1/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("rerank: build embed request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ouroboros-brain-rerank/0.1")

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rerank: embed request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, openAIMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("rerank: read embed response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rerank: embed HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed openAIEmbedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("rerank: decode embed response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("rerank: embed count mismatch: got %d want %d", len(parsed.Data), len(texts))
	}
	// OpenAI may return data out of order; place by index when present.
	out := make([][]float32, len(texts))
	for _, row := range parsed.Data {
		idx := row.Index
		if idx < 0 || idx >= len(out) {
			return nil, fmt.Errorf("rerank: embed index out of range: %d", idx)
		}
		if out[idx] != nil {
			return nil, fmt.Errorf("rerank: duplicate embed index: %d", idx)
		}
		vec := make([]float32, len(row.Embedding))
		for i, v := range row.Embedding {
			vec[i] = float32(v)
		}
		out[idx] = vec
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("rerank: missing embed index: %d", i)
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

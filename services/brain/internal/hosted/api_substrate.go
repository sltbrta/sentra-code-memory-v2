package hosted

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

// API-side substrate names (llm / embed / ranker). Not product forks.
const (
	SubstrateAPIHosted = "hosted" // remote vendor defaults (Cohere/OpenAI/ZE)
	SubstrateAPIMLX    = "mlx"    // local MLX OpenAI-compatible server (only local backend for now)
	SubstrateAPINone   = "none"   // extractive / bag / lexical CE
)

// mlxEnv reads the standalone setting first and keeps the source-compatible
// legacy setting as a fallback for existing local brain scripts.
func mlxEnv(standalone, legacy string) string {
	if value := strings.TrimSpace(os.Getenv(standalone)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(legacy))
}

// mlxBaseURL returns BYOC base for local MLX (OpenAI-compatible /v1).
func mlxBaseURL() string {
	u := mlxEnv("SENTRA_CODE_MEMORY_MLX_BASE_URL", "OUROBOROS_BRAIN_MLX_BASE_URL")
	if u == "" {
		u = strings.TrimSpace(os.Getenv("MLX_BASE_URL"))
	}
	if u == "" {
		return "http://127.0.0.1:8080/v1"
	}
	return strings.TrimRight(u, "/")
}

func mlxAPIKey() string {
	k := mlxEnv("SENTRA_CODE_MEMORY_MLX_API_KEY", "OUROBOROS_BRAIN_MLX_API_KEY")
	if k == "" {
		k = strings.TrimSpace(os.Getenv("MLX_API_KEY"))
	}
	if k == "" {
		return "not-needed" // many local servers ignore auth; still send Bearer for BYOK shape
	}
	return k
}

type openAIEmbedCfg struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

func mlxEmbedConfig() openAIEmbedCfg {
	model := mlxEnv("SENTRA_CODE_MEMORY_MLX_EMBED_MODEL", "OUROBOROS_BRAIN_MLX_EMBED_MODEL")
	if model == "" {
		model = "mlx-community/Qwen3-VL-Embedding-2B-4bit"
	}
	return openAIEmbedCfg{
		BaseURL: mlxBaseURL(),
		APIKey:  mlxAPIKey(),
		Model:   model,
		Timeout: 30 * time.Second,
	}
}

// openaiEmbedConfig is remote OpenAI-compatible embeddings (BYOK OPENAI_API_KEY).
func openaiEmbedConfig() openAIEmbedCfg {
	base := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_OPENAI_EMBED_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OPENAI_EMBED_MODEL"))
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	return openAIEmbedCfg{
		BaseURL: strings.TrimRight(base, "/"),
		APIKey:  strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		Model:   model,
		Timeout: 30 * time.Second,
	}
}

func mlxChatConfig() openAIEmbedCfg {
	model := mlxEnv("SENTRA_CODE_MEMORY_MLX_CHAT_MODEL", "OUROBOROS_BRAIN_MLX_CHAT_MODEL")
	if model == "" {
		model = "mlx-community/LFM2.5-VL-1.6B-8bit"
	}
	return openAIEmbedCfg{
		BaseURL: mlxBaseURL(),
		APIKey:  mlxAPIKey(),
		Model:   model,
		Timeout: 90 * time.Second,
	}
}

func mlxRankModel() string {
	model := mlxEnv("SENTRA_CODE_MEMORY_MLX_RANK_MODEL", "OUROBOROS_BRAIN_MLX_RANK_MODEL")
	if model == "" {
		model = "mlx-community/Qwen3-VL-Reranker-2B-4bit"
	}
	return model
}

func mlxChatFallbackModel() string {
	model := mlxEnv("SENTRA_CODE_MEMORY_MLX_CHAT_FALLBACK_MODEL", "OUROBOROS_BRAIN_MLX_CHAT_FALLBACK_MODEL")
	if model == "" {
		model = "mlx-community/gemma-4-e2b-it-4bit"
	}
	return model
}

// embedOpenAICompatible calls POST {base}/embeddings (OpenAI shape) for MLX/BYOC.
func embedOpenAICompatible(ctx context.Context, text string, cfg openAIEmbedCfg) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("hosted: empty embed text")
	}
	if len(text) > 8000 {
		text = text[:8000]
	}
	body, _ := json.Marshal(map[string]any{
		"model": cfg.Model,
		"input": text,
	})
	url := cfg.BaseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := providerHTTPClient(timeout)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mlx/openai embed HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("mlx/openai embed empty")
	}
	out := make([]float32, len(parsed.Data[0].Embedding))
	for i, v := range parsed.Data[0].Embedding {
		out[i] = float32(v)
	}
	return out, nil
}

// mlxRerank calls a Cohere-compatible or simple {query,documents} → scores API
// on the MLX BYOC base (POST /rerank). Falls back with error for lexical CE.
func mlxRerank(ctx context.Context, question string, passages []Passage, topN int) ([]Passage, error) {
	if len(passages) == 0 {
		return passages, nil
	}
	results, err := mlxRerankResults(ctx, mlxRankModel(), question, passages, topN)
	if err != nil {
		return nil, err
	}
	return assembleRemoteRerank(passages, results, "mlx")
}

func mlxRerankResults(ctx context.Context, model, question string, passages []Passage, topN int) ([]remoteRerankResult, error) {
	if len(passages) == 0 {
		return nil, nil
	}
	docs := make([]string, len(passages))
	for i, p := range passages {
		docs[i] = clippedRerankText(p.Text, "mlx")
	}
	body, _ := json.Marshal(map[string]any{
		"model":     model,
		"query":     question,
		"documents": docs,
		"top_n":     topN,
	})
	url := mlxBaseURL() + "/rerank"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+mlxAPIKey())
	req.Header.Set("Content-Type", "application/json")
	client := providerHTTPClient(60 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mlx rerank HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	// Accept Cohere-like results: [{index, relevance_score}].
	var parsed struct {
		Results []remoteRerankResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Results, nil
}

// resolveAPISubstrate picks hosted|mlx|none from env/config with hosted preference.
func resolveAPISubstrate(explicit, envKey string) string {
	v := strings.ToLower(strings.TrimSpace(explicit))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(os.Getenv(envKey)))
	}
	switch v {
	case SubstrateAPIMLX, "local":
		return SubstrateAPIMLX
	case SubstrateAPINone, "off", "extractive":
		return SubstrateAPINone
	case SubstrateAPIHosted, "remote", "cloud":
		return SubstrateAPIHosted
	case "":
		// Prefer hosted when remote keys exist; else none (not mlx — mlx needs a server).
		if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("COHERE_API_KEY") != "" ||
			os.Getenv("CO_API_KEY") != "" || os.Getenv("ZEROENTROPY_API_KEY") != "" {
			return SubstrateAPIHosted
		}
		return SubstrateAPINone
	default:
		return v
	}
}

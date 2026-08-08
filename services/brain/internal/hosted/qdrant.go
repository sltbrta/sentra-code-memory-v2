package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Hit is one retrieved chunk/document.
type Hit struct {
	ChunkID   string
	DSID      string
	Text      string
	SourceURI string
	Score     float64
	Channel   string
}

// residualQdrantDense binds Qdrant HTTP as residual denseBackend (dense=qdrant).
type residualQdrantDense struct {
	cfg Config
}

func (q *residualQdrantDense) Close() error { return nil }

func (q *residualQdrantDense) Upsert(points []DensePoint) error {
	if q == nil {
		return fmt.Errorf("hosted: nil qdrant dense")
	}
	return upsertDenseHTTP(context.Background(), q.cfg, points)
}

func (q *residualQdrantDense) Search(query denseQuery, topK int) (denseSearchResult, error) {
	if q == nil {
		return denseSearchResult{}, fmt.Errorf("hosted: nil qdrant dense")
	}
	cfg := q.cfg
	if topK > 0 {
		cfg.DenseLimit = topK
	}
	hits, err := denseSearch(context.Background(), cfg, query.Vector)
	return denseSearchResult{Hits: hits, Diagnostics: denseRemoteDiagnostics("qdrant")}, err
}

var _ denseBackend = (*residualQdrantDense)(nil)

// UpsertDense upserts dense points into the bound dense substrate
// (local sqlite/postgres/faiss or residual Qdrant). Errors when qdrant keys missing
// and no localDense (no silent no-op for residual).
func (c *Client) UpsertDense(ctx context.Context, points []DensePoint) error {
	if c == nil {
		return fmt.Errorf("hosted: nil client")
	}
	if len(points) == 0 {
		return nil
	}
	if c.localDense != nil {
		return c.localDense.Upsert(points)
	}
	cfg := c.cfg
	if strings.TrimSpace(cfg.QdrantURL) == "" || strings.TrimSpace(cfg.QdrantAPIKey) == "" {
		return fmt.Errorf("hosted: UpsertDense: no dense backend (bind dense=sqlite|postgres|faiss|qdrant)")
	}
	return upsertDenseHTTP(ctx, cfg, points)
}

func upsertDenseHTTP(ctx context.Context, cfg Config, points []DensePoint) error {
	type qPoint struct {
		ID      any            `json:"id"`
		Vector  map[string]any `json:"vector"`
		Payload map[string]any `json:"payload,omitempty"`
	}
	qPoints := make([]qPoint, 0, len(points))
	for _, p := range points {
		if len(p.Vector) == 0 {
			continue
		}
		id := any(p.ID)
		// Prefer numeric string ids as strings; Qdrant accepts string UUIDs.
		if id == "" {
			continue
		}
		payload := p.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		if _, ok := payload["brain_id"]; !ok && cfg.BrainID != "" {
			payload["brain_id"] = cfg.BrainID
		}
		qPoints = append(qPoints, qPoint{
			ID: id,
			Vector: map[string]any{
				"name":   cfg.ChunkVectorName,
				"vector": p.Vector,
			},
			Payload: payload,
		})
	}
	if len(qPoints) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"points": qPoints,
	})
	if err != nil {
		return err
	}
	url := strings.TrimRight(cfg.QdrantURL, "/") + "/collections/" + cfg.ChunkCollection + "/points?wait=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", cfg.QdrantAPIKey)
	client := providerHTTPClient(8 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant upsert HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return nil
}

// denseSearch queries Qdrant ANN for the brain-scoped chunk collection.
func denseSearch(ctx context.Context, cfg Config, vector []float32) ([]Hit, error) {
	if len(vector) == 0 {
		return nil, fmt.Errorf("empty query vector")
	}
	payload := map[string]any{
		"vector": map[string]any{
			"name":   cfg.ChunkVectorName,
			"vector": vector,
		},
		"limit":        cfg.DenseLimit,
		"with_payload": true,
		"filter": map[string]any{
			"must": []map[string]any{
				{
					"key": "brain_id",
					"match": map[string]any{
						"value": cfg.BrainID,
					},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := cfg.QdrantURL + "/collections/" + cfg.ChunkCollection + "/points/search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", cfg.QdrantAPIKey)
	client := providerHTTPClient(8 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("qdrant HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Result []struct {
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(parsed.Result))
	for _, r := range parsed.Result {
		p := r.Payload
		if p == nil {
			p = map[string]any{}
		}
		hits = append(hits, Hit{
			ChunkID:   strField(p, "chunk_id"),
			DSID:      strField(p, "dsid"),
			Text:      strField(p, "text_content"),
			SourceURI: strField(p, "source_uri"),
			Score:     r.Score,
			Channel:   "dense",
		})
	}
	return hits, nil
}

func strField(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

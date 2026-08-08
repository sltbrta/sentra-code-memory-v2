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

// faissDense is a BYOC HTTP client for a FAISS/usearch-style ANN sidecar.
// Expected API (minimal):
//
//	POST {base}/upsert  {"points":[{"id":"…","vector":[…],"payload":{…}}]}
//	POST {base}/search  {"vector":[…],"top_k":N} → {"hits":[{"id":"…","score":0.9,"payload":{…}}]}
//
// No CGo FAISS in-process; operators run their own sidecar (or skip and use sqlite).
type faissDense struct {
	base   string
	client *http.Client
}

func openFAISSDense(base string) *faissDense {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	return &faissDense{
		base:   base,
		client: providerHTTPClient(30 * time.Second),
	}
}

func (f *faissDense) Close() error { return nil }

func (f *faissDense) Upsert(points []DensePoint) error {
	if f == nil || f.base == "" {
		return fmt.Errorf("hosted: nil faiss dense")
	}
	type pt struct {
		ID      string         `json:"id"`
		Vector  []float32      `json:"vector"`
		Payload map[string]any `json:"payload,omitempty"`
	}
	body := make([]pt, 0, len(points))
	for _, p := range points {
		if p.ID == "" || len(p.Vector) == 0 {
			continue
		}
		body = append(body, pt{ID: p.ID, Vector: p.Vector, Payload: p.Payload})
	}
	if len(body) == 0 {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"points": body})
	req, err := http.NewRequest(http.MethodPost, f.base+"/upsert", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("faiss upsert HTTP %d", resp.StatusCode)
	}
	return nil
}

func (f *faissDense) Search(query denseQuery, topK int) (denseSearchResult, error) {
	if f == nil || f.base == "" {
		return denseSearchResult{}, fmt.Errorf("hosted: nil faiss dense")
	}
	if topK <= 0 {
		topK = 20
	}
	raw, _ := json.Marshal(map[string]any{
		"vector": query.Vector, "top_k": topK, "model_id": query.ModelID,
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.base+"/search", bytes.NewReader(raw))
	if err != nil {
		return denseSearchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return denseSearchResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return denseSearchResult{}, fmt.Errorf("faiss search HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Hits []struct {
			ID      string         `json:"id"`
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return denseSearchResult{}, err
	}
	out := make([]Hit, 0, len(parsed.Hits))
	for _, h := range parsed.Hits {
		dsid := strField(h.Payload, "document_id")
		if dsid == "" {
			dsid = strField(h.Payload, "dsid")
		}
		if dsid == "" {
			dsid = h.ID
		}
		chunkID := strField(h.Payload, "chunk_id")
		if chunkID == "" {
			chunkID = h.ID + "#0"
		}
		out = append(out, Hit{
			ChunkID:   chunkID,
			DSID:      dsid,
			SourceURI: strField(h.Payload, "source_uri"),
			Score:     h.Score,
			Channel:   "dense_faiss",
		})
	}
	return denseSearchResult{Hits: out, Diagnostics: denseRemoteDiagnostics("faiss_http")}, nil
}

var _ denseBackend = (*faissDense)(nil)

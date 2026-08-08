package hosted

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
)

// hnswDense is in-process pure-Go HNSW (FAISS-class local ANN, no CGo).
// Used when dense=faiss without DENSE_URL (or as primary local faiss substrate).
type hnswDense struct {
	mu   sync.RWMutex
	idx  *dense.HNSW
	path string // optional persistence under Dir/dense.hnsw
	gen  string
	mode dense.SearchMode
}

func openHNSWDense(dir, brainID string, modes ...dense.SearchMode) (*hnswDense, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("hosted: hnsw dense requires Dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "dense.hnsw")
	h := &hnswDense{path: path, gen: strings.TrimSpace(brainID), mode: dense.SearchModeAuto}
	if len(modes) > 0 && modes[0] != "" {
		h.mode = modes[0]
	}
	if h.gen == "" {
		h.gen = "local"
	}
	if _, err := os.Stat(path); err == nil {
		idx, err := dense.LoadHNSW(path)
		if err == nil && idx != nil && (idx.Identity().Scope == "" || idx.Identity().Scope == h.gen) {
			h.idx = idx
			return h, nil
		}
	}
	return h, nil
}

func (h *hnswDense) Close() error {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.idx == nil {
		return nil
	}
	return nil
}

func (h *hnswDense) Upsert(points []DensePoint) error {
	if h == nil {
		return fmt.Errorf("hosted: nil hnsw dense")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	candidate := h.idx.Clone()
	for _, p := range points {
		id := strings.TrimSpace(p.ID)
		if id == "" || len(p.Vector) == 0 {
			continue
		}
		docID, chunkID, sourceURI := id, id+"#0", ""
		if p.Payload != nil {
			if v, ok := p.Payload["document_id"].(string); ok && strings.TrimSpace(v) != "" {
				docID = strings.TrimSpace(v)
			} else if v, ok := p.Payload["dsid"].(string); ok && strings.TrimSpace(v) != "" {
				docID = strings.TrimSpace(v)
			}
			if v, ok := p.Payload["chunk_id"].(string); ok && strings.TrimSpace(v) != "" {
				chunkID = strings.TrimSpace(v)
			}
			if v, ok := p.Payload["source_uri"].(string); ok {
				sourceURI = strings.TrimSpace(v)
			}
		}
		modelID := strings.TrimSpace(p.ModelID)
		if modelID == "" {
			return fmt.Errorf("hosted: hnsw point %q missing embedding model identity", id)
		}
		identity := dense.IndexIdentity{Scope: h.gen, Model: modelID, Dimensions: len(p.Vector)}
		if candidate == nil {
			candidate = dense.NewScopedHNSW(identity, 16, 64)
		} else {
			// Pre-identity HNSW files have vectors and dimensions but blank scope
			// and model fields. Bind those fields on the cloned candidate so the
			// durable upgrade is atomic and the old in-memory index remains intact
			// if validation or Save fails.
			if err := candidate.UpgradeLegacyIdentity(identity); err != nil {
				return fmt.Errorf("hosted: hnsw projection identity changed: %w", err)
			}
			if actual := candidate.Identity(); !sameProjectionIdentity(actual, identity) {
				return fmt.Errorf("hosted: hnsw projection identity changed: got %+v want %+v", identity, actual)
			}
		}
		if err := candidate.UpsertWithMetadata(id, p.Vector, dense.HitMetadata{
			DocumentID: docID, ChunkID: chunkID, SourceURI: sourceURI,
		}); err != nil {
			return err
		}
	}
	if candidate != nil && h.path != "" {
		if err := candidate.Save(h.path); err != nil {
			return err
		}
	}
	h.idx = candidate
	return nil
}

func (h *hnswDense) Search(query denseQuery, topK int) (denseSearchResult, error) {
	if h == nil {
		return denseSearchResult{Diagnostics: denseMissingDiagnostics("missing")}, fmt.Errorf("hosted: nil hnsw dense")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.idx == nil {
		return denseSearchResult{Diagnostics: denseMissingDiagnostics("missing")}, nil
	}
	dh, diag, err := h.idx.SearchScopedMode(query.Vector, topK, dense.IndexIdentity{
		Scope: h.gen, Model: query.ModelID, Dimensions: len(query.Vector),
	}, h.mode)
	if err != nil {
		return denseSearchResult{Diagnostics: diag}, err
	}
	out := make([]Hit, 0, len(dh))
	for _, x := range dh {
		out = append(out, Hit{
			ChunkID:   x.ChunkID,
			DSID:      x.DocumentID,
			SourceURI: x.SourceURI,
			Score:     x.Score,
			Channel:   "dense_hnsw",
		})
	}
	return denseSearchResult{Hits: out, Diagnostics: diag}, nil
}

var _ denseBackend = (*hnswDense)(nil)

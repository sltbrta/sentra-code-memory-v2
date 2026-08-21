package hosted

import (
	"fmt"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
)

// A deletion could not reach the dense vectors.
//
// deletion.Purge fanned out to the corpus, the lexical index, the memory
// cortex and the query log, and named `vectors` as skipped because none of the
// five backends behind denseBackend exposed a delete. Erased content stayed
// answerable by vector search after every other substrate had dropped it.
//
// Three of the five can now purge and be verified here: the in-memory store,
// the HNSW index, and the SQLite-backed local projection. Postgres implements
// the same SQL as its writer. FAISS and Qdrant return ErrPurgeUnsupported:
// they are remote services whose delete surfaces this repository cannot
// exercise, and an HTTP call against an endpoint nobody here can confirm --
// reported as an erasure -- is the overclaiming this branch exists to remove.

// DeleteDocuments removes the documents' vectors from the SQLite projection
// and the in-process ANN index, and persists the result.
//
// Both must be purged. The ANN index is what answers a search, and the SQL
// rows are what it is rebuilt from, so removing one and not the other either
// leaves the content answerable now or restores it on the next load.
func (d *localDense) DeleteDocuments(docIDs []string) (int, error) {
	if d == nil || d.store == nil {
		return 0, fmt.Errorf("hosted: nil local dense")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	removed, err := d.store.DeleteDocuments(docIDs)
	if err != nil {
		return removed, err
	}
	if d.ann != nil {
		d.ann.DeleteDocuments(docIDs)
		if d.annPath != "" && d.saveANN != nil {
			if err := d.saveANN(d.ann, d.annPath); err != nil {
				// The rows are gone; the persisted index still holds the
				// vectors and would restore them on the next load. That is a
				// failed purge, not a partial success.
				return removed, fmt.Errorf("hosted: persist purged ann: %w", err)
			}
		}
	}
	return removed, nil
}

// HasDocuments reports what either the SQL rows or the ANN index still holds.
func (d *localDense) HasDocuments(docIDs []string) ([]string, error) {
	if d == nil || d.store == nil {
		return nil, fmt.Errorf("hosted: nil local dense")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	found, err := d.store.HasDocuments(docIDs)
	if err != nil {
		return nil, err
	}
	if d.ann == nil {
		return found, nil
	}
	return mergeDocumentIDs(found, d.ann.HasDocuments(docIDs)), nil
}

// DeleteDocuments removes the documents' vectors from the ANN index and
// persists it.
func (h *hnswDense) DeleteDocuments(docIDs []string) (int, error) {
	if h == nil || h.idx == nil {
		return 0, fmt.Errorf("hosted: nil hnsw dense")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	removed := h.idx.DeleteDocuments(docIDs)
	if removed > 0 && h.path != "" {
		if err := h.idx.Save(h.path); err != nil {
			return removed, fmt.Errorf("hosted: persist purged hnsw: %w", err)
		}
	}
	return removed, nil
}

// HasDocuments reports what the ANN index still holds.
func (h *hnswDense) HasDocuments(docIDs []string) ([]string, error) {
	if h == nil || h.idx == nil {
		return nil, fmt.Errorf("hosted: nil hnsw dense")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.idx.HasDocuments(docIDs), nil
}

// DeleteDocuments removes the documents' rows from the Postgres projection.
//
// It matches on dsid with document_id as a fallback, mirroring what the writer
// records, rather than a LIKE on the vector id -- "doc-1%" would claim
// "doc-10#0".
func (d *postgresDense) DeleteDocuments(docIDs []string) (int, error) {
	if d == nil || d.db == nil {
		return 0, fmt.Errorf("hosted: nil postgres dense")
	}
	total := 0
	for _, docID := range docIDs {
		if docID == "" {
			continue
		}
		result, err := d.db.Exec(
			`DELETE FROM dense_vectors WHERE dsid = $1 OR document_id = $1`, docID)
		if err != nil {
			return total, fmt.Errorf("hosted: delete postgres dense vectors: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			total += int(n)
		}
	}
	return total, nil
}

// HasDocuments returns the document ids that still have rows.
func (d *postgresDense) HasDocuments(docIDs []string) ([]string, error) {
	if d == nil || d.db == nil {
		return nil, fmt.Errorf("hosted: nil postgres dense")
	}
	var found []string
	for _, docID := range docIDs {
		if docID == "" {
			continue
		}
		var count int
		if err := d.db.QueryRow(
			`SELECT COUNT(1) FROM dense_vectors WHERE dsid = $1 OR document_id = $1`,
			docID).Scan(&count); err != nil {
			return nil, fmt.Errorf("hosted: count postgres dense vectors: %w", err)
		}
		if count > 0 {
			found = append(found, docID)
		}
	}
	return found, nil
}

// DeleteDocuments is unsupported for the FAISS sidecar. See
// ErrPurgeUnsupported: its delete surface cannot be exercised from this
// repository, and reporting an unverified HTTP call as an erasure is worse
// than a named gap.
func (f *faissDense) DeleteDocuments([]string) (int, error) {
	return 0, ErrPurgeUnsupported
}

// HasDocuments is unsupported for the FAISS sidecar.
func (f *faissDense) HasDocuments([]string) ([]string, error) {
	return nil, ErrPurgeUnsupported
}

// DeleteDocuments is unsupported for Qdrant. See ErrPurgeUnsupported.
func (q *residualQdrantDense) DeleteDocuments([]string) (int, error) {
	return 0, ErrPurgeUnsupported
}

// HasDocuments is unsupported for Qdrant.
func (q *residualQdrantDense) HasDocuments([]string) ([]string, error) {
	return nil, ErrPurgeUnsupported
}

// mergeDocumentIDs unions two residual sets without duplicating.
func mergeDocumentIDs(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, set := range [][]string{a, b} {
		for _, id := range set {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// compile-time assertion that the ANN type used above still exposes what this
// file depends on.
var _ interface {
	DeleteDocuments([]string) int
	HasDocuments([]string) []string
} = (*dense.HNSW)(nil)

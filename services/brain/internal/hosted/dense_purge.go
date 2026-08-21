package hosted

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// A deletion could not reach the dense vectors.
//
// deletion.Purge fanned out to the corpus, the lexical index, the memory
// cortex and the query log, and named `vectors` as skipped because none of the
// five backends behind denseBackend exposed a delete. Erased content stayed
// answerable by vector search after every other substrate had dropped it.
//
// All five now purge. Three are exercised directly here -- the in-memory
// store, the HNSW index, and the SQLite-backed local projection -- Postgres
// implements the same SQL as its writer, and the two remote ones are
// implemented against their documented APIs and exercised against fakes.

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

// The two remote backends were left returning ErrPurgeUnsupported, on the
// grounds that shipping an erasure path this repository cannot exercise is
// worse than a named gap. That reasoning missed something.
//
// The fan-out verifies by *re-querying* each substrate after the delete. A
// wrong implementation therefore surfaces as a leak -- the ids come back, the
// receipt reports them, and VerifiedComplete stays false -- rather than as a
// silent success. Refusing to try leaves a deployment permanently unable to
// erase; trying, against a self-checking fan-out, can only report honestly.
//
// So both are implemented against their documented APIs and exercised against
// fakes that speak them. What is *not* claimed is that either has run against
// a live server; if an endpoint is wrong, the verification pass is what says
// so, which is why it is safe to have tried.

// DeleteDocuments removes a document's vectors from the FAISS sidecar.
//
// The sidecar's documented surface is upsert and search (see the type's own
// comment). Delete is the same shape: a POST carrying the ids to remove.
func (f *faissDense) DeleteDocuments(docIDs []string) (int, error) {
	if f == nil || f.base == "" {
		return 0, fmt.Errorf("hosted: nil faiss dense")
	}
	ids := trimmedIDs(docIDs)
	if len(ids) == 0 {
		return 0, nil
	}
	var reply struct {
		Deleted int `json:"deleted"`
	}
	if err := f.postJSON("/delete", map[string]any{"document_ids": ids}, &reply); err != nil {
		return 0, err
	}
	return reply.Deleted, nil
}

// HasDocuments asks the sidecar which of the ids it still holds.
func (f *faissDense) HasDocuments(docIDs []string) ([]string, error) {
	// Configuration is checked before the empty-input short-circuit, so an
	// empty probe still reports an unreachable backend. The fan-out uses
	// exactly that probe to decide whether to wire this as a purger, and a
	// backend that answers "nothing to do" while being unconfigured would be
	// wired and then report zero removals as an erasure.
	if f == nil || f.base == "" {
		return nil, fmt.Errorf("hosted: faiss dense not configured")
	}
	ids := trimmedIDs(docIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	var reply struct {
		DocumentIDs []string `json:"document_ids"`
	}
	if err := f.postJSON("/documents", map[string]any{"document_ids": ids}, &reply); err != nil {
		return nil, err
	}
	sort.Strings(reply.DocumentIDs)
	return reply.DocumentIDs, nil
}

// postJSON posts a body to one of the sidecar's endpoints and decodes the
// reply. A non-2xx is an error rather than an empty result, so a sidecar that
// does not implement the endpoint reports a failed purge instead of a
// successful one.
func (f *faissDense) postJSON(path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, f.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("faiss %s HTTP %d: %s", path, resp.StatusCode,
			textbound.Bytes(string(payload), 200))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// DeleteDocuments removes a document's points from the Qdrant collection.
//
// Points are deleted by filter on the `dsid` payload field, which is the
// document identity the writer records and the search path reads back. Deleting
// by point id would require knowing every chunk id, which the caller does not
// have and the collection does.
func (q *residualQdrantDense) DeleteDocuments(docIDs []string) (int, error) {
	if q == nil {
		return 0, fmt.Errorf("hosted: nil qdrant dense")
	}
	ids := trimmedIDs(docIDs)
	if len(ids) == 0 {
		return 0, nil
	}
	body := map[string]any{"filter": qdrantDocumentFilter(ids)}
	var reply struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := qdrantPost(q.cfg, "/points/delete?wait=true", body, &reply); err != nil {
		return 0, err
	}
	// Qdrant reports an operation status rather than a row count. The count is
	// not the evidence here -- the verification pass below is.
	return len(ids), nil
}

// HasDocuments counts the points still matching each document.
func (q *residualQdrantDense) HasDocuments(docIDs []string) ([]string, error) {
	// See the FAISS note above: configuration before the empty short-circuit,
	// because the fan-out probes with an empty list to decide reachability.
	if q == nil {
		return nil, fmt.Errorf("hosted: nil qdrant dense")
	}
	if strings.TrimSpace(q.cfg.QdrantURL) == "" || strings.TrimSpace(q.cfg.ChunkCollection) == "" {
		return nil, fmt.Errorf("hosted: qdrant not configured")
	}
	ids := trimmedIDs(docIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	var found []string
	for _, id := range ids {
		body := map[string]any{"filter": qdrantDocumentFilter([]string{id}), "exact": true}
		var reply struct {
			Result struct {
				Count int `json:"count"`
			} `json:"result"`
		}
		if err := qdrantPost(q.cfg, "/points/count", body, &reply); err != nil {
			return nil, err
		}
		if reply.Result.Count > 0 {
			found = append(found, id)
		}
	}
	sort.Strings(found)
	return found, nil
}

// qdrantDocumentFilter matches any point whose dsid is one of the ids.
func qdrantDocumentFilter(ids []string) map[string]any {
	values := make([]any, 0, len(ids))
	for _, id := range ids {
		values = append(values, id)
	}
	return map[string]any{
		"must": []any{
			map[string]any{
				"key":   "dsid",
				"match": map[string]any{"any": values},
			},
		},
	}
}

// qdrantPost posts to a collection endpoint and decodes the reply.
func qdrantPost(cfg Config, path string, body any, out any) error {
	base := strings.TrimRight(strings.TrimSpace(cfg.QdrantURL), "/")
	if base == "" || strings.TrimSpace(cfg.ChunkCollection) == "" {
		return fmt.Errorf("hosted: qdrant not configured")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := base + "/collections/" + cfg.ChunkCollection + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", cfg.QdrantAPIKey)
	resp, err := providerHTTPClient(8 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant %s HTTP %d: %s", path, resp.StatusCode,
			textbound.Bytes(string(payload), 200))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// trimmedIDs drops blanks from a purge target list.
func trimmedIDs(docIDs []string) []string {
	out := make([]string, 0, len(docIDs))
	for _, id := range docIDs {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
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

package projections

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
)

// ExactDenseFallbackLimit is the largest corpus that may use the exact
// SQLite fallback when its ANN projection is absent or incompatible.
const ExactDenseFallbackLimit = 512

// ExactFallbackLimitError prevents a missing ANN projection from degrading
// into an unbounded query-time vector scan.
type ExactFallbackLimitError struct {
	Scope string
	Limit int
}

func (e *ExactFallbackLimitError) Error() string {
	return fmt.Sprintf("projections: dense ANN index unavailable and exact fallback exceeds %d vectors", e.Limit)
}

// SQLDenseStore persists generation-scoped dense vectors as little-endian float32 blobs.
type SQLDenseStore struct {
	DB *sql.DB
}

// DenseVector is one durable vector plus the evidence identity that must
// survive ANN projection and hydration. VectorID is the ANN key (often a
// chunk id); DocumentID is the source DSID.
type DenseVector struct {
	VectorID   string
	DocumentID string
	ChunkID    string
	SourceURI  string
	Vector     []float32
}

// SnapshotScoped returns a canonical, ordered projection snapshot and its
// SHA-256 content digest. The digest changes for vector or evidence metadata
// replacement even when the row count does not.
func (s *SQLDenseStore) SnapshotScoped(identity dense.IndexIdentity) ([]DenseVector, string, error) {
	if s == nil || s.DB == nil {
		return nil, "", fmt.Errorf("projections: nil dense store")
	}
	rows, err := s.DB.Query(`
SELECT document_id, dsid, chunk_id, source_uri, embedding
FROM dense_vectors
WHERE generation_id=? AND model_id=? AND dim=?
ORDER BY document_id`, identity.Scope, identity.Model, identity.Dimensions)
	if err != nil {
		return nil, "", fmt.Errorf("projections: snapshot dense: %w", err)
	}
	defer rows.Close()
	h := sha256.New()
	writeDigestString(h, identity.Scope)
	writeDigestString(h, identity.Model)
	var dim [8]byte
	binary.LittleEndian.PutUint64(dim[:], uint64(identity.Dimensions))
	_, _ = h.Write(dim[:])
	var out []DenseVector
	for rows.Next() {
		var vectorID, dsid, chunkID, sourceURI string
		var blob []byte
		if err := rows.Scan(&vectorID, &dsid, &chunkID, &sourceURI, &blob); err != nil {
			return nil, "", fmt.Errorf("projections: scan dense snapshot: %w", err)
		}
		vec, err := unpackFloat32LE(blob, identity.Dimensions)
		if err != nil {
			return nil, "", err
		}
		if dsid == "" {
			dsid = vectorID
		}
		if chunkID == "" {
			chunkID = vectorID + "#0"
		}
		for _, value := range []string{vectorID, dsid, chunkID, sourceURI} {
			writeDigestString(h, value)
		}
		writeDigestBytes(h, blob)
		out = append(out, DenseVector{
			VectorID: vectorID, DocumentID: dsid, ChunkID: chunkID,
			SourceURI: sourceURI, Vector: vec,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("projections: iterate dense snapshot: %w", err)
	}
	return out, hex.EncodeToString(h.Sum(nil)), nil
}

func writeDigestString(h hash.Hash, value string) { writeDigestBytes(h, []byte(value)) }

func writeDigestBytes(h hash.Hash, value []byte) {
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(value)
}

// Upsert stores or replaces the embedding for (generationID, documentID).
func (s *SQLDenseStore) Upsert(generationID, documentID string, vec []float32) error {
	return s.UpsertScoped(dense.IndexIdentity{
		Scope: generationID, Model: "legacy", Dimensions: len(vec),
	}, documentID, vec)
}

// UpsertScoped stores a vector under a pinned scope/model/dimension identity.
func (s *SQLDenseStore) UpsertScoped(identity dense.IndexIdentity, documentID string, vec []float32) error {
	return s.UpsertVectorScoped(identity, DenseVector{
		VectorID: documentID, DocumentID: documentID, ChunkID: documentID + "#0", Vector: vec,
	})
}

// UpsertVectorScoped stores a vector and its original chunk/source metadata.
func (s *SQLDenseStore) UpsertVectorScoped(identity dense.IndexIdentity, point DenseVector) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("projections: nil dense store")
	}
	if identity.Scope == "" {
		return fmt.Errorf("projections: empty scope")
	}
	if identity.Model == "" {
		return fmt.Errorf("projections: empty embedding model")
	}
	if point.VectorID == "" {
		return fmt.Errorf("projections: empty document id")
	}
	if point.DocumentID == "" {
		point.DocumentID = point.VectorID
	}
	if point.ChunkID == "" {
		point.ChunkID = point.VectorID + "#0"
	}
	if len(point.Vector) == 0 {
		return fmt.Errorf("projections: empty vector")
	}
	if identity.Dimensions != len(point.Vector) {
		return &dense.IdentityError{
			Field: "dimensions", Expected: fmt.Sprint(identity.Dimensions), Actual: fmt.Sprint(len(point.Vector)),
		}
	}
	blob := packFloat32LE(point.Vector)
	_, err := s.DB.Exec(`
INSERT INTO dense_vectors (generation_id, document_id, model_id, dim, embedding, chunk_id, dsid, source_uri)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(generation_id, document_id) DO UPDATE SET
	model_id = excluded.model_id,
  dim = excluded.dim,
	embedding = excluded.embedding,
	chunk_id = excluded.chunk_id,
	dsid = excluded.dsid,
	source_uri = excluded.source_uri`,
		identity.Scope, point.VectorID, identity.Model, len(point.Vector), blob,
		point.ChunkID, point.DocumentID, point.SourceURI)
	if err != nil {
		return fmt.Errorf("projections: upsert dense: %w", err)
	}
	return nil
}

// UpsertVectorsScoped commits a same-identity batch atomically.
func (s *SQLDenseStore) UpsertVectorsScoped(identity dense.IndexIdentity, points []DenseVector) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("projections: nil dense store")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("projections: begin dense batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, point := range points {
		if point.VectorID == "" || len(point.Vector) == 0 {
			continue
		}
		if point.DocumentID == "" {
			point.DocumentID = point.VectorID
		}
		if point.ChunkID == "" {
			point.ChunkID = point.VectorID + "#0"
		}
		if identity.Scope == "" || identity.Model == "" || identity.Dimensions != len(point.Vector) {
			return fmt.Errorf("projections: invalid dense batch identity")
		}
		_, err = tx.Exec(`
INSERT INTO dense_vectors (generation_id, document_id, model_id, dim, embedding, chunk_id, dsid, source_uri)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(generation_id, document_id) DO UPDATE SET
	model_id=excluded.model_id, dim=excluded.dim, embedding=excluded.embedding,
	chunk_id=excluded.chunk_id, dsid=excluded.dsid, source_uri=excluded.source_uri`,
			identity.Scope, point.VectorID, identity.Model, identity.Dimensions,
			packFloat32LE(point.Vector), point.ChunkID, point.DocumentID, point.SourceURI)
		if err != nil {
			return fmt.Errorf("projections: upsert dense batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("projections: commit dense batch: %w", err)
	}
	return nil
}

// MetadataScoped resolves ANN vector ids back to original evidence identity.
func (s *SQLDenseStore) MetadataScoped(identity dense.IndexIdentity, vectorIDs []string) (map[string]DenseVector, error) {
	out := make(map[string]DenseVector, len(vectorIDs))
	for _, vectorID := range vectorIDs {
		var dsid, chunkID, sourceURI string
		err := s.DB.QueryRow(`SELECT dsid, chunk_id, source_uri FROM dense_vectors
			WHERE generation_id=? AND model_id=? AND dim=? AND document_id=?`,
			identity.Scope, identity.Model, identity.Dimensions, vectorID,
		).Scan(&dsid, &chunkID, &sourceURI)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("projections: dense metadata: %w", err)
		}
		if dsid == "" {
			dsid = vectorID
		}
		if chunkID == "" {
			chunkID = vectorID + "#0"
		}
		out[vectorID] = DenseVector{VectorID: vectorID, DocumentID: dsid, ChunkID: chunkID, SourceURI: sourceURI}
	}
	return out, nil
}

// Search preserves legacy exact semantics for small corpora while enforcing a
// hard fallback bound. Identity-aware ANN serving should call SearchExactBounded.
func (s *SQLDenseStore) Search(generationID string, query []float32, topK int) ([]dense.Hit, error) {
	hits, _, err := s.SearchExactBounded(dense.IndexIdentity{
		Scope: generationID, Model: "legacy", Dimensions: len(query),
	}, query, topK, ExactDenseFallbackLimit)
	return hits, err
}

// SearchExactBounded scores at most maxVectors vectors. The extra LIMIT row is
// used only to detect overflow; no query-time path can stream the full corpus.
func (s *SQLDenseStore) SearchExactBounded(
	identity dense.IndexIdentity, query []float32, topK, maxVectors int,
) ([]dense.Hit, dense.SearchDiagnostics, error) {
	diag := dense.SearchDiagnostics{
		Route: "exact_fallback", IndexState: "missing", ExactFallbackLimit: maxVectors,
	}
	if s == nil || s.DB == nil {
		return nil, diag, fmt.Errorf("projections: nil dense store")
	}
	if identity.Scope == "" || identity.Model == "" || len(query) == 0 {
		return nil, diag, nil
	}
	if identity.Dimensions != len(query) {
		return nil, diag, &dense.IdentityError{
			Field: "dimensions", Expected: fmt.Sprint(identity.Dimensions), Actual: fmt.Sprint(len(query)),
		}
	}
	if maxVectors <= 0 {
		maxVectors = ExactDenseFallbackLimit
		diag.ExactFallbackLimit = maxVectors
	}
	rows, err := s.DB.Query(`
SELECT document_id, dim, embedding
FROM dense_vectors
WHERE generation_id = ? AND model_id = ? AND dim = ?
ORDER BY document_id
LIMIT ?`, identity.Scope, identity.Model, identity.Dimensions, maxVectors+1)
	if err != nil {
		return nil, diag, fmt.Errorf("projections: query dense: %w", err)
	}
	defer rows.Close()

	hits := make([]dense.Hit, 0, 64)
	for rows.Next() {
		var docID string
		var dim int
		var blob []byte
		if err := rows.Scan(&docID, &dim, &blob); err != nil {
			return nil, diag, fmt.Errorf("projections: scan dense: %w", err)
		}
		if len(hits) == maxVectors {
			diag.CorpusVectors = maxVectors + 1
			return nil, diag, &ExactFallbackLimitError{Scope: identity.Scope, Limit: maxVectors}
		}
		vec, err := unpackFloat32LE(blob, dim)
		if err != nil {
			return nil, diag, err
		}
		score := dense.Cosine(query, vec)
		hits = append(hits, dense.Hit{VectorID: docID, DocumentID: docID, Score: score})
		diag.DistanceCalculations++
	}
	if err := rows.Err(); err != nil {
		return nil, diag, fmt.Errorf("projections: iterate dense: %w", err)
	}
	diag.CorpusVectors = len(hits)
	if len(hits) == 0 {
		return nil, diag, nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].DocumentID < hits[j].DocumentID
	})
	if topK <= 0 || topK > len(hits) {
		topK = len(hits)
	}
	return hits[:topK], diag, nil
}

// Get returns a copy of the stored vector for (generationID, documentID).
func (s *SQLDenseStore) Get(generationID, documentID string) ([]float32, bool, error) {
	return s.GetScoped(dense.IndexIdentity{Scope: generationID, Model: "legacy"}, documentID)
}

// GetScoped returns a vector only when scope and model identity both match.
func (s *SQLDenseStore) GetScoped(identity dense.IndexIdentity, documentID string) ([]float32, bool, error) {
	if s == nil || s.DB == nil {
		return nil, false, fmt.Errorf("projections: nil dense store")
	}
	if identity.Scope == "" || identity.Model == "" || documentID == "" {
		return nil, false, nil
	}
	var dim int
	var blob []byte
	err := s.DB.QueryRow(`
SELECT dim, embedding FROM dense_vectors
WHERE generation_id = ? AND model_id = ? AND document_id = ?`,
		identity.Scope, identity.Model, documentID,
	).Scan(&dim, &blob)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("projections: get dense: %w", err)
	}
	vec, err := unpackFloat32LE(blob, dim)
	if err != nil {
		return nil, false, err
	}
	return vec, true, nil
}

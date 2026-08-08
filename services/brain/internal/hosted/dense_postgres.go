package hosted

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/projections"
)

// postgresDense stores float32 vectors in Postgres for parallel local/team dense.
// Prefer pgvector halfvec/vector when extension is available; else BYTEA + app cosine.
type postgresDense struct {
	db       *sql.DB
	gen      string
	pgvector bool // true when CREATE EXTENSION vector succeeded / vector type works
}

type postgresDenseHit struct {
	vectorID string
	hit      Hit
}

const postgresDenseSchemaBYTEA = `
CREATE TABLE IF NOT EXISTS residual_dense_vectors (
  generation_id TEXT NOT NULL,
  document_id TEXT NOT NULL,
  dim INTEGER NOT NULL,
  embedding BYTEA NOT NULL,
	chunk_id TEXT NOT NULL DEFAULT '',
	dsid TEXT NOT NULL DEFAULT '',
	source_uri TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (generation_id, document_id)
);
CREATE INDEX IF NOT EXISTS residual_dense_gen ON residual_dense_vectors(generation_id);
`

const postgresDenseSchemaVector = `
CREATE TABLE IF NOT EXISTS residual_dense_vectors_v (
  generation_id TEXT NOT NULL,
  document_id TEXT NOT NULL,
  dim INTEGER NOT NULL,
  embedding vector,
	chunk_id TEXT NOT NULL DEFAULT '',
	dsid TEXT NOT NULL DEFAULT '',
	source_uri TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (generation_id, document_id)
);
CREATE INDEX IF NOT EXISTS residual_dense_v_gen ON residual_dense_vectors_v(generation_id);
`

func openPostgresDense(dsn, brainID string) (*postgresDense, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("hosted: empty postgres dense dsn")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hosted: postgres dense ping: %w", err)
	}
	pd := &postgresDense{db: db, gen: strings.TrimSpace(brainID)}
	if pd.gen == "" {
		pd.gen = "local"
	}
	// Try pgvector extension (no-op failure → BYTEA path).
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err == nil {
		if _, err := db.Exec(postgresDenseSchemaVector); err == nil {
			pd.pgvector = true
		}
	}
	// Keep the bounded BYTEA fallback available even when pgvector initializes;
	// a later vector cast/query failure must not target a table that was never
	// created.
	if _, err := db.Exec(postgresDenseSchemaBYTEA); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hosted: postgres dense schema: %w", err)
	}
	tables := []string{"residual_dense_vectors"}
	if pd.pgvector {
		tables = append(tables, "residual_dense_vectors_v")
	}
	for _, table := range tables {
		for _, column := range []string{"chunk_id", "dsid", "source_uri"} {
			if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN IF NOT EXISTS ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("hosted: postgres dense metadata schema: %w", err)
			}
		}
	}
	return pd, nil
}

func (d *postgresDense) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *postgresDense) Upsert(points []DensePoint) error {
	if d == nil || d.db == nil {
		return fmt.Errorf("hosted: nil postgres dense")
	}
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
		if d.pgvector {
			if err := d.upsertPGVector(id, docID, chunkID, sourceURI, p.Vector); err != nil {
				return err
			}
			continue
		}
		if err := d.upsertBYTEA(d.db, id, docID, chunkID, sourceURI, p.Vector); err != nil {
			return err
		}
	}
	return nil
}

type postgresDenseExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (d *postgresDense) upsertPGVector(vectorID, docID, chunkID, sourceURI string, vec []float32) (retErr error) {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("hosted: begin postgres dense vector upsert: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = fmt.Errorf("%v (rollback postgres dense vector upsert: %w)", retErr, rollbackErr)
		}
	}()

	// The BYTEA exception and pgvector row are two representations of one
	// logical point. Keep their transition in one transaction so every error
	// restores the previously committed representation.
	if _, err := tx.Exec(`
DELETE FROM residual_dense_vectors
WHERE generation_id = $1 AND document_id = $2`, d.gen, vectorID); err != nil {
		return fmt.Errorf("hosted: clear postgres dense fallback before vector upsert: %w", err)
	}

	// PostgreSQL aborts a transaction after a statement error. A savepoint lets
	// the expected vector-write fallback continue inside the same transaction.
	if _, err := tx.Exec(`SAVEPOINT postgres_dense_vector_write`); err != nil {
		return fmt.Errorf("hosted: savepoint postgres dense vector upsert: %w", err)
	}
	_, vectorErr := tx.Exec(`
INSERT INTO residual_dense_vectors_v (generation_id, document_id, dim, embedding, chunk_id, dsid, source_uri)
VALUES ($1, $2, $3, $4::vector, $5, $6, $7)
ON CONFLICT (generation_id, document_id) DO UPDATE SET
	dim = EXCLUDED.dim, embedding = EXCLUDED.embedding, chunk_id = EXCLUDED.chunk_id,
	dsid = EXCLUDED.dsid, source_uri = EXCLUDED.source_uri`,
		d.gen, vectorID, len(vec), vectorLiteral(vec), chunkID, docID, sourceURI)
	if vectorErr != nil {
		if _, err := tx.Exec(`ROLLBACK TO SAVEPOINT postgres_dense_vector_write`); err != nil {
			return fmt.Errorf("hosted: restore postgres dense transaction after vector upsert failure: %w (vector upsert: %v)", err, vectorErr)
		}
		// A failed INSERT ... ON CONFLICT leaves the previously committed
		// pgvector row in place. Remove it before publishing the newer BYTEA
		// exception, otherwise an overflowed exception scan could serve stale data.
		if _, err := tx.Exec(`
DELETE FROM residual_dense_vectors_v
WHERE generation_id = $1 AND document_id = $2`, d.gen, vectorID); err != nil {
			return fmt.Errorf("hosted: clear stale postgres dense vector after upsert failure: %w (vector upsert: %v)", err, vectorErr)
		}
		if err := d.upsertBYTEA(tx, vectorID, docID, chunkID, sourceURI, vec); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("hosted: commit postgres dense vector upsert: %w", err)
	}
	return nil
}

func (d *postgresDense) upsertBYTEA(exec postgresDenseExecer, vectorID, docID, chunkID, sourceURI string, vec []float32) error {
	blob := packF32(vec)
	_, err := exec.Exec(`
INSERT INTO residual_dense_vectors (generation_id, document_id, dim, embedding, chunk_id, dsid, source_uri)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (generation_id, document_id) DO UPDATE SET
	dim = EXCLUDED.dim, embedding = EXCLUDED.embedding, chunk_id = EXCLUDED.chunk_id,
	dsid = EXCLUDED.dsid, source_uri = EXCLUDED.source_uri`,
		d.gen, vectorID, len(vec), blob, chunkID, docID, sourceURI)
	return err
}

func (d *postgresDense) Search(query denseQuery, topK int) (denseSearchResult, error) {
	if d == nil || d.db == nil {
		return denseSearchResult{}, fmt.Errorf("hosted: nil postgres dense")
	}
	if topK <= 0 {
		topK = 20
	}
	if len(query.Vector) == 0 {
		return denseSearchResult{}, nil
	}
	if d.pgvector {
		// Per-point vector cast failures are stored only in BYTEA. Always inspect
		// that bounded table before accepting a successful pgvector result; an
		// early pgvector-only return would silently omit those committed points,
		// including after a process restart.
		fallback, fallbackErr := d.searchBYTEA(query.Vector, projections.ExactDenseFallbackLimit)
		if fallbackErr != nil {
			var limitErr *projections.ExactFallbackLimitError
			if errors.As(fallbackErr, &limitErr) {
				// An oversized exception table must not make a healthy pgvector
				// arm disappear. The probe remains bounded, and because it is not
				// a complete exception set its partial rows are intentionally not
				// merged. Serve the provider-ranked arm as degraded instead.
				vectorHits, vectorErr := d.searchVector(query.Vector, topK)
				diag := denseRemoteDiagnostics("pgvector_bytea_overflow")
				diag.ExactFallbackLimit = limitErr.Limit
				if vectorErr != nil {
					return denseSearchResult{Diagnostics: diag}, vectorErr
				}
				return denseSearchResult{
					Hits:        mergePostgresDenseHits(vectorHits, fallback, topK, true),
					Diagnostics: diag,
				}, nil
			}
			diag := denseRemoteDiagnostics("pgvector_bytea_merge")
			diag.ExactFallbackLimit = projections.ExactDenseFallbackLimit
			return denseSearchResult{Diagnostics: diag}, fallbackErr
		}
		vectorLimit := topK + len(fallback)
		if vectorLimit < topK { // integer overflow guard
			vectorLimit = topK
		}
		vectorHits, vectorErr := d.searchVector(query.Vector, vectorLimit)
		if vectorErr == nil {
			diag := denseRemoteDiagnostics("pgvector")
			if len(fallback) > 0 {
				diag.Route = "pgvector_bytea_merge"
				diag.ExactFallbackLimit = projections.ExactDenseFallbackLimit
			}
			return denseSearchResult{Hits: mergePostgresDenseHits(vectorHits, fallback, topK, false), Diagnostics: diag}, nil
		}
		// The vector arm failed, but the bounded BYTEA path remains observable in
		// diagnostics and can still serve points committed through that path.
		return denseSearchResult{
			Hits:        postgresHits(fallback, topK),
			Diagnostics: postgresFallbackDiagnostics(),
		}, nil
	}
	hits, err := d.searchBYTEA(query.Vector, topK)
	return denseSearchResult{Hits: postgresHits(hits, topK), Diagnostics: postgresFallbackDiagnostics()}, err
}

func postgresFallbackDiagnostics() dense.SearchDiagnostics {
	diag := denseMissingDiagnostics("exact_fallback")
	diag.ExactFallbackLimit = projections.ExactDenseFallbackLimit
	return diag
}

func (d *postgresDense) searchVector(query []float32, topK int) ([]postgresDenseHit, error) {
	lit := vectorLiteral(query)
	// Cosine distance operator <=> ; convert to similarity 1 - distance when unit-ish.
	rows, err := d.db.Query(`
SELECT document_id, dsid, chunk_id, source_uri, 1 - (embedding <=> $1::vector) AS score
FROM residual_dense_vectors_v
WHERE generation_id = $2
ORDER BY embedding <=> $1::vector
LIMIT $3`, lit, d.gen, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []postgresDenseHit
	for rows.Next() {
		var id, dsid, chunkID, sourceURI string
		var score float64
		if err := rows.Scan(&id, &dsid, &chunkID, &sourceURI, &score); err != nil {
			return nil, err
		}
		if dsid == "" {
			dsid = id
		}
		if chunkID == "" {
			chunkID = id + "#0"
		}
		out = append(out, postgresDenseHit{vectorID: id, hit: Hit{
			ChunkID:   chunkID,
			DSID:      dsid,
			SourceURI: sourceURI,
			Score:     score,
			Channel:   "dense_pgvector",
		}})
	}
	return out, rows.Err()
}

func (d *postgresDense) searchBYTEA(query []float32, topK int) ([]postgresDenseHit, error) {
	rows, err := d.db.Query(`
SELECT document_id, dsid, chunk_id, source_uri, dim, embedding
FROM residual_dense_vectors
WHERE generation_id=$1
ORDER BY document_id
LIMIT $2`, d.gen, projections.ExactDenseFallbackLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		id, dsid, chunkID, sourceURI string
		score                        float64
	}
	var all []scored
	qNorm := l2(query)
	if qNorm == 0 {
		return nil, nil
	}
	for rows.Next() {
		if len(all) == projections.ExactDenseFallbackLimit {
			return nil, &projections.ExactFallbackLimitError{
				Scope: d.gen, Limit: projections.ExactDenseFallbackLimit,
			}
		}
		var id, dsid, chunkID, sourceURI string
		var dim int
		var blob []byte
		if err := rows.Scan(&id, &dsid, &chunkID, &sourceURI, &dim, &blob); err != nil {
			return nil, err
		}
		vec, err := unpackF32(blob, dim)
		if err != nil || len(vec) != len(query) {
			continue
		}
		all = append(all, scored{id: id, dsid: dsid, chunkID: chunkID, sourceURI: sourceURI, score: cosineF32(query, vec, qNorm)})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].id < all[j].id
	})
	if topK > len(all) {
		topK = len(all)
	}
	out := make([]postgresDenseHit, 0, topK)
	for i := 0; i < topK; i++ {
		row := all[i]
		if row.dsid == "" {
			row.dsid = row.id
		}
		if row.chunkID == "" {
			row.chunkID = row.id + "#0"
		}
		out = append(out, postgresDenseHit{vectorID: row.id, hit: Hit{
			ChunkID:   row.chunkID,
			DSID:      row.dsid,
			SourceURI: row.sourceURI,
			Score:     all[i].score,
			Channel:   "dense_postgres",
		}})
	}
	return out, nil
}

// mergePostgresDenseHits combines the provider result with a complete bounded
// BYTEA exception set. BYTEA wins on duplicate vector ids because it represents
// the latest successful write when the corresponding vector upsert failed. An
// overflowed set is incomplete and therefore cannot be merged safely; in that
// degraded case the provider arm remains independently servable.
func mergePostgresDenseHits(vector, fallback []postgresDenseHit, topK int, fallbackOverflow bool) []Hit {
	if fallbackOverflow {
		fallback = nil
	}
	merged := make(map[string]postgresDenseHit, len(vector)+len(fallback))
	for _, candidate := range vector {
		merged[candidate.vectorID] = candidate
	}
	for _, candidate := range fallback {
		merged[candidate.vectorID] = candidate
	}
	ordered := make([]postgresDenseHit, 0, len(merged))
	for _, candidate := range merged {
		ordered = append(ordered, candidate)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].hit.Score != ordered[j].hit.Score {
			return ordered[i].hit.Score > ordered[j].hit.Score
		}
		return ordered[i].vectorID < ordered[j].vectorID
	})
	return postgresHits(ordered, topK)
}

func postgresHits(candidates []postgresDenseHit, topK int) []Hit {
	if topK > len(candidates) {
		topK = len(candidates)
	}
	if topK <= 0 {
		return nil
	}
	out := make([]Hit, topK)
	for i := 0; i < topK; i++ {
		out[i] = candidates[i].hit
	}
	return out
}

// vectorLiteral formats pgvector text input: [1,2,3]
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", x)
	}
	b.WriteByte(']')
	return b.String()
}

func packF32(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func unpackF32(b []byte, dim int) ([]float32, error) {
	if dim <= 0 || len(b) < 4*dim {
		return nil, fmt.Errorf("bad dense blob")
	}
	out := make([]float32, dim)
	for i := 0; i < dim; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

func l2(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func cosineF32(a, b []float32, aNorm float64) float64 {
	if aNorm == 0 || len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	bn := l2(b)
	if bn == 0 {
		return 0
	}
	return dot / (aNorm * bn)
}

var _ denseBackend = (*postgresDense)(nil)

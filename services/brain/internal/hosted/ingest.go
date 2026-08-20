package hosted

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// product-owned chunk metadata DDL (independent of SMF path2_chunk_metadata).
// Executed as separate statements — pgx simple-protocol does not multi-statement.
var productChunkSchemaStmts = []string{
	`CREATE TABLE IF NOT EXISTS product_chunk_metadata (
  brain_id TEXT NOT NULL,
  chunk_id TEXT NOT NULL,
  dsid TEXT NOT NULL,
  text_content TEXT NOT NULL,
  source_uri TEXT NOT NULL DEFAULT '',
  tsv tsvector,
  PRIMARY KEY (brain_id, chunk_id)
)`,
	`CREATE INDEX IF NOT EXISTS product_chunk_metadata_brain_dsid
  ON product_chunk_metadata (brain_id, dsid)`,
	`CREATE INDEX IF NOT EXISTS product_chunk_metadata_tsv
  ON product_chunk_metadata USING GIN (tsv)`,
	// Structure graph (hosted-first residual parity with productbrain edges/entities/facts).
	`CREATE TABLE IF NOT EXISTS product_edges (
  brain_id TEXT NOT NULL,
  src TEXT NOT NULL,
  dst TEXT NOT NULL,
  relation TEXT NOT NULL DEFAULT 'cooccur',
  PRIMARY KEY (brain_id, src, dst, relation)
)`,
	`CREATE INDEX IF NOT EXISTS product_edges_src ON product_edges (brain_id, src)`,
	`CREATE TABLE IF NOT EXISTS product_entities (
  brain_id TEXT NOT NULL,
  entity TEXT NOT NULL,
  document_id TEXT NOT NULL,
  PRIMARY KEY (brain_id, entity, document_id)
)`,
	`CREATE INDEX IF NOT EXISTS product_entities_entity ON product_entities (brain_id, entity)`,
	`CREATE TABLE IF NOT EXISTS product_facts (
  brain_id TEXT NOT NULL,
  document_id TEXT NOT NULL,
  fact_text TEXT NOT NULL,
  PRIMARY KEY (brain_id, document_id, fact_text)
)`,
	`CREATE INDEX IF NOT EXISTS product_facts_doc ON product_facts (brain_id, document_id)`,
}

// ChunkWrite is one product-owned chunk upsert unit.
type ChunkWrite struct {
	DocumentID string // dsid
	ChunkID    string
	Text       string
	SourceURI  string
}

// ChunkReceipt records one chunk outcome inside a burst shard.
type ChunkReceipt struct {
	ChunkID string `json:"chunk_id"`
	DSID    string `json:"dsid,omitempty"`
	Shard   int    `json:"shard"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// IngestResult is the collective outcome of BurstUpsert / UpsertChunks.
type IngestResult struct {
	BrainID        string         `json:"brain_id"`
	Ingested       int            `json:"ingested"`
	Upserted       int            `json:"upserted"`
	Workers        int            `json:"workers,omitempty"`
	ProductOwned   bool           `json:"product_owned"`
	Mode           string         `json:"mode,omitempty"` // "burst" | "batch" | "delta"
	GenerationID   string         `json:"generation_id,omitempty"`
	EnrichJobs     int            `json:"enrich_jobs,omitempty"`
	EnrichSidecars int            `json:"enrich_sidecars,omitempty"`
	Receipts       []ChunkReceipt `json:"receipts,omitempty"`
}

// ChunkStore is the product-owned write (and offline lexical) surface.
// NeonChunkStore backs product_chunk_metadata; MemoryChunkStore is the
// offline profile used when no Neon URL is set.
type ChunkStore interface {
	EnsureSchema(ctx context.Context) error
	UpsertChunks(ctx context.Context, brainID string, chunks []ChunkWrite) error
	// LexicalSearch is used by the memory/offline retrieve profile.
	LexicalSearch(ctx context.Context, brainID, question string, limit int) ([]Hit, error)
	// SiblingChunks loads more chunks for a document (hydrate).
	SiblingChunks(ctx context.Context, brainID, dsid string, limit int) ([]Hit, error)
}

// DensePoint is one model-pinned upsert unit for any dense substrate.
type DensePoint struct {
	ID     string
	Vector []float32
	// ModelID is the immutable embedding model identity for Vector. Local ANN
	// substrates reject mixed or missing identities instead of guessing from dim.
	ModelID string
	Payload map[string]any
}

// EnsureSchema creates product_chunk_metadata (product-owned; not path2).
func (c *Client) EnsureSchema(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("hosted: nil client")
	}
	if c.store != nil {
		return c.store.EnsureSchema(ctx)
	}
	if c.db == nil {
		return fmt.Errorf("hosted: no chunk store or db")
	}
	// Lazy-attach Neon product store so Open()+EnsureSchema works without
	// callers wiring store explicitly.
	c.store = &neonChunkStore{db: c.db}
	return c.store.EnsureSchema(ctx)
}

// UpsertChunks writes product-owned chunks (Neon product_chunk_metadata or memory).
func (c *Client) UpsertChunks(ctx context.Context, brainID string, chunks []ChunkWrite) error {
	ctx, qualitySpan := c.startIngestQualitySpan(ctx, "batch", len(chunks))
	var qualityErr error
	qualityOutput := 0
	defer func() { finishIngestQualitySpan(qualitySpan, len(chunks), qualityOutput, qualityErr) }()
	if c == nil {
		qualityErr = fmt.Errorf("hosted: nil client")
		return qualityErr
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		brainID = c.cfg.BrainID
	}
	if brainID == "" {
		qualityErr = fmt.Errorf("hosted: empty brain_id")
		return qualityErr
	}
	if len(chunks) == 0 {
		qualityErr = fmt.Errorf("hosted: empty chunks")
		return qualityErr
	}
	if err := c.ensureStore(ctx); err != nil {
		qualityErr = err
		return err
	}
	if err := c.store.UpsertChunks(ctx, brainID, chunks); err != nil {
		qualityErr = err
		return err
	}
	qualityOutput = len(chunks)
	// Mutations must not leave stale retrieve results in the TTL cache.
	c.InvalidateQueryCache()
	// Rebuild interactive HotLex from product store.
	c.EnsureHotLex()
	return nil
}

// BurstUpsert shards chunks across workers and returns per-chunk receipts.
// Local residual: use workers as a local burst fleet (in lieu of hosted burst
// compute). When store is durable FS, flushes once after the fan-out so parallel
// writers do not each rewrite chunks.jsonl.
func (c *Client) BurstUpsert(ctx context.Context, brainID string, chunks []ChunkWrite, workers int) (result IngestResult, retErr error) {
	ctx, qualitySpan := c.startIngestQualitySpan(ctx, "burst", len(chunks))
	var qualityErr error
	qualityOutput := 0
	defer func() { finishIngestQualitySpan(qualitySpan, len(chunks), qualityOutput, qualityErr) }()
	if c == nil {
		qualityErr = fmt.Errorf("hosted: nil client")
		return IngestResult{}, qualityErr
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		brainID = c.cfg.BrainID
	}
	if brainID == "" {
		qualityErr = fmt.Errorf("hosted: empty brain_id")
		return IngestResult{}, qualityErr
	}
	if len(chunks) == 0 {
		qualityErr = fmt.Errorf("hosted: empty burst")
		return IngestResult{}, qualityErr
	}
	if workers < 1 {
		workers = defaultLocalWorkers()
	}
	if workers > len(chunks) {
		workers = len(chunks)
	}
	if err := c.ensureStore(ctx); err != nil {
		qualityErr = err
		return IngestResult{}, err
	}

	// Parallel-safe durable batch: one disk flush after workers.
	//
	// endBatch is the ONLY disk write for the whole burst -- per-upsert flushes
	// are deferred -- so discarding its error meant a fully successful
	// IngestResult could be returned having written nothing at all.
	if d, ok := c.store.(*durableStore); ok {
		d.beginBatch()
		defer func() {
			if err := d.endBatch(); err != nil && retErr == nil {
				retErr = fmt.Errorf("hosted: burst flush failed: %w", err)
			}
		}()
	}

	receipts := make([]ChunkReceipt, len(chunks))
	type item struct {
		chunk ChunkWrite
		idx   int
		shard int
	}
	shardItems := make([][]item, workers)
	for i, ch := range chunks {
		it := item{chunk: ch, idx: i, shard: i % workers}
		shardItems[it.shard] = append(shardItems[it.shard], it)
	}

	var okCount int64
	var wg sync.WaitGroup
	for s := 0; s < workers; s++ {
		batch := shardItems[s]
		if len(batch) == 0 {
			continue
		}
		wg.Add(1)
		go func(shard int, batch []item) {
			defer wg.Done()
			writes := make([]ChunkWrite, len(batch))
			for i, it := range batch {
				writes[i] = it.chunk
			}
			err := c.store.UpsertChunks(ctx, brainID, writes)
			for _, it := range batch {
				rec := ChunkReceipt{
					ChunkID: it.chunk.ChunkID,
					DSID:    it.chunk.DocumentID,
					Shard:   shard,
					OK:      err == nil,
				}
				if err != nil {
					rec.Error = err.Error()
				} else {
					atomic.AddInt64(&okCount, 1)
				}
				receipts[it.idx] = rec
			}
		}(s, batch)
	}
	wg.Wait()

	n := int(atomic.LoadInt64(&okCount))
	qualityOutput = n
	if n < len(chunks) {
		qualityErr = fmt.Errorf("hosted: one or more burst shards failed")
	}
	if n > 0 {
		// At least one successful write → drop retrieve cache for this process.
		c.InvalidateQueryCache()
		c.EnsureHotLex()
	}
	result = IngestResult{
		BrainID:      brainID,
		Ingested:     n,
		Upserted:     n,
		Workers:      workers,
		ProductOwned: true,
		Mode:         "burst",
		Receipts:     receipts,
	}
	// qualityErr used to feed only a deferred telemetry span while this
	// returned nil, so a caller saw success for an ingest that wrote nothing.
	// Ingested carries the reduced count, but nothing compares it against
	// len(chunks) -- the documents simply never entered the corpus, and
	// retrieval answered "not found" for data the operator watched succeed.
	return result, qualityErr
}

func (c *Client) ensureStore(ctx context.Context) error {
	if c.store != nil {
		return c.store.EnsureSchema(ctx)
	}
	if c.db != nil {
		c.store = &neonChunkStore{db: c.db}
		return c.store.EnsureSchema(ctx)
	}
	return fmt.Errorf("hosted: no chunk store (OpenMemory or Neon URL required)")
}

// neonChunkStore writes product_chunk_metadata on Neon/Postgres.
type neonChunkStore struct {
	db *sql.DB
	mu sync.Mutex
}

func (s *neonChunkStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("hosted: nil neon store")
	}
	for _, stmt := range productChunkSchemaStmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("hosted: ensure product_chunk_metadata: %w", err)
		}
	}
	return nil
}

const productUpsertSQL = `
INSERT INTO product_chunk_metadata (brain_id, chunk_id, dsid, text_content, source_uri, tsv)
VALUES ($1, $2, $3, $4, $5, to_tsvector('english', $4))
ON CONFLICT (brain_id, chunk_id) DO UPDATE SET
  dsid = EXCLUDED.dsid,
  text_content = EXCLUDED.text_content,
  source_uri = EXCLUDED.source_uri,
  tsv = to_tsvector('english', EXCLUDED.text_content)
`

func (s *neonChunkStore) UpsertChunks(ctx context.Context, brainID string, chunks []ChunkWrite) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("hosted: nil neon store")
	}
	// Serialize writers — Neon connection pool is fine, but keep tx simple.
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("hosted: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, productUpsertSQL)
	if err != nil {
		return fmt.Errorf("hosted: prepare upsert: %w", err)
	}
	defer stmt.Close()
	for i, ch := range chunks {
		ch.ChunkID = strings.TrimSpace(ch.ChunkID)
		ch.DocumentID = strings.TrimSpace(ch.DocumentID)
		ch.Text = strings.TrimSpace(ch.Text)
		if ch.ChunkID == "" {
			return fmt.Errorf("hosted: chunk %d missing chunk_id", i)
		}
		if ch.DocumentID == "" {
			return fmt.Errorf("hosted: chunk %s missing document_id/dsid", ch.ChunkID)
		}
		if ch.Text == "" {
			return fmt.Errorf("hosted: chunk %s empty text", ch.ChunkID)
		}
		if _, err := stmt.ExecContext(ctx, brainID, ch.ChunkID, ch.DocumentID, ch.Text, ch.SourceURI); err != nil {
			return fmt.Errorf("hosted: upsert %s: %w", ch.ChunkID, err)
		}
	}
	// Structure graph (edges / entities / facts) — hosted-first residual parity.
	if err := upsertProductStructure(ctx, tx, brainID, chunks); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("hosted: commit: %w", err)
	}
	return nil
}

// upsertProductStructure writes product_edges / product_entities / product_facts.
func upsertProductStructure(ctx context.Context, tx *sql.Tx, brainID string, chunks []ChunkWrite) error {
	if tx == nil || len(chunks) == 0 {
		return nil
	}
	// Aggregate text per document.
	byDoc := map[string]string{}
	for _, ch := range chunks {
		id := strings.TrimSpace(ch.DocumentID)
		if id == "" {
			continue
		}
		byDoc[id] = byDoc[id] + "\n" + ch.Text
	}
	idx := newStructureIndex()
	for id, text := range byDoc {
		idx.indexDocument(id, "", text)
	}
	idx.rebuildEdges()

	entStmt, err := tx.PrepareContext(ctx, `
INSERT INTO product_entities (brain_id, entity, document_id) VALUES ($1, $2, $3)
ON CONFLICT (brain_id, entity, document_id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("hosted: prepare entities: %w", err)
	}
	defer entStmt.Close()
	factStmt, err := tx.PrepareContext(ctx, `
INSERT INTO product_facts (brain_id, document_id, fact_text) VALUES ($1, $2, $3)
ON CONFLICT (brain_id, document_id, fact_text) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("hosted: prepare facts: %w", err)
	}
	defer factStmt.Close()
	edgeStmt, err := tx.PrepareContext(ctx, `
INSERT INTO product_edges (brain_id, src, dst, relation) VALUES ($1, $2, $3, $4)
ON CONFLICT (brain_id, src, dst, relation) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("hosted: prepare edges: %w", err)
	}
	defer edgeStmt.Close()

	for docID, toks := range idx.docTokens {
		for _, e := range toks {
			if _, err := entStmt.ExecContext(ctx, brainID, e, docID); err != nil {
				return fmt.Errorf("hosted: entity %s: %w", docID, err)
			}
		}
		for _, f := range idx.docFacts[docID] {
			if _, err := factStmt.ExecContext(ctx, brainID, docID, f); err != nil {
				return fmt.Errorf("hosted: fact %s: %w", docID, err)
			}
		}
	}
	for src, dsts := range idx.edges {
		for _, dst := range dsts {
			if _, err := edgeStmt.ExecContext(ctx, brainID, src, dst, "cooccur"); err != nil {
				return fmt.Errorf("hosted: edge %s→%s: %w", src, dst, err)
			}
		}
	}
	return nil
}

const productLexicalORSQL = `
SELECT chunk_id, dsid, text_content, COALESCE(source_uri, '') AS source_uri,
       ts_rank(tsv, to_tsquery('english', $2)) AS rank
FROM product_chunk_metadata
WHERE brain_id = $1 AND tsv @@ to_tsquery('english', $2)
ORDER BY rank DESC
LIMIT $3
`

const productSiblingsSQL = `
SELECT chunk_id, text_content, COALESCE(source_uri, '') AS source_uri
FROM product_chunk_metadata
WHERE brain_id = $1 AND dsid = $2
LIMIT $3
`

func (s *neonChunkStore) LexicalSearch(ctx context.Context, brainID, question string, limit int) ([]Hit, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("hosted: nil neon store")
	}
	if limit <= 0 {
		limit = 30
	}
	fts := orTSQuery(question, 24)
	if fts == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, productLexicalORSQL, brainID, fts, limit)
	if err != nil {
		return nil, fmt.Errorf("hosted product lexical: %w", err)
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		var rank float64
		if err := rows.Scan(&h.ChunkID, &h.DSID, &h.Text, &h.SourceURI, &rank); err != nil {
			return nil, err
		}
		h.Score = rank
		h.Channel = "lexical"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *neonChunkStore) SiblingChunks(ctx context.Context, brainID, dsid string, limit int) ([]Hit, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("hosted: nil neon store")
	}
	if limit <= 0 {
		limit = 4
	}
	rows, err := s.db.QueryContext(ctx, productSiblingsSQL, brainID, dsid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.Text, &h.SourceURI); err != nil {
			return nil, err
		}
		h.DSID = dsid
		h.Channel = "hydrate"
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *neonChunkStore) WarmSidecars(ctx context.Context, brainID string, items []SidecarWrite) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("hosted: nil neon store")
	}
	const q = `INSERT INTO product_sidecars (brain_id, document_id, kind, text)
VALUES ($1, $2, $3, $4)
ON CONFLICT (brain_id, document_id, kind) DO UPDATE SET text = EXCLUDED.text`
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, it := range items {
		it.DocumentID = strings.TrimSpace(it.DocumentID)
		it.Kind = strings.TrimSpace(it.Kind)
		it.Text = strings.TrimSpace(it.Text)
		if it.DocumentID == "" || it.Kind == "" || it.Text == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, q, brainID, it.DocumentID, it.Kind, it.Text); err != nil {
			return n, fmt.Errorf("hosted: warm sidecar %s: %w", it.DocumentID, err)
		}
		n++
	}
	return n, nil
}

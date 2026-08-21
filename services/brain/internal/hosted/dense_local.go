package hosted

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/projections"
)

type denseQuery struct {
	Vector  []float32
	ModelID string
}

type denseSearchResult struct {
	Hits        []Hit
	Diagnostics dense.SearchDiagnostics
}

func denseRemoteDiagnostics(route string) dense.SearchDiagnostics {
	return dense.SearchDiagnostics{Route: route, IndexState: "provider_managed"}
}

func denseMissingDiagnostics(route string) dense.SearchDiagnostics {
	return dense.SearchDiagnostics{Route: route, IndexState: "missing"}
}

// denseBackend is the local/remote dense ANN substrate behind residual retrieve.
type denseBackend interface {
	Close() error
	Upsert(points []DensePoint) error
	Search(query denseQuery, topK int) (denseSearchResult, error)
	// DeleteDocuments removes every vector belonging to the named documents
	// and reports how many were removed. A backend that cannot verifiably
	// purge returns ErrPurgeUnsupported rather than reporting a success it
	// cannot stand behind -- a purge receipt that says "removed" about a store
	// that silently no-opped is worse than a receipt that names the gap.
	DeleteDocuments(docIDs []string) (int, error)
	// HasDocuments returns the document ids that still have vectors. It is the
	// verification half: a delete count says how many rows a statement
	// matched, not whether the document survives.
	HasDocuments(docIDs []string) ([]string, error)
}

// ErrPurgeUnsupported reports a dense backend that cannot verifiably delete.
//
// Two of the five are in this state, and it is a deliberate answer rather than
// an unfinished one. The FAISS sidecar and Qdrant are remote services whose
// delete surfaces this repository cannot exercise; implementing an HTTP call
// against an endpoint nobody here can confirm, and then reporting its result
// as an erasure, would ship an erasure path that has never run. The purge
// receipt names them as skipped and refuses to report completeness, so the gap
// is visible in the artifact rather than assumed away.
var ErrPurgeUnsupported = errors.New("hosted: dense backend does not support verifiable purge")

// localDense holds a durable SQLite dense projection under the residual Dir.
type localDense struct {
	mu      sync.RWMutex
	db      *projections.DB
	store   *projections.SQLDenseStore
	ann     *dense.HNSW
	annPath string
	// indexState is missing|ready|corrupt|incompatible and is receipt-safe.
	indexState string
	// gen keys vectors (brain id or generation pin).
	gen  string
	mode dense.SearchMode
	// saveANN is replaceable only by package tests to prove readiness ordering.
	saveANN func(*dense.HNSW, string) error
}

var _ denseBackend = (*localDense)(nil)

func openLocalDense(dir, brainID string, modes ...dense.SearchMode) (*localDense, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("hosted: dense sqlite requires Dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "dense.db")
	db, err := projections.Open(path)
	if err != nil {
		return nil, err
	}
	gen := strings.TrimSpace(brainID)
	if gen == "" {
		gen = "local"
	}
	ld := &localDense{
		db:         db,
		store:      &projections.SQLDenseStore{DB: db.SQL},
		gen:        gen,
		annPath:    filepath.Join(dir, localANNFilename(gen)),
		indexState: "missing",
		mode:       dense.SearchModeAuto,
		saveANN:    func(idx *dense.HNSW, path string) error { return idx.Save(path) },
	}
	if len(modes) > 0 && modes[0] != "" {
		ld.mode = modes[0]
	}
	if idx, loadErr := dense.LoadHNSW(ld.annPath); loadErr == nil {
		identity := idx.Identity()
		snapshotIdentity := identity
		snapshotIdentity.ContentDigest = ""
		rows, digest, digestErr := ld.store.SnapshotScoped(snapshotIdentity)
		if identity.Scope == gen && identity.ContentDigest != "" && digestErr == nil &&
			digest == identity.ContentDigest && len(rows) == idx.Len() {
			ld.ann = idx
			ld.indexState = "ready"
		} else {
			ld.indexState = "incompatible"
		}
	} else if !os.IsNotExist(loadErr) {
		ld.indexState = "corrupt"
	}
	return ld, nil
}

func denseSearchMode(value string) dense.SearchMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(dense.SearchModeExact):
		return dense.SearchModeExact
	case string(dense.SearchModeANN):
		return dense.SearchModeANN
	default:
		return dense.SearchModeAuto
	}
}

func localANNFilename(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("dense.%x.ann", sum[:8])
}

func (d *localDense) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *localDense) Upsert(points []DensePoint) error {
	if d == nil || d.store == nil {
		return fmt.Errorf("hosted: nil local dense")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var batchIdentity *dense.IndexIdentity
	for _, p := range points {
		if strings.TrimSpace(p.ID) == "" || len(p.Vector) == 0 {
			continue
		}
		modelID := strings.TrimSpace(p.ModelID)
		if modelID == "" && p.Payload != nil {
			modelID, _ = p.Payload["embedding_model"].(string)
			modelID = strings.TrimSpace(modelID)
		}
		if modelID == "" {
			return fmt.Errorf("hosted: local dense point %q missing embedding model identity", p.ID)
		}
		identity := dense.IndexIdentity{Scope: d.gen, Model: modelID, Dimensions: len(p.Vector)}
		if batchIdentity == nil {
			batchIdentity = &identity
		} else if *batchIdentity != identity {
			return fmt.Errorf("hosted: mixed dense projection identities in one upsert: got %+v want %+v", identity, *batchIdentity)
		}
	}
	if batchIdentity != nil && d.ann != nil && !sameProjectionIdentity(d.ann.Identity(), *batchIdentity) {
		return fmt.Errorf("hosted: local dense projection identity changed: got %+v want %+v", *batchIdentity, d.ann.Identity())
	}
	var stored []projections.DenseVector
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
		if modelID == "" && p.Payload != nil {
			modelID, _ = p.Payload["embedding_model"].(string)
			modelID = strings.TrimSpace(modelID)
		}
		if modelID == "" {
			return fmt.Errorf("hosted: local dense point %q missing embedding model identity", docID)
		}
		identity := dense.IndexIdentity{Scope: d.gen, Model: modelID, Dimensions: len(p.Vector)}
		if d.ann != nil && !sameProjectionIdentity(d.ann.Identity(), identity) {
			return fmt.Errorf("hosted: local dense projection identity changed: got %+v want %+v", identity, d.ann.Identity())
		}
		stored = append(stored, projections.DenseVector{
			VectorID: id, DocumentID: docID, ChunkID: chunkID,
			SourceURI: sourceURI, Vector: p.Vector,
		})
	}
	if len(stored) > 0 {
		if err := d.store.UpsertVectorsScoped(*batchIdentity, stored); err != nil {
			return err
		}
		// The durable vector commit invalidates the previously published ANN.
		// Keep it invisible while rebuilding; only a fully fsynced rename below
		// may transition the serving state to ready.
		d.ann = nil
		d.indexState = "building"
		rows, digest, err := d.store.SnapshotScoped(*batchIdentity)
		if err != nil {
			d.indexState = "stale"
			return err
		}
		publishedIdentity := *batchIdentity
		publishedIdentity.ContentDigest = digest
		candidate := dense.NewScopedHNSW(publishedIdentity, 16, 64)
		for _, row := range rows {
			if err := candidate.UpsertWithMetadata(row.VectorID, row.Vector, dense.HitMetadata{
				DocumentID: row.DocumentID, ChunkID: row.ChunkID, SourceURI: row.SourceURI,
			}); err != nil {
				d.indexState = "stale"
				return err
			}
		}
		if err := d.saveANN(candidate, d.annPath); err != nil {
			d.indexState = "stale"
			return fmt.Errorf("hosted: persist local dense ANN: %w", err)
		}
		d.ann = candidate
		d.indexState = "ready"
	}
	return nil
}

func sameProjectionIdentity(a, b dense.IndexIdentity) bool {
	return a.Scope == b.Scope && a.Model == b.Model && a.Dimensions == b.Dimensions
}

func (d *localDense) Search(query denseQuery, topK int) (denseSearchResult, error) {
	result := denseSearchResult{}
	if d == nil || d.store == nil {
		return result, fmt.Errorf("hosted: nil local dense")
	}
	if topK <= 0 {
		topK = 20
	}
	identity := dense.IndexIdentity{
		Scope: d.gen, Model: strings.TrimSpace(query.ModelID), Dimensions: len(query.Vector),
	}
	if identity.Model == "" {
		return result, fmt.Errorf("hosted: dense query missing embedding model identity")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	idx, state := d.ann, d.indexState
	var annErr error
	if idx != nil {
		dh, diag, err := idx.SearchScopedMode(query.Vector, topK, identity, d.mode)
		if err == nil {
			metadata, metadataErr := d.denseMetadata(identity, dh)
			if metadataErr != nil {
				return result, metadataErr
			}
			result.Diagnostics = diag
			result.Hits = denseHitsToHosted(dh, metadata, "dense_sqlite_ann")
			return result, nil
		}
		annErr = err
		state = "incompatible"
	}
	dh, diag, err := d.store.SearchExactBounded(
		identity, query.Vector, topK, projections.ExactDenseFallbackLimit,
	)
	diag.IndexState = state
	result.Diagnostics = diag
	if err != nil {
		return result, err
	}
	if len(dh) == 0 && annErr != nil {
		return result, annErr
	}
	metadata, metadataErr := d.denseMetadata(identity, dh)
	if metadataErr != nil {
		return result, metadataErr
	}
	result.Hits = denseHitsToHosted(dh, metadata, "dense_sqlite_exact_fallback")
	return result, nil
}

func (d *localDense) denseMetadata(identity dense.IndexIdentity, hits []dense.Hit) (map[string]projections.DenseVector, error) {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		id := hit.VectorID
		if id == "" {
			id = hit.DocumentID
		}
		ids = append(ids, id)
	}
	return d.store.MetadataScoped(identity, ids)
}

func denseHitsToHosted(dh []dense.Hit, metadata map[string]projections.DenseVector, channel string) []Hit {
	out := make([]Hit, 0, len(dh))
	for _, h := range dh {
		vectorID := h.VectorID
		if vectorID == "" {
			vectorID = h.DocumentID
		}
		point := metadata[vectorID]
		// HNSW hits carry persisted metadata. Exact-fallback hits carry only the
		// vector id; in that case the SQLite metadata above is authoritative.
		if h.ChunkID != "" && h.DocumentID != "" {
			point.DocumentID = h.DocumentID
		}
		if h.ChunkID != "" {
			point.ChunkID = h.ChunkID
		}
		if h.SourceURI != "" {
			point.SourceURI = h.SourceURI
		}
		if point.DocumentID == "" {
			point.DocumentID = vectorID
		}
		if point.ChunkID == "" {
			point.ChunkID = vectorID + "#0"
		}
		out = append(out, Hit{
			ChunkID: point.ChunkID, DSID: point.DocumentID, SourceURI: point.SourceURI,
			Score: h.Score, Channel: channel,
		})
	}
	return out
}

// bagEmbed is offline dense (no network). Fixed dim for residual bag path.
func bagEmbed(text string, dim int) []float32 {
	if dim <= 0 {
		dim = 256
	}
	v := make([]float32, dim)
	text = strings.ToLower(text)
	var b strings.Builder
	flush := func() {
		t := b.String()
		b.Reset()
		if len(t) < 3 {
			return
		}
		h := 0
		for _, c := range t {
			h = h*31 + int(c)
		}
		if h < 0 {
			h = -h
		}
		v[h%dim] += 1
	}
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return v
}

// mergeLocalDenseArm fuses local sqlite/memory dense hits into the passage pool.
// Safe no-op when local dense is unbound.
func (c *Client) mergeLocalDenseArm(ctx context.Context, question string, pool []Passage, diag map[string]any, poolCap int) []Passage {
	if c == nil || c.localDense == nil {
		return pool
	}
	limit := c.cfg.DenseLimit
	if limit <= 0 {
		limit = 20
	}
	vec, embKind, embErr := c.embedForDense(ctx, question)
	modelID := c.denseEmbeddingModel(embKind)
	if diag != nil {
		diag["dense_embed"] = embKind
		if embErr != nil {
			diag["dense_embed_err"] = embErr.Error()
		}
	}
	if len(vec) == 0 {
		return pool
	}
	search, derr := c.localDense.Search(denseQuery{Vector: vec, ModelID: modelID}, limit)
	dHits := search.Hits
	if diag != nil {
		diag["dense_index_state"] = search.Diagnostics.IndexState
		diag["dense_route"] = search.Diagnostics.Route
		diag["dense_corpus_vectors"] = search.Diagnostics.CorpusVectors
		diag["dense_distance_calculations"] = search.Diagnostics.DistanceCalculations
		diag["dense_candidate_limit"] = search.Diagnostics.CandidateLimit
		diag["dense_exact_fallback_limit"] = search.Diagnostics.ExactFallbackLimit
		diag["dense_model"] = modelID
		diag["dense_dimensions"] = len(vec)
	}
	if derr != nil {
		if diag != nil {
			diag["dense_err"] = derr.Error()
		}
		return pool
	}
	if diag != nil {
		diag["dense_hits"] = len(dHits)
		arm := c.substrates.Dense
		if arm == "" {
			arm = SubstrateDenseSQLite
		}
		diag["dense_arm"] = arm
	}
	for i := range dHits {
		if dHits[i].Text != "" {
			continue
		}
		if expander, ok := c.store.(structureExpander); ok {
			ps := expander.PassagesForDocs(c.cfg.BrainID, []string{dHits[i].DSID}, c.cfg.MaxPassageChars)
			if len(ps) > 0 {
				dHits[i].Text = ps[0].Text
				if dHits[i].ChunkID == "" {
					dHits[i].ChunkID = ps[0].ChunkID
				}
				if dHits[i].SourceURI == "" {
					dHits[i].SourceURI = ps[0].SourceURI
				}
			}
		}
	}
	if poolCap <= 0 {
		poolCap = limit + 16
	}
	return mergePassagesStructure(pool, hitsToPassages(dHits, limit, c.cfg.MaxPassageChars), poolCap)
}

// seedDenseAfterIngest upserts bag (or remote) embeddings into local dense store.
// Fan-out uses local workers (same fleet as BurstUpsert) so local residual does
// not serialize embed behind hosted burst.
func (c *Client) seedDenseAfterIngest(ctx context.Context, docs []LocalDocument) {
	if c == nil || c.localDense == nil || len(docs) == 0 {
		return
	}
	workers := defaultLocalWorkers()
	if workers > len(docs) {
		workers = len(docs)
	}
	type item struct {
		d LocalDocument
	}
	ch := make(chan item, len(docs))
	for _, d := range docs {
		if d.ID == "" || d.Text == "" {
			continue
		}
		ch <- item{d: d}
	}
	close(ch)
	var (
		mu     sync.Mutex
		points []DensePoint
		wg     sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]DensePoint, 0, 8)
			for it := range ch {
				vec, kind, _ := c.embedForDense(ctx, it.d.Text)
				if len(vec) == 0 {
					continue
				}
				local = append(local, DensePoint{
					ID:      it.d.ID,
					Vector:  vec,
					ModelID: c.denseEmbeddingModel(kind),
					Payload: map[string]any{
						"document_id": it.d.ID, "chunk_id": it.d.ID + "#0",
						"source_uri": it.d.SourceURI, "brain_id": c.cfg.BrainID,
					},
				})
			}
			if len(local) == 0 {
				return
			}
			mu.Lock()
			points = append(points, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	_ = c.UpsertDense(ctx, points)
}

func (c *Client) denseEmbeddingModel(kind string) string {
	switch kind {
	case "cohere":
		model := strings.TrimSpace(c.cfg.CohereModel)
		if model == "" {
			model = "embed-v4.0"
		}
		return "cohere:" + model
	case "openai", "openai_fallback":
		return "openai:" + openaiEmbedConfig().Model
	case "mlx":
		return "mlx:" + mlxEmbedConfig().Model
	default:
		return "bag:v1"
	}
}

// embedForDense selects embed backend from substrate (hosted Cohere / OpenAI / mlx / bag).
// Hosted path prefers Cohere when COHERE_API_KEY is set, else OpenAI embeddings
// (OPENAI_API_KEY), else soft bag so residual never hard-fails offline.
func (c *Client) embedForDense(ctx context.Context, text string) ([]float32, string, error) {
	mode := c.substrates.Embed
	if mode == "" {
		mode = SubstrateAPIHosted
	}
	switch mode {
	case SubstrateAPINone, "bag":
		return bagEmbed(text, 256), "bag", nil
	case SubstrateAPIMLX:
		vec, err := embedOpenAICompatible(ctx, text, mlxEmbedConfig())
		if err != nil {
			// Fail soft to bag so local residual still works offline.
			return bagEmbed(text, 256), "bag_fallback", err
		}
		return vec, "mlx", nil
	default: // hosted
		if strings.TrimSpace(os.Getenv("COHERE_API_KEY")) != "" {
			vec, err := EmbedQuery(ctx, text, c.cfg.CohereModel, c.cfg.CohereDim)
			if err == nil {
				return vec, "cohere", nil
			}
			// try OpenAI before bag
			if vec2, err2 := embedOpenAICompatible(ctx, text, openaiEmbedConfig()); err2 == nil {
				return vec2, "openai_fallback", nil
			}
			return bagEmbed(text, 256), "bag_fallback", err
		}
		if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
			vec, err := embedOpenAICompatible(ctx, text, openaiEmbedConfig())
			if err != nil {
				return bagEmbed(text, 256), "bag_fallback", err
			}
			return vec, "openai", nil
		}
		return bagEmbed(text, 256), "bag", fmt.Errorf("hosted: no COHERE_API_KEY or OPENAI_API_KEY for dense embed")
	}
}

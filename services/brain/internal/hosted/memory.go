package hosted

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// MemoryChunkStore is an offline in-memory ChunkStore for product-owned brains
// without Neon/SMF. Used by OpenMemory / NewMemoryBackend.
//
// Holds a per-brain structureIndex (edge hop / entity / facts) so the hosted
// memory path has full residual structure arms without SQLite productbrain.
type MemoryChunkStore struct {
	mu sync.RWMutex
	// key: brainID → chunkID → chunk
	chunks map[string]map[string]memChunk
	// sidecars: brainID → documentID → kind → text (gardener warm artifacts)
	sidecars map[string]map[string]map[string]string
	// structure: brainID → index (co-occur / entities / facts)
	structure map[string]*structureIndex
}

type memChunk struct {
	dsid      string
	text      string
	sourceURI string
	tokens    map[string]int
}

// NewMemoryChunkStore returns an empty product-owned memory store.
func NewMemoryChunkStore() *MemoryChunkStore {
	return &MemoryChunkStore{
		chunks:    map[string]map[string]memChunk{},
		sidecars:  map[string]map[string]map[string]string{},
		structure: map[string]*structureIndex{},
	}
}

// EnsureSchema is a no-op for memory (schema is the in-process map).
func (m *MemoryChunkStore) EnsureSchema(ctx context.Context) error {
	_ = ctx
	if m == nil {
		return fmt.Errorf("hosted: nil memory store")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chunks == nil {
		m.chunks = map[string]map[string]memChunk{}
	}
	return nil
}

// UpsertChunks inserts/replaces chunks for brainID.
func (m *MemoryChunkStore) UpsertChunks(ctx context.Context, brainID string, chunks []ChunkWrite) error {
	_ = ctx
	if m == nil {
		return fmt.Errorf("hosted: nil memory store")
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		return fmt.Errorf("hosted: empty brain_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chunks == nil {
		m.chunks = map[string]map[string]memChunk{}
	}
	bag, ok := m.chunks[brainID]
	if !ok {
		bag = map[string]memChunk{}
		m.chunks[brainID] = bag
	}
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
		bag[ch.ChunkID] = memChunk{
			dsid:      ch.DocumentID,
			text:      ch.Text,
			sourceURI: ch.SourceURI,
			tokens:    memTokenFreq(ch.Text),
		}
	}
	// Rebuild structure index for this brain (hosted-first residual arms).
	m.reindexStructureLocked(brainID)
	return nil
}

// BuildHotLex projects all chunks for brainID into an interactive HotLex index.
func (m *MemoryChunkStore) BuildHotLex(brainID string) *HotLex {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	h := NewHotLex(brainID)
	bag := m.chunks[brainID]
	for chunkID, ch := range bag {
		h.AddChunk(chunkID, ch.dsid, ch.text, ch.sourceURI)
	}
	return h
}

// reindexStructureLocked rebuilds co-occur / entity / facts for brainID.
func (m *MemoryChunkStore) reindexStructureLocked(brainID string) {
	if m.structure == nil {
		m.structure = map[string]*structureIndex{}
	}
	idx := newStructureIndex()
	// Aggregate text per document.
	byDoc := map[string]string{}
	for _, ch := range m.chunks[brainID] {
		byDoc[ch.dsid] = byDoc[ch.dsid] + "\n" + ch.text
	}
	for docID, text := range byDoc {
		idx.indexDocument(docID, "", text)
	}
	idx.rebuildEdges()
	m.structure[brainID] = idx
}

// Structure returns a snapshot of structure expand results (tests / retrieve).
func (m *MemoryChunkStore) StructureExpand(brainID string, seeds []string, maxN int) (edge, entity, facts []string) {
	if m == nil {
		return nil, nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	idx := m.structure[brainID]
	if idx == nil {
		return nil, nil, nil
	}
	return idx.edgeExpand(seeds, maxN), idx.entityFanout(seeds, maxN), nil
}

// StructureFacts ranks docs by fact-channel match to question.
func (m *MemoryChunkStore) StructureFacts(brainID, question string, limit int) []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	idx := m.structure[brainID]
	if idx == nil {
		return nil
	}
	return idx.factsSearch(question, limit)
}

// PassagesForDocs returns one passage per document id (first chunk text).
func (m *MemoryChunkStore) PassagesForDocs(brainID string, docIDs []string, maxChars int) []Passage {
	if m == nil || len(docIDs) == 0 {
		return nil
	}
	if maxChars <= 0 {
		maxChars = 2000
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	bag := m.chunks[brainID]
	var out []Passage
	for _, id := range docIDs {
		for chunkID, ch := range bag {
			if ch.dsid != id {
				continue
			}
			text := clipPassageText(ch.text, maxChars)
			out = append(out, Passage{
				DocumentID: id,
				Text:       text,
				ChunkID:    chunkID,
				Score:      0.35,
				Channel:    "structure_hop",
			})
			break
		}
	}
	return out
}

// LexicalSearch ranks memory chunks by query-token TF sum (bag-of-words).
func (m *MemoryChunkStore) LexicalSearch(ctx context.Context, brainID, question string, limit int) ([]Hit, error) {
	_ = ctx
	if m == nil {
		return nil, fmt.Errorf("hosted: nil memory store")
	}
	if limit <= 0 {
		limit = 30
	}
	qTokens := memTokenize(question)
	if len(qTokens) == 0 {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	bag := m.chunks[brainID]
	if len(bag) == 0 {
		return nil, nil
	}
	type scored struct {
		h Hit
	}
	var hits []scored
	seenQ := map[string]struct{}{}
	for _, t := range qTokens {
		seenQ[t] = struct{}{}
	}
	// Sidecar boost: gardener d2q / context_header text for the same document.
	sideByDoc := m.sidecars[brainID]
	for chunkID, ch := range bag {
		var score float64
		for t := range seenQ {
			if n, ok := ch.tokens[t]; ok {
				score += float64(n)
			}
		}
		// Soft-boost when a warmed sidecar for this dsid matches query tokens.
		if sideByDoc != nil {
			if kinds, ok := sideByDoc[ch.dsid]; ok {
				for _, text := range kinds {
					for _, st := range memTokenize(text) {
						if _, hit := seenQ[st]; hit {
							score += 0.5
						}
					}
				}
			}
		}
		if score <= 0 {
			continue
		}
		hits = append(hits, scored{h: Hit{
			ChunkID:   chunkID,
			DSID:      ch.dsid,
			Text:      ch.text,
			SourceURI: ch.sourceURI,
			Score:     score,
			Channel:   "lexical",
		}})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].h.Score != hits[j].h.Score {
			return hits[i].h.Score > hits[j].h.Score
		}
		return hits[i].h.ChunkID < hits[j].h.ChunkID
	})
	if limit > len(hits) {
		limit = len(hits)
	}
	out := make([]Hit, limit)
	for i := 0; i < limit; i++ {
		out[i] = hits[i].h
	}
	return out, nil
}

// SiblingChunks returns other chunks sharing the same dsid.
func (m *MemoryChunkStore) SiblingChunks(ctx context.Context, brainID, dsid string, limit int) ([]Hit, error) {
	_ = ctx
	if m == nil {
		return nil, fmt.Errorf("hosted: nil memory store")
	}
	if limit <= 0 {
		limit = 4
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	bag := m.chunks[brainID]
	var out []Hit
	for chunkID, ch := range bag {
		if ch.dsid != dsid {
			continue
		}
		out = append(out, Hit{
			ChunkID:   chunkID,
			DSID:      dsid,
			Text:      ch.text,
			SourceURI: ch.sourceURI,
			Channel:   "hydrate",
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ChunkCount returns how many chunks are stored for brainID (tests/diagnostics).
func (m *MemoryChunkStore) ChunkCount(brainID string) int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.chunks[brainID])
}

// DocumentIDs returns distinct document (dsid) ids for brainID.
func (m *MemoryChunkStore) DocumentIDs(brainID string) []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, ch := range m.chunks[brainID] {
		if ch.dsid != "" {
			seen[ch.dsid] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// DeleteDocuments removes all chunks for the given document ids. Returns chunks removed.
func (m *MemoryChunkStore) DeleteDocuments(brainID string, docIDs []string) int {
	if m == nil || len(docIDs) == 0 {
		return 0
	}
	brainID = strings.TrimSpace(brainID)
	want := map[string]struct{}{}
	for _, id := range docIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			want[id] = struct{}{}
		}
	}
	if len(want) == 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bag := m.chunks[brainID]
	if len(bag) == 0 {
		return 0
	}
	n := 0
	for chunkID, ch := range bag {
		if _, ok := want[ch.dsid]; ok {
			delete(bag, chunkID)
			n++
		}
	}
	if n > 0 {
		// Drop sidecars for deleted docs.
		if sides := m.sidecars[brainID]; sides != nil {
			for id := range want {
				delete(sides, id)
			}
		}
		m.reindexStructureLocked(brainID)
	}
	return n
}

func memTokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		out = append(out, f)
	}
	return out
}

func memTokenFreq(s string) map[string]int {
	toks := memTokenize(s)
	if len(toks) == 0 {
		return nil
	}
	freq := make(map[string]int, len(toks)/2+1)
	for _, t := range toks {
		freq[t]++
	}
	return freq
}

// OpenMemory builds a product-owned hosted Client backed only by MemoryChunkStore.
// No Neon/Qdrant/SMF required — Create + Burst + Retrieve work offline.
// When substrate env/Dir is set (OUROBOROS_BRAIN_DIR or profile), binds
// queue+cortex so memory chunks still run the full residual pipeline (ADR 0024).
func OpenMemory(brainID string) *Client {
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		brainID = "memory-local"
	}
	store := NewMemoryChunkStore()
	c := &Client{
		cfg: Config{
			BrainID:         brainID,
			LexicalLimit:    30,
			DenseLimit:      0,
			RRFK:            60,
			PoolK:           40,
			TopK:            8,
			MaxCite:         3,
			MaxPassageChars: 2000,
		},
		store:        store,
		productOwned: true,
		qcache:       newQueryCache(90 * time.Second),
		rerankScores: newRerankScoreCache(0, 0),
	}
	// HotLex rebuilt on each Upsert via EnsureHotLex after writes.
	c.hot = NewHotLex(brainID)
	sub := SubstrateFromEnv()
	if sub.Dir != "" || sub.Queue != "" || sub.Cortex != "" || sub.Profile != "" {
		if sub.Chunks == "" {
			sub.Chunks = SubstrateChunksMemory
		}
		_ = ApplySubstrates(c, sub)
	}
	return c
}

// OpenMemoryWithSubstrates opens in-process chunks and binds queue/cortex explicitly.
func OpenMemoryWithSubstrates(brainID string, cfg SubstrateConfig) (*Client, error) {
	c := OpenMemory(brainID)
	cfg.Chunks = SubstrateChunksMemory
	if err := ApplySubstrates(c, cfg); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// EnsureHotLex rebuilds interactive index from the memory/local store.
func (c *Client) EnsureHotLex() {
	if c == nil {
		return
	}
	switch s := c.store.(type) {
	case *MemoryChunkStore:
		c.hot = s.BuildHotLex(c.cfg.BrainID)
	case *durableStore:
		c.hot = s.inner.BuildHotLex(c.cfg.BrainID)
		if c.hot != nil {
			c.hot.Generation = s.generationID()
		}
	}
}

// EnsureHotLexIncremental updates HotLex for the given documents when an index
// is already loaded; otherwise falls back to full EnsureHotLex.
// Prefer this on ContinualDeltaLocal / small BurstIngestLocal deltas.
// GAP-IR-HOTLEX-SCALE: stays O(delta) unless delta dominates live N.
func (c *Client) EnsureHotLexIncremental(docs []LocalDocument) {
	if c == nil {
		return
	}
	if c.hot == nil || c.hot.Len() == 0 {
		c.EnsureHotLex()
		return
	}
	chunks := DocumentsToChunks(docs)
	if len(chunks) == 0 {
		return
	}
	// A mapped snapshot is immutable by design. Rebuild from the authoritative
	// store on the first write instead of decoding the serving image into heap.
	if c.hot.mapped != nil {
		_ = c.hot.Close()
		c.hot = nil
		c.EnsureHotLex()
		return
	}
	// Heuristic: if delta is large vs corpus, full rebuild is cheaper/cleaner.
	liveN := c.hot.Len()
	if liveN > 0 && len(chunks) > liveN/2 && len(chunks) > 32 {
		c.EnsureHotLex()
		return
	}
	// Cap single incremental batch to avoid pathological long critical sections.
	const maxIncr = 512
	if len(chunks) > maxIncr {
		c.EnsureHotLex()
		return
	}
	for _, ch := range chunks {
		c.hot.AddChunk(ch.ChunkID, ch.DocumentID, ch.Text, ch.SourceURI)
	}
	c.hot.Finalize()
	// Optional gob flush without full rebuild from chunks.
	c.flushHotLexFromClient()
}

// flushHotLexFromClient persists c.hot to hotlex.gob when durable local dir set.
func (c *Client) flushHotLexFromClient() {
	if c == nil || c.hot == nil || c.local == nil {
		return
	}
	dir := c.local.dir
	if dir == "" {
		return
	}
	c.hot.Generation = c.local.generationID()
	_ = c.hot.SaveGob(filepath.Join(dir, "hotlex.gob"))
}

// NewMemoryBackend is an alias for OpenMemory (acceptance / short name).
func NewMemoryBackend(brainID string) *Client {
	return OpenMemory(brainID)
}

// WarmSidecars stores gardener warm artifacts in-process and enables soft
// LexicalSearch boost when sidecar text matches the query.
func (m *MemoryChunkStore) WarmSidecars(ctx context.Context, brainID string, items []SidecarWrite) (int, error) {
	_ = ctx
	if m == nil {
		return 0, fmt.Errorf("hosted: nil memory store")
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		return 0, fmt.Errorf("hosted: empty brain_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sidecars == nil {
		m.sidecars = map[string]map[string]map[string]string{}
	}
	bag, ok := m.sidecars[brainID]
	if !ok {
		bag = map[string]map[string]string{}
		m.sidecars[brainID] = bag
	}
	n := 0
	for _, it := range items {
		doc := strings.TrimSpace(it.DocumentID)
		text := strings.TrimSpace(it.Text)
		kind := strings.TrimSpace(it.Kind)
		if doc == "" || text == "" {
			continue
		}
		if kind == "" {
			kind = "d2q"
		}
		if bag[doc] == nil {
			bag[doc] = map[string]string{}
		}
		bag[doc][kind] = text
		n++
	}
	return n, nil
}

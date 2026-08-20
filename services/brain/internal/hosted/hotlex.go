package hosted

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode"
)

// HotLex is the interactive lexical serving index: inverted postings over
// chunks, BM25-scored. Built at project/upsert time; query-time is a mutable
// in-memory map walk or direct read-only mmap-table lookup (no Neon GIN).
//
// Architecture: Neon/path2 remain source-of-truth + hydrate-by-id.
// Interactive Ask prefers HotLex + dense ANN, then hydrates text by chunk_id.
//
// Build path is optimized for multi-million chunk projection:
//   - O(1) AvgDL via running sumLen (never O(N) scan per AddChunk)
//   - bulk/shard builders without per-doc lock; Finalize once
//   - MergeShards for parallel Modal/local workers
type HotLex struct {
	mu sync.RWMutex

	// BrainID scopes this projection.
	BrainID string
	// Generation pins invalidation.
	Generation string

	// N is the live document (chunk) count; mutable docs may retain tombstone slots.
	N int
	// AvgDL is average document length in tokens.
	AvgDL float64
	// sumLen is running token-length sum for O(1) AvgDL.
	sumLen int64

	// postings: token → list of (chunk index, tf)
	postings map[string][]hotPosting
	// docs: dense array of chunk metadata (+ optional text for offline/local).
	docs []hotDoc

	// byChunk maps chunk_id → index in docs
	byChunk map[string]int

	// mapped is the immutable serving view used by v2 snapshots. Corpus tables
	// remain in the file mapping; only BrainID/Generation and query results are
	// decoded into Go strings. Builders and legacy recovery use the fields above.
	mapped *hotLexMapped

	// sourceFormat is diagnostic-only. It records the successfully validated
	// on-disk format; it never participates in ranking or authorization.
	sourceFormat HotLexSnapshotFormat
}

type hotPosting struct {
	Doc int32
	TF  int32
}

type hotDoc struct {
	ChunkID   string
	DSID      string
	SourceURI string
	Length    int32 // token count
	Text      string
	HasText   bool
}

// HotLexParams BM25 parameters.
type HotLexParams struct {
	K1 float64
	B  float64
}

func defaultBM25() HotLexParams { return HotLexParams{K1: 1.2, B: 0.75} }

// NewHotLex empty index.
func NewHotLex(brainID string) *HotLex {
	return &HotLex{
		BrainID:  brainID,
		postings: map[string][]hotPosting{},
		byChunk:  map[string]int{},
	}
}

// AddChunk indexes one chunk (replaces existing chunk_id). Stores full text
// for offline/local serve. Path2 full projections should call StripStoredText
// before SaveGob and hydrate-by-id at query time.
func (h *HotLex) AddChunk(chunkID, dsid, text, sourceURI string) {
	h.addChunk(chunkID, dsid, text, sourceURI, true, true)
}

// AddChunkIndexOnly indexes tokens but does not retain body text (hydrate later).
func (h *HotLex) AddChunkIndexOnly(chunkID, dsid, text, sourceURI string) {
	h.addChunk(chunkID, dsid, text, sourceURI, false, true)
}

// AddChunkBulk is lock-free for a single-threaded builder (shard worker).
// Caller must not share h across goroutines; call Finalize after bulk load.
func (h *HotLex) AddChunkBulk(chunkID, dsid, text, sourceURI string, storeText bool) {
	h.addChunk(chunkID, dsid, text, sourceURI, storeText, false)
}

func (h *HotLex) addChunk(chunkID, dsid, text, sourceURI string, storeText, withLock bool) {
	if h == nil || strings.TrimSpace(chunkID) == "" || strings.TrimSpace(text) == "" {
		return
	}
	if withLock {
		h.mu.Lock()
		defer h.mu.Unlock()
	}
	h.materializeMappedLocked()
	if h.postings == nil {
		h.postings = map[string][]hotPosting{}
	}
	if h.byChunk == nil {
		h.byChunk = map[string]int{}
	}
	// Replace existing: adjust sumLen for tombstone path.
	if idx, ok := h.byChunk[chunkID]; ok {
		h.removeDocLocked(idx)
	}
	toks := hotTokenize(text)
	if len(toks) == 0 {
		return
	}
	tf := map[string]int32{}
	for _, t := range toks {
		tf[t]++
	}
	docIdx := len(h.docs)
	doc := hotDoc{
		ChunkID:   chunkID,
		DSID:      dsid,
		SourceURI: sourceURI,
		Length:    int32(len(toks)),
	}
	if storeText {
		doc.Text = text
		doc.HasText = true
	}
	h.docs = append(h.docs, doc)
	h.byChunk[chunkID] = docIdx
	for t, c := range tf {
		h.postings[t] = append(h.postings[t], hotPosting{Doc: int32(docIdx), TF: c})
	}
	h.sumLen += int64(len(toks))
	h.recomputeNLocked()
	h.recomputeAvgDLLocked()
}

// Finalize recomputes AvgDL from sumLen (idempotent).
func (h *HotLex) Finalize() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mapped != nil {
		return
	}
	h.recomputeAvgDLLocked()
}

func (h *HotLex) recomputeAvgDLLocked() {
	if h.N > 0 && h.sumLen > 0 {
		h.AvgDL = float64(h.sumLen) / float64(h.N)
	} else if h.N > 0 {
		// Fallback if sumLen unset (legacy loads): one scan.
		var sum int64
		for i := range h.docs {
			sum += int64(h.docs[i].Length)
		}
		h.sumLen = sum
		h.AvgDL = float64(sum) / float64(h.N)
	} else {
		h.AvgDL = 0
	}
}

// StripStoredText drops body text from all docs (keeps BM25 postings).
func (h *HotLex) StripStoredText() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.materializeMappedLocked()
	for i := range h.docs {
		h.docs[i].Text = ""
		h.docs[i].HasText = false
	}
}

func (h *HotLex) removeDocLocked(idx int) {
	if idx < 0 || idx >= len(h.docs) {
		return
	}
	old := h.docs[idx]
	if old.Length > 0 {
		h.sumLen -= int64(old.Length)
		if h.sumLen < 0 {
			h.sumLen = 0
		}
	}
	for t, plist := range h.postings {
		dst := plist[:0]
		for _, p := range plist {
			if int(p.Doc) != idx {
				dst = append(dst, p)
			}
		}
		if len(dst) == 0 {
			delete(h.postings, t)
		} else {
			h.postings[t] = dst
		}
	}
	delete(h.byChunk, old.ChunkID)
	h.docs[idx] = hotDoc{} // tombstone
	// Keep N as active (non-tombstone) count so BM25 IDF/AvgDL stay honest.
	h.recomputeNLocked()
}

// recomputeNLocked sets N to the count of live (non-tombstone) docs.
func (h *HotLex) recomputeNLocked() {
	n := 0
	for _, d := range h.docs {
		if d.ChunkID != "" {
			n++
		}
	}
	h.N = n
}

// RemoveDocument tombstones all chunks for the given document (dsid).
func (h *HotLex) RemoveDocument(dsid string) int {
	if h == nil || strings.TrimSpace(dsid) == "" {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.materializeMappedLocked()
	n := 0
	for i := range h.docs {
		if h.docs[i].ChunkID != "" && h.docs[i].DSID == dsid {
			h.removeDocLocked(i)
			n++
		}
	}
	if n > 0 {
		h.recomputeAvgDLLocked()
		h.maybeCompactLocked()
	}
	return n
}

// maybeCompactLocked rebuilds docs/postings when tombstones dominate (GAP-IR-HOTLEX-SCALE).
func (h *HotLex) maybeCompactLocked() {
	if h == nil {
		return
	}
	live := 0
	for _, d := range h.docs {
		if d.ChunkID != "" {
			live++
		}
	}
	total := len(h.docs)
	if total == 0 || live == 0 {
		return
	}
	// Compact when >25% slots are tombstones and at least 32 slots wasted.
	if total-live < 32 || float64(total-live)/float64(total) < 0.25 {
		return
	}
	// Rebuild via bulk add of live docs.
	//
	// This only works while the docs still carry their text: addChunk returns
	// early on empty text, and StripStoredText -- the index-only serving shape
	// -- clears it on every doc. Replaying it there dropped every live
	// document and left N=0, so the whole lexical index went silently empty
	// once tombstones crossed the threshold.
	//
	// A stripped index keeps its tombstones instead. They cost a little space
	// and are filtered at query time; an empty index costs every answer.
	type snap struct {
		chunkID, dsid, text, uri string
		hasText                  bool
	}
	var keep []snap
	for _, d := range h.docs {
		if d.ChunkID == "" {
			continue
		}
		if !d.HasText {
			return
		}
		keep = append(keep, snap{d.ChunkID, d.DSID, d.Text, d.SourceURI, d.HasText})
	}
	h.docs = nil
	h.postings = map[string][]hotPosting{}
	h.byChunk = map[string]int{}
	h.sumLen = 0
	h.N = 0
	for _, s := range keep {
		h.addChunk(s.chunkID, s.dsid, s.text, s.uri, s.hasText, false)
	}
	h.recomputeAvgDLLocked()
}

// MergeShards concatenates shard indexes into one (doc indices remapped).
// Shards must be disjoint by chunk_id. Result is a new HotLex.
func MergeShards(brainID string, shards []*HotLex) *HotLex {
	out, err := MergeHotLexShards(brainID, shards)
	if err != nil {
		// Historical callers did not accept an error. An invalid cross-scope merge
		// must nevertheless fail closed instead of publishing a partial index.
		return NewHotLex(brainID)
	}
	return out
}

// MergeHotLexShards merges compatible shards and rejects mixed brain or
// generation scope. Shards must be disjoint by chunk_id.
func MergeHotLexShards(brainID string, shards []*HotLex) (*HotLex, error) {
	out := NewHotLex(brainID)
	wantGeneration := ""
	generationSet := false
	total := 0
	for _, s := range shards {
		if s != nil {
			if err := validateHotLexShardScope(brainID, s); err != nil {
				return nil, err
			}
			if generationSet && s.Generation != wantGeneration {
				return nil, fmt.Errorf("%w: shard %q target %q", ErrHotLexStale, s.Generation, wantGeneration)
			}
			if !generationSet {
				wantGeneration = s.Generation
				generationSet = true
			}
			s.materializeMapped()
			total += len(s.docs)
		}
	}
	if total > 0 {
		out.docs = make([]hotDoc, 0, total)
	}
	for _, s := range shards {
		if s == nil || len(s.docs) == 0 {
			continue
		}
		oldToNew := make([]int32, len(s.docs))
		for i := range oldToNew {
			oldToNew[i] = -1
		}
		for i, d := range s.docs {
			if d.ChunkID == "" {
				continue
			}
			if _, exists := out.byChunk[d.ChunkID]; exists {
				return nil, fmt.Errorf("%w: duplicate shard chunk %q", ErrHotLexCorrupt, d.ChunkID)
			}
			newIdx := len(out.docs)
			out.docs = append(out.docs, d)
			out.byChunk[d.ChunkID] = newIdx
			out.sumLen += int64(d.Length)
			oldToNew[i] = int32(newIdx)
		}
		for term, plist := range s.postings {
			for _, p := range plist {
				di := int(p.Doc)
				if di < 0 || di >= len(oldToNew) {
					continue
				}
				ni := oldToNew[di]
				if ni < 0 {
					continue
				}
				out.postings[term] = append(out.postings[term], hotPosting{Doc: ni, TF: p.TF})
			}
		}
		if s.Generation != "" && out.Generation == "" {
			out.Generation = s.Generation
		}
	}
	out.N = len(out.docs)
	out.recomputeAvgDLLocked()
	out.Generation = wantGeneration
	return out, nil
}

// Search BM25-ranks the query; returns Hits with Text when stored in index.
// It uses bounded top-k selection with the same strict total order (score
// desc, chunk-id asc) as the historical full sort, so rankings are identical.
func (h *HotLex) Search(query string, limit int) []Hit {
	hits, _ := h.SearchWithOptions(query, limit, HotLexSearchOptions{})
	return hits
}

// SearchWithOptions is Search with explicit, observable deviations (see
// HotLexSearchOptions). The zero option value reproduces Search exactly.
func (h *HotLex) SearchWithOptions(query string, limit int, opts HotLexSearchOptions) ([]Hit, HotLexSearchStats) {
	var stats HotLexSearchStats
	if h == nil || limit <= 0 {
		return nil, stats
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.N == 0 {
		return nil, stats
	}
	if h.mapped != nil {
		return h.searchMappedLocked(query, limit, opts)
	}
	qtoks := hotTokenize(query)
	if len(qtoks) == 0 {
		return nil, stats
	}
	seenQ := map[string]struct{}{}
	var terms []string
	for _, t := range qtoks {
		if _, ok := seenQ[t]; ok {
			continue
		}
		seenQ[t] = struct{}{}
		terms = append(terms, t)
	}
	params := defaultBM25()
	scores := map[int]float64{}
	N := float64(h.N)
	avgdl := h.AvgDL
	if avgdl < 1 {
		avgdl = 1
	}
	for _, t := range terms {
		plist := h.postings[t]
		df := float64(len(plist))
		if df == 0 {
			continue
		}
		if hotPruneStats(&stats, t, len(plist), opts.MaxDocumentFrequency) {
			continue
		}
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))
		for _, p := range plist {
			di := int(p.Doc)
			if di < 0 || di >= len(h.docs) || h.docs[di].ChunkID == "" {
				continue
			}
			tf := float64(p.TF)
			dl := float64(h.docs[di].Length)
			if dl < 1 {
				dl = 1
			}
			num := tf * (params.K1 + 1)
			den := tf + params.K1*(1-params.B+params.B*dl/avgdl)
			scores[di] += idf * num / den
		}
	}
	stats.MatchedDocs = len(scores)
	// Bounded selection under the same strict total order as the historical
	// full sort (score desc, chunk-id asc): identical ranking, O(n log k).
	arr := hotTopKMap(scores, limit, func(a, b hotScored) bool {
		if scoreOrder := hotScoreCompare(a.s, b.s); scoreOrder != 0 {
			return scoreOrder > 0
		}
		return h.docs[a.i].ChunkID < h.docs[b.i].ChunkID
	})
	limit = len(arr)
	out := make([]Hit, 0, limit)
	for i := 0; i < limit; i++ {
		d := h.docs[arr[i].i]
		hit := Hit{
			ChunkID:   d.ChunkID,
			DSID:      d.DSID,
			SourceURI: d.SourceURI,
			Score:     arr[i].s,
			Channel:   "hot_lex",
		}
		if d.HasText {
			hit.Text = d.Text
		}
		out = append(out, hit)
	}
	return out, stats
}

// Len returns doc slot count.
func (h *HotLex) Len() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.N
}

// ProjectChunks builds a HotLex from chunk writes (one-shot).
func ProjectChunks(brainID string, chunks []ChunkWrite) *HotLex {
	h := NewHotLex(brainID)
	for _, ch := range chunks {
		h.AddChunkBulk(ch.ChunkID, ch.DocumentID, ch.Text, ch.SourceURI, true)
	}
	h.Finalize()
	return h
}

// hotTokenize lowercases alphanumeric tokens len>=2.
func hotTokenize(s string) []string {
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

// Strong enough for early exit: enough hits, top score floor, and top-heavy
// (top score not a flat plate of near-ties — that often means topic mush without gold).
func hotLexStrong(hits []Hit, minHits int, minTopScore float64) bool {
	if len(hits) < minHits {
		return false
	}
	if hits[0].Score < minTopScore {
		return false
	}
	// Require dominance: top score ≥ 1.15× score at rank min(minHits, len-1).
	// Flat top-K (all ~same score) is common on semantic near-dups and is NOT strong.
	tail := minHits - 1
	if tail >= len(hits) {
		tail = len(hits) - 1
	}
	if tail > 0 && hits[tail].Score > 0 && hits[0].Score < hits[tail].Score*1.15 {
		return false
	}
	return true
}

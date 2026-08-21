package hosted

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"io"
	"sort"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/durablefile"
)

const (
	localMetaName     = "meta.json"
	localChunksName   = "chunks.jsonl"
	localDeltaName    = "chunks.delta.jsonl" // append-only upserts; compacted into chunks.jsonl
	localSidecarName  = "sidecars.jsonl"
	localSidecarDelta = "sidecars.delta.jsonl"
	localStackStamp   = "residual_parity_v2"
	// compactDeltaLines: when delta file has this many lines, rewrite base + clear delta.
	compactDeltaLines = 512
)

// localMeta is durable brain identity on disk (FS projection of product store).
type localMeta struct {
	BrainID      string `json:"brain_id"`
	GenerationID string `json:"generation_id"`
	Stack        string `json:"product_stack"`
	Store        string `json:"store"`
}

// durableStore wraps MemoryChunkStore and flushes corpus + sidecars to dir.
// Local is not a second retrieve engine: same Client + retrieveMemory path.
// Parallel BurstUpsert: workers update memory under the inner mutex; disk flush
// is deferred (deferFlush) so N workers do not each rewrite chunks.jsonl.
// Dirty chunk IDs flush via append-only chunks.delta.jsonl (GAP-IR-DELTA-STORE);
// compact rewrites base chunks.jsonl when the delta grows past compactDeltaLines.
type durableStore struct {
	inner      *MemoryChunkStore
	dir        string
	brainID    string
	gen        atomic.Value // string
	mu         sync.Mutex
	deferFlush atomic.Int32 // >0: skip per-upsert flush (batch mode)
	// dirty chunk_ids waiting for delta append (protected by mu).
	dirty map[string]struct{}
	// dirtySidecars document_ids with kind changes (append sidecars.delta.jsonl).
	dirtySidecars map[string]struct{}
	// forceFullFlush rewrites entire base (create empty / compact / explicit).
	forceFullFlush bool
	// hotDirty: when true, rebuild hotlex.gob; false skips gob rewrite if file exists.
	hotDirty bool
}

func (d *durableStore) EnsureSchema(ctx context.Context) error {
	return d.inner.EnsureSchema(ctx)
}

// beginBatch defers disk flush until endBatch (for parallel local workers).
func (d *durableStore) beginBatch() {
	if d != nil {
		d.deferFlush.Add(1)
	}
}

// endBatch flushes once after parallel workers finish.
func (d *durableStore) endBatch() error {
	if d == nil {
		return nil
	}
	if d.deferFlush.Add(-1) == 0 {
		return d.flush()
	}
	return nil
}

func (d *durableStore) UpsertChunks(ctx context.Context, brainID string, chunks []ChunkWrite) error {
	if err := d.inner.UpsertChunks(ctx, brainID, chunks); err != nil {
		return err
	}
	d.mu.Lock()
	if d.dirty == nil {
		d.dirty = map[string]struct{}{}
	}
	for _, ch := range chunks {
		id := strings.TrimSpace(ch.ChunkID)
		if id == "" {
			id = strings.TrimSpace(ch.DocumentID) + "#0"
		}
		if id != "" {
			d.dirty[id] = struct{}{}
		}
	}
	d.hotDirty = true
	d.mu.Unlock()
	if d.deferFlush.Load() > 0 {
		return nil
	}
	return d.flush()
}

func (d *durableStore) LexicalSearch(ctx context.Context, brainID, question string, limit int) ([]Hit, error) {
	return d.inner.LexicalSearch(ctx, brainID, question, limit)
}

func (d *durableStore) SiblingChunks(ctx context.Context, brainID, dsid string, limit int) ([]Hit, error) {
	return d.inner.SiblingChunks(ctx, brainID, dsid, limit)
}

func (d *durableStore) WarmSidecars(ctx context.Context, brainID string, items []SidecarWrite) (int, error) {
	n, err := d.inner.WarmSidecars(ctx, brainID, items)
	if err != nil {
		return n, err
	}
	if n > 0 {
		d.mu.Lock()
		if d.dirtySidecars == nil {
			d.dirtySidecars = map[string]struct{}{}
		}
		for _, it := range items {
			if it.DocumentID != "" {
				d.dirtySidecars[it.DocumentID] = struct{}{}
			}
		}
		d.mu.Unlock()
		if err := d.flush(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// StructureExpand delegates to the in-memory index (same as MemoryChunkStore).
func (d *durableStore) StructureExpand(brainID string, seeds []string, maxN int) (edge, entity, facts []string) {
	return d.inner.StructureExpand(brainID, seeds, maxN)
}

func (d *durableStore) StructureFacts(brainID, question string, limit int) []string {
	return d.inner.StructureFacts(brainID, question, limit)
}

func (d *durableStore) PassagesForDocs(brainID string, docIDs []string, maxChars int) []Passage {
	return d.inner.PassagesForDocs(brainID, docIDs, maxChars)
}

// DocumentIDs lists distinct document ids (delegates to memory bag).
func (d *durableStore) DocumentIDs(brainID string) []string {
	if d == nil || d.inner == nil {
		return nil
	}
	return d.inner.DocumentIDs(brainID)
}

// DeleteDocuments removes docs from the bag and force-rewrites durable base.
func (d *durableStore) DeleteDocuments(brainID string, docIDs []string) int {
	if d == nil || d.inner == nil || len(docIDs) == 0 {
		return 0
	}
	n := d.inner.DeleteDocuments(brainID, docIDs)
	if n == 0 {
		return 0
	}
	d.mu.Lock()
	d.forceFullFlush = true
	d.hotDirty = true
	// Clear dirty maps so flush rewrites base cleanly without stale upserts.
	d.dirty = map[string]struct{}{}
	d.mu.Unlock()
	_ = d.flush()
	return n
}

func (d *durableStore) flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dir == "" {
		return fmt.Errorf("hosted: durable store missing dir")
	}
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return err
	}
	gen, _ := d.gen.Load().(string)
	if gen == "" {
		gen = "gen-0"
	}
	meta := localMeta{
		BrainID:      d.brainID,
		GenerationID: gen,
		Stack:        localStackStamp,
		Store:        "local_fs",
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := durablefile.Write(filepath.Join(d.dir, localMetaName), append(raw, '\n'), localFilePerm); err != nil {
		return err
	}

	// Snapshot under the lock rather than aliasing the live maps.
	//
	// This took inner.mu.RLock, read the two map references, released the lock,
	// and then ranged over them in writeFullChunksLocked / flushSidecarsLocked.
	// Those are the *live* maps: the auto-gardener calls flush on a 500ms loop
	// while ingest writes into them under inner.mu -- a different mutex from
	// the one flush holds. Go reports concurrent map iteration and map write as
	// a fatal error, which no recover can catch.
	//
	// The copy costs one pass over the corpus per flush; the alternative costs
	// the process.
	d.inner.mu.RLock()
	liveBag := d.inner.chunks[d.brainID]
	bag := make(map[string]memChunk, len(liveBag))
	for id, chunk := range liveBag {
		bag[id] = chunk
	}
	liveSide := d.inner.sidecars[d.brainID]
	side := make(map[string]map[string]string, len(liveSide))
	for docID, kinds := range liveSide {
		copied := make(map[string]string, len(kinds))
		for kind, text := range kinds {
			copied[kind] = text
		}
		side[docID] = copied
	}
	d.inner.mu.RUnlock()

	basePath := filepath.Join(d.dir, localChunksName)
	deltaPath := filepath.Join(d.dir, localDeltaName)
	baseInfo, baseErr := os.Stat(basePath)
	baseMissing := os.IsNotExist(baseErr)
	baseEmpty := baseMissing || (baseErr == nil && baseInfo.Size() == 0)
	// Snapshot dirty chunk ids for incremental HotLex merge before clear.
	dirtyIDs := make([]string, 0, len(d.dirty))
	for id := range d.dirty {
		dirtyIDs = append(dirtyIDs, id)
	}
	// First non-empty corpus write or explicit compact: full base rewrite.
	//
	// forcedFull is read once, here, and passed on. The corpus branch below
	// used to clear d.forceFullFlush itself, before flushSidecarsLocked read
	// the same field -- so an explicit full-rewrite request never reached the
	// sidecar writer at all. DeleteDocuments is the caller that sets it, which
	// meant a deleted document's derived sidecar text stayed in sidecars.jsonl
	// indefinitely while its chunks were correctly removed from the corpus.
	//
	// It is cleared after the sidecars are written, so a failure part way
	// through leaves the request pending rather than swallowing it.
	forcedFull := d.forceFullFlush
	needFull := forcedFull || (baseEmpty && len(bag) > 0)
	if !needFull && len(d.dirty) == 0 {
		// Only meta/sidecars/hotlex may need update.
	} else if needFull {
		if err := d.writeFullChunksLocked(basePath, bag); err != nil {
			return err
		}
		_ = os.Remove(deltaPath)
		d.dirty = map[string]struct{}{}
	} else {
		// Append-only delta for dirty chunk ids (no full corpus rewrite).
		if err := d.appendDeltaLocked(deltaPath, bag, d.dirty); err != nil {
			return err
		}
		d.dirty = map[string]struct{}{}
		// Compact when delta grows.
		if n := countJSONLLines(deltaPath); n >= compactDeltaLines {
			if err := d.writeFullChunksLocked(basePath, bag); err != nil {
				return err
			}
			_ = os.Remove(deltaPath)
		}
	}

	// Sidecars: append delta for dirty docs; full rewrite only when base missing
	// or compacting, or when a full rewrite was explicitly requested.
	if err := d.flushSidecarsLocked(side, forcedFull); err != nil {
		return err
	}
	d.forceFullFlush = false
	// HotLex: incremental merge when gob exists and only dirty ids changed.
	gobPath := filepath.Join(d.dir, "hotlex.gob")
	_, gobErr := os.Stat(gobPath)
	if d.hotDirty || os.IsNotExist(gobErr) {
		var h *HotLex
		if gobErr == nil && len(dirtyIDs) > 0 && len(dirtyIDs) < 256 {
			// Incremental: load gob, AddChunk dirty only, Finalize + Save.
			if existing, err := LoadHotLexSnapshot(gobPath, HotLexSnapshotScope{
				BrainID: d.brainID, Generation: d.generationID(), AllowLegacyGob: true,
			}); err == nil && existing != nil {
				if existing.mapped == nil {
					h = existing
					for _, id := range dirtyIDs {
						if ch, ok := bag[id]; ok {
							h.AddChunk(id, ch.dsid, ch.text, ch.sourceURI)
						}
					}
					h.Finalize()
				} else {
					_ = existing.Close()
				}
			}
		}
		if h == nil {
			h = d.inner.BuildHotLex(d.brainID)
		}
		if h != nil {
			h.Generation = d.generationID()
			if err := h.SaveGob(gobPath); err != nil {
				return fmt.Errorf("hosted: persist hotlex.gob: %w", err)
			}
		}
		d.hotDirty = false
	}
	return nil
}

func (d *durableStore) flushSidecarsLocked(side map[string]map[string]string, forceFull bool) error {
	base := filepath.Join(d.dir, localSidecarName)
	delta := filepath.Join(d.dir, localSidecarDelta)

	// writeBase had the same defect as the corpus writer: os.Create truncated
	// the live file, every Encode error was discarded, and Close's error --
	// where a buffered-write failure such as ENOSPC actually surfaces -- was
	// thrown away before the delta was removed.
	writeBase := func() error {
		docIDs := make([]string, 0, len(side))
		for docID := range side {
			docIDs = append(docIDs, docID)
		}
		sort.Strings(docIDs)
		return durablefile.WriteFunc(base, localFilePerm, func(w io.Writer) error {
			enc := json.NewEncoder(w)
			for _, docID := range docIDs {
				kinds := side[docID]
				names := make([]string, 0, len(kinds))
				for kind := range kinds {
					names = append(names, kind)
				}
				sort.Strings(names)
				for _, kind := range names {
					if err := enc.Encode(map[string]string{
						"document_id": docID, "kind": kind, "text": kinds[kind], "op": "upsert",
					}); err != nil {
						return err
					}
				}
			}
			return nil
		})
	}

	info, err := os.Stat(base)
	baseEmpty := os.IsNotExist(err) || (err == nil && info.Size() == 0)
	if baseEmpty || forceFull {
		if err := writeBase(); err != nil {
			return err
		}
		// Only now is it safe to drop the delta: the base write is complete
		// and fsynced. Removing it first is how the records were lost.
		_ = os.Remove(delta)
		d.dirtySidecars = map[string]struct{}{}
		return nil
	}
	if len(d.dirtySidecars) == 0 {
		return nil
	}
	f, err := os.OpenFile(delta, os.O_APPEND|os.O_CREATE|os.O_WRONLY, localFilePerm)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	docIDs := make([]string, 0, len(d.dirtySidecars))
	for docID := range d.dirtySidecars {
		docIDs = append(docIDs, docID)
	}
	sort.Strings(docIDs)
	for _, docID := range docIDs {
		kinds := side[docID]
		names := make([]string, 0, len(kinds))
		for kind := range kinds {
			names = append(names, kind)
		}
		sort.Strings(names)
		for _, kind := range names {
			if err := enc.Encode(map[string]string{
				"document_id": docID, "kind": kind, "text": kinds[kind], "op": "upsert",
			}); err != nil {
				_ = f.Close()
				return err
			}
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Cleared only after the append is durable: clearing first meant a failed
	// write silently dropped the dirty set and the records with it.
	d.dirtySidecars = map[string]struct{}{}
	if countJSONLLines(delta) >= compactDeltaLines {
		if err := writeBase(); err != nil {
			return err
		}
		_ = os.Remove(delta)
	}
	return nil
}

// localFilePerm is 0600, not the 0644 these files used to carry. chunks.jsonl
// holds the full plaintext of every ingested document and the sidecars hold
// derived text from it; both were readable by every local account.
const localFilePerm os.FileMode = 0o600

// maxJSONLLineBytes matches the readers in this file, which raise the scanner
// bound to 8 MiB. countJSONLLines used the 64 KiB default.
const maxJSONLLineBytes = 8 * 1024 * 1024

func (d *durableStore) writeFullChunksLocked(path string, bag map[string]memChunk) error {
	// This function used to os.Create the live corpus -- truncating it before
	// the replacement existed -- then discard every Encode error and return
	// cf.Close()'s only. On a full disk it reported success having written a
	// truncated corpus, and the caller then removed the delta that still held
	// the missing records. Both copies gone, no error anywhere.
	//
	// durablefile writes to a temp file and renames, so the live corpus is
	// untouched until a complete, fsynced replacement exists.
	//
	// Iteration order is sorted so a rewrite of unchanged content produces
	// identical bytes; Go map order would otherwise make every flush look like
	// a change to anything comparing digests.
	ids := make([]string, 0, len(bag))
	for chunkID := range bag {
		ids = append(ids, chunkID)
	}
	sort.Strings(ids)
	return durablefile.WriteFunc(path, localFilePerm, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, chunkID := range ids {
			ch := bag[chunkID]
			if err := enc.Encode(map[string]string{
				"chunk_id":    chunkID,
				"document_id": ch.dsid,
				"text":        ch.text,
				"source_uri":  ch.sourceURI,
				"op":          "upsert",
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *durableStore) appendDeltaLocked(path string, bag map[string]memChunk, dirty map[string]struct{}) error {
	if len(dirty) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, localFilePerm)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	ids := make([]string, 0, len(dirty))
	for id := range dirty {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ch, ok := bag[id]
		if !ok {
			// Tombstone for deleted ids (future); skip if missing.
			continue
		}
		if err := enc.Encode(map[string]string{
			"chunk_id":    id,
			"document_id": ch.dsid,
			"text":        ch.text,
			"source_uri":  ch.sourceURI,
			"op":          "upsert",
		}); err != nil {
			_ = f.Close()
			return err
		}
	}
	// fsync before returning: the caller clears the dirty set on success, so
	// an unflushed append means those chunks exist nowhere.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// countJSONLLines counts non-empty lines. It used the default 64 KiB scanner
// limit while the readers deliberately raise theirs to 8 MiB, and it discarded
// scanner.Err(), so a single oversized chunk line silently returned a short
// count -- and the delta, never reaching the compaction threshold, grew without
// bound and was replayed in full on every restart.
func countJSONLLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxJSONLLineBytes)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	if sc.Err() != nil {
		// A line past the bound means the true count is at least what was
		// seen. Returning the short count would keep compaction from ever
		// firing; returning the threshold guarantees the next flush compacts,
		// which rewrites the base and drops the unreadable delta.
		return compactDeltaLines
	}
	return n
}

func (d *durableStore) load() error {
	metaPath := filepath.Join(d.dir, localMetaName)
	if raw, err := os.ReadFile(metaPath); err == nil {
		var m localMeta
		if json.Unmarshal(raw, &m) == nil {
			if m.BrainID != "" {
				d.brainID = m.BrainID
			}
			if m.GenerationID != "" {
				d.gen.Store(m.GenerationID)
			}
		}
	}
	// Base chunks + append-only delta (later lines win for same chunk_id).
	chunks, err := loadChunkJSONL(filepath.Join(d.dir, localChunksName))
	if err != nil {
		return err
	}
	delta, err := loadChunkJSONL(filepath.Join(d.dir, localDeltaName))
	if err != nil {
		return err
	}
	// Merge: base then delta (delta overwrites).
	byID := map[string]ChunkWrite{}
	for _, ch := range chunks {
		if ch.ChunkID != "" {
			byID[ch.ChunkID] = ch
		}
	}
	for _, ch := range delta {
		if ch.ChunkID != "" {
			byID[ch.ChunkID] = ch
		}
	}
	merged := make([]ChunkWrite, 0, len(byID))
	for _, ch := range byID {
		merged = append(merged, ch)
	}
	if len(merged) > 0 {
		if err := d.inner.UpsertChunks(context.Background(), d.brainID, merged); err != nil {
			return err
		}
	}
	// Sidecars: base then delta (later wins per doc+kind).
	items, err := loadSidecarJSONL(filepath.Join(d.dir, localSidecarName))
	if err != nil {
		return err
	}
	dlt, err := loadSidecarJSONL(filepath.Join(d.dir, localSidecarDelta))
	if err != nil {
		return err
	}
	// Merge by document_id|kind
	byKey := map[string]SidecarWrite{}
	for _, it := range items {
		byKey[it.DocumentID+"|"+it.Kind] = it
	}
	for _, it := range dlt {
		byKey[it.DocumentID+"|"+it.Kind] = it
	}
	sideMerged := make([]SidecarWrite, 0, len(byKey))
	for _, it := range byKey {
		sideMerged = append(sideMerged, it)
	}
	if len(sideMerged) > 0 {
		_, _ = d.inner.WarmSidecars(context.Background(), d.brainID, sideMerged)
	}
	return nil
}

func loadSidecarJSONL(path string) ([]SidecarWrite, error) {
	sf, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer sf.Close()
	var items []SidecarWrite
	sc2 := bufio.NewScanner(sf)
	sc2.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for sc2.Scan() {
		line := strings.TrimSpace(sc2.Text())
		if line == "" {
			continue
		}
		var row map[string]string
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if row["op"] == "delete" {
			continue
		}
		items = append(items, SidecarWrite{
			DocumentID: row["document_id"],
			Kind:       row["kind"],
			Text:       row["text"],
		})
	}
	return items, sc2.Err()
}

func loadChunkJSONL(path string) ([]ChunkWrite, error) {
	cf, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer cf.Close()
	var chunks []ChunkWrite
	sc := bufio.NewScanner(cf)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row map[string]string
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if row["op"] == "delete" {
			continue // tombstones reserved; not loaded into bag
		}
		chunks = append(chunks, ChunkWrite{
			ChunkID:    row["chunk_id"],
			DocumentID: row["document_id"],
			Text:       row["text"],
			SourceURI:  row["source_uri"],
		})
	}
	return chunks, sc.Err()
}

func (d *durableStore) bumpGeneration() string {
	cur, _ := d.gen.Load().(string)
	if cur == "" {
		cur = "gen-0"
	}
	// gen-N → gen-(N+1)
	n := 0
	if strings.HasPrefix(cur, "gen-") {
		fmt.Sscanf(cur, "gen-%d", &n)
	}
	next := fmt.Sprintf("gen-%d", n+1)
	d.gen.Store(next)
	return next
}

func (d *durableStore) generationID() string {
	g, _ := d.gen.Load().(string)
	if g == "" {
		return "gen-0"
	}
	return g
}

// CreateLocal creates an empty durable product brain under dir.
func CreateLocal(dir, brainID string) (*Client, error) {
	dir = strings.TrimSpace(dir)
	brainID = strings.TrimSpace(brainID)
	if dir == "" {
		return nil, fmt.Errorf("hosted: empty local dir")
	}
	if brainID == "" {
		brainID = "local"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// Refuse to clobber an existing non-empty brain silently.
	if _, err := os.Stat(filepath.Join(dir, localMetaName)); err == nil {
		return nil, fmt.Errorf("hosted: local brain already exists at %s (use OpenLocal)", dir)
	}
	inner := NewMemoryChunkStore()
	d := &durableStore{
		inner: inner, dir: dir, brainID: brainID,
		dirty: map[string]struct{}{}, dirtySidecars: map[string]struct{}{},
		forceFullFlush: true, hotDirty: true,
	}
	d.gen.Store("gen-0")
	if err := d.flush(); err != nil {
		return nil, err
	}
	// Phase 2: default vault-capable single_user security metadata.
	if err := productsec.SaveSecurity(dir, productsec.BrainSecurity{
		Profile: productsec.ProfileSingleUser, Owner: brainID, VaultCapable: true,
	}); err != nil {
		return nil, err
	}
	return newLocalClient(d), nil
}

// OpenLocal opens a durable FS-backed product brain (creates empty if missing).
func OpenLocal(dir, brainID string) (*Client, error) {
	dir = strings.TrimSpace(dir)
	brainID = strings.TrimSpace(brainID)
	if dir == "" {
		return nil, fmt.Errorf("hosted: empty local dir")
	}
	if brainID == "" {
		brainID = "local"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	inner := NewMemoryChunkStore()
	d := &durableStore{
		inner: inner, dir: dir, brainID: brainID,
		dirty: map[string]struct{}{}, dirtySidecars: map[string]struct{}{},
	}
	d.gen.Store("gen-0")
	if err := d.load(); err != nil {
		return nil, err
	}
	// If meta had a different brain id, keep loaded one.
	if d.brainID == "" {
		d.brainID = brainID
	}
	// Ensure meta exists on disk.
	if _, err := os.Stat(filepath.Join(dir, localMetaName)); os.IsNotExist(err) {
		if err := d.flush(); err != nil {
			return nil, err
		}
	}
	return newLocalClient(d), nil
}

func newLocalClient(d *durableStore) *Client {
	c := &Client{
		cfg: Config{
			BrainID:         d.brainID,
			LexicalLimit:    30,
			DenseLimit:      0,
			RRFK:            60,
			PoolK:           40,
			TopK:            8,
			MaxCite:         3,
			MaxPassageChars: 2000,
		},
		store:        d,
		productOwned: true,
		qcache:       newQueryCache(90 * time.Second),
		rerankScores: newRerankScoreCache(0, 0),
		local:        d,
	}
	// Interactive HotLex from local chunks + optional on-disk gob.
	c.hot = d.inner.BuildHotLex(d.brainID)
	if c.hot != nil {
		c.hot.Generation = d.generationID()
	}
	// Prefer prebuilt gob if present (larger projections).
	gobPath := filepath.Join(d.dir, "hotlex.gob")
	if h, err := LoadHotLexSnapshot(gobPath, HotLexSnapshotScope{
		BrainID: d.brainID, Generation: d.generationID(), AllowLegacyGob: true,
	}); err == nil && h != nil && h.Len() > 0 {
		c.hot = h
	}
	// Phase 2 security context from brain dir (single_user default).
	if sec, err := productsec.ContextFromBrain(d.dir, "", ""); err == nil {
		c.Security = sec
	}
	// ADR 0024: bind queue + cortex as substrates (solo defaults under dir).
	// Env may override module choices; always same residual pipeline.
	cfg := SoloSubstrate(d.dir)
	env := SubstrateFromEnv()
	if env.Queue != "" {
		cfg.Queue = env.Queue
	}
	if env.QueuePath != "" {
		cfg.QueuePath = env.QueuePath
	}
	if env.Cortex != "" {
		cfg.Cortex = env.Cortex
	}
	if env.CortexPath != "" {
		cfg.CortexPath = env.CortexPath
	}
	if env.Dense != "" {
		cfg.Dense = env.Dense
	}
	if env.AutoGardener {
		cfg.AutoGardener = true
	}
	if env.Profile != "" {
		cfg.Profile = env.Profile
	}
	_ = ApplySubstrates(c, cfg)
	return c
}

// startAutoGardener launches Daemon.Run in a background goroutine that drains
// enrich jobs and triggers post-wave cortex maintenance via RunGardenerWave.
// The loop is canceled from Client.Close via autoGardenerCancel.
func (c *Client) startAutoGardener() {
	if c == nil || c.gardenerQ == nil {
		return
	}
	if c.autoGardenerCancel != nil {
		c.autoGardenerCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.autoGardenerCancel = cancel
	go func() {
		// Prefer RunGardenerWave loop so cortex maintenance runs post-wave.
		//
		// A panic in this goroutine has no caller to recover it and would take
		// the host process down. Maintenance failing is not a reason to lose
		// the service it maintains.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "hosted: auto-gardener panicked, maintenance stopped: %v\n%s\n",
					r, debug.Stack())
			}
		}()
		for {
			if ctx.Err() != nil {
				return
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(os.Stderr, "hosted: auto-gardener wave panicked: %v\n", r)
					}
				}()
				_, _ = c.RunGardenerWave(ctx)
			}()
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
}

// local holds optional durable FS backend for generation pin + flush.
// (Client field added in retrieve.go Client struct — see local_client.go helpers.)

// DocumentsToChunks maps one document body to a single product chunk.
func DocumentsToChunks(docs []LocalDocument) []ChunkWrite {
	out := make([]ChunkWrite, 0, len(docs))
	for _, d := range docs {
		id := strings.TrimSpace(d.ID)
		if id == "" {
			continue
		}
		text := strings.TrimSpace(d.Text)
		if d.Title != "" {
			text = strings.TrimSpace(d.Title + "\n\n" + text)
		}
		if text == "" {
			continue
		}
		out = append(out, ChunkWrite{
			DocumentID: id,
			ChunkID:    id + "#0",
			Text:       text,
			SourceURI:  d.SourceURI,
		})
	}
	return out
}

// LocalDocument is one product corpus unit for local/JSONL ingest.
type LocalDocument struct {
	ID        string
	Title     string
	Text      string
	SourceURI string
}

// BurstIngestLocal shards document upserts and bumps generation (product burst).
// workers<=0 uses defaultLocalWorkers (CPU-bound local fleet, not hosted compute).
func (c *Client) BurstIngestLocal(ctx context.Context, docs []LocalDocument, workers int) (IngestResult, error) {
	if c == nil {
		return IngestResult{}, fmt.Errorf("hosted: nil client")
	}
	// Redact before anything is derived from the documents.
	//
	// This is above DocumentsToChunks on purpose: a secret split across a
	// chunk boundary is invisible to a per-chunk detector, and the fan-out
	// below hands the *documents* to the cortex and the vector store, not the
	// chunks. Redacting only the chunks would sanitise the corpus and leave
	// the raw text in the other two.
	privacy, err := c.redactDocuments(ctx, docs)
	if err != nil {
		return IngestResult{}, err
	}
	docs = privacy.Admitted
	if len(docs) == 0 {
		return IngestResult{Withheld: privacy.Withheld},
			fmt.Errorf("hosted: every document was withheld by content policy")
	}
	chunks := DocumentsToChunks(docs)
	if len(chunks) == 0 {
		return IngestResult{}, fmt.Errorf("hosted: empty local burst")
	}
	if workers < 1 {
		workers = defaultLocalWorkers()
	}
	res, err := c.BurstUpsert(ctx, c.cfg.BrainID, chunks, workers)
	if err != nil {
		return res, err
	}
	if d := c.local; d != nil {
		res.GenerationID = d.bumpGeneration()
		_ = d.flush()
	}
	res.ProductOwned = true
	res.Mode = "burst"
	res.Withheld = privacy.Withheld
	res.Redacted = privacy.Redacted
	// retrieval_ready first; gardener async by default (enqueue) or sync for tests.
	if er, eerr := c.EnrichAfterIngest(ctx, c.cfg.BrainID, res.GenerationID, docsFromChunks(chunks)); eerr == nil {
		res.EnrichJobs = er.JobsEnqueued
		res.EnrichSidecars = er.SidecarsWarm
	}
	c.seedMemoryAfterIngest(docs, res.GenerationID)
	c.seedDenseAfterIngest(ctx, docs)
	// Prefer incremental HotLex when already loaded and delta is small.
	if c.hot != nil && c.hot.Len() > 0 && len(docs) < 64 {
		c.EnsureHotLexIncremental(docs)
	}
	return res, nil
}

// PruneMissingDocuments deletes stored documents not present in liveDocIDs
// (watch/continual tombstone path). Updates HotLex when bound.
func (c *Client) PruneMissingDocuments(ctx context.Context, liveDocIDs map[string]struct{}) (int, error) {
	_ = ctx
	if c == nil {
		return 0, fmt.Errorf("hosted: nil client")
	}
	if liveDocIDs == nil {
		liveDocIDs = map[string]struct{}{}
	}
	var stored []string
	switch s := c.store.(type) {
	case *durableStore:
		stored = s.DocumentIDs(c.cfg.BrainID)
	case *MemoryChunkStore:
		stored = s.DocumentIDs(c.cfg.BrainID)
	default:
		return 0, nil // neon/path2: prune not supported here
	}
	var missing []string
	for _, id := range stored {
		if _, ok := liveDocIDs[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	n := 0
	switch s := c.store.(type) {
	case *durableStore:
		n = s.DeleteDocuments(c.cfg.BrainID, missing)
	case *MemoryChunkStore:
		n = s.DeleteDocuments(c.cfg.BrainID, missing)
	}
	if n > 0 {
		c.InvalidateQueryCache()
		if c.hot != nil {
			for _, id := range missing {
				c.hot.RemoveDocument(id)
			}
		}
		if d := c.local; d != nil {
			_ = d.bumpGeneration()
		}
	}
	return n, nil
}

// ContinualDeltaLocal upserts only docs whose text changed vs stored chunks.
// Callers that manage a source tree should also call PruneMissingDocuments for
// files deleted from disk (WatchDocs does this automatically).
func (c *Client) ContinualDeltaLocal(ctx context.Context, docs []LocalDocument) (IngestResult, error) {
	if c == nil {
		return IngestResult{}, fmt.Errorf("hosted: nil client")
	}
	// Empty set is a valid "all deleted" signal when prune is applied separately.
	chunks := DocumentsToChunks(docs)
	if len(chunks) == 0 {
		return IngestResult{
			BrainID: c.cfg.BrainID, Ingested: 0, Upserted: 0,
			ProductOwned: true, Mode: "delta",
		}, nil
	}
	// Filter unchanged when durable/memory store exposes chunks.
	var changed []ChunkWrite
	switch s := c.store.(type) {
	case *durableStore:
		s.inner.mu.RLock()
		bag := s.inner.chunks[c.cfg.BrainID]
		for _, ch := range chunks {
			old, ok := bag[ch.ChunkID]
			if ok && old.text == ch.Text {
				continue
			}
			changed = append(changed, ch)
		}
		s.inner.mu.RUnlock()
	case *MemoryChunkStore:
		s.mu.RLock()
		bag := s.chunks[c.cfg.BrainID]
		for _, ch := range chunks {
			old, ok := bag[ch.ChunkID]
			if ok && old.text == ch.Text {
				continue
			}
			changed = append(changed, ch)
		}
		s.mu.RUnlock()
	default:
		changed = chunks
	}
	if len(changed) == 0 {
		gen := ""
		if c.local != nil {
			gen = c.local.generationID()
		}
		return IngestResult{
			BrainID:      c.cfg.BrainID,
			Ingested:     0,
			Upserted:     0,
			ProductOwned: true,
			Mode:         "delta",
			GenerationID: gen,
		}, nil
	}
	res, err := c.BurstUpsert(ctx, c.cfg.BrainID, changed, 1)
	if err != nil {
		return res, err
	}
	if d := c.local; d != nil {
		res.GenerationID = d.bumpGeneration()
		_ = d.flush()
	}
	res.ProductOwned = true
	res.Mode = "delta"
	if er, eerr := c.EnrichAfterIngest(ctx, c.cfg.BrainID, res.GenerationID, docsFromChunks(changed)); eerr == nil {
		res.EnrichJobs = er.JobsEnqueued
		res.EnrichSidecars = er.SidecarsWarm
	}
	// Map changed chunks back to LocalDocument stubs for memory seed.
	var deltaDocs []LocalDocument
	for _, ch := range changed {
		deltaDocs = append(deltaDocs, LocalDocument{ID: ch.DocumentID, Text: ch.Text, SourceURI: ch.SourceURI})
	}
	c.seedMemoryAfterIngest(deltaDocs, res.GenerationID)
	// Dense seed on delta (same as BurstIngestLocal) so watch/continual fills ANN.
	c.seedDenseAfterIngest(ctx, deltaDocs)
	// Incremental HotLex for deltas when index already warm.
	c.EnsureHotLexIncremental(deltaDocs)
	return res, nil
}

// GenerationID returns the local durable generation pin when present.
func (c *Client) GenerationID() string {
	if c == nil || c.local == nil {
		return ""
	}
	return c.local.generationID()
}

// LocalDir returns the durable directory when this client is OpenLocal.
func (c *Client) LocalDir() string {
	if c == nil || c.local == nil {
		return ""
	}
	return c.local.dir
}

// StoreKind reports store adapter for diagnostics.
func (c *Client) StoreKind() string {
	if c == nil {
		return ""
	}
	if c.local != nil {
		return "local_fs"
	}
	if c.productOwned && c.db == nil {
		return "memory"
	}
	if c.productOwned {
		return "product_neon"
	}
	if c.db != nil {
		return "path2"
	}
	return "unknown"
}

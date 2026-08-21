package dense

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// HNSW is a pure-Go hierarchical NSW approximate NN index (FAISS-class local ANN
// without CGo). Suitable for residual dense=faiss in-process when no HTTP sidecar.
// Not bit-identical to FAISS; same Client denseBackend contract.
type HNSW struct {
	mu     sync.RWMutex
	dim    int
	scope  string
	model  string
	digest string
	M      int // max neighbors per node (default 16)
	ef     int // search candidate list size
	ids    []string
	// byID maps a vector id to its slot in ids/vecs/meta/graph.
	//
	// Upsert used a linear scan over ids to find an existing entry, so loading
	// n vectors cost O(n^2) comparisons -- the load path calls Upsert once per
	// vector, and every one of them walked everything inserted before it. The
	// map makes that a lookup. It is derived state and is rebuilt wherever ids
	// is assigned, of which there are exactly two places: here and Clone.
	byID map[string]int
	vecs [][]float32
	meta []HitMetadata
	// graph[i] = neighbor indices of i (at most M)
	graph [][]int
}

const (
	hnswFileMagic      = uint32(0x324e4e41) // "ANN2", little endian
	hnswFileVersion    = uint32(4)
	hnswExactThreshold = 512
)

// NewHNSW returns an empty in-process ANN index.
func NewHNSW(dim, m, ef int) *HNSW {
	return NewScopedHNSW(IndexIdentity{Dimensions: dim}, m, ef)
}

// NewScopedHNSW returns an empty in-process ANN index pinned to identity.
func NewScopedHNSW(identity IndexIdentity, m, ef int) *HNSW {
	if m <= 0 {
		m = 16
	}
	if ef <= 0 {
		ef = 64
	}
	return &HNSW{
		dim: identity.Dimensions, scope: identity.Scope, model: identity.Model,
		digest: identity.ContentDigest,
		M:      m, ef: ef,
	}
}

// Identity returns the immutable projection identity.
func (h *HNSW) Identity() IndexIdentity {
	if h == nil {
		return IndexIdentity{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return IndexIdentity{
		Scope: h.scope, Model: h.model, Dimensions: h.dim, ContentDigest: h.digest,
	}
}

// UpgradeLegacyIdentity binds identity fields that were absent from legacy
// index files. Existing non-empty fields remain immutable and must match the
// supplied identity, so an incompatible index is rejected instead of being
// relabeled.
func (h *HNSW) UpgradeLegacyIdentity(expected IndexIdentity) error {
	if h == nil {
		return fmt.Errorf("dense: nil hnsw")
	}
	if expected.Scope == "" || expected.Model == "" || expected.Dimensions <= 0 {
		return fmt.Errorf("dense: incomplete hnsw identity")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, field := range []struct {
		name     string
		expected string
		actual   string
	}{
		{name: "scope", expected: expected.Scope, actual: h.scope},
		{name: "model", expected: expected.Model, actual: h.model},
		{name: "content_digest", expected: expected.ContentDigest, actual: h.digest},
	} {
		if field.actual != "" && field.expected != "" && field.actual != field.expected {
			return &IdentityError{Field: field.name, Expected: field.expected, Actual: field.actual}
		}
	}
	if h.dim != 0 && h.dim != expected.Dimensions {
		return &IdentityError{
			Field: "dimensions", Expected: fmt.Sprint(expected.Dimensions), Actual: fmt.Sprint(h.dim),
		}
	}
	if h.scope == "" {
		h.scope = expected.Scope
	}
	if h.model == "" {
		h.model = expected.Model
	}
	if h.dim == 0 {
		h.dim = expected.Dimensions
	}
	if h.digest == "" {
		h.digest = expected.ContentDigest
	}
	return nil
}

// Dim returns fixed dimension after first insert (0 if empty).
func (h *HNSW) Dim() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.dim
}

// Len returns vector count.
func (h *HNSW) Len() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.ids)
}

// Clone returns an independent copy suitable for build-then-publish updates.
// The clone is never visible to readers until its durable Save succeeds.
func (h *HNSW) Clone() *HNSW {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := NewScopedHNSW(IndexIdentity{
		Scope: h.scope, Model: h.model, Dimensions: h.dim, ContentDigest: h.digest,
	}, h.M, h.ef)
	out.ids = append([]string(nil), h.ids...)
	out.byID = make(map[string]int, len(h.ids))
	for i, id := range out.ids {
		out.byID[id] = i
	}
	out.vecs = make([][]float32, len(h.vecs))
	for i := range h.vecs {
		out.vecs[i] = append([]float32(nil), h.vecs[i]...)
	}
	out.meta = append([]HitMetadata(nil), h.meta...)
	out.graph = make([][]int, len(h.graph))
	for i := range h.graph {
		out.graph[i] = append([]int(nil), h.graph[i]...)
	}
	return out
}

// EstimatedMemoryBytes reports the index-owned backing storage (excluding Go
// allocator/object overhead) for stable bakeoff receipts.
func (h *HNSW) EstimatedMemoryBytes() int64 {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	var total int64
	for i := range h.ids {
		total += int64(len(h.ids[i])+len(h.meta[i].DocumentID)+len(h.meta[i].ChunkID)+len(h.meta[i].SourceURI)) +
			int64(len(h.vecs[i])*4) + int64(len(h.graph[i])*8)
	}
	return total
}

// Upsert inserts or replaces id with vec (cosine space, L2-normalized).
func (h *HNSW) Upsert(id string, vec []float32) error {
	return h.UpsertWithMetadata(id, vec, HitMetadata{DocumentID: id, ChunkID: id + "#0"})
}

// UpsertWithMetadata inserts or replaces a vector without discarding its
// original document/chunk/source identity.
func (h *HNSW) UpsertWithMetadata(id string, vec []float32, metadata HitMetadata) error {
	if h == nil {
		return fmt.Errorf("dense: nil hnsw")
	}
	if id == "" || len(vec) == 0 {
		return fmt.Errorf("dense: empty id or vector")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dim == 0 {
		h.dim = len(vec)
	}
	if len(vec) != h.dim {
		return fmt.Errorf("dense: dim mismatch want %d got %d", h.dim, len(vec))
	}
	v := normalizeCopy(vec)
	metadata = normalizeHitMetadata(id, metadata)
	// Replace existing id.
	if i, ok := h.byID[id]; ok && i < len(h.ids) && h.ids[i] == id {
		h.vecs[i] = v
		h.meta[i] = metadata
		h.rewireLocked(i)
		return nil
	}
	idx := len(h.ids)
	h.ids = append(h.ids, id)
	if h.byID == nil {
		h.byID = make(map[string]int, len(h.ids))
	}
	h.byID[id] = idx
	h.vecs = append(h.vecs, v)
	h.meta = append(h.meta, metadata)
	h.graph = append(h.graph, nil)
	h.rewireLocked(idx)
	return nil
}

func normalizeHitMetadata(id string, metadata HitMetadata) HitMetadata {
	if metadata.DocumentID == "" {
		metadata.DocumentID = id
	}
	if metadata.ChunkID == "" {
		metadata.ChunkID = id + "#0"
	}
	return metadata
}

func (h *HNSW) hitLocked(index int, score float64) Hit {
	metadata := normalizeHitMetadata(h.ids[index], h.meta[index])
	return Hit{
		VectorID: h.ids[index], DocumentID: metadata.DocumentID,
		ChunkID: metadata.ChunkID, SourceURI: metadata.SourceURI, Score: score,
	}
}

func (h *HNSW) rewireLocked(idx int) {
	if len(h.ids) == 1 {
		h.graph[idx] = nil
		return
	}
	// Brute neighbors among existing (honest small-N; scales via ef sample for large N).
	type nb struct {
		j int
		s float64
	}
	var cands []nb
	n := len(h.ids)
	if n <= 256 {
		for j := 0; j < n; j++ {
			if j == idx {
				continue
			}
			cands = append(cands, nb{j: j, s: cosineUnit(h.vecs[idx], h.vecs[j])})
		}
	} else {
		// Sample + always compare to previous node for connectivity.
		rng := rand.New(rand.NewSource(int64(idx)*997 + int64(n)))
		seen := map[int]struct{}{idx: {}}
		for k := 0; k < h.ef*4 && len(cands) < h.ef*2; k++ {
			j := rng.Intn(n)
			if _, ok := seen[j]; ok {
				continue
			}
			seen[j] = struct{}{}
			cands = append(cands, nb{j: j, s: cosineUnit(h.vecs[idx], h.vecs[j])})
		}
		if idx > 0 {
			if _, ok := seen[idx-1]; !ok {
				cands = append(cands, nb{j: idx - 1, s: cosineUnit(h.vecs[idx], h.vecs[idx-1])})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].s > cands[j].s })
	m := h.M
	if m > len(cands) {
		m = len(cands)
	}
	neigh := make([]int, 0, m)
	for i := 0; i < m; i++ {
		j := cands[i].j
		neigh = append(neigh, j)
		// Symmetric edge.
		h.graph[j] = addNeighbor(h.graph[j], idx, h.M)
	}
	h.graph[idx] = neigh
}

func addNeighbor(list []int, j, m int) []int {
	for _, x := range list {
		if x == j {
			return list
		}
	}
	list = append(list, j)
	if len(list) > m {
		list = list[len(list)-m:]
	}
	return list
}

// Search returns topK ids by cosine similarity. It preserves the legacy
// convenience API; identity-aware serving paths should use SearchScoped.
func (h *HNSW) Search(query []float32, topK int) []Hit {
	hits, _, _ := h.SearchScoped(query, topK, IndexIdentity{})
	return hits
}

// SearchScoped returns deterministic ANN hits and bounded-work diagnostics.
// Corpora no larger than hnswExactThreshold retain exact-search semantics.
func (h *HNSW) SearchScoped(query []float32, topK int, expected IndexIdentity) ([]Hit, SearchDiagnostics, error) {
	return h.SearchScopedMode(query, topK, expected, SearchModeAuto)
}

// SearchScopedMode searches with an explicit exact/ANN selection. Exact is an
// intentional in-memory truth route, not the bounded SQLite missing-index
// fallback. Invalid modes fail closed.
func (h *HNSW) SearchScopedMode(query []float32, topK int, expected IndexIdentity, mode SearchMode) ([]Hit, SearchDiagnostics, error) {
	diag := SearchDiagnostics{Route: "ann", IndexState: "ready"}
	if h == nil {
		diag.IndexState = "missing"
		return nil, diag, nil
	}
	if len(query) == 0 || topK == 0 {
		return nil, diag, nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	diag.CorpusVectors = len(h.ids)
	diag.CandidateLimit = h.ef*(2*h.M+4) + 1
	if mode == "" {
		mode = SearchModeAuto
	}
	if mode != SearchModeAuto && mode != SearchModeExact && mode != SearchModeANN {
		return nil, diag, fmt.Errorf("dense: invalid search mode %q", mode)
	}
	exact := mode == SearchModeExact || (mode == SearchModeAuto && len(h.ids) <= hnswExactThreshold)
	if exact {
		diag.CandidateLimit = len(h.ids)
	}
	if expected.Scope != "" && expected.Scope != h.scope {
		return nil, diag, &IdentityError{Field: "scope", Expected: expected.Scope, Actual: h.scope}
	}
	if expected.Model != "" && expected.Model != h.model {
		return nil, diag, &IdentityError{Field: "model", Expected: expected.Model, Actual: h.model}
	}
	if expected.Dimensions > 0 && expected.Dimensions != h.dim {
		return nil, diag, &IdentityError{
			Field: "dimensions", Expected: fmt.Sprint(expected.Dimensions), Actual: fmt.Sprint(h.dim),
		}
	}
	if expected.ContentDigest != "" && expected.ContentDigest != h.digest {
		return nil, diag, &IdentityError{
			Field: "content_digest", Expected: expected.ContentDigest, Actual: h.digest,
		}
	}
	if len(h.ids) == 0 || len(query) != h.dim {
		if len(query) != h.dim {
			return nil, diag, &IdentityError{
				Field: "dimensions", Expected: fmt.Sprint(h.dim), Actual: fmt.Sprint(len(query)),
			}
		}
		return nil, diag, nil
	}
	if topK < 0 {
		topK = len(h.ids)
	}
	q := normalizeCopy(query)
	type cand struct {
		i int
		s float64
	}
	if exact {
		if mode == SearchModeExact {
			diag.Route = "exact_override"
		} else {
			diag.Route = "exact_small"
		}
		heap := make([]cand, 0, len(h.ids))
		for i := range h.ids {
			heap = append(heap, cand{i, cosineUnit(q, h.vecs[i])})
			diag.DistanceCalculations++
		}
		sort.Slice(heap, func(i, j int) bool {
			if heap[i].s != heap[j].s {
				return heap[i].s > heap[j].s
			}
			return h.ids[heap[i].i] < h.ids[heap[j].i]
		})
		if topK > len(heap) {
			topK = len(heap)
		}
		out := make([]Hit, 0, topK)
		for i := 0; i < topK; i++ {
			out = append(out, h.hitLocked(heap[i].i, heap[i].s))
		}
		return out, diag, nil
	}
	// Entry: best among sample of nodes.
	best := 0
	bestSc := cosineUnit(q, h.vecs[0])
	diag.DistanceCalculations++
	step := 1
	if len(h.ids) > h.ef {
		step = len(h.ids) / h.ef
		if step < 1 {
			step = 1
		}
	}
	for i := 0; i < len(h.ids); i += step {
		sc := cosineUnit(q, h.vecs[i])
		diag.DistanceCalculations++
		if sc > bestSc {
			bestSc = sc
			best = i
		}
	}
	// Greedy expand.
	visited := map[int]struct{}{best: {}}
	frontier := []cand{{best, bestSc}}
	var heap []cand
	for len(frontier) > 0 && len(heap) < h.ef*2 {
		// Pop best frontier
		sort.Slice(frontier, func(a, b int) bool {
			if frontier[a].s != frontier[b].s {
				return frontier[a].s > frontier[b].s
			}
			return h.ids[frontier[a].i] < h.ids[frontier[b].i]
		})
		cur := frontier[0]
		frontier = frontier[1:]
		heap = append(heap, cur)
		for _, nb := range h.graph[cur.i] {
			if _, ok := visited[nb]; ok {
				continue
			}
			visited[nb] = struct{}{}
			sc := cosineUnit(q, h.vecs[nb])
			diag.DistanceCalculations++
			frontier = append(frontier, cand{nb, sc})
		}
	}
	if mode == SearchModeANN {
		diag.Route = "ann_override"
	}
	sort.Slice(heap, func(i, j int) bool {
		if heap[i].s != heap[j].s {
			return heap[i].s > heap[j].s
		}
		return h.ids[heap[i].i] < h.ids[heap[j].i]
	})
	if topK > len(heap) {
		topK = len(heap)
	}
	out := make([]Hit, 0, topK)
	seenID := map[string]struct{}{}
	for i := 0; i < len(heap) && len(out) < topK; i++ {
		id := h.ids[heap[i].i]
		if _, ok := seenID[id]; ok {
			continue
		}
		seenID[id] = struct{}{}
		out = append(out, h.hitLocked(heap[i].i, heap[i].s))
	}
	return out, diag, nil
}

// Save atomically writes a versioned binary dump. Graph edges are rebuildable
// and reconstructed deterministically on load.
func (h *HNSW) Save(path string) error {
	if h == nil {
		return fmt.Errorf("dense: nil hnsw")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dense-ann-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = tmp.Close() }()
	defer func() { _ = os.Remove(tmpName) }()
	f := tmp
	w := func(v uint32) error {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		_, err := f.Write(b[:])
		return err
	}
	if err := w(hnswFileMagic); err != nil {
		return err
	}
	if err := w(hnswFileVersion); err != nil {
		return err
	}
	if err := writeString(f, h.scope, w); err != nil {
		return err
	}
	if err := writeString(f, h.model, w); err != nil {
		_ = f.Close()
		return err
	}
	if err := writeString(f, h.digest, w); err != nil {
		_ = f.Close()
		return err
	}
	if err := w(uint32(h.dim)); err != nil {
		return err
	}
	if err := w(uint32(h.M)); err != nil {
		return err
	}
	if err := w(uint32(h.ef)); err != nil {
		return err
	}
	if err := w(uint32(len(h.ids))); err != nil {
		return err
	}
	for i, id := range h.ids {
		ib := []byte(id)
		if err := w(uint32(len(ib))); err != nil {
			return err
		}
		if _, err := f.Write(ib); err != nil {
			return err
		}
		for _, value := range []string{h.meta[i].DocumentID, h.meta[i].ChunkID, h.meta[i].SourceURI} {
			if err := writeString(f, value, w); err != nil {
				return err
			}
		}
		for _, x := range h.vecs[i] {
			if err := w(math.Float32bits(x)); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// fsync the containing directory so a successful Save means the rename is
	// durable across a crash, not merely visible in the current page cache.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// LoadHNSW loads Save format and rebuilds edges.
func LoadHNSW(path string) (*HNSW, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("dense: short hnsw file")
	}
	off := 0
	rd32 := func() uint32 {
		v := binary.LittleEndian.Uint32(raw[off:])
		off += 4
		return v
	}
	first := rd32()
	identity := IndexIdentity{}
	m, ef := 16, 64
	var n int
	if first == hnswFileMagic {
		if off+4 > len(raw) {
			return nil, fmt.Errorf("dense: corrupt hnsw version")
		}
		version := rd32()
		if version != 2 && version != 3 && version != hnswFileVersion {
			return nil, fmt.Errorf("dense: unsupported hnsw version")
		}
		var readErr error
		identity.Scope, readErr = readString(raw, &off)
		if readErr != nil {
			return nil, readErr
		}
		identity.Model, readErr = readString(raw, &off)
		if readErr != nil {
			return nil, readErr
		}
		if version >= 3 {
			identity.ContentDigest, readErr = readString(raw, &off)
			if readErr != nil {
				return nil, readErr
			}
		}
		if off+16 > len(raw) {
			return nil, fmt.Errorf("dense: corrupt hnsw header")
		}
		identity.Dimensions = int(rd32())
		m, ef, n = int(rd32()), int(rd32()), int(rd32())
	} else {
		// Legacy v1: first two fields were dim and vector count.
		identity.Dimensions = int(first)
		if off+4 > len(raw) {
			return nil, fmt.Errorf("dense: corrupt legacy hnsw")
		}
		n = int(rd32())
	}
	if identity.ContentDigest != "" && (len(identity.ContentDigest) != 64 || strings.ToLower(identity.ContentDigest) != identity.ContentDigest) {
		return nil, fmt.Errorf("dense: invalid content digest")
	}
	if identity.Dimensions <= 0 || n < 0 || m <= 0 || ef <= 0 {
		return nil, fmt.Errorf("dense: invalid hnsw header")
	}
	h := NewScopedHNSW(identity, m, ef)
	for i := 0; i < n; i++ {
		if off+4 > len(raw) {
			return nil, fmt.Errorf("dense: corrupt hnsw")
		}
		l := int(rd32())
		if off+l > len(raw) {
			return nil, fmt.Errorf("dense: corrupt id")
		}
		id := string(raw[off : off+l])
		off += l
		metadata := HitMetadata{DocumentID: id, ChunkID: id + "#0"}
		if first == hnswFileMagic {
			version := binary.LittleEndian.Uint32(raw[4:8])
			if version >= 4 {
				metadata.DocumentID, err = readString(raw, &off)
				if err != nil {
					return nil, err
				}
				metadata.ChunkID, err = readString(raw, &off)
				if err != nil {
					return nil, err
				}
				metadata.SourceURI, err = readString(raw, &off)
				if err != nil {
					return nil, err
				}
			}
		}
		vec := make([]float32, identity.Dimensions)
		for j := 0; j < identity.Dimensions; j++ {
			if off+4 > len(raw) {
				return nil, fmt.Errorf("dense: corrupt vec")
			}
			vec[j] = math.Float32frombits(rd32())
		}
		if err := h.UpsertWithMetadata(id, vec, metadata); err != nil {
			return nil, err
		}
	}
	return h, nil
}

func writeString(w io.Writer, value string, writeUint32 func(uint32) error) error {
	if err := writeUint32(uint32(len(value))); err != nil {
		return err
	}
	_, err := io.WriteString(w, value)
	return err
}

func readString(raw []byte, off *int) (string, error) {
	if *off+4 > len(raw) {
		return "", fmt.Errorf("dense: corrupt hnsw string length")
	}
	length := int(binary.LittleEndian.Uint32(raw[*off:]))
	*off += 4
	if length < 0 || *off+length > len(raw) {
		return "", fmt.Errorf("dense: corrupt hnsw string")
	}
	value := string(raw[*off : *off+length])
	*off += length
	return value, nil
}

func normalizeCopy(v []float32) []float32 {
	out := make([]float32, len(v))
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		copy(out, v)
		return out
	}
	inv := float32(1 / math.Sqrt(s))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func cosineUnit(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}

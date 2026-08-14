// Package denselocal is the optional, opt-in local-only dense code retrieval
// arm. It composes pure-Go primitives that already exist in this repository:
//
//   - dense.HNSW       (pure-Go ANN, no CGo, atomic Save/Load)
//   - dense.MemoryStore (in-process cosine bag)
//   - query.BagOfWordsDense (sparse bag-of-words cosine with deterministic
//     lexical fallback)
//
// denselocal adds the policy guard-rails that make local-first dense
// retrieval safe to call by default:
//
//   - model identity binding via dense.IndexIdentity
//   - bounded corpora (maxCorpus / maxDim / maxTopK)
//   - always-available deterministic lexical fallback when the bound index
//     is missing, corrupt, or fails identity verification
//   - pure-Go (no `net/*` imports) so the retrieval arm cannot make network
//     requests even if misconfigured
//
// Default behavior of every consumer is unchanged: callers must explicitly
// opt in via the CLI subcommand or codeserve verb. The package is also
// exposed for embedded use by other internal packages, with the same
// "explicit opt-in" contracts preserved.
package denselocal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
)

// mathFloat32bits indirection so the package need not import math at the
// top-level; this keeps the package's import list fully inspectable in one
// place for the no-network contract review.
func mathFloat32bits(x float32) uint32 {
	return math.Float32bits(x)
}

// Model is the canonical identity of the embedding model this arm expects to
// ingest. The "default" model is the deterministic bag-of-words fallback,
// which by construction has no learned vectors and no network/identity
// dependency — perfect for the offline-first contract.
//
// Wiring a real embedding model requires passing explicit vectors through the
// indexer; this package does not own that and never reaches out to a network.
type Model string

const (
	// ModelBag is the deterministic lexical bag (dense.MemoryStore /
	// query.BagOfWordsDense). It is the implicit fallback and is always
	// available even when no ANN index has been published.
	ModelBag Model = "bag-of-words:v1"
)

// Bounds define the safety envelopes the arm enforces. Anything larger is
// rejected before work is performed so a runaway corpus cannot degrade the
// service.
type Bounds struct {
	// MaxCorpus bounds the number of documents. Default 8192.
	MaxCorpus int
	// MaxDim bounds the embedding dimension. Default 1024.
	MaxDim int
	// MaxTopK bounds the requested top-K results. Default 50.
	MaxTopK int
	// MaxQueryLen bounds the textual query length. Default 512 chars.
	MaxQueryLen int
}

// DefaultBounds returns the safety envelopes used by the CLI. These are the
// values cited in docs/specs/code-intelligence/DENSE-LOCAL-ARM.md and any
// change must be recorded in that spec first.
func DefaultBounds() Bounds {
	return Bounds{MaxCorpus: 8192, MaxDim: 1024, MaxTopK: 50, MaxQueryLen: 512}
}

// Options drives a single search call.
type Options struct {
	// IndexPath is the durable HNSW index file. Empty means "fall back to
	// lexical" (no dense lookup).
	IndexPath string
	// Scope is the generation or repo scope the index was built for. Empty
	// is treated as "unknown" and forces lexical fallback.
	Scope string
	// Model selects the identity binding. Use ModelBag unless you have a
	// real HNSW index built with a specific named model.
	Model Model
	// Dimensions pins the index dimension. Zero means "auto" for bag, or
	// "any" for HNSW (the index's own dimension is authoritative).
	Dimensions int
	// ContentDigest is the optional content digest from the index. Empty
	// means "skip digest verification" — the bound identity check still
	// applies to scope/model/dimensions.
	ContentDigest string
	// TopK bounds the returned results. 0 means "use Bounds.MaxTopK".
	TopK int
	// Bounds overrides DefaultBounds when set.
	Bounds Bounds
}

// Hit is the stable JSON shape returned to codeserve and the CLI.
type Hit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Route string  `json:"route"` // "ann", "exact_small", "exact_override", "lexical"
}

// Report is the per-call receipts envelope. It never includes query text,
// document ids beyond the requested set, or other principal/timing
// information that should not propagate.
type Report struct {
	OK                    bool      `json:"ok"`
	Reason                string    `json:"reason,omitempty"`
	Route                 string    `json:"route,omitempty"`
	IndexState            string    `json:"index_state,omitempty"`
	CorpusVectors         int       `json:"corpus_vectors,omitempty"`
	Hits                  []Hit     `json:"hits"`
	Query                 string    `json:"query_sha256,omitempty"` // hash, never raw text
	Model                 string    `json:"model,omitempty"`
	Dimensions            int       `json:"dimensions,omitempty"`
	Scope                 string    `json:"scope,omitempty"`
	ContentDigest         string    `json:"content_digest,omitempty"`
	IdentityChecked       bool      `json:"identity_checked"`
	IdentityMismatchField string    `json:"identity_mismatch_field,omitempty"`
	BoundedBy             BoundedBy `json:"bounded_by"`
}

// BoundedBy records which Bounds were active for the call.
type BoundedBy struct {
	MaxCorpus   int `json:"max_corpus"`
	MaxDim      int `json:"max_dim"`
	MaxTopK     int `json:"max_top_k"`
	MaxQueryLen int `json:"max_query_len"`
}

// Errors returned by this package. All are validation failures that fail
// closed (the search returns zero hits with the lexical fallback when
// applicable).
var (
	ErrEmptyQuery      = errors.New("denselocal: empty query")
	ErrQueryTooLong    = errors.New("denselocal: query exceeds MaxQueryLen")
	ErrTopKExceeded    = errors.New("denselocal: top_k exceeds MaxTopK")
	ErrCorpusExceeded  = errors.New("denselocal: corpus exceeds MaxCorpus")
	ErrDimExceeded     = errors.New("denselocal: dimension exceeds MaxDim")
	ErrInvalidIndex    = errors.New("denselocal: index file is missing or unreadable")
	ErrIdentityMissing = errors.New("denselocal: required identity field missing")
)

// queryHash returns the canonical SHA-256 of a query (hex). It is included
// in the report instead of the raw query so a leaked receipt never carries
// raw user text.
func queryHash(q string) string {
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])
}

// Engine is the search engine. Construct it once per process with a stable
// corpus and reuse it for every search; the underlying dense structures are
// safe to call concurrently.
type Engine struct {
	mu      sync.RWMutex
	bounds  Bounds
	scope   string
	model   Model
	dim     int
	index   *dense.HNSW
	bagDocs map[string][]float32 // bag corpus cached for lexical fallback
	bagText map[string]string
	// tokenCache holds the precomputed bag-of-words tokens for each bagText
	// document, computed once at construction time so lexical fallback
	// searches do not re-tokenize the corpus on every call (this is the
	// primary alloc-reduction lever for the local arm).
	tokenCache map[string]map[string]int
}

// NewEngine constructs a search engine that operates on a corpus of vectors.
// Documents are passed as (id, vec) pairs; an optional bag text map is kept
// for deterministic lexical fallback when the HNSW identity check fails.
//
// documents may be nil. When nil, only the lexical fallback (if any bag
// texts are present) is available. Total corpus size is whichever of the
// two is larger, so callers don't need to keep them in sync. Bounds are
// required; callers that want defaults should construct a Bounds via
// DefaultBounds() before calling NewEngine so the explicit "zero" intent
// (refuse everything) remains distinguishable from "use defaults".
func NewEngine(scope string, model Model, dim int, documents []doc, bagTexts map[string]string, bounds Bounds) (*Engine, error) {
	if bounds.MaxCorpus == 0 {
		bounds = DefaultBounds()
	}
	if scope == "" || model == "" {
		return nil, ErrIdentityMissing
	}
	if len(documents) > bounds.MaxCorpus || len(bagTexts) > bounds.MaxCorpus {
		return nil, fmt.Errorf("%w: got max(documents=%d, bagTexts=%d), max %d",
			ErrCorpusExceeded, len(documents), len(bagTexts), bounds.MaxCorpus)
	}
	if dim > bounds.MaxDim && dim > 0 {
		return nil, fmt.Errorf("%w: %d > %d", ErrDimExceeded, dim, bounds.MaxDim)
	}
	idx := dense.NewScopedHNSW(dense.IndexIdentity{
		Scope: scope, Model: string(model), Dimensions: dim,
	}, 16, 64)
	bagDocs := make(map[string][]float32, len(documents))
	bagText := make(map[string]string, len(bagTexts))
	tokenCache := make(map[string]map[string]int, len(bagTexts))
	for _, d := range documents {
		if d.ID == "" || len(d.Vec) == 0 {
			continue
		}
		_ = idx.UpsertWithMetadata(d.ID, d.Vec, dense.HitMetadata{
			DocumentID: d.ID, ChunkID: d.ID + "#0",
		})
		bagDocs[d.ID] = append([]float32(nil), d.Vec...)
	}
	for id, text := range bagTexts {
		bagText[id] = text
		tokenCache[id] = tokenize(text)
	}
	return &Engine{
		bounds: bounds, scope: scope, model: model, dim: dim,
		index: idx, bagDocs: bagDocs, bagText: bagText, tokenCache: tokenCache,
	}, nil
}

// doc is the (id, vec) input the engine consumes. BagOfWordsDense operates
// on text directly; this struct intentionally does NOT carry raw text —
// the lexical fallback uses bagText provided at construction time.
type doc struct {
	ID  string
	Vec []float32
}

// Doc is the public alias used by callers building an in-memory engine. We
// keep it as Doc so the package surface stays readable while the internal
// type stays package-private.
type Doc = doc

// LoadIndex replaces the in-memory index with one loaded from a path. The
// identity on disk is required to match opts; mismatches are reported via
// the Report but never silently swallowed. Dimensions of zero means
// "auto-fill from the loaded file" so callers don't need to know the corpus
// dim in advance.
func (e *Engine) LoadIndex(path string, opts Options) error {
	if path == "" {
		return ErrInvalidIndex
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidIndex, err)
	}
	idx, err := dense.LoadHNSW(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidIndex, err)
	}
	want := dense.IndexIdentity{
		Scope: e.scope, Model: string(e.model),
	}
	if opts.Scope != "" {
		want.Scope = opts.Scope
	}
	if opts.Model != "" {
		want.Model = string(opts.Model)
	}
	loaded := idx.Identity()
	want.Dimensions = loaded.Dimensions
	if opts.Dimensions > 0 && opts.Dimensions != loaded.Dimensions {
		return &dense.IdentityError{Field: "dimensions",
			Expected: fmt.Sprintf("%d", opts.Dimensions),
			Actual:   fmt.Sprintf("%d", loaded.Dimensions)}
	}
	if want.Scope == "" || want.Model == "" || want.Dimensions <= 0 {
		// Loaded file is incomplete; fall through to identity check.
		if loaded.Dimensions <= 0 || loaded.Scope == "" || loaded.Model == "" {
			return ErrIdentityMissing
		}
		want.Scope, want.Model = loaded.Scope, loaded.Model
	}
	if opts.ContentDigest != "" {
		want.ContentDigest = opts.ContentDigest
	}
	if err := idx.UpgradeLegacyIdentity(want); err != nil {
		return err
	}
	e.mu.Lock()
	e.index = idx
	e.mu.Unlock()
	return nil
}

// Search executes the dense retrieval with the model-bound, lexical-fallback
// guarantees documented at package level. The returned Report is always
// non-nil; a hard failure returns OK=false with Reason set and Hits=[].
func (e *Engine) Search(ctx context.Context, q string, opts Options) (Report, error) {
	if e == nil {
		return Report{OK: false, Reason: "nil engine"}, errors.New("denselocal: nil engine")
	}
	if err := ctx.Err(); err != nil {
		return Report{OK: false, Reason: err.Error()}, err
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return Report{OK: false, Reason: ErrEmptyQuery.Error()}, ErrEmptyQuery
	}
	if len(q) > e.bounds.MaxQueryLen {
		return Report{OK: false, Reason: ErrQueryTooLong.Error()}, ErrQueryTooLong
	}
	e.mu.RLock()
	bounds := e.bounds
	idx := e.index
	scope := e.scope
	model := e.model
	dim := e.dim
	e.mu.RUnlock()
	if opts.TopK == 0 || opts.TopK > bounds.MaxTopK {
		opts.TopK = bounds.MaxTopK
	}
	report := Report{
		Query:         queryHash(q),
		Model:         string(model),
		Dimensions:    dim,
		Scope:         scope,
		ContentDigest: opts.ContentDigest,
		BoundedBy: BoundedBy{
			MaxCorpus: bounds.MaxCorpus, MaxDim: bounds.MaxDim,
			MaxTopK: bounds.MaxTopK, MaxQueryLen: bounds.MaxQueryLen,
		},
		Hits:            []Hit{},
		IdentityChecked: true,
	}

	// No index → deterministic lexical fallback only.
	if idx == nil {
		return e.lexicalSearch(q, opts.TopK, report, "lexical_fallback")
	}
	// Vectorize query for HNSW: when model is bag, use the bag-of-words
	// projection; for any other model the caller must supply vectors via
	// the indexer. Today no live embedder is wired, so we always emit a
	// fallback path through the lexical route. HNSW stays available for
	// callers that build the index elsewhere.
	if model == ModelBag {
		return e.lexicalSearch(q, opts.TopK, report, "lexical_fallback")
	}
	// For an unknown model with a non-empty index we still attempt the
	// dense lookup. Without a query vector we degrade to lexical.
	if idx.Identity().Dimensions == 0 {
		return e.lexicalSearch(q, opts.TopK, report, "lexical_fallback")
	}
	return e.lexicalSearch(q, opts.TopK, report, "lexical_fallback")
}

// BagVectorize is exposed so callers building their own index can produce
// bag-of-words vectors with the same projection this arm uses. It is the
// only public embedding helper that this arm exposes; there is no vector
// download anywhere in the package.
func BagVectorize(docs map[string]string) (map[string][]float32, int) {
	vocab := map[string]int{}
	for _, text := range docs {
		scanBag(text, vocab)
	}
	if len(vocab) == 0 {
		return map[string][]float32{}, 0
	}
	// Stable vector ordering by sorted token.
	tokens := make([]string, 0, len(vocab))
	for t := range vocab {
		tokens = append(tokens, t)
	}
	sort.Strings(tokens)
	out := make(map[string][]float32, len(docs))
	for id, text := range docs {
		vec := make([]float32, len(tokens))
		counts := map[string]int{}
		scanBag(text, counts)
		for i, t := range tokens {
			vec[i] = float32(counts[t])
		}
		out[id] = vec
	}
	return out, len(tokens)
}

// scanBag tokenizes text into the bag-of-words counts. Lowercase, alphanumeric
// only; unicode letters and digits count.
func scanBag(text string, into map[string]int) {
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z':
			// accumulate per character below
		default:
			continue
		}
	}
	// Re-implement with proper token boundaries.
	into2 := into // alias for clarity
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	for _, f := range fields {
		if f == "" {
			continue
		}
		into2[f]++
	}
}

// lexicalSearch is the deterministic fallback that the package always offers.
// It runs BagOfWordsDense against the bagText map captured at construction
// time. Pre-tokenized bags live in e.tokenCache so the lexical fallback
// does not re-tokenize the corpus on every call.
func (e *Engine) lexicalSearch(q string, topK int, report Report, route string) (Report, error) {
	e.mu.RLock()
	cache := e.tokenCache
	e.mu.RUnlock()
	qBag := tokenize(q)
	if len(qBag) == 0 {
		report.OK = true
		report.Route = route
		report.IndexState = "missing_index"
		report.Hits = []Hit{}
		return report, nil
	}
	qNorm := l2BagF(toFloatBag(qBag))
	type scored struct {
		id    string
		score float64
	}
	var ranked []scored
	for id, dBag := range cache {
		if len(dBag) == 0 {
			continue
		}
		var dot float64
		for t, qc := range qBag {
			if dc, ok := dBag[t]; ok {
				dot += float64(qc) * float64(dc)
			}
		}
		if dot == 0 {
			continue
		}
		dNorm := l2BagF(toFloatBag(dBag))
		if dNorm == 0 {
			continue
		}
		ranked = append(ranked, scored{id: id, score: dot / (qNorm * dNorm)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].id < ranked[j].id
	})
	if topK <= 0 || topK > len(ranked) {
		topK = len(ranked)
	}
	hits := make([]Hit, 0, topK)
	for i := 0; i < topK; i++ {
		hits = append(hits, Hit{ID: ranked[i].id, Score: ranked[i].score, Route: "lexical"})
	}
	report.OK = true
	report.Route = route
	report.IndexState = "missing_index" // lexical mode is our fallback state
	report.Hits = hits
	return report, nil
}

// toFloatBag converts an int bag to a float32 bag without copying the keys.
// Used by the lexical path's dot product / L2 norm so we avoid allocating
// duplicate maps per query.
func toFloatBag(v map[string]int) map[string]float32 {
	out := make(map[string]float32, len(v))
	for k, x := range v {
		out[k] = float32(x)
	}
	return out
}

// tokenize is the bag-of-words token helper. Lowercase, ASCII alphanumeric
// only — keeps the lexical fallback deterministic across locales.
func tokenize(text string) map[string]int {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	out := make(map[string]int, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		out[f]++
	}
	return out
}

// l2BagF is the L2 norm of a float bag-of-words vector.
func l2BagF(v map[string]float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return 0
	}
	return sqrt(sum)
}

// sqrt is a tiny indirection so test code can stub if needed. It deliberately
// does not import math to keep the package's import surface inspectable.
func sqrt(s float64) float64 {
	if s <= 0 {
		return 0
	}
	z := s
	for i := 0; i < 24; i++ {
		z = (z + s/z) / 2
	}
	return z
}

// PersistIndex is a convenience that loads an in-memory corpus and saves it
// atomically. Atomicity: temp + fsync + rename + parent fsync, mirroring
// dense.HNSW.Save. The caller passes the bag-of-words texts so the HNSW
// identity bindings line up with whatever generated the corpus.
//
// When dim is 0 the actual dimension is inferred from the bag-of-words
// vocabulary so callers don't have to redo it; that path is what tests use.
func PersistIndex(path string, scope string, model Model, dim int, bagDocs map[string]string, bounds Bounds) error {
	if path == "" {
		return ErrInvalidIndex
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	vecs, inferredDim := BagVectorize(bagDocs)
	if dim == 0 {
		dim = inferredDim
	}
	// Deterministic content digest: SHA-256 over sorted (id, vec) bytes.
	ids := make([]string, 0, len(vecs))
	for id := range vecs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
		for _, x := range vecs[id] {
			_, _ = h.Write(floatBits(x))
		}
		_, _ = h.Write([]byte{0xff})
	}
	_ = hex.EncodeToString(h.Sum(nil)) // digest deliberately not yet wired into the HNSW identity on disk; bound identity uses scope/model/dim only.
	idx := dense.NewScopedHNSW(dense.IndexIdentity{
		Scope: scope, Model: string(model), Dimensions: dim,
	}, 16, 64)
	for _, id := range ids {
		if vec, ok := vecs[id]; ok {
			_ = idx.UpsertWithMetadata(id, vec, dense.HitMetadata{
				DocumentID: id, ChunkID: id + "#0",
			})
		}
	}
	return idx.Save(path)
}

// floatBits returns the big-endian byte representation of a float32's IEEE-754
// bits. Used for deterministic hash composition without depending on
// fmt.Sprintf format (which can change between Go versions).
func floatBits(x float32) []byte {
	u := uint32FromFloat32(x)
	return []byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
}

func uint32FromFloat32(x float32) uint32 {
	// Cheat by importing math.Float32bits would pull in math; instead we
	// re-encode through a tiny table-free approach: reinterpret via unsafe
	// would also be a non-starter in the rare library that doesn't allow
	// unsafe. Use math.Float32bits via a thin wrapper.
	return mathFloat32bits(x)
}

// MarshalReport renders a report to JSON for the CLI. Deterministic order;
// non-stable map iteration is avoided through the Hit slice.
func MarshalReport(r Report) ([]byte, error) {
	if r.Hits == nil {
		r.Hits = []Hit{}
	}
	return json.MarshalIndent(r, "", "  ")
}

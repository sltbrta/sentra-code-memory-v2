// Package denselocal is the optional, opt-in local-only code retrieval arm.
// It serves deterministic bag-of-words cosine search over a corpus captured
// at engine construction. The arm is deliberately lexical-only:
//
//   - No ANN/HNSW serving is implemented. A query never becomes a vector
//     here and no index file is loaded or served. Identity fields (scope,
//     model) are bound at construction and carried in every receipt so a
//     future vector arm cannot silently share this arm's receipts.
//   - Bounded corpora, query length, and top-K (see Bounds) are enforced
//     before work is performed so a runaway corpus cannot degrade the
//     service.
//   - Pure-Go (no `net/*` imports) so the retrieval arm cannot make network
//     requests even if misconfigured.
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
	"sort"
	"strings"
)

// Model is the canonical identity label of the retrieval model this arm
// serves. The only model today is the deterministic bag-of-words lexical
// projection, which by construction has no learned vectors and no
// network/identity dependency — perfect for the offline-first contract.
type Model string

const (
	// ModelBag is the deterministic lexical bag-of-words cosine model. It is
	// the only model this arm serves.
	ModelBag Model = "bag-of-words:v1"
)

// Bounds define the safety envelopes the arm enforces. Anything larger is
// rejected (corpus, query length) or clamped (top-K) before work is
// performed so a runaway request cannot degrade the service.
type Bounds struct {
	// MaxCorpus bounds the number of documents. Default 8192.
	MaxCorpus int
	// MaxDim bounds the embedding dimension. Default 1024. The lexical arm
	// holds no dense vectors; the ceiling is declared now and reported in
	// every receipt so a future vector arm inherits a hard limit.
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
	// Scope is the generation or repo scope label carried in the receipt.
	Scope string
	// Model is the identity label carried in the receipt. Use ModelBag.
	Model Model
	// TopK bounds the returned results. Values above Bounds.MaxTopK are
	// clamped to the ceiling; 0 means "use Bounds.MaxTopK".
	TopK int
	// Bounds is informational on search calls; the engine's construction-time
	// Bounds are authoritative.
	Bounds Bounds
}

// Hit is the stable JSON shape returned to codeserve and the CLI.
type Hit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	Route string  `json:"route"` // always "lexical"
}

// Report is the per-call receipts envelope. It never includes query text,
// document ids beyond the requested set, or other principal/timing
// information that should not propagate.
type Report struct {
	OK              bool      `json:"ok"`
	Reason          string    `json:"reason,omitempty"`
	Route           string    `json:"route,omitempty"` // always "lexical"
	Hits            []Hit     `json:"hits"`
	Query           string    `json:"query_sha256,omitempty"` // hash, never raw text
	Model           string    `json:"model,omitempty"`
	Scope           string    `json:"scope,omitempty"`
	IdentityChecked bool      `json:"identity_checked"`
	BoundedBy       BoundedBy `json:"bounded_by"`
}

// BoundedBy records which Bounds were active for the call.
type BoundedBy struct {
	MaxCorpus   int `json:"max_corpus"`
	MaxDim      int `json:"max_dim"`
	MaxTopK     int `json:"max_top_k"`
	MaxQueryLen int `json:"max_query_len"`
}

// Errors returned by this package. All are validation failures that fail
// closed.
var (
	ErrEmptyQuery       = errors.New("denselocal: empty query")
	ErrQueryTooLong     = errors.New("denselocal: query exceeds MaxQueryLen")
	ErrCorpusExceeded   = errors.New("denselocal: corpus exceeds MaxCorpus")
	ErrIdentityMissing  = errors.New("denselocal: required identity field missing")
	ErrModelUnsupported = errors.New("denselocal: unsupported model")
)

// queryHash returns the canonical SHA-256 of a query (hex). It is included
// in the report instead of the raw query so a leaked receipt never carries
// raw user text.
func queryHash(q string) string {
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])
}

// Engine is the search engine. Construct it once per process with a stable
// corpus and reuse it for every search; the engine is immutable after
// construction and safe to call concurrently.
type Engine struct {
	bounds Bounds
	scope  string
	model  Model
	// tokenCache holds the precomputed bag-of-words tokens for each corpus
	// document, computed once at construction time so searches do not
	// re-tokenize the corpus on every call (this is the primary
	// alloc-reduction lever for the local arm).
	tokenCache map[string]map[string]int
}

// NewEngine constructs a lexical search engine over bagTexts. The corpus is
// tokenized once here so searches never re-tokenize. Bounds are required;
// callers that want defaults should construct a Bounds via DefaultBounds()
// before calling NewEngine so the explicit "zero" intent (use defaults)
// remains distinguishable from a partially specified envelope.
func NewEngine(scope string, model Model, bagTexts map[string]string, bounds Bounds) (*Engine, error) {
	if bounds.MaxCorpus == 0 {
		bounds = DefaultBounds()
	}
	if scope == "" || model == "" {
		return nil, ErrIdentityMissing
	}
	if model != ModelBag {
		return nil, fmt.Errorf("%w: %s", ErrModelUnsupported, model)
	}
	if len(bagTexts) > bounds.MaxCorpus {
		return nil, fmt.Errorf("%w: got %d documents, max %d",
			ErrCorpusExceeded, len(bagTexts), bounds.MaxCorpus)
	}
	tokenCache := make(map[string]map[string]int, len(bagTexts))
	for id, text := range bagTexts {
		tokenCache[id] = tokenize(text)
	}
	return &Engine{
		bounds: bounds, scope: scope, model: model, tokenCache: tokenCache,
	}, nil
}

// Search executes the lexical retrieval with the bounded, deterministic
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
	bounds := e.bounds
	topK := opts.TopK
	if topK <= 0 || topK > bounds.MaxTopK {
		topK = bounds.MaxTopK
	}
	report := Report{
		Query: queryHash(q),
		Model: string(e.model),
		Scope: e.scope,
		BoundedBy: BoundedBy{
			MaxCorpus: bounds.MaxCorpus, MaxDim: bounds.MaxDim,
			MaxTopK: bounds.MaxTopK, MaxQueryLen: bounds.MaxQueryLen,
		},
		Hits:            []Hit{},
		IdentityChecked: true,
	}
	return e.lexicalSearch(ctx, q, topK, report)
}

// lexicalSearch is the deterministic bag-of-words cosine ranking over the
// tokenCache captured at construction time. Tie-breaking is by document id
// ascending so two engines with the same corpus produce byte-equivalent hit
// lists.
func (e *Engine) lexicalSearch(ctx context.Context, q string, topK int, report Report) (Report, error) {
	qBag := tokenize(q)
	report.OK = true
	report.Route = "lexical"
	if len(qBag) == 0 {
		return report, nil
	}
	qNorm := l2Bag(toFloatBag(qBag))
	type scored struct {
		id    string
		score float64
	}
	var ranked []scored
	for id, dBag := range e.tokenCache {
		if err := ctx.Err(); err != nil {
			return Report{OK: false, Reason: err.Error()}, err
		}
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
		dNorm := l2Bag(toFloatBag(dBag))
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
	if topK > len(ranked) {
		topK = len(ranked)
	}
	hits := make([]Hit, 0, topK)
	for i := 0; i < topK; i++ {
		hits = append(hits, Hit{ID: ranked[i].id, Score: ranked[i].score, Route: "lexical"})
	}
	report.Hits = hits
	return report, nil
}

// toFloatBag converts an int bag to a float64 bag. Used by the dot product /
// L2 norm so we avoid allocating duplicate maps per query beyond this one.
func toFloatBag(v map[string]int) map[string]float64 {
	out := make(map[string]float64, len(v))
	for k, x := range v {
		out[k] = float64(x)
	}
	return out
}

// tokenize is the bag-of-words token helper. Lowercase, ASCII alphanumeric
// only — keeps the ranking deterministic across locales.
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

// l2Bag is the L2 norm of a bag-of-words vector.
func l2Bag(v map[string]float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	return math.Sqrt(sum)
}

// MarshalReport renders a report to JSON for the CLI. Deterministic order;
// non-stable map iteration is avoided through the Hit slice.
func MarshalReport(r Report) ([]byte, error) {
	if r.Hits == nil {
		r.Hits = []Hit{}
	}
	return json.MarshalIndent(r, "", "  ")
}

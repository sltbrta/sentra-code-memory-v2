package dense

import "fmt"

// Embedding is one document vector pinned by documentID.
type Embedding struct {
	DocumentID string
	Dim        int
	Vector     []float32
}

// Hit is one nearest-neighbor result from Search.
type Hit struct {
	// VectorID is the index key. DocumentID is the evidence DSID; for legacy
	// callers both are the inserted id.
	VectorID   string
	DocumentID string
	ChunkID    string
	SourceURI  string
	Score      float64 // cosine similarity in [-1, 1]; higher is nearer
}

// HitMetadata is rebuildable evidence identity stored with a vector. It keeps
// ANN serving from inventing synthetic chunks or dropping source provenance.
type HitMetadata struct {
	DocumentID string
	ChunkID    string
	SourceURI  string
}

// IndexIdentity pins an ANN projection to the authority scope and embedding
// model that produced it. Dimensions are included separately so aliases that
// resolve to different model revisions cannot silently share an index.
type IndexIdentity struct {
	Scope      string
	Model      string
	Dimensions int
	// ContentDigest pins the index to the canonical vector projection content.
	// Scope/model/dim and row count alone cannot detect same-count replacement.
	ContentDigest string
}

// SearchMode is an explicit serving choice. Auto retains the production
// small-corpus exact threshold, Exact is useful for truth/bakeoff baselines,
// and ANN forces the bounded approximate route even for a small corpus.
type SearchMode string

const (
	SearchModeAuto  SearchMode = "auto"
	SearchModeExact SearchMode = "exact"
	SearchModeANN   SearchMode = "ann"
)

// SearchDiagnostics contains bounded, receipt-safe ANN measurements. It never
// contains query text, document ids, ACL principals, filters, or benchmark gold.
type SearchDiagnostics struct {
	Route                string
	IndexState           string
	CorpusVectors        int
	DistanceCalculations int
	CandidateLimit       int
	ExactFallbackLimit   int
}

// IdentityError reports why an ANN projection cannot serve a query. Expected
// and Actual contain only non-secret projection identity.
type IdentityError struct {
	Field    string
	Expected string
	Actual   string
}

func (e *IdentityError) Error() string {
	if e == nil {
		return "dense: index identity mismatch"
	}
	return fmt.Sprintf("dense: index %s mismatch: got %q want %q", e.Field, e.Actual, e.Expected)
}

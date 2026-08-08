package query

import (
	"context"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

// Corpus serves generation-pinned, principal-independent corpus views. It is
// the engine's only read path: canonical revision facts are always available
// for a published generation, while the rebuildable occurrence projection and
// hydrated bytes arrive together as one ProjectionView. Implementations must
// return ErrUnknownScope for any unknown, revoked, or unpublished source or
// generation so the engine can fail without disclosing existence.
type Corpus interface {
	// Snapshot returns the canonical facts and projection view for one pinned
	// generation of one source. A non-ready ProjectionView.State carries no
	// index or hydration, which is a coverage fact, never deletion evidence.
	Snapshot(ctx context.Context, sourceID, generationID string) (Snapshot, error)
	// CurrentGeneration resolves the source's current complete generation pin.
	CurrentGeneration(ctx context.Context, sourceID string) (GenerationPin, error)
}

// GenerationPin names one immutable complete generation of one source.
type GenerationPin struct {
	SourceID     string
	GenerationID string
}

// Snapshot is the engine's pinned view of one complete generation: canonical
// revision facts from the manifest plus the rebuildable projection view.
type Snapshot struct {
	GenerationID string
	Sequence     uint64
	CommitOID    string
	TreeOID      string
	State        GenerationState
	Readiness    []LaneReadiness
	Revisions    []ingestion.FileRevision
	Projection   ProjectionView
}

// ProjectionView is the rebuildable lexical projection for one pinned
// generation. Index and Files are populated exactly when State is
// ProjectionReady; Files carries the hydrated canonical bytes keyed by
// repository-relative path, mirroring the Stage 03 published source.
type ProjectionView struct {
	State ProjectionState
	Index *codeindex.Snapshot
	Files map[string]ingestion.HydratedFile
}

// Action names one authorization checkpoint in the query funnel.
type Action string

const (
	// ActionQuery authorizes the principal's relationship to the source before
	// any retrieval; denial skips the projection entirely.
	ActionQuery Action = "query"
	// ActionHydrate reauthorizes after candidate selection, before canonical
	// bytes are hydrated; denial discards every candidate.
	ActionHydrate Action = "hydrate"
	// ActionEmit reauthorizes immediately before results are emitted, catching
	// revocation that lands during retrieval and synthesis; denial discards
	// every claim and collapses to absent_support for Ask, or to
	// ErrUnknownScope for Status.
	ActionEmit Action = "emit"
)

// Decision records one current, non-sensitive authorization outcome and the
// epoch it was evaluated at.
type Decision struct {
	Allowed bool
	Epoch   uint64
}

// Authorizer evaluates the principal's current relationships immediately
// before each funnel effect. Implementations must evaluate current policy and
// stale epochs; any error is treated as denial so authorization failure can
// never widen a query's evidence.
type Authorizer interface {
	Authorize(ctx context.Context, principal Principal, action Action, sourceID string) (Decision, error)
}

// EvidenceAdmissionRequest names the generation and ACL epoch about to cross
// the final answer emission boundary. At is the boundary observation time.
type EvidenceAdmissionRequest struct {
	SourceID     string
	GenerationID string
	ACLEpoch     uint64
	At           time.Time
}

// EvidenceAdmitter applies projection-receipt admission at the final answer
// boundary. A false result fails closed and emits no claims, citations, or
// prose. Implementations must use one linearizable view of current canonical
// events and receipt state; request fields alone are not authority. The engine
// invokes the port again with a fresh observation time after a successful
// admission and canonical-generation recheck, so implementations must not
// cache an earlier decision across calls.
type EvidenceAdmitter interface {
	AdmitEvidence(ctx context.Context, request EvidenceAdmissionRequest) bool
}

// EvidenceAdmitterFunc adapts a function to EvidenceAdmitter.
type EvidenceAdmitterFunc func(ctx context.Context, request EvidenceAdmissionRequest) bool

// AdmitEvidence implements EvidenceAdmitter.
func (f EvidenceAdmitterFunc) AdmitEvidence(ctx context.Context, request EvidenceAdmissionRequest) bool {
	return f != nil && f(ctx, request)
}

// Clock supplies the engine's observation time so answers are reproducible
// under test.
type Clock interface {
	Now() time.Time
}

// GraphHopper optionally expands retrieval seeds via ontology/document edges.
// Implementations must not disclose unauthorized documents — caller seeds are
// already ACL-selected candidates. A nil GraphHopper disables graph hop.
type GraphHopper interface {
	// Expand returns neighbor document paths/ids for the pinned generation.
	Expand(generationID string, seedPaths []string, limit int) []string
}

// DenseSearcher returns document ids by dense similarity for a query string.
// A nil DenseSearcher disables hybrid dense expansion. Implementations must
// not disclose unauthorized documents; generationID scopes the corpus view.
type DenseSearcher interface {
	Search(ctx context.Context, generationID, query string, topK int) []string
}

// CandidateReranker reorders candidate paths using query + path bodies.
// A nil CandidateReranker leaves fused candidate order unchanged.
// topN bounds the returned path list; implementations may return fewer.
type CandidateReranker interface {
	Rerank(ctx context.Context, query string, paths []string, bodies map[string]string, topN int) []string
}

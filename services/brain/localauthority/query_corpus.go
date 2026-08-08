package localauthority

import (
	"context"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/query"
)

// This file adapts the durable local authority's published source into the
// grounded-query engine's corpus and authorization ports. Canonical revision
// facts and the rebuildable projection arrive together per pinned generation;
// revocation removes eligibility at the authorization checkpoints, never by
// rewriting immutable generation facts.

// ingestionScope returns the configured ingestion scope when the runtime has
// one, or ErrDenied when the runtime is closed or ingestion is not configured.
func (r *Runtime) ingestionScope() (localstate.IngestionScope, error) {
	r.ingestionMu.RLock()
	defer r.ingestionMu.RUnlock()
	if r.closed || r.ingestion == nil {
		return localstate.IngestionScope{}, ErrDenied
	}
	return r.ingestion.scope, nil
}

// restoreQuerySource rebuilds the published source and its projection from the
// durable checkpoint before the query surface serves, exactly as the Stage 03
// reads do. After a restart the first query triggers the rebind, exact-commit
// validation, and projection rebuild; a restored revoked source keeps its
// immutable generations servable to the corpus while every authorization
// checkpoint denies. The restore identity is the configured authority identity
// the command authenticated, never a request-body principal.
func (r *Runtime) restoreQuerySource(ctx context.Context, identity Identity) error {
	if ctx == nil || r == nil || !validIdentityLoose(identity) {
		return ErrInvalid
	}
	r.ingestionMu.Lock()
	defer r.ingestionMu.Unlock()
	if r.closed || r.ingestion == nil {
		return ErrDenied
	}
	return r.restoreIngestionLocked(ctx, identity)
}

// validIdentityLoose requires the principal, tenant, and session facts the
// checkpoint load authenticates against the session ledger.
func validIdentityLoose(identity Identity) bool {
	return validID(identity.Principal, "principal") && validID(identity.Tenant, "tenant") &&
		validID(identity.Session, "session")
}

// queryCorpus serves generation-pinned corpus views over the runtime's
// published source. The current and superseded generations resolve with their
// rebuildable projections; unknown sources or generations fail with the
// non-disclosing ErrUnknownScope. Revoked generations stay resolvable: the
// engine's authorization checkpoints convert denial into the absent_support
// abstention while freshness and coverage stay truthful.
type queryCorpus struct {
	runtime *Runtime
}

func (c queryCorpus) Snapshot(ctx context.Context, sourceID, generationID string) (query.Snapshot, error) {
	if ctx == nil || c.runtime == nil {
		return query.Snapshot{}, query.ErrUnknownScope
	}
	if err := ctx.Err(); err != nil {
		return query.Snapshot{}, err
	}
	c.runtime.ingestionMu.RLock()
	defer c.runtime.ingestionMu.RUnlock()
	if c.runtime.closed || c.runtime.ingestion == nil || sourceID != c.runtime.ingestion.scope.SourceID {
		return query.Snapshot{}, query.ErrUnknownScope
	}
	for _, published := range []*publishedSource{c.runtime.ingestion.current, c.runtime.ingestion.previous} {
		if published != nil && published.generation.ID == generationID {
			return snapshotFromPublished(published), nil
		}
	}
	return query.Snapshot{}, query.ErrUnknownScope
}

func (c queryCorpus) CurrentGeneration(ctx context.Context, sourceID string) (query.GenerationPin, error) {
	if ctx == nil || c.runtime == nil {
		return query.GenerationPin{}, query.ErrUnknownScope
	}
	if err := ctx.Err(); err != nil {
		return query.GenerationPin{}, err
	}
	c.runtime.ingestionMu.RLock()
	defer c.runtime.ingestionMu.RUnlock()
	if c.runtime.closed || c.runtime.ingestion == nil || sourceID != c.runtime.ingestion.scope.SourceID ||
		c.runtime.ingestion.current == nil {
		return query.GenerationPin{}, query.ErrUnknownScope
	}
	return query.GenerationPin{
		SourceID:     sourceID,
		GenerationID: c.runtime.ingestion.current.generation.ID,
	}, nil
}

// snapshotFromPublished projects one retained published source into the
// engine's pinned view: canonical manifest revisions, the occurrence index,
// and the hydrated canonical bytes arrive together as one ready projection.
// The published source is immutable after construction, so the projection is
// shared, never copied.
func snapshotFromPublished(published *publishedSource) query.Snapshot {
	generation := published.generation
	readiness := make([]query.LaneReadiness, 0, len(published.readiness))
	state := query.GenerationReady
	for _, lane := range published.readiness {
		if lane.Coverage == "lexical_degraded" {
			state = query.GenerationDegraded
		}
		readiness = append(readiness, query.LaneReadiness(lane))
	}
	revisions := make([]ingestion.FileRevision, 0, len(generation.Manifest.Files))
	revisions = append(revisions, generation.Manifest.Files...)
	index := &published.index
	return query.Snapshot{
		GenerationID: generation.ID,
		Sequence:     generation.Sequence,
		CommitOID:    generation.CommitOID,
		TreeOID:      generation.TreeOID,
		State:        state,
		Readiness:    readiness,
		Revisions:    revisions,
		Projection: query.ProjectionView{
			State: query.ProjectionReady,
			Index: index,
			Files: published.files,
		},
	}
}

// queryCheckpointAuthorizer evaluates the engine's query/hydrate/emit
// checkpoints through the command-bound broker callback and additionally
// requires the named source to be servable: configured, published, and not
// revoked. A mid-flight revocation therefore removes eligibility at the very
// next checkpoint, which is exactly the frozen revoke-during-query semantics.
type queryCheckpointAuthorizer struct {
	runtime   *Runtime
	authorize QueryAuthorizerFunc
}

func (a queryCheckpointAuthorizer) Authorize(
	ctx context.Context,
	principal query.Principal,
	action query.Action,
	sourceID string,
) (query.Decision, error) {
	allowed, epoch, err := a.authorize(ctx, queryIdentity(principal), string(action), sourceID)
	if err != nil || !allowed || !a.servable(sourceID) {
		return query.Decision{Allowed: false, Epoch: epoch}, err
	}
	return query.Decision{Allowed: true, Epoch: epoch}, nil
}

// servable reports whether the named source is the configured source with a
// published current generation that has not been revoked.
func (a queryCheckpointAuthorizer) servable(sourceID string) bool {
	a.runtime.ingestionMu.RLock()
	defer a.runtime.ingestionMu.RUnlock()
	return !a.runtime.closed && a.runtime.ingestion != nil && !a.runtime.ingestion.revoked &&
		a.runtime.ingestion.current != nil && sourceID == a.runtime.ingestion.scope.SourceID
}

// compile-time port checks keep the facade honest against the engine's ports.
var (
	_ query.Corpus     = queryCorpus{}
	_ query.Authorizer = queryCheckpointAuthorizer{}
)

package localauthority

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/conversation"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/query"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/rerank"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func hybridDenseEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("OUROBOROS_BRAIN_HYBRID_DENSE")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// Public aliases over the bounded Stage 04 query engine, conversation store,
// and provider adapter shapes. They let the composing gateway command wire the
// production query surface without importing brain-internal packages; every
// alias is the exact internal type, so invariants are never re-declared.
type (
	QueryRequest                      = query.Query
	QueryResult                       = query.Result
	QueryAnswer                       = query.Answer
	QueryClaim                        = query.Claim
	QueryCitation                     = query.Citation
	QueryFreshness                    = query.Freshness
	QueryCoverage                     = query.Coverage
	QueryPrincipal                    = query.Principal
	QuerySourceStatus                 = query.SourceStatus
	QuerySynthesizer                  = query.Synthesizer
	QuerySynthesisRequest             = query.SynthesisRequest
	QuerySynthesis                    = query.Synthesis
	QueryEvidenceEntry                = query.EvidenceEntry
	QuerySynthesisLimits              = query.SynthesisLimits
	QueryProviderClient               = query.ProviderClient
	QueryProviderRequest              = query.ProviderRequest
	QueryProviderResponse             = query.ProviderResponse
	QueryProposedClaim                = query.ProposedClaim
	QueryProposedCitation             = query.ProposedCitation
	QueryFreshnessRequired            = query.FreshnessRequirement
	QueryReason                       = query.Reason
	QueryStatus                       = query.Status
	QueryGenerationState              = query.GenerationState
	QueryFreshnessState               = query.FreshnessState
	QueryProjectionState              = query.ProjectionState
	QueryEvidenceAdmissionRequest     = query.EvidenceAdmissionRequest
	QueryEvidenceAdmitter             = query.EvidenceAdmitter
	QueryEvidenceAdmitterFunc         = query.EvidenceAdmitterFunc
	QueryFactualConsistencyResult     = factualconsistency.Result
	QueryFactualConsistencyStatus     = factualconsistency.Status
	QueryFactualConsistencyReason     = factualconsistency.Reason
	QueryFactualConsistencyProvenance = factualconsistency.Provenance
	ConversationStore                 = conversation.Store
	ConversationAdmission             = conversation.Admission
	ConversationAdmissionRes          = conversation.AdmissionResult
	ConversationCompletion            = conversation.Completion
	ConversationCompleteRes           = conversation.CompletionResult
	ConversationResolution            = conversation.Resolution
	ConversationTurn                  = conversation.Turn
	ConversationPage                  = conversation.Page
)

var (
	// ErrQueryUnknownScope is the non-disclosing engine failure for an unknown,
	// revoked, or unservable source or generation scope.
	ErrQueryUnknownScope = query.ErrUnknownScope
	// ErrQueryInvalidInput marks a malformed query request.
	ErrQueryInvalidInput = query.ErrInvalidInput
	// ErrConversationIdempotencyConflict marks a reused key with a different
	// request digest.
	ErrConversationIdempotencyConflict = conversation.ErrIdempotencyConflict
	// ErrConversationUnknownAdmission marks completion or resolution of a key
	// that was never admitted.
	ErrConversationUnknownAdmission = conversation.ErrUnknownAdmission
	// ErrConversationCompletionConflict marks a second, differing completion.
	ErrConversationCompletionConflict = conversation.ErrCompletionConflict
	// ErrConversationUnknownSession marks admission under a session the
	// canonical ledger never opened.
	ErrConversationUnknownSession = conversation.ErrUnknownSession
)

// QueryAuthorizerFunc evaluates one current authorization checkpoint for the
// composed query funnel. The command binds it to the production broker; the
// facade additionally requires the named source to be servable (configured,
// published, and not revoked) before any checkpoint allows. It returns the
// allow bit and the current revocation epoch; any error is treated as denial.
type QueryAuthorizerFunc func(ctx context.Context, identity Identity, action string, sourceID string) (allowed bool, epoch uint64, err error)

// NewDeterministicQuerySynthesizer returns the reproducible fixture/conformance
// synthesizer. It is the production default: the query surface never requires
// a live provider.
func NewDeterministicQuerySynthesizer() QuerySynthesizer {
	return query.NewDeterministicSynthesizer()
}

// NewProviderQuerySynthesizer binds one policy-approved provider client with a
// per-call deadline. The adapter fails closed on every provider error with no
// silent fallback to another provider or billing identity.
func NewProviderQuerySynthesizer(
	providerID, model string,
	client QueryProviderClient,
	timeout time.Duration,
) (QuerySynthesizer, error) {
	synthesizer, err := query.NewProviderSynthesizer(query.ProviderConfig{
		ProviderID: providerID, Model: model, Client: client, Timeout: timeout,
	})
	if err != nil {
		return nil, ErrInvalid
	}
	return synthesizer, nil
}

// QueryLaneFacts reports one P5 language lane's publication disposition.
type QueryLaneFacts struct {
	Language   string
	Coverage   string
	ReasonCode string
}

// QueryGenerationFacts is the contract-visible immutable catalog record of one
// published generation: snapshot identity, policy digest, watermark, and the
// five P5 lane readiness records.
type QueryGenerationFacts struct {
	GenerationID    string
	Sequence        uint64
	SnapshotID      string
	CommitOID       string
	TreeOID         string
	PolicyDigest    string
	State           string
	SourceWatermark uint64
	Readiness       []QueryLaneFacts
}

// QueryCatalogSource is the catalog view of the configured source: lifecycle
// state plus the current complete generation identity when one exists.
type QueryCatalogSource struct {
	SourceID            string
	RepositoryID        string
	BrainID             string
	State               string
	CurrentGenerationID string
}

// QueryEngine composes the grounded-query engine over this runtime's corpus
// and authorizer. Answer and Status are safe for concurrent use.
type QueryEngine struct {
	engine   *query.Engine
	runtime  *Runtime
	identity Identity
}

// QuerySurface is the composed Stage 04 query surface: the grounded-query
// engine, the durable private conversation store, and the migration-003 source
// catalog over one durable local authority. Close releases only the
// conversation store; the owning Runtime closes the authority itself.
type QuerySurface struct {
	runtime       *Runtime
	engine        *QueryEngine
	conversations *conversation.Store
}

// OpenQuerySurface composes the receipt-enforced Stage 04 query surface over
// one durable ingestion-configured runtime. The identity is the configured authority
// identity the command authenticated; it restores the published source from
// the durable checkpoint, and request principals never reach it. The call
// fails closed without a durable payload vault, a database path, or the
// ingestion runtime: the query surface widens the served surface of the same
// authority, never its trust boundary. Exactly one non-nil evidence admitter
// is required; omission or ambiguity fails closed at composition.
func (r *Runtime) OpenQuerySurface(
	ctx context.Context,
	identity Identity,
	authorize QueryAuthorizerFunc,
	synthesizer QuerySynthesizer,
	evidenceAdmitters ...QueryEvidenceAdmitter,
) (*QuerySurface, error) {
	if len(evidenceAdmitters) != 1 || evidenceAdmitters[0] == nil {
		return nil, ErrInvalid
	}
	return r.openQuerySurface(ctx, identity, authorize, synthesizer, evidenceAdmitters[0], false)
}

// OpenLegacyQuerySurfaceWithoutEvidenceAdmission explicitly preserves the
// retired pre-#316 local Stage 04 gateway and its frozen stale-best-effort
// contract. Organization-brain compositions must use OpenQuerySurface.
func (r *Runtime) OpenLegacyQuerySurfaceWithoutEvidenceAdmission(
	ctx context.Context,
	identity Identity,
	authorize QueryAuthorizerFunc,
	synthesizer QuerySynthesizer,
) (*QuerySurface, error) {
	return r.openQuerySurface(ctx, identity, authorize, synthesizer, nil, true)
}

func (r *Runtime) openQuerySurface(
	ctx context.Context,
	identity Identity,
	authorize QueryAuthorizerFunc,
	synthesizer QuerySynthesizer,
	evidenceAdmitter QueryEvidenceAdmitter,
	allowLegacyUnadmittedEvidence bool,
) (*QuerySurface, error) {
	if r == nil || ctx == nil || authorize == nil || synthesizer == nil || !validIdentityLoose(identity) {
		return nil, ErrInvalid
	}
	r.ingestionMu.RLock()
	configured := r.ingestion != nil
	r.ingestionMu.RUnlock()
	if !configured || r.databasePath == "" || r.conversationPayloads == nil || r.clock == nil {
		return nil, ErrInvalid
	}
	// Product hybrid ports: ontology hop always; dense+CE opt-in so Stage 04
	// grounding fixtures stay stable (dense-only path hits can false-match Go
	// keywords like "return"). Company-doc LiveCorpus / product-brain-eval always
	// hybrid. Enable with OUROBOROS_BRAIN_HYBRID_DENSE=1.
	_ = r.EnsureProductMemory()
	var graph query.GraphHopper
	if hopper := r.ProductGraphHopper(); hopper.Store != nil {
		graph = hopper
	}
	cfg := query.Config{
		Corpus:                        queryCorpus{runtime: r},
		Authorizer:                    queryCheckpointAuthorizer{runtime: r, authorize: authorize},
		Synthesizer:                   synthesizer,
		Clock:                         queryClock{clock: r.clock},
		Limits:                        query.DefaultLimits(),
		Graph:                         graph,
		EvidenceAdmitter:              evidenceAdmitter,
		AllowLegacyUnadmittedEvidence: allowLegacyUnadmittedEvidence,
	}
	if hybridDenseEnabled() {
		densePort := query.NewHybridEmbedDense(nil, r.generationBodies)
		densePort.Tenant = identity.Tenant.Value
		if emb, err := rerank.NewHTTPEmbedderFromEnv(); err == nil && emb != nil {
			if cached, cerr := rerank.NewEmbedCache(emb, 0, 0); cerr == nil && cached != nil {
				densePort = query.NewHybridEmbedDense(cached, r.generationBodies)
				densePort.Tenant = identity.Tenant.Value
			} else if emb != nil {
				densePort = query.NewHybridEmbedDense(emb, r.generationBodies)
				densePort.Tenant = identity.Tenant.Value
			}
		}
		cfg.Dense = densePort
		cfg.Reranker = query.NewHTTPCandidateRerankerFromEnv()
	}
	engine, err := query.NewEngine(cfg)
	if err != nil {
		return nil, ErrInvalid
	}
	store, err := conversation.Open(ctx, r.databasePath, r.conversationPayloads, r.clock)
	if err != nil {
		return nil, ErrUnavailable
	}
	return &QuerySurface{
		runtime: r,
		engine: &QueryEngine{
			engine: engine, runtime: r, identity: identity,
		},
		conversations: store,
	}, nil
}

// Engine returns the composed grounded-query engine.
func (s *QuerySurface) Engine() *QueryEngine {
	if s == nil {
		return nil
	}
	return s.engine
}

// Conversations returns the durable private conversation store.
func (s *QuerySurface) Conversations() *conversation.Store {
	if s == nil {
		return nil
	}
	return s.conversations
}

// RecoverInterrupted marks every admitted-but-uncompleted assistant completion
// visibly failed exactly once. The composing command runs it once at startup
// before the query surface serves.
func (s *QuerySurface) RecoverInterrupted(ctx context.Context) error {
	if s == nil || s.conversations == nil {
		return ErrInvalid
	}
	if _, err := s.conversations.RecoverInterrupted(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

// CatalogSource resolves the configured source's lifecycle state and current
// generation pointer from the migration 003 tables. Any load failure collapses
// to the non-disclosing ErrDenied; caller cancellation passes through.
func (s *QuerySurface) CatalogSource(ctx context.Context) (QueryCatalogSource, error) {
	if s == nil || s.runtime == nil {
		return QueryCatalogSource{}, ErrInvalid
	}
	scope, err := s.runtime.ingestionScope()
	if err != nil {
		return QueryCatalogSource{}, err
	}
	state, err := s.runtime.store.LoadIngestionSourceState(ctx, scope)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return QueryCatalogSource{}, ctxErr
		}
		return QueryCatalogSource{}, ErrDenied
	}
	return QueryCatalogSource{
		SourceID: scope.SourceID, RepositoryID: state.RepositoryID, BrainID: scope.Brain.Value,
		State: state.State, CurrentGenerationID: state.CurrentGenerationID,
	}, nil
}

// CatalogGenerationFacts resolves one published generation's immutable facts.
// Superseded and revoked-source generations resolve exactly as published so an
// admitted Ask discloses freshness without leaking revocation through a
// changed outcome shape; an unknown generation is the non-disclosing ErrDenied.
func (s *QuerySurface) CatalogGenerationFacts(ctx context.Context, generationID string) (QueryGenerationFacts, error) {
	if s == nil || s.runtime == nil || generationID == "" {
		return QueryGenerationFacts{}, ErrInvalid
	}
	scope, err := s.runtime.ingestionScope()
	if err != nil {
		return QueryGenerationFacts{}, err
	}
	facts, err := s.runtime.store.LoadIngestionGenerationFacts(ctx, scope, generationID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return QueryGenerationFacts{}, ctxErr
		}
		return QueryGenerationFacts{}, ErrDenied
	}
	readiness := make([]QueryLaneFacts, 0, len(facts.Readiness))
	for _, lane := range facts.Readiness {
		readiness = append(readiness, QueryLaneFacts(lane))
	}
	return QueryGenerationFacts{
		GenerationID: facts.GenerationID, Sequence: facts.Sequence, SnapshotID: facts.SnapshotID,
		CommitOID: facts.CommitOID, TreeOID: facts.TreeOID, PolicyDigest: facts.PolicyDigest,
		State: facts.State, SourceWatermark: facts.SourceWatermark, Readiness: readiness,
	}, nil
}

// Close releases the conversation store handle. It is idempotent and does not
// close the owning runtime.
func (s *QuerySurface) Close() error {
	if s == nil || s.conversations == nil {
		return nil
	}
	return s.conversations.Close()
}

// Answer executes one grounded query against one pinned generation. The
// runtime restores the published source from its durable checkpoint before the
// engine runs so a restarted authority rebuilds the projection before serving.
func (e *QueryEngine) Answer(ctx context.Context, request QueryRequest) (QueryResult, error) {
	if e == nil || e.engine == nil {
		return QueryResult{}, ErrInvalid
	}
	if err := e.runtime.restoreQuerySource(ctx, e.identity); err != nil {
		return QueryResult{}, err
	}
	return e.engine.Answer(ctx, request)
}

// Status composes the authorized per-source status view after the same
// restore-before-serve rule as Answer.
func (e *QueryEngine) Status(ctx context.Context, principal QueryPrincipal, sourceID string) (QuerySourceStatus, error) {
	if e == nil || e.engine == nil {
		return QuerySourceStatus{}, ErrInvalid
	}
	if err := e.runtime.restoreQuerySource(ctx, e.identity); err != nil {
		return QuerySourceStatus{}, err
	}
	return e.engine.Status(ctx, principal, sourceID)
}

// queryIdentity rebuilds the trusted mapped identity from engine principal
// facts. The gateway authenticated and cross-checked these facts against the
// peer before they reached the engine, so reconstruction never widens trust.
func queryIdentity(principal query.Principal) Identity {
	return Identity{
		Principal: shared.Identifier{Namespace: "principal", Value: principal.Principal},
		Tenant:    shared.Identifier{Namespace: "tenant", Value: principal.Tenant},
		Session:   shared.Identifier{Namespace: "session", Value: principal.Session},
	}
}

// queryClock adapts the authority clock to the engine's observation-time port.
type queryClock struct{ clock Clock }

func (c queryClock) Now() time.Time { return time.UnixMilli(c.clock.NowUnixMilli()).UTC() }

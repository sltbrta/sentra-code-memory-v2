package query

import (
	"context"
	"fmt"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

// Engine is the bounded Stage 04 grounded-query engine. It is safe for
// concurrent use: every dependency is a port, and Answer and Status hold no
// mutable state between calls.
type Engine struct {
	corpus      Corpus
	authorizer  Authorizer
	synthesizer Synthesizer
	clock       Clock
	limits      Limits
	graph       GraphHopper               // optional; nil disables multi-hop expand
	dense       DenseSearcher             // optional; nil disables hybrid dense fusion
	reranker    CandidateReranker         // optional; nil disables CE/lexical rerank
	packing     PackPolicy                // optional; zero value keeps retrieval order and frozen bounds
	admitter    EvidenceAdmitter          // required unless the composition explicitly selects the legacy boundary
	scorer      factualconsistency.Scorer // optional; nil produces an explicit unknown score
}

// Config binds an Engine to its ports. Corpus, Authorizer, Synthesizer, and
// Clock and EvidenceAdmitter are required. Graph, Dense, and Reranker are
// optional. AllowLegacyUnadmittedEvidence is reserved for frozen query paths
// outside the receipt-enforced organization-brain boundary.
type Config struct {
	Corpus           Corpus
	Authorizer       Authorizer
	Synthesizer      Synthesizer
	Clock            Clock
	Limits           Limits
	Graph            GraphHopper
	Dense            DenseSearcher
	Reranker         CandidateReranker
	Packing          PackPolicy
	EvidenceAdmitter EvidenceAdmitter
	// FactualConsistencyScorer evaluates only verified claims and exact cited
	// spans. Nil is allowed and produces an explicit scorer_unavailable result.
	FactualConsistencyScorer factualconsistency.Scorer
	// AllowLegacyUnadmittedEvidence explicitly preserves a pre-#316 caller
	// that has no canonical propagation-receipt provider. New production
	// compositions must leave this false and supply EvidenceAdmitter.
	AllowLegacyUnadmittedEvidence bool
}

// NewEngine validates the configuration; a misconfigured engine fails at
// construction, never at request time. Nil Dense and Reranker leave hybrid
// expansion disabled without changing the required-port contract.
func NewEngine(config Config) (*Engine, error) {
	if config.Corpus == nil || config.Authorizer == nil || config.Synthesizer == nil || config.Clock == nil {
		return nil, fmt.Errorf("%w: engine requires corpus, authorizer, synthesizer, and clock", ErrInvalidInput)
	}
	if config.EvidenceAdmitter == nil && !config.AllowLegacyUnadmittedEvidence {
		return nil, fmt.Errorf("%w: engine requires evidence admission", ErrInvalidInput)
	}
	if err := config.Limits.validate(); err != nil {
		return nil, err
	}
	if err := config.Packing.validate(config.Limits); err != nil {
		return nil, err
	}
	return &Engine{
		corpus:      config.Corpus,
		authorizer:  config.Authorizer,
		synthesizer: config.Synthesizer,
		clock:       config.Clock,
		limits:      config.Limits,
		graph:       config.Graph,
		dense:       config.Dense,
		reranker:    config.Reranker,
		packing:     config.Packing,
		admitter:    config.EvidenceAdmitter,
		scorer:      config.FactualConsistencyScorer,
	}, nil
}

// Answer executes one grounded query against one pinned generation.
//
// The funnel is ACL-first and never widens: admission authorization is
// evaluated before any corpus read, an admission denial returns exactly the
// absent_support abstention regardless of corpus, staleness, or projection
// state (byte-identical to genuinely absent support), hydration
// reauthorization precedes canonical byte access, and emission
// reauthorization follows synthesis so a mid-query revocation can only
// remove output.
//
// The returned error is only ever ErrInvalidInput (malformed request),
// ErrUnknownScope (unknown, revoked, or unservable source or generation,
// mapped by the gateway to not_found_or_denied), or a context error. Every
// other outcome — absence, degradation, staleness, provider failure,
// projection absence — is a composed Answer with frozen disclosures.
func (e *Engine) Answer(ctx context.Context, query Query) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := query.validate(e.limits); err != nil {
		return Result{}, err
	}
	observedAt := e.clock.Now()
	decision, authorizeErr := e.authorizer.Authorize(ctx, query.Principal, ActionQuery, query.SourceID)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	snapshot, err := e.corpus.Snapshot(ctx, query.SourceID, query.GenerationID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Result{}, contextErr
		}
		return Result{}, ErrUnknownScope
	}
	current, err := e.corpus.CurrentGeneration(ctx, query.SourceID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Result{}, contextErr
		}
		return Result{}, ErrUnknownScope
	}
	evaluation := evaluateFreshness(query.Freshness, snapshot, current.GenerationID)
	result := pinResult(snapshot, evaluation, decision.Epoch, observedAt)
	if authorizeErr != nil || !decision.Allowed {
		// Admission denial is enforced before any freshness, projection, or
		// retrieval outcome can shape the answer: the abstention is exactly
		// absent_support, so a denied principal cannot distinguish denial
		// from genuinely absent support in any corpus state. Freshness and
		// coverage stay truthful, as the frozen AskSuccess disclosure does
		// for authorized absent answers.
		result.Answer = composeAnswer(query.QueryID, nil, "", 0, []Reason{ReasonAbsentSupport}, factualconsistency.Abstained(), e.limits.MaxReasons)
		return result, nil
	}
	var reasons []Reason
	if evaluation.Stale {
		reasons = append(reasons, ReasonStaleSupport)
	}
	abstain := func() (Result, error) {
		result.Answer = composeAnswer(query.QueryID, nil, "", 0, reasons, factualconsistency.Abstained(), e.limits.MaxReasons)
		return result, nil
	}
	if evaluation.AbstainStale {
		return abstain()
	}
	if snapshot.Projection.State != ProjectionReady || snapshot.Projection.Index == nil {
		reasons = append(reasons, ReasonRetrievalUnavailable)
		return abstain()
	}

	terms := tokenizeQuery(query.Text)
	// Multi-query + RRF expands path-free definition matching; exact path
	// mentions remain scoped by selectCandidatesMulti (no widening).
	candidates := selectCandidatesMulti(snapshot, query.Text, e.limits.MaxCandidates)
	// Optional ontology multi-hop: expand from already-selected seeds only.
	if e.graph != nil && len(candidates) > 0 {
		seeds := make([]string, 0, len(candidates))
		for _, c := range candidates {
			seeds = append(seeds, c.path)
		}
		neighbors := e.graph.Expand(snapshot.GenerationID, seeds, 16)
		candidates = expandWithGraph(candidates, neighbors, 12)
	}
	// Optional hybrid dense: RRF-fuse lexical candidates with dense hits, then
	// optional CE/lexical rerank over projection bodies. Both ports are nil-safe.
	candidates = e.expandHybridDense(ctx, snapshot, query.Text, candidates)
	unindexed := unindexedMentions(snapshot, terms)
	laneDegraded := false
	var selections []evidenceSelection
	files := filesByPath(snapshot)
	for _, selected := range candidates {
		if selected.degraded {
			laneDegraded = true
			continue
		}
		occurrence, found := selectDefinition(files[selected.path], terms)
		if !found {
			continue
		}
		selections = append(selections, evidenceSelection{file: files[selected.path], occurrence: occurrence})
	}
	// Hydration authorization precedes any canonical byte or digest work: a
	// denied principal triggers no hydration, no integrity verification, and
	// no synthesis.
	deniedMidQuery := false
	if len(selections) > 0 {
		hydrateDecision, hydrateErr := e.authorizer.Authorize(ctx, query.Principal, ActionHydrate, query.SourceID)
		if hydrateErr != nil || !hydrateDecision.Allowed {
			selections = nil
			deniedMidQuery = true
		} else {
			result.Freshness.ACLEpoch = hydrateDecision.Epoch
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var entries []EvidenceEntry
	for _, selection := range selections {
		entry, hydrateErr := hydrateEntry(snapshot, selection.file, selection.occurrence, e.limits.MaxBlockLines)
		if hydrateErr != nil {
			reasons = append(reasons, ReasonCitationVerificationFailed)
			continue
		}
		entries = append(entries, entry)
	}
	integrityFailed := containsReason(reasons, ReasonCitationVerificationFailed)
	packed, dropped := packEvidenceWithPolicy(entries, e.packing, e.limits)
	if len(dropped) > 0 || len(unindexed) > 0 {
		reasons = append(reasons, ReasonPartialCoverage)
	}
	if laneDegraded {
		reasons = append(reasons, ReasonLaneDegraded)
	}
	if len(packed) == 0 {
		switch {
		case integrityFailed:
			// Support existed but failed canonical verification; that is never absence.
		case deniedMidQuery:
			reasons = append(reasons, ReasonAbsentSupport)
		default:
			reasons = append(reasons, ReasonAbsentSupport)
			if result.Coverage.IndexedRevisionCount < result.Coverage.CanonicalRevisionCount {
				reasons = append(reasons, ReasonPartialCoverage)
			}
		}
		return abstain()
	}

	synthesis, err := e.synthesizer.Synthesize(ctx, SynthesisRequest{
		Query: query.Text, Evidence: packed, Limits: e.limits.Synthesis,
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Result{}, contextErr
		}
		reasons = append(reasons, ReasonSynthesisUnavailable)
		return abstain()
	}
	verdict := verifySynthesis(synthesis, packed, snapshot, e.limits.Synthesis)
	if verdict.structureFault {
		reasons = append(reasons, ReasonSynthesisUnavailable)
		return abstain()
	}
	if verdict.citationFault {
		reasons = append(reasons, ReasonCitationVerificationFailed)
	}
	if len(verdict.claims) == 0 {
		if !verdict.citationFault {
			reasons = append(reasons, ReasonAbsentSupport)
		}
		return abstain()
	}
	prose := synthesis.Prose
	if verdict.citationFault {
		prose, err = regenerateProse(verdict.claims, e.limits.Synthesis.MaxProseBytes)
		if err != nil {
			reasons = append(reasons, ReasonSynthesisUnavailable)
			return abstain()
		}
	}
	consistency, scoreErr := factualconsistency.Evaluate(ctx, e.scorer, factualconsistency.Request{Claims: verdict.scoreClaims}, e.limits.FactualConsistency)
	if scoreErr != nil {
		return Result{}, scoreErr
	}
	emitDecision, emitErr := e.authorizer.Authorize(ctx, query.Principal, ActionEmit, query.SourceID)
	if emitErr != nil || !emitDecision.Allowed {
		reasons = append(reasons, ReasonAbsentSupport)
		return abstain()
	}
	if e.admitter != nil && !e.admitEvidenceAtEmitBoundary(ctx, query.SourceID, snapshot.GenerationID, emitDecision.Epoch) {
		if contextErr := ctx.Err(); contextErr != nil {
			return Result{}, contextErr
		}
		reasons = append(reasons, ReasonAbsentSupport)
		return abstain()
	}
	result.Answer = composeAnswer(query.QueryID, verdict.claims, prose, synthesis.TokenUsage, reasons, consistency, e.limits.MaxReasons)
	return result, nil
}

// admitEvidenceAtEmitBoundary performs a post-admission canonical recheck and
// then a final admission at a newly observed time. The first admission may
// block while a receipt source catches up. A generation, tombstone, or ACL
// event committed during that wait must therefore participate in the final
// decision instead of being filtered by the first request's timestamp.
func (e *Engine) admitEvidenceAtEmitBoundary(
	ctx context.Context,
	sourceID string,
	generationID string,
	aclEpoch uint64,
) bool {
	request := EvidenceAdmissionRequest{
		SourceID: sourceID, GenerationID: generationID, ACLEpoch: aclEpoch, At: e.clock.Now(),
	}
	if !e.admitter.AdmitEvidence(ctx, request) {
		return false
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	current, err := e.corpus.CurrentGeneration(ctx, sourceID)
	if err != nil || current.SourceID != sourceID || current.GenerationID != generationID {
		return false
	}
	request.At = e.clock.Now()
	return e.admitter.AdmitEvidence(ctx, request)
}

// Status composes the authorized GetStatus view for one source: the current
// complete generation's freshness, canonical-versus-indexed coverage, and
// projection truth. Denied, unknown, and revoked sources share the single
// non-disclosing ErrUnknownScope.
func (e *Engine) Status(ctx context.Context, principal Principal, sourceID string) (SourceStatus, error) {
	if err := ctx.Err(); err != nil {
		return SourceStatus{}, err
	}
	if sourceID == "" || len(sourceID) > e.limits.MaxIdentifierLength ||
		principal.Tenant == "" || len(principal.Tenant) > e.limits.MaxIdentifierLength ||
		principal.Principal == "" || len(principal.Principal) > e.limits.MaxIdentifierLength ||
		principal.Session == "" || len(principal.Session) > e.limits.MaxIdentifierLength {
		return SourceStatus{}, ErrInvalidInput
	}
	decision, authorizeErr := e.authorizer.Authorize(ctx, principal, ActionQuery, sourceID)
	if authorizeErr != nil || !decision.Allowed {
		return SourceStatus{}, ErrUnknownScope
	}
	current, err := e.corpus.CurrentGeneration(ctx, sourceID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return SourceStatus{}, contextErr
		}
		return SourceStatus{}, ErrUnknownScope
	}
	snapshot, err := e.corpus.Snapshot(ctx, sourceID, current.GenerationID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return SourceStatus{}, contextErr
		}
		return SourceStatus{}, ErrUnknownScope
	}
	// Re-read the current pin after the snapshot: a reconcile landing between
	// the two reads supersedes the served generation, and the status must
	// disclose staleness instead of reporting it as current.
	latest, err := e.corpus.CurrentGeneration(ctx, sourceID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return SourceStatus{}, contextErr
		}
		return SourceStatus{}, ErrUnknownScope
	}
	// Reauthorize immediately before emission: a revocation landing while the
	// corpus reads ran collapses the whole status to the same non-disclosing
	// scope failure as denial or absence, never emitting generation metadata.
	emitDecision, emitErr := e.authorizer.Authorize(ctx, principal, ActionEmit, sourceID)
	if emitErr != nil || !emitDecision.Allowed {
		return SourceStatus{}, ErrUnknownScope
	}
	evaluation := evaluateFreshness(FreshnessBestEffort, snapshot, latest.GenerationID)
	return SourceStatus{
		SourceID: sourceID,
		Freshness: Freshness{
			GenerationID:    snapshot.GenerationID,
			Sequence:        snapshot.Sequence,
			CommitOID:       snapshot.CommitOID,
			TreeOID:         snapshot.TreeOID,
			GenerationState: snapshot.State,
			State:           evaluation.State,
			ACLEpoch:        decision.Epoch,
			ObservedAt:      e.clock.Now(),
		},
		Coverage:   computeCoverage(snapshot),
		Projection: snapshot.Projection.State,
	}, nil
}

// pinResult composes the truthful freshness, coverage, and projection
// disclosures one pinned generation carries into every Ask result.
func pinResult(snapshot Snapshot, evaluation freshnessEvaluation, aclEpoch uint64, observedAt time.Time) Result {
	return Result{
		Freshness: Freshness{
			GenerationID:    snapshot.GenerationID,
			Sequence:        snapshot.Sequence,
			CommitOID:       snapshot.CommitOID,
			TreeOID:         snapshot.TreeOID,
			GenerationState: snapshot.State,
			State:           evaluation.State,
			ACLEpoch:        aclEpoch,
			ObservedAt:      observedAt,
		},
		Coverage:   computeCoverage(snapshot),
		Projection: snapshot.Projection.State,
	}
}

func containsReason(reasons []Reason, want Reason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

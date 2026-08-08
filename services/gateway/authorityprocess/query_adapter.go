// Package authorityprocess maps the frozen Stage 04 query ports onto the composed local
// authority: the brain query surface, the conversation store, the migration
// 003 source catalog, and the current-relationship broker. Every adapter is a
// thin field-for-field mapping that never invents authority facts.
package authorityprocess

import (
	"context"
	"errors"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/queryapi"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// queryAuthorityAdapter mounts the queryapi handler on the frozen QueryService
// procedures. The transport maps any returned error to its static
// request-denied shape, so no error detail crosses the boundary.
type queryAuthorityAdapter struct {
	handler *queryapi.Handler
}

func (adapter queryAuthorityAdapter) Ask(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.AskRequest,
) (*contractsv1.AskResponse, error) {
	return adapter.handler.Ask(ctx, peer, request)
}

func (adapter queryAuthorityAdapter) ListSources(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.ListSourcesRequest,
) (*contractsv1.ListSourcesResponse, error) {
	return adapter.handler.ListSources(ctx, peer, request)
}

func (adapter queryAuthorityAdapter) GetHistory(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetHistoryRequest,
) (*contractsv1.GetHistoryResponse, error) {
	return adapter.handler.GetHistory(ctx, peer, request)
}

func (adapter queryAuthorityAdapter) GetStatus(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetStatusRequest,
) (*contractsv1.GetStatusResponse, error) {
	return adapter.handler.GetStatus(ctx, peer, request)
}

// queryEngineAdapter maps the composed brain query engine onto the queryapi
// engine port, mirroring field for field.
type queryEngineAdapter struct {
	engine *brain.QueryEngine
}

func (adapter queryEngineAdapter) Answer(ctx context.Context, request queryapi.EngineQuery) (queryapi.EngineResult, error) {
	if adapter.engine == nil {
		return queryapi.EngineResult{}, queryapi.ErrUnknownScope
	}
	result, err := adapter.engine.Answer(ctx, brain.QueryRequest{
		QueryID:        request.QueryID,
		Principal:      queryPrincipal(request.Principal),
		SourceID:       request.SourceID,
		GenerationID:   request.GenerationID,
		Text:           request.Text,
		Freshness:      brain.QueryFreshnessRequired(request.Freshness),
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		return queryapi.EngineResult{}, mapQueryEngineError(ctx, err)
	}
	return engineResultFromBrain(result), nil
}

func (adapter queryEngineAdapter) Status(ctx context.Context, principal queryapi.Principal, sourceID string) (queryapi.EngineStatus, error) {
	if adapter.engine == nil {
		return queryapi.EngineStatus{}, queryapi.ErrUnknownScope
	}
	status, err := adapter.engine.Status(ctx, queryPrincipal(principal), sourceID)
	if err != nil {
		return queryapi.EngineStatus{}, mapQueryEngineError(ctx, err)
	}
	return queryapi.EngineStatus{
		SourceID:   status.SourceID,
		Freshness:  engineFreshnessFromBrain(status.Freshness),
		Coverage:   engineCoverageFromBrain(status.Coverage),
		Projection: string(status.Projection),
	}, nil
}

// mapQueryEngineError preserves caller cancellation and maps the engine's
// non-disclosing scope failure onto the port sentinel; every other failure
// collapses to the same non-disclosing scope error.
func mapQueryEngineError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return queryapi.ErrUnknownScope
}

func queryPrincipal(principal queryapi.Principal) brain.QueryPrincipal {
	return brain.QueryPrincipal{
		Tenant: principal.Tenant, Principal: principal.PrincipalID, Session: principal.Session,
	}
}

func engineResultFromBrain(result brain.QueryResult) queryapi.EngineResult {
	return queryapi.EngineResult{
		Answer:     engineAnswerFromBrain(result.Answer),
		Freshness:  engineFreshnessFromBrain(result.Freshness),
		Coverage:   engineCoverageFromBrain(result.Coverage),
		Projection: string(result.Projection),
	}
}

func engineAnswerFromBrain(answer brain.QueryAnswer) queryapi.EngineAnswer {
	mapped := queryapi.EngineAnswer{
		QueryID:    answer.QueryID,
		Status:     string(answer.Status),
		Prose:      answer.Prose,
		TokenUsage: answer.TokenUsage,
		FactualConsistency: queryapi.EngineFactualConsistency{
			Status: string(answer.FactualConsistency.Status), ScorePerMille: answer.FactualConsistency.ScorePerMille,
			Reason:              string(answer.FactualConsistency.Reason),
			EvaluatedClaimCount: answer.FactualConsistency.EvaluatedClaimCount,
			TotalClaimCount:     answer.FactualConsistency.TotalClaimCount,
		},
	}
	if answer.FactualConsistency.Provenance != nil {
		mapped.FactualConsistency.Provenance = &queryapi.EngineFactualConsistencyProvenance{
			ScorerID:          answer.FactualConsistency.Provenance.ScorerID,
			ScorerVersion:     answer.FactualConsistency.Provenance.ScorerVersion,
			CalibrationID:     answer.FactualConsistency.Provenance.CalibrationID,
			CalibrationDigest: answer.FactualConsistency.Provenance.CalibrationDigest,
		}
	}
	for _, reason := range answer.DegradedReasons {
		mapped.DegradedReasons = append(mapped.DegradedReasons, string(reason))
	}
	for _, claim := range answer.Claims {
		mappedClaim := queryapi.EngineClaim{
			ClaimID:            claim.ClaimID,
			Statement:          claim.Statement,
			ConfidencePerMille: claim.ConfidencePerMille,
		}
		for _, citation := range claim.Citations {
			mappedClaim.Citations = append(mappedClaim.Citations, queryapi.EngineCitation{
				EvidenceID:           citation.EvidenceID,
				SourceRevisionID:     citation.SourceRevisionID,
				GitOID:               citation.GitOID,
				Path:                 citation.Path,
				StartLine:            citation.StartLine,
				StartColumn:          citation.StartColumn,
				EndLine:              citation.EndLine,
				EndColumn:            citation.EndColumn,
				SupportingTextDigest: citation.SupportingTextDigest,
			})
		}
		mapped.Claims = append(mapped.Claims, mappedClaim)
	}
	return mapped
}

func engineFreshnessFromBrain(freshness brain.QueryFreshness) queryapi.EngineFreshness {
	return queryapi.EngineFreshness{
		GenerationID:    freshness.GenerationID,
		Sequence:        freshness.Sequence,
		CommitOID:       freshness.CommitOID,
		TreeOID:         freshness.TreeOID,
		GenerationState: string(freshness.GenerationState),
		State:           string(freshness.State),
		ACLEpoch:        freshness.ACLEpoch,
		ObservedAt:      freshness.ObservedAt,
	}
}

func engineCoverageFromBrain(coverage brain.QueryCoverage) queryapi.EngineCoverage {
	return queryapi.EngineCoverage{
		CanonicalRevisionCount: coverage.CanonicalRevisionCount,
		IndexedRevisionCount:   coverage.IndexedRevisionCount,
	}
}

// engineResultToBrain maps a validated queryapi engine result back onto the
// brain result the conversation store persists for byte-faithful replay.
func engineResultToBrain(result *queryapi.EngineResult) *brain.QueryResult {
	if result == nil {
		return nil
	}
	stored := &brain.QueryResult{
		Freshness: brain.QueryFreshness{
			GenerationID:    result.Freshness.GenerationID,
			Sequence:        result.Freshness.Sequence,
			CommitOID:       result.Freshness.CommitOID,
			TreeOID:         result.Freshness.TreeOID,
			GenerationState: brain.QueryGenerationState(result.Freshness.GenerationState),
			State:           brain.QueryFreshnessState(result.Freshness.State),
			ACLEpoch:        result.Freshness.ACLEpoch,
			ObservedAt:      result.Freshness.ObservedAt,
		},
		Coverage: brain.QueryCoverage{
			CanonicalRevisionCount: result.Coverage.CanonicalRevisionCount,
			IndexedRevisionCount:   result.Coverage.IndexedRevisionCount,
		},
		Projection: brain.QueryProjectionState(result.Projection),
	}
	stored.Answer = brain.QueryAnswer{
		QueryID:    result.Answer.QueryID,
		Status:     brain.QueryStatus(result.Answer.Status),
		Prose:      result.Answer.Prose,
		TokenUsage: result.Answer.TokenUsage,
		FactualConsistency: brain.QueryFactualConsistencyResult{
			Status:              brain.QueryFactualConsistencyStatus(result.Answer.FactualConsistency.Status),
			ScorePerMille:       result.Answer.FactualConsistency.ScorePerMille,
			Reason:              brain.QueryFactualConsistencyReason(result.Answer.FactualConsistency.Reason),
			EvaluatedClaimCount: result.Answer.FactualConsistency.EvaluatedClaimCount,
			TotalClaimCount:     result.Answer.FactualConsistency.TotalClaimCount,
		},
	}
	if result.Answer.FactualConsistency.Provenance != nil {
		stored.Answer.FactualConsistency.Provenance = &brain.QueryFactualConsistencyProvenance{
			ScorerID:          result.Answer.FactualConsistency.Provenance.ScorerID,
			ScorerVersion:     result.Answer.FactualConsistency.Provenance.ScorerVersion,
			CalibrationID:     result.Answer.FactualConsistency.Provenance.CalibrationID,
			CalibrationDigest: result.Answer.FactualConsistency.Provenance.CalibrationDigest,
		}
	}
	for _, reason := range result.Answer.DegradedReasons {
		stored.Answer.DegradedReasons = append(stored.Answer.DegradedReasons, brain.QueryReason(reason))
	}
	for _, claim := range result.Answer.Claims {
		storedClaim := brain.QueryClaim{
			ClaimID:            claim.ClaimID,
			Statement:          claim.Statement,
			ConfidencePerMille: claim.ConfidencePerMille,
		}
		for _, citation := range claim.Citations {
			storedClaim.Citations = append(storedClaim.Citations, brain.QueryCitation{
				EvidenceID:           citation.EvidenceID,
				SourceRevisionID:     citation.SourceRevisionID,
				GitOID:               citation.GitOID,
				Path:                 citation.Path,
				StartLine:            citation.StartLine,
				StartColumn:          citation.StartColumn,
				EndLine:              citation.EndLine,
				EndColumn:            citation.EndColumn,
				SupportingTextDigest: citation.SupportingTextDigest,
			})
		}
		stored.Answer.Claims = append(stored.Answer.Claims, storedClaim)
	}
	return stored
}

// queryConversationsAdapter maps the durable conversation store onto the
// queryapi conversations port with the documented sentinel mapping.
type queryConversationsAdapter struct {
	store *brain.ConversationStore
}

func (adapter queryConversationsAdapter) Admit(ctx context.Context, admission queryapi.Admission) (queryapi.AdmissionResult, error) {
	if adapter.store == nil {
		return queryapi.AdmissionResult{}, queryapi.ErrRequestDenied
	}
	admitted, err := adapter.store.Admit(ctx, brain.ConversationAdmission{
		Principal:      queryPrincipal(admission.Principal),
		SourceID:       admission.SourceID,
		GenerationID:   admission.GenerationID,
		Text:           admission.Text,
		Freshness:      brain.QueryFreshnessRequired(admission.Freshness),
		IdempotencyKey: admission.IdempotencyKey,
	})
	if err != nil {
		return queryapi.AdmissionResult{}, mapConversationError(err)
	}
	return queryapi.AdmissionResult{
		QueryID: admitted.QueryID, UserTurnID: admitted.UserTurnID, Replayed: admitted.Replayed,
	}, nil
}

func (adapter queryConversationsAdapter) Complete(ctx context.Context, completion queryapi.Completion) (queryapi.CompletionResult, error) {
	if adapter.store == nil {
		return queryapi.CompletionResult{}, queryapi.ErrRequestDenied
	}
	completed, err := adapter.store.Complete(ctx, brain.ConversationCompletion{
		Tenant:         completion.Tenant,
		Principal:      completion.PrincipalID,
		IdempotencyKey: completion.IdempotencyKey,
		Result:         engineResultToBrain(completion.Result),
		Failed:         completion.Failed,
	})
	if err != nil {
		return queryapi.CompletionResult{}, mapConversationError(err)
	}
	return queryapi.CompletionResult{
		AssistantTurnID: completed.AssistantTurnID, Sequence: completed.Sequence, Replayed: completed.Replayed,
	}, nil
}

func (adapter queryConversationsAdapter) Resolve(ctx context.Context, tenant, principal, idempotencyKey string) (queryapi.Resolution, error) {
	if adapter.store == nil {
		return queryapi.Resolution{}, queryapi.ErrRequestDenied
	}
	resolution, err := adapter.store.Resolve(ctx, tenant, principal, idempotencyKey)
	if err != nil {
		return queryapi.Resolution{}, mapConversationError(err)
	}
	mapped := queryapi.Resolution{
		QueryID:         resolution.QueryID,
		UserTurnID:      resolution.UserTurnID,
		SessionID:       resolution.SessionID,
		Completed:       resolution.Completed,
		Status:          string(resolution.Status),
		AssistantTurnID: resolution.AssistantTurnID,
	}
	if resolution.Result != nil {
		mapped.Result = &queryapi.EngineResult{}
		*mapped.Result = engineResultFromBrain(*resolution.Result)
	}
	return mapped, nil
}

func (adapter queryConversationsAdapter) History(
	ctx context.Context,
	tenant, principal, after string,
	limit uint32,
) (queryapi.HistoryPage, error) {
	if adapter.store == nil {
		return queryapi.HistoryPage{}, queryapi.ErrRequestDenied
	}
	page, err := adapter.store.History(ctx, tenant, principal, after, limit)
	if err != nil {
		return queryapi.HistoryPage{}, mapConversationError(err)
	}
	mapped := queryapi.HistoryPage{NextCursor: page.NextCursor}
	for _, turn := range page.Turns {
		mappedTurn := queryapi.HistoryTurn{
			TurnID:       turn.TurnID,
			SessionID:    turn.SessionID,
			Sequence:     turn.Sequence,
			Role:         string(turn.Role),
			Status:       string(turn.Status),
			OccurredAtMs: turn.OccurredAtMs,
			Text:         turn.Text,
		}
		if turn.Answer != nil {
			answer := engineAnswerFromBrain(*turn.Answer)
			mappedTurn.Answer = &answer
		}
		mapped.Turns = append(mapped.Turns, mappedTurn)
	}
	return mapped, nil
}

// mapConversationError maps the conversation store's exported sentinels onto
// the queryapi sentinels exactly as the L2 handoff documents:
// ErrUnknownSession maps to ErrRequestDenied. Any other failure collapses to
// ErrRequestDenied so the handler returns its static shape without detail.
func mapConversationError(err error) error {
	switch {
	case errors.Is(err, brain.ErrConversationIdempotencyConflict):
		return queryapi.ErrIdempotencyConflict
	case errors.Is(err, brain.ErrConversationUnknownAdmission):
		return queryapi.ErrUnknownAdmission
	case errors.Is(err, brain.ErrConversationCompletionConflict):
		return queryapi.ErrCompletionConflict
	case errors.Is(err, brain.ErrConversationUnknownSession):
		return queryapi.ErrRequestDenied
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return queryapi.ErrRequestDenied
	}
}

// queryCatalogAdapter serves authorized source pages, immutable per-generation
// facts, and source references from the migration 003 tables. Authorization is
// evaluated against the current broker relationship before every read; unknown,
// unauthorized, or revoked scopes collapse to ErrUnknownScope.
type queryCatalogAdapter struct {
	surface *brain.QuerySurface
	broker  *broker.Broker
	brain   brain.Identifier
	source  string
}

func (adapter queryCatalogAdapter) List(
	ctx context.Context,
	principal queryapi.Principal,
	_ string,
	_ uint32,
) (queryapi.SourcePage, error) {
	if err := adapter.authorizeQuery(ctx, principal); err != nil {
		return queryapi.SourcePage{}, err
	}
	catalog, err := adapter.surface.CatalogSource(ctx)
	if err != nil {
		return queryapi.SourcePage{}, mapCatalogError(ctx, err)
	}
	if catalog.State != "ready" || catalog.CurrentGenerationID == "" {
		// A revoked or unpublished source never lists; the page stays silent.
		return queryapi.SourcePage{}, nil
	}
	facts, err := adapter.surface.CatalogGenerationFacts(ctx, catalog.CurrentGenerationID)
	if err != nil {
		return queryapi.SourcePage{}, mapCatalogError(ctx, err)
	}
	mapped := sourceFactsFromCatalog(catalog)
	current := generationFactsFromCatalog(facts)
	mapped.Current = &current
	return queryapi.SourcePage{Sources: []queryapi.SourceFacts{mapped}}, nil
}

func (adapter queryCatalogAdapter) Facts(
	ctx context.Context,
	principal queryapi.Principal,
	sourceID, generationID string,
) (queryapi.GenerationFacts, error) {
	if sourceID != adapter.source {
		return queryapi.GenerationFacts{}, queryapi.ErrUnknownScope
	}
	if err := adapter.authorizeQuery(ctx, principal); err != nil {
		return queryapi.GenerationFacts{}, err
	}
	facts, err := adapter.surface.CatalogGenerationFacts(ctx, generationID)
	if err != nil {
		return queryapi.GenerationFacts{}, mapCatalogError(ctx, err)
	}
	return generationFactsFromCatalog(facts), nil
}

func (adapter queryCatalogAdapter) Reference(
	ctx context.Context,
	principal queryapi.Principal,
	sourceID string,
) (queryapi.SourceFacts, error) {
	if sourceID != adapter.source {
		return queryapi.SourceFacts{}, queryapi.ErrUnknownScope
	}
	if err := adapter.authorizeQuery(ctx, principal); err != nil {
		return queryapi.SourceFacts{}, err
	}
	catalog, err := adapter.surface.CatalogSource(ctx)
	if err != nil {
		return queryapi.SourceFacts{}, mapCatalogError(ctx, err)
	}
	if catalog.State != "ready" || catalog.CurrentGenerationID == "" {
		return queryapi.SourceFacts{}, queryapi.ErrUnknownScope
	}
	mapped := sourceFactsFromCatalog(catalog)
	facts, err := adapter.surface.CatalogGenerationFacts(ctx, catalog.CurrentGenerationID)
	if err != nil {
		return queryapi.SourceFacts{}, mapCatalogError(ctx, err)
	}
	current := generationFactsFromCatalog(facts)
	mapped.Current = &current
	return mapped, nil
}

// authorizeQuery evaluates the current query relationship before any catalog
// read. Denial and backend failure share the one non-disclosing scope error.
func (adapter queryCatalogAdapter) authorizeQuery(ctx context.Context, principal queryapi.Principal) error {
	decision, err := adapter.broker.AuthorizeSource(ctx, brokerIdentity(principal), "query", broker.Identifier(adapter.brain))
	if err != nil || !decision.Allowed {
		return queryapi.ErrUnknownScope
	}
	return nil
}

func sourceFactsFromCatalog(catalog brain.QueryCatalogSource) queryapi.SourceFacts {
	return queryapi.SourceFacts{
		SourceID:     catalog.SourceID,
		RepositoryID: catalog.RepositoryID,
		BrainID:      catalog.BrainID,
		State:        catalog.State,
	}
}

func generationFactsFromCatalog(facts brain.QueryGenerationFacts) queryapi.GenerationFacts {
	mapped := queryapi.GenerationFacts{
		GenerationID:    facts.GenerationID,
		Sequence:        facts.Sequence,
		SnapshotID:      facts.SnapshotID,
		CommitOID:       facts.CommitOID,
		TreeOID:         facts.TreeOID,
		PolicyDigest:    facts.PolicyDigest,
		State:           facts.State,
		SourceWatermark: facts.SourceWatermark,
	}
	for _, lane := range facts.Readiness {
		mapped.Readiness = append(mapped.Readiness, queryapi.LaneFacts{
			Language: lane.Language, Coverage: lane.Coverage, ReasonCode: lane.ReasonCode,
		})
	}
	return mapped
}

func mapCatalogError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return queryapi.ErrUnknownScope
}

// queryAuthorizerAdapter evaluates the queryapi authorization checkpoints
// against the current broker relationship. The resource names the configured
// source for Ask and GetStatus and the fixed conversation lane for history
// hydration; any other resource denies.
type queryAuthorizerAdapter struct {
	broker *broker.Broker
	brain  brain.Identifier
	source string
}

func (adapter queryAuthorizerAdapter) Authorize(
	ctx context.Context,
	principal queryapi.Principal,
	action queryapi.Action,
	resource string,
) (queryapi.Decision, error) {
	if adapter.broker == nil || (resource != adapter.source && !(action == queryapi.ActionHydrate && resource == "conversation")) {
		return queryapi.Decision{}, errRequestDenied
	}
	decision, err := adapter.broker.AuthorizeSource(ctx, brokerIdentity(principal), string(action), broker.Identifier(adapter.brain))
	if err != nil {
		return queryapi.Decision{Epoch: decision.RevocationEpoch}, errRequestDenied
	}
	return queryapi.Decision{Allowed: decision.Allowed, Epoch: decision.RevocationEpoch}, nil
}

func brokerIdentity(principal queryapi.Principal) broker.Identity {
	return broker.Identity{
		Principal: shared.Identifier{Namespace: "principal", Value: principal.PrincipalID},
		Tenant:    shared.Identifier{Namespace: "tenant", Value: principal.Tenant},
		Session:   shared.Identifier{Namespace: "session", Value: principal.Session},
	}
}

// queryClock supplies receipt and observation time for the query surface.
type queryClock struct{}

func (queryClock) Now() time.Time { return time.Now().UTC() }

var _ gateway.QueryAuthority = queryAuthorityAdapter{}

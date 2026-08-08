package queryapi

import (
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// deniedCode is the single static public error code every unknown,
// unauthorized, or revoked read shares; it discloses no existence detail.
const deniedCode = "not_found_or_denied"

// Identifier namespaces mirror the Stage 03 ingestion adapter's conventions.
const (
	namespaceQuery      = "query"
	namespaceTurn       = "turn"
	namespaceClaim      = "claim"
	namespaceEvidence   = "evidence"
	namespaceRevision   = "revision"
	namespaceSource     = "source"
	namespaceRepository = "repository"
	namespaceBrain      = "brain"
	namespaceGeneration = "generation"
	namespaceSnapshot   = "snapshot"
	namespaceReceipt    = "receipt"
	namespaceOperation  = "operation"
)

func mapAnswer(answer EngineAnswer) *contractsv1.QueryAnswer {
	if answer.FactualConsistency.Status == "" {
		if answer.Status == "abstained" {
			answer.FactualConsistency.Status = "abstained"
			answer.FactualConsistency.Reason = "answer_abstained"
		} else {
			answer.FactualConsistency.Status = "unknown"
			answer.FactualConsistency.Reason = "scorer_unavailable"
			answer.FactualConsistency.TotalClaimCount = uint32(len(answer.Claims))
		}
	}
	mapped := &contractsv1.QueryAnswer{
		QueryId:            &contractsv1.Identifier{Namespace: namespaceQuery, Value: answer.QueryID},
		Status:             mapAnswerStatus(answer.Status),
		Prose:              answer.Prose,
		DegradedReasons:    answer.DegradedReasons,
		TokenUsage:         answer.TokenUsage,
		FactualConsistency: mapFactualConsistency(answer.FactualConsistency),
	}
	for _, claim := range answer.Claims {
		mapped.Claims = append(mapped.Claims, mapClaim(claim))
	}
	return mapped
}

func mapFactualConsistency(result EngineFactualConsistency) *contractsv1.FactualConsistencyScore {
	mapped := &contractsv1.FactualConsistencyScore{
		Status:              mapFactualConsistencyStatus(result.Status),
		ScorePerMille:       result.ScorePerMille,
		Reason:              mapFactualConsistencyReason(result.Reason),
		EvaluatedClaimCount: result.EvaluatedClaimCount,
		TotalClaimCount:     result.TotalClaimCount,
	}
	if result.Provenance != nil {
		mapped.Provenance = &contractsv1.FactualConsistencyProvenance{
			ScorerId: result.Provenance.ScorerID, ScorerVersion: result.Provenance.ScorerVersion,
			CalibrationId:     result.Provenance.CalibrationID,
			CalibrationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: result.Provenance.CalibrationDigest},
		}
	}
	return mapped
}

func mapFactualConsistencyStatus(status string) contractsv1.FactualConsistencyStatus {
	switch status {
	case "scored":
		return contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_SCORED
	case "abstained":
		return contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_ABSTAINED
	case "unknown":
		return contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_UNKNOWN
	default:
		return contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_UNSPECIFIED
	}
}

func mapFactualConsistencyReason(reason string) contractsv1.FactualConsistencyReason {
	switch reason {
	case "answer_abstained":
		return contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_ANSWER_ABSTAINED
	case "scorer_unavailable":
		return contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_SCORER_UNAVAILABLE
	case "scorer_failed":
		return contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_SCORER_FAILED
	case "invalid_result":
		return contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_INVALID_RESULT
	case "budget_exceeded":
		return contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_BUDGET_EXCEEDED
	default:
		return contractsv1.FactualConsistencyReason_FACTUAL_CONSISTENCY_REASON_UNSPECIFIED
	}
}

func mapAnswerStatus(status string) contractsv1.AnswerStatus {
	switch status {
	case "answered":
		return contractsv1.AnswerStatus_ANSWER_STATUS_ANSWERED
	case "partial":
		return contractsv1.AnswerStatus_ANSWER_STATUS_PARTIAL
	case "abstained":
		return contractsv1.AnswerStatus_ANSWER_STATUS_ABSTAINED
	default:
		return contractsv1.AnswerStatus_ANSWER_STATUS_UNSPECIFIED
	}
}

func mapClaim(claim EngineClaim) *contractsv1.GroundedAnswerClaim {
	mapped := &contractsv1.GroundedAnswerClaim{
		ClaimId:            &contractsv1.Identifier{Namespace: namespaceClaim, Value: claim.ClaimID},
		Statement:          claim.Statement,
		AuthorityClass:     contractsv1.AuthorityClass_AUTHORITY_CLASS_MODEL_PROPOSAL,
		ConfidencePerMille: claim.ConfidencePerMille,
	}
	for _, citation := range claim.Citations {
		mapped.Citations = append(mapped.Citations, mapCitation(citation))
	}
	return mapped
}

func mapCitation(citation EngineCitation) *contractsv1.GroundedCitation {
	return &contractsv1.GroundedCitation{
		Evidence: &contractsv1.EvidenceRef{
			EvidenceId:       &contractsv1.Identifier{Namespace: namespaceEvidence, Value: citation.EvidenceID},
			SourceRevisionId: &contractsv1.Identifier{Namespace: namespaceRevision, Value: citation.SourceRevisionID},
		},
		Anchor: &contractsv1.EvidenceAnchor_CodeAnchor{
			GitOid: citation.GitOID,
			Range: &contractsv1.SourceRange{
				Path:        citation.Path,
				StartLine:   citation.StartLine,
				StartColumn: citation.StartColumn,
				EndLine:     citation.EndLine,
				EndColumn:   citation.EndColumn,
			},
		},
		SupportingTextDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: citation.SupportingTextDigest},
	}
}

// mapFreshness composes the frozen QueryFreshness: the contract-visible
// generation metadata comes from the source catalog (immutable per generation
// identity), while state, ACL epoch, and observation time come from the
// engine's pinned disclosure.
func mapFreshness(freshness EngineFreshness, facts GenerationFacts) *contractsv1.QueryFreshness {
	return &contractsv1.QueryFreshness{
		Generation: mapGeneration(facts),
		State:      mapFreshnessState(freshness.State),
		AclEpoch:   freshness.ACLEpoch,
		ObservedAt: timestamppb.New(freshness.ObservedAt.UTC()),
	}
}

func mapFreshnessState(state string) contractsv1.FreshnessState {
	switch state {
	case "current":
		return contractsv1.FreshnessState_FRESHNESS_STATE_CURRENT
	case "stale_disclosed":
		return contractsv1.FreshnessState_FRESHNESS_STATE_STALE_DISCLOSED
	case "degraded":
		return contractsv1.FreshnessState_FRESHNESS_STATE_DEGRADED
	default:
		return contractsv1.FreshnessState_FRESHNESS_STATE_UNSPECIFIED
	}
}

func mapGeneration(facts GenerationFacts) *contractsv1.IngestionGeneration {
	generation := &contractsv1.IngestionGeneration{
		GenerationId: &contractsv1.Identifier{Namespace: namespaceGeneration, Value: facts.GenerationID},
		Sequence:     facts.Sequence,
		Snapshot: &contractsv1.GitSnapshot{
			SnapshotId:   &contractsv1.Identifier{Namespace: namespaceSnapshot, Value: facts.SnapshotID},
			CommitOid:    facts.CommitOID,
			TreeOid:      facts.TreeOID,
			PolicyDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: facts.PolicyDigest},
		},
		State:           mapGenerationState(facts.State),
		SourceWatermark: facts.SourceWatermark,
	}
	for _, lane := range facts.Readiness {
		generation.LanguageReadiness = append(generation.LanguageReadiness, &contractsv1.LanguageReadiness{
			Language:   mapCodeLanguage(lane.Language),
			Coverage:   mapCoverageState(lane.Coverage),
			ReasonCode: lane.ReasonCode,
		})
	}
	return generation
}

func mapGenerationState(state string) contractsv1.GenerationState {
	switch state {
	case "ready":
		return contractsv1.GenerationState_GENERATION_STATE_READY
	case "degraded":
		return contractsv1.GenerationState_GENERATION_STATE_DEGRADED
	default:
		return contractsv1.GenerationState_GENERATION_STATE_UNSPECIFIED
	}
}

func mapCodeLanguage(language string) contractsv1.CodeLanguage {
	switch language {
	case "go":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_GO
	case "typescript":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_TYPESCRIPT
	case "python":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_PYTHON
	case "rust":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_RUST
	case "java":
		return contractsv1.CodeLanguage_CODE_LANGUAGE_JAVA
	default:
		return contractsv1.CodeLanguage_CODE_LANGUAGE_UNSPECIFIED
	}
}

func mapCoverageState(coverage string) contractsv1.CoverageState {
	switch coverage {
	case "syntax_aware":
		return contractsv1.CoverageState_COVERAGE_STATE_SYNTAX_AWARE
	case "lexical_degraded":
		return contractsv1.CoverageState_COVERAGE_STATE_LEXICAL_DEGRADED
	default:
		return contractsv1.CoverageState_COVERAGE_STATE_UNSPECIFIED
	}
}

func mapCoverage(coverage EngineCoverage) *contractsv1.QueryCoverage {
	return &contractsv1.QueryCoverage{
		CanonicalRevisionCount: coverage.CanonicalRevisionCount,
		IndexedRevisionCount:   coverage.IndexedRevisionCount,
	}
}

func mapProjectionState(state string) contractsv1.ProjectionState {
	switch state {
	case "ready":
		return contractsv1.ProjectionState_PROJECTION_STATE_READY
	case "rebuilding":
		return contractsv1.ProjectionState_PROJECTION_STATE_REBUILDING
	case "absent":
		return contractsv1.ProjectionState_PROJECTION_STATE_ABSENT
	default:
		return contractsv1.ProjectionState_PROJECTION_STATE_UNSPECIFIED
	}
}

func mapSourceReference(facts SourceFacts, identity shared.MappedIdentityFact) *contractsv1.SourceReference {
	return &contractsv1.SourceReference{
		SourceId:     &contractsv1.Identifier{Namespace: namespaceSource, Value: facts.SourceID},
		RepositoryId: &contractsv1.Identifier{Namespace: namespaceRepository, Value: facts.RepositoryID},
		BrainId:      &contractsv1.Identifier{Namespace: namespaceBrain, Value: facts.BrainID},
		TenantId:     &contractsv1.Identifier{Namespace: identity.Tenant.Namespace, Value: identity.Tenant.Value},
	}
}

// mapSourceState filters defensively at the boundary: only present, defined,
// non-revoked states ever reach a listing, so a catalog defect cannot smuggle
// a revoked or unknown state through.
func mapSourceState(state string) (contractsv1.SourceState, bool) {
	switch state {
	case "admitted":
		return contractsv1.SourceState_SOURCE_STATE_ADMITTED, true
	case "ready":
		return contractsv1.SourceState_SOURCE_STATE_READY, true
	case "reconciling":
		return contractsv1.SourceState_SOURCE_STATE_RECONCILING, true
	default:
		return contractsv1.SourceState_SOURCE_STATE_UNSPECIFIED, false
	}
}

func mapTurn(turn HistoryTurn, identity shared.MappedIdentityFact) *contractsv1.ConversationTurn {
	mapped := &contractsv1.ConversationTurn{
		TurnId:     &contractsv1.Identifier{Namespace: namespaceTurn, Value: turn.TurnID},
		SessionId:  &contractsv1.Identifier{Namespace: identity.Session.Namespace, Value: turn.SessionID},
		Sequence:   turn.Sequence,
		Role:       mapRole(turn.Role),
		Status:     mapTurnStatus(turn.Status),
		OccurredAt: timestamppb.New(time.UnixMilli(turn.OccurredAtMs).UTC()),
		Text:       turn.Text,
	}
	if turn.Answer != nil {
		mapped.Answer = mapAnswer(*turn.Answer)
	}
	return mapped
}

func mapRole(role string) contractsv1.ConversationRole {
	switch role {
	case "user":
		return contractsv1.ConversationRole_CONVERSATION_ROLE_USER
	case "assistant":
		return contractsv1.ConversationRole_CONVERSATION_ROLE_ASSISTANT
	default:
		return contractsv1.ConversationRole_CONVERSATION_ROLE_UNSPECIFIED
	}
}

func mapTurnStatus(status string) contractsv1.ConversationTurnStatus {
	switch status {
	case "active":
		return contractsv1.ConversationTurnStatus_CONVERSATION_TURN_STATUS_ACTIVE
	case "failed":
		return contractsv1.ConversationTurnStatus_CONVERSATION_TURN_STATUS_FAILED
	default:
		return contractsv1.ConversationTurnStatus_CONVERSATION_TURN_STATUS_UNSPECIFIED
	}
}

func mapFreshnessRequirement(requirement contractsv1.FreshnessRequirement) string {
	switch requirement {
	case contractsv1.FreshnessRequirement_FRESHNESS_REQUIREMENT_COMPLETE_GENERATION:
		return "complete_generation"
	case contractsv1.FreshnessRequirement_FRESHNESS_REQUIREMENT_ABSTAIN_IF_STALE:
		return "abstain_if_stale"
	default:
		return "best_effort"
	}
}

func sessionCausal(identity shared.MappedIdentityFact) *contractsv1.CausalContext {
	return &contractsv1.CausalContext{
		CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: identity.Session.Value},
		CausationId:   &contractsv1.Identifier{Namespace: "session", Value: identity.Session.Value},
		TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: identity.Session.Value},
	}
}

func staticPublicError() *contractsv1.PublicError {
	return &contractsv1.PublicError{Code: deniedCode}
}

// isLowerHexSHA256 reports whether value is a canonical SHA-256 hex string:
// exactly 64 lowercase hexadecimal characters, as the frozen Digest shapes
// require.
func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

package conversation

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/query"
)

// storedPayload is the canonical serialized turn body kept in the encrypted
// vault. Version pins the shape for forward-only schema evolution; Text is set
// only for user turns and Result only for active assistant turns, so a failed
// assistant turn serializes as the bare versioned envelope. Field order is
// declaration order, keeping marshaling deterministic for replay comparison.
type storedPayload struct {
	Version int           `json:"version"`
	Text    string        `json:"text,omitempty"`
	Result  *storedResult `json:"result,omitempty"`
}

// storedResult preserves the complete grounded outcome of one assistant
// completion so an idempotent replay reconstructs the original Ask success —
// answer, freshness, coverage, and projection — exactly as first served.
type storedResult struct {
	Answer    storedAnswer    `json:"answer"`
	Freshness storedFreshness `json:"freshness"`
	Coverage  storedCoverage  `json:"coverage"`
	// Projection preserves the engine's projection disclosure so replay
	// reconstructs the complete result. Payloads written before projection
	// persistence decode with an empty state; an absent projection is a
	// coverage fact, never deletion evidence, so replaying it as unknown is
	// the honest reconstruction for those rows.
	Projection string `json:"projection,omitempty"`
}

type storedAnswer struct {
	QueryID            string                   `json:"query_id"`
	Status             string                   `json:"status"`
	Prose              string                   `json:"prose,omitempty"`
	Claims             []storedClaim            `json:"claims,omitempty"`
	DegradedReasons    []string                 `json:"degraded_reasons,omitempty"`
	TokenUsage         uint64                   `json:"token_usage"`
	FactualConsistency storedFactualConsistency `json:"factual_consistency,omitempty"`
}

type storedFactualConsistency struct {
	Status              string                              `json:"status"`
	ScorePerMille       uint32                              `json:"score_per_mille,omitempty"`
	Reason              string                              `json:"reason,omitempty"`
	Provenance          *storedFactualConsistencyProvenance `json:"provenance,omitempty"`
	EvaluatedClaimCount uint32                              `json:"evaluated_claim_count,omitempty"`
	TotalClaimCount     uint32                              `json:"total_claim_count,omitempty"`
}

type storedFactualConsistencyProvenance struct {
	ScorerID          string `json:"scorer_id"`
	ScorerVersion     string `json:"scorer_version"`
	CalibrationID     string `json:"calibration_id"`
	CalibrationDigest string `json:"calibration_digest"`
}

type storedClaim struct {
	ClaimID            string           `json:"claim_id"`
	Statement          string           `json:"statement"`
	Citations          []storedCitation `json:"citations"`
	ConfidencePerMille uint32           `json:"confidence_per_mille"`
}

type storedCitation struct {
	EvidenceID           string `json:"evidence_id"`
	SourceRevisionID     string `json:"source_revision_id"`
	GitOID               string `json:"git_oid"`
	Path                 string `json:"path"`
	StartLine            uint32 `json:"start_line"`
	StartColumn          uint32 `json:"start_column"`
	EndLine              uint32 `json:"end_line"`
	EndColumn            uint32 `json:"end_column"`
	SupportingTextDigest string `json:"supporting_text_digest"`
}

type storedFreshness struct {
	GenerationID    string `json:"generation_id"`
	Sequence        uint64 `json:"sequence"`
	CommitOID       string `json:"commit_oid"`
	TreeOID         string `json:"tree_oid"`
	GenerationState string `json:"generation_state"`
	State           string `json:"state"`
	ACLEpoch        uint64 `json:"acl_epoch"`
	ObservedAtMs    int64  `json:"observed_at_ms"`
}

type storedCoverage struct {
	CanonicalRevisionCount uint64 `json:"canonical_revision_count"`
	IndexedRevisionCount   uint64 `json:"indexed_revision_count"`
}

const payloadVersion = 2

func marshalUserPayload(text string) ([]byte, error) {
	return marshalPayload(storedPayload{Version: payloadVersion, Text: text})
}

func marshalCompletionPayload(result *query.Result) ([]byte, error) {
	payload := storedPayload{Version: payloadVersion}
	if result != nil {
		payload.Result = storeResult(result)
	}
	return marshalPayload(payload)
}

func marshalPayload(payload storedPayload) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal turn payload", ErrInvalidInput)
	}
	if len(encoded) == 0 || len(encoded) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: turn payload bytes", ErrInvalidInput)
	}
	return encoded, nil
}

func unmarshalPayload(encoded []byte) (storedPayload, error) {
	var payload storedPayload
	if len(encoded) == 0 || len(encoded) > MaxPayloadBytes {
		return storedPayload{}, ErrPayloadUnavailable
	}
	if err := json.Unmarshal(encoded, &payload); err != nil || payload.Version < 1 || payload.Version > payloadVersion {
		return storedPayload{}, ErrPayloadUnavailable
	}
	return payload, nil
}

func storeResult(result *query.Result) *storedResult {
	stored := &storedResult{
		Answer: storedAnswer{
			QueryID:            result.Answer.QueryID,
			Status:             string(result.Answer.Status),
			Prose:              result.Answer.Prose,
			DegradedReasons:    make([]string, 0, len(result.Answer.DegradedReasons)),
			TokenUsage:         result.Answer.TokenUsage,
			FactualConsistency: storeFactualConsistency(result.Answer.FactualConsistency),
		},
		Freshness: storedFreshness{
			GenerationID:    result.Freshness.GenerationID,
			Sequence:        result.Freshness.Sequence,
			CommitOID:       result.Freshness.CommitOID,
			TreeOID:         result.Freshness.TreeOID,
			GenerationState: string(result.Freshness.GenerationState),
			State:           string(result.Freshness.State),
			ACLEpoch:        result.Freshness.ACLEpoch,
			ObservedAtMs:    result.Freshness.ObservedAt.UTC().UnixMilli(),
		},
		Coverage: storedCoverage{
			CanonicalRevisionCount: result.Coverage.CanonicalRevisionCount,
			IndexedRevisionCount:   result.Coverage.IndexedRevisionCount,
		},
		Projection: string(result.Projection),
	}
	for _, reason := range result.Answer.DegradedReasons {
		stored.Answer.DegradedReasons = append(stored.Answer.DegradedReasons, string(reason))
	}
	for _, claim := range result.Answer.Claims {
		storedClaim := storedClaim{
			ClaimID:            claim.ClaimID,
			Statement:          claim.Statement,
			ConfidencePerMille: claim.ConfidencePerMille,
		}
		for _, citation := range claim.Citations {
			storedClaim.Citations = append(storedClaim.Citations, storedCitation{
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

func domainResult(stored *storedResult) *query.Result {
	result := &query.Result{
		Freshness: query.Freshness{
			GenerationID:    stored.Freshness.GenerationID,
			Sequence:        stored.Freshness.Sequence,
			CommitOID:       stored.Freshness.CommitOID,
			TreeOID:         stored.Freshness.TreeOID,
			GenerationState: query.GenerationState(stored.Freshness.GenerationState),
			State:           query.FreshnessState(stored.Freshness.State),
			ACLEpoch:        stored.Freshness.ACLEpoch,
			ObservedAt:      time.UnixMilli(stored.Freshness.ObservedAtMs).UTC(),
		},
		Coverage: query.Coverage{
			CanonicalRevisionCount: stored.Coverage.CanonicalRevisionCount,
			IndexedRevisionCount:   stored.Coverage.IndexedRevisionCount,
		},
		Projection: query.ProjectionState(stored.Projection),
	}
	result.Answer = *domainAnswer(&stored.Answer)
	return result
}

func domainAnswer(stored *storedAnswer) *query.Answer {
	answer := &query.Answer{
		QueryID:            stored.QueryID,
		Status:             query.Status(stored.Status),
		Prose:              stored.Prose,
		TokenUsage:         stored.TokenUsage,
		FactualConsistency: domainFactualConsistency(stored.FactualConsistency, stored.Status, len(stored.Claims)),
	}
	for _, reason := range stored.DegradedReasons {
		answer.DegradedReasons = append(answer.DegradedReasons, query.Reason(reason))
	}
	for _, storedClaim := range stored.Claims {
		claim := query.Claim{
			ClaimID:            storedClaim.ClaimID,
			Statement:          storedClaim.Statement,
			ConfidencePerMille: storedClaim.ConfidencePerMille,
		}
		for _, storedCitation := range storedClaim.Citations {
			claim.Citations = append(claim.Citations, query.Citation{
				EvidenceID:           storedCitation.EvidenceID,
				SourceRevisionID:     storedCitation.SourceRevisionID,
				GitOID:               storedCitation.GitOID,
				Path:                 storedCitation.Path,
				StartLine:            storedCitation.StartLine,
				StartColumn:          storedCitation.StartColumn,
				EndLine:              storedCitation.EndLine,
				EndColumn:            storedCitation.EndColumn,
				SupportingTextDigest: storedCitation.SupportingTextDigest,
			})
		}
		answer.Claims = append(answer.Claims, claim)
	}
	return answer
}

func storeFactualConsistency(result factualconsistency.Result) storedFactualConsistency {
	stored := storedFactualConsistency{
		Status: string(result.Status), ScorePerMille: result.ScorePerMille, Reason: string(result.Reason),
		EvaluatedClaimCount: result.EvaluatedClaimCount, TotalClaimCount: result.TotalClaimCount,
	}
	if result.Provenance != nil {
		stored.Provenance = &storedFactualConsistencyProvenance{
			ScorerID: result.Provenance.ScorerID, ScorerVersion: result.Provenance.ScorerVersion,
			CalibrationID: result.Provenance.CalibrationID, CalibrationDigest: result.Provenance.CalibrationDigest,
		}
	}
	return stored
}

func domainFactualConsistency(stored storedFactualConsistency, answerStatus string, claimCount int) factualconsistency.Result {
	if stored.Status == "" {
		if answerStatus == string(query.StatusAbstained) {
			return factualconsistency.Abstained()
		}
		return factualconsistency.Unknown(factualconsistency.ReasonScorerUnavailable, claimCount)
	}
	result := factualconsistency.Result{
		Status: factualconsistency.Status(stored.Status), ScorePerMille: stored.ScorePerMille,
		Reason: factualconsistency.Reason(stored.Reason), EvaluatedClaimCount: stored.EvaluatedClaimCount,
		TotalClaimCount: stored.TotalClaimCount,
	}
	if stored.Provenance != nil {
		result.Provenance = &factualconsistency.Provenance{
			ScorerID: stored.Provenance.ScorerID, ScorerVersion: stored.Provenance.ScorerVersion,
			CalibrationID: stored.Provenance.CalibrationID, CalibrationDigest: stored.Provenance.CalibrationDigest,
		}
	}
	return result
}

// payloadDigest is the canonical SHA-256 the migration persists beside the
// vault artifact identity; hydration reverifies it before any payload use.
func payloadDigest(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// requestDigest binds one idempotency key to the exact admitted request
// payload — tenant, principal, source, generation, freshness, and query text.
// The session is deliberately excluded so a reconnect may replay an exact
// retry; the key itself is the lookup identity and never needs binding.
func requestDigest(admission Admission) string {
	return canonicalDigest("ouroboros-conversation-query-v1",
		admission.Principal.Tenant, admission.Principal.Principal, admission.SourceID,
		admission.GenerationID, string(admission.Freshness), admission.Text)
}

// queryID derives the server-authored admitted query identity deterministically
// from the idempotency scope, so an exact replay echoes the original identity
// without the schema storing it.
func queryID(tenant, principal, idempotencyKey string) string {
	return "query-" + canonicalDigest("ouroboros-conversation-query-id-v1", tenant, principal, idempotencyKey)[:32]
}

// userTurnID derives the admitted user turn identity; the dense per-session
// sequence makes it unique per principal.
func userTurnID(tenant, principal, session string, sequence uint64) string {
	return "turn-" + canonicalDigest("ouroboros-conversation-user-turn-v1",
		tenant, principal, session, strconv.FormatUint(sequence, 10))[:32]
}

// assistantTurnID derives the completion turn identity; the idempotency key is
// unique per principal, so the completion identity is too.
func assistantTurnID(tenant, principal, idempotencyKey string) string {
	return "turn-" + canonicalDigest("ouroboros-conversation-assistant-turn-v1", tenant, principal, idempotencyKey)[:32]
}

func canonicalDigest(domain string, fields ...string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(domain))
	for _, field := range fields {
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(field))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// encodeCursor renders the opaque history continuation: the frozen
// (occurred_at_ms, turn_id) position of the last returned turn, versioned and
// base64url-wrapped so callers treat it as an atom.
func encodeCursor(occurredAtMs int64, turnID string) string {
	raw := strconv.FormatInt(occurredAtMs, 10) + "/" + turnID
	return "v1." + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses an opaque continuation produced by encodeCursor. Any
// other shape — wrong version, bad encoding, missing separator, empty
// identity — is rejected so a forged cursor cannot widen the history scope.
func decodeCursor(cursor string) (int64, string, error) {
	if cursor == "" {
		return 0, "", nil
	}
	if !strings.HasPrefix(cursor, "v1.") {
		return 0, "", ErrInvalidInput
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cursor, "v1."))
	if err != nil {
		return 0, "", ErrInvalidInput
	}
	milliseconds, turnID, found := strings.Cut(string(raw), "/")
	if !found || turnID == "" {
		return 0, "", ErrInvalidInput
	}
	occurredAtMs, err := strconv.ParseInt(milliseconds, 10, 64)
	if err != nil || occurredAtMs < 0 {
		return 0, "", ErrInvalidInput
	}
	return occurredAtMs, turnID, nil
}

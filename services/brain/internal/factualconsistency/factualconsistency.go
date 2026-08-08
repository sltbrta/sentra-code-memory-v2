// Package factualconsistency defines the bounded, fail-closed scoring seam for
// grounded answers. It does not select or authorize evidence.
package factualconsistency

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"
)

// Status distinguishes a calibrated score from explicit non-score outcomes.
type Status string

const (
	StatusScored    Status = "scored"
	StatusAbstained Status = "abstained"
	StatusUnknown   Status = "unknown"
)

// Reason is the bounded non-sensitive explanation for a non-score.
type Reason string

const (
	ReasonAnswerAbstained   Reason = "answer_abstained"
	ReasonScorerUnavailable Reason = "scorer_unavailable"
	ReasonScorerFailed      Reason = "scorer_failed"
	ReasonInvalidResult     Reason = "invalid_result"
	ReasonBudgetExceeded    Reason = "budget_exceeded"
)

// Provenance pins the scoring implementation and immutable calibration bytes.
// None of these fields identify answer evidence or an authenticated caller.
type Provenance struct {
	ScorerID          string `json:"scorer_id"`
	ScorerVersion     string `json:"scorer_version"`
	CalibrationID     string `json:"calibration_id"`
	CalibrationDigest string `json:"calibration_digest"`
}

// Result is the answer-level disclosure returned to grounded-answer contracts.
// ScorePerMille is meaningful only when Status is StatusScored.
type Result struct {
	Status              Status      `json:"status"`
	ScorePerMille       uint32      `json:"score_per_mille,omitempty"`
	Reason              Reason      `json:"reason,omitempty"`
	Provenance          *Provenance `json:"provenance,omitempty"`
	EvaluatedClaimCount uint32      `json:"evaluated_claim_count"`
	TotalClaimCount     uint32      `json:"total_claim_count"`
}

// Claim contains one already-authorized, citation-verified assertion and the
// exact supporting spans resolved from its citations. Scorers receive no ACL,
// tenant, source, revision, or caller identifiers.
type Claim struct {
	Statement string
	Supports  []string
}

// Request is the complete bounded claim set for one non-abstained answer.
type Request struct {
	Claims []Claim
}

// Limits bounds both data presented to a scorer and its cooperative deadline.
type Limits struct {
	MaxClaims           int
	MaxSupportsPerClaim int
	MaxStatementBytes   int
	MaxSupportBytes     int
	MaxTotalBytes       int
	Timeout             time.Duration
}

// DefaultLimits align with the public grounded-query claim, citation, and
// evidence-pack ceilings. Callers may tighten but never widen them.
func DefaultLimits() Limits {
	return Limits{
		MaxClaims:           64,
		MaxSupportsPerClaim: 16,
		MaxStatementBytes:   4096,
		MaxSupportBytes:     4096,
		MaxTotalBytes:       64 * 1024,
		Timeout:             50 * time.Millisecond,
	}
}

// Scorer returns a fully calibrated result for every supplied claim. It must
// honor context cancellation. Errors and malformed results become UNKNOWN;
// they never change, remove, or add answer claims and citations.
type Scorer interface {
	Score(context.Context, Request) (Result, error)
}

// Abstained returns the only valid score shape for an answer with no claims.
func Abstained() Result {
	return Result{Status: StatusAbstained, Reason: ReasonAnswerAbstained}
}

// Unknown returns a non-numeric result with a bounded public reason.
func Unknown(reason Reason, totalClaims int) Result {
	if reason != ReasonScorerUnavailable && reason != ReasonScorerFailed &&
		reason != ReasonInvalidResult && reason != ReasonBudgetExceeded {
		reason = ReasonInvalidResult
	}
	if totalClaims < 0 || totalClaims > 64 {
		totalClaims = 0
	}
	return Result{Status: StatusUnknown, Reason: reason, TotalClaimCount: uint32(totalClaims)}
}

// Evaluate invokes a scorer only after enforcing the hard input ceilings. A
// missing, failed, late, or malformed scorer yields an explicit UNKNOWN result.
// Caller cancellation is returned so the owning answer operation can preserve
// its cancellation semantics.
func Evaluate(ctx context.Context, scorer Scorer, request Request, limits Limits) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	total := len(request.Claims)
	if !validLimits(limits) || !withinBudget(request, limits) {
		return Unknown(ReasonBudgetExceeded, total), nil
	}
	if total == 0 {
		return Unknown(ReasonInvalidResult, 0), nil
	}
	if scorer == nil {
		return Unknown(ReasonScorerUnavailable, total), nil
	}
	scoreCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	result, err := callScorer(scoreCtx, scorer, request)
	if contextErr := ctx.Err(); contextErr != nil {
		return Result{}, contextErr
	}
	if err != nil || scoreCtx.Err() != nil {
		return Unknown(ReasonScorerFailed, total), nil
	}
	if !validScoredResult(result, total) {
		return Unknown(ReasonInvalidResult, total), nil
	}
	return result, nil
}

func callScorer(ctx context.Context, scorer Scorer, request Request) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = Result{}
			err = fmt.Errorf("factualconsistency: scorer failed")
		}
	}()
	return scorer.Score(ctx, request)
}

func validLimits(limits Limits) bool {
	hard := DefaultLimits()
	return limits.MaxClaims > 0 && limits.MaxClaims <= hard.MaxClaims &&
		limits.MaxSupportsPerClaim > 0 && limits.MaxSupportsPerClaim <= hard.MaxSupportsPerClaim &&
		limits.MaxStatementBytes > 0 && limits.MaxStatementBytes <= hard.MaxStatementBytes &&
		limits.MaxSupportBytes > 0 && limits.MaxSupportBytes <= hard.MaxSupportBytes &&
		limits.MaxTotalBytes > 0 && limits.MaxTotalBytes <= hard.MaxTotalBytes &&
		limits.Timeout > 0 && limits.Timeout <= hard.Timeout
}

func withinBudget(request Request, limits Limits) bool {
	if len(request.Claims) == 0 || len(request.Claims) > limits.MaxClaims {
		return false
	}
	totalBytes := 0
	for _, claim := range request.Claims {
		if len(claim.Statement) == 0 || len(claim.Statement) > limits.MaxStatementBytes ||
			len(claim.Supports) == 0 || len(claim.Supports) > limits.MaxSupportsPerClaim {
			return false
		}
		totalBytes += len(claim.Statement)
		for _, support := range claim.Supports {
			if len(support) == 0 || len(support) > limits.MaxSupportBytes {
				return false
			}
			totalBytes += len(support)
			if totalBytes > limits.MaxTotalBytes {
				return false
			}
		}
	}
	return true
}

func validScoredResult(result Result, total int) bool {
	return result.Status == StatusScored && result.ScorePerMille <= 1000 &&
		result.Reason == "" && result.Provenance != nil &&
		result.EvaluatedClaimCount == uint32(total) && result.TotalClaimCount == uint32(total) &&
		validProvenance(*result.Provenance)
}

func validProvenance(provenance Provenance) bool {
	if provenance.ScorerID == "" || len(provenance.ScorerID) > 128 ||
		provenance.ScorerVersion == "" || len(provenance.ScorerVersion) > 64 ||
		provenance.CalibrationID == "" || len(provenance.CalibrationID) > 128 ||
		len(provenance.CalibrationDigest) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(provenance.CalibrationDigest)
	return err == nil && len(decoded) == 32 && provenance.CalibrationDigest == stringLowerHex(decoded)
}

func stringLowerHex(value []byte) string { return hex.EncodeToString(value) }

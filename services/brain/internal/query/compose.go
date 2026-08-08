package query

import (
	"fmt"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

// synthesisVerdict is the outcome of whole-response verification: the
// surviving claims and the fault classes observed. Citation faults remove
// only their own claim, exactly as SPEC-DELTA-001 freezes; structural faults
// are provider misbehavior outside any single claim and discard everything.
type synthesisVerdict struct {
	claims         []Claim
	citationFault  bool
	structureFault bool
	scoreClaims    []factualconsistency.Claim
}

// verifySynthesis re-verifies every proposal against the frozen bounds and
// the canonical pack before emission. A claim whose citation anchor, digest,
// or support fails verification is removed on its own; surviving claims are
// emitted, and only when no claim survives does the result abstain with
// citation_verification_failed. Structural violations — over-bound prose or
// claim sets, malformed statements, out-of-range confidence — mark the whole
// response unusable. Prose never carries a material span without a supported
// claim: when claims are removed, the engine regenerates prose from the
// surviving statements.
func verifySynthesis(synthesis Synthesis, entries []EvidenceEntry, snapshot Snapshot, limits SynthesisLimits) synthesisVerdict {
	if len(synthesis.Claims) == 0 {
		return synthesisVerdict{}
	}
	if synthesis.Prose == "" || len(synthesis.Prose) > limits.MaxProseBytes ||
		len(synthesis.Claims) > limits.MaxClaims {
		return synthesisVerdict{structureFault: true}
	}
	verdict := synthesisVerdict{claims: make([]Claim, 0, len(synthesis.Claims))}
	for _, proposed := range synthesis.Claims {
		if proposed.Statement == "" || len(proposed.Statement) > limits.MaxStatementBytes ||
			len(proposed.Citations) == 0 || len(proposed.Citations) > limits.MaxCitationsPerClaim ||
			proposed.ConfidencePerMille > 1000 {
			return synthesisVerdict{structureFault: true}
		}
		claim := Claim{
			Statement:          proposed.Statement,
			ConfidencePerMille: proposed.ConfidencePerMille,
		}
		claimFailed := false
		scoreClaim := factualconsistency.Claim{Statement: proposed.Statement}
		for _, citation := range proposed.Citations {
			verified, support, err := verifyCitation(citation, entries, snapshot)
			if err != nil {
				claimFailed = true
				break
			}
			claim.Citations = append(claim.Citations, verified)
			scoreClaim.Supports = append(scoreClaim.Supports, string(support))
		}
		if claimFailed {
			verdict.citationFault = true
			continue
		}
		verdict.claims = append(verdict.claims, claim)
		verdict.scoreClaims = append(verdict.scoreClaims, scoreClaim)
	}
	for index := range verdict.claims {
		verdict.claims[index].ClaimID = fmt.Sprintf("claim-%04d", index+1)
	}
	return verdict
}

// regenerateProse rebuilds the rendered answer from surviving claim
// statements after per-claim removal, the downgrade the implementation
// packet sanctions when prose would otherwise reference a removed claim. An
// over-bound regeneration fails the whole synthesis closed.
func regenerateProse(claims []Claim, maxProseBytes int) (string, error) {
	statements := make([]string, 0, len(claims))
	for _, claim := range claims {
		statements = append(statements, claim.Statement)
	}
	prose := strings.Join(statements, " ")
	if len(prose) > maxProseBytes {
		return "", fmt.Errorf("%w: regenerated prose exceeds the prose bound", ErrSynthesisFailed)
	}
	return prose, nil
}

// verifyCitation resolves one proposed citation against its pack entry,
// recomputes the supporting-text digest from canonical bytes, and binds the
// pinned commit and revision facts.
func verifyCitation(proposed ProposedCitation, entries []EvidenceEntry, snapshot Snapshot) (Citation, string, error) {
	if proposed.EvidenceIndex < 0 || proposed.EvidenceIndex >= len(entries) {
		return Citation{}, "", fmt.Errorf("%w: evidence index %d", errIntegrity, proposed.EvidenceIndex)
	}
	entry := entries[proposed.EvidenceIndex]
	support, digest, err := resolveSupportingText(entry, proposed)
	if err != nil {
		return Citation{}, "", err
	}
	return Citation{
		EvidenceID:           evidenceID(entry, proposed),
		SourceRevisionID:     entry.RevisionID,
		GitOID:               snapshot.CommitOID,
		Path:                 entry.Path,
		StartLine:            proposed.StartLine,
		StartColumn:          proposed.StartColumn,
		EndLine:              proposed.EndLine,
		EndColumn:            proposed.EndColumn,
		SupportingTextDigest: digest,
	}, string(support), nil
}

// canonicalReasons deduplicates, orders by the frozen vocabulary, and bounds
// the degraded-reason set.
func canonicalReasons(reasons []Reason, maxReasons int) []Reason {
	present := make(map[Reason]bool, len(reasons))
	for _, reason := range reasons {
		present[reason] = true
	}
	ordered := make([]Reason, 0, len(present))
	for _, reason := range reasonOrder {
		if present[reason] && len(ordered) < maxReasons {
			ordered = append(ordered, reason)
		}
	}
	return ordered
}

// composeAnswer assembles the final answer under the frozen status
// consistency rule: answered requires claims and zero reasons, partial
// requires claims and reasons, and abstention carries neither prose nor
// claims.
func composeAnswer(queryID string, claims []Claim, prose string, tokenUsage uint64, reasons []Reason, consistency factualconsistency.Result, maxReasons int) Answer {
	reasons = canonicalReasons(reasons, maxReasons)
	answer := Answer{
		QueryID:            queryID,
		DegradedReasons:    reasons,
		TokenUsage:         tokenUsage,
		FactualConsistency: consistency,
	}
	if len(claims) == 0 {
		answer.Status = StatusAbstained
		return answer
	}
	answer.Claims = claims
	answer.Prose = prose
	if len(reasons) == 0 {
		answer.Status = StatusAnswered
	} else {
		answer.Status = StatusPartial
	}
	return answer
}

// filesByPath indexes the pinned projection's files by path.
func filesByPath(snapshot Snapshot) map[string]codeindex.FileProjection {
	files := make(map[string]codeindex.FileProjection)
	if snapshot.Projection.State != ProjectionReady || snapshot.Projection.Index == nil {
		return files
	}
	for _, file := range snapshot.Projection.Index.Files {
		files[file.Path] = file
	}
	return files
}

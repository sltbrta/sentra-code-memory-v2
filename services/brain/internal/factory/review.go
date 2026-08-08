package factory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var reviewerFamilyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// FindingDraft is one typed fresh-review observation before canonical commit.
type FindingDraft struct {
	// Severity is the bounded typed severity.
	Severity contractsv1.ReviewSeverity
	// Category is the bounded typed category.
	Category contractsv1.ReviewCategory
	// Summary is a bounded non-sensitive description, at most 2048 bytes.
	Summary string
	// Evidence supports the finding; dismissal later requires it.
	Evidence []*contractsv1.EvidenceRef
	// ReviewerPrincipal is the authenticated reviewer identity; it must differ
	// from the admitting principal, which initiates every leaf grant.
	ReviewerPrincipal string
	// ReviewerSession identifies the reviewer's authenticated session.
	ReviewerSession string
	// ReviewerFamily records the certified reviewer family metadata.
	ReviewerFamily string
}

// FindingResult returns the canonical finding identity.
type FindingResult struct {
	// FindingID is the server-authored finding identity.
	FindingID string
	// Replayed reports an exact resubmission collapsed to the original finding.
	Replayed bool
}

// RecordFinding commits one typed fresh-review finding in the OPEN
// disposition. The run must be under review; the reviewer identity must be
// disjoint from the admitting principal, which initiates every leaf grant.
// Summary and evidence persist in the encrypted vault; the ledger holds only
// the typed fields, identities, and digest. Findings are replay-safe: an exact
// resubmission collapses to the original finding identity.
func (k *Kernel) RecordFinding(ctx context.Context, authenticated Identity, runID string, draft FindingDraft) (FindingResult, error) {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" {
		return FindingResult{}, ErrInvalidInput
	}
	if err := validFindingDraft(draft); err != nil {
		return FindingResult{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return FindingResult{}, ErrInvalidInput
	}
	run, found, err := lookupRun(ctx, k.db, authenticated, runID)
	if err != nil {
		return FindingResult{}, err
	}
	if !found {
		return FindingResult{}, ErrNotFoundOrDenied
	}
	if !reviewerDisjoint(draft.ReviewerPrincipal, run.principal) {
		return FindingResult{}, ErrReviewerConflict
	}
	state, err := currentRunState(ctx, k.db, authenticated, runID)
	if err != nil {
		return FindingResult{}, err
	}
	if state != contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW {
		return FindingResult{}, ErrNotFoundOrDenied
	}
	payload, err := marshalFindingPayload(draft)
	if err != nil {
		return FindingResult{}, err
	}
	staged, err := k.stagePayload(ctx, authenticated.Tenant, payload)
	if err != nil {
		return FindingResult{}, err
	}
	findingID := identity("ouroboros.stage05.finding.v1", runID, draft.ReviewerPrincipal, staged.digestHex)
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return FindingResult{}, fmt.Errorf("factory: begin finding: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	severityText, err := severityText(draft.Severity)
	if err != nil {
		return FindingResult{}, err
	}
	categoryText, err := categoryText(draft.Category)
	if err != nil {
		return FindingResult{}, err
	}
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT payload_digest FROM factory_findings
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND finding_id=?`,
		authenticated.Tenant, authenticated.Principal, runID, findingID).Scan(&existingDigest)
	if err == nil {
		if existingDigest != staged.digestHex {
			return FindingResult{}, ErrNotFoundOrDenied
		}
		return FindingResult{FindingID: findingID, Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return FindingResult{}, fmt.Errorf("factory: re-read finding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO factory_findings
		(tenant_id,principal_id,run_id,finding_id,severity,category,reviewer_principal_id,reviewer_session_id,
		 reviewer_family,payload_artifact_id,payload_digest,evidence_count,occurred_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, findingID, severityText, categoryText,
		draft.ReviewerPrincipal, draft.ReviewerSession, draft.ReviewerFamily, staged.artifactID, staged.digestHex,
		len(draft.Evidence), k.clock.NowUnixMilli()); err != nil {
		return FindingResult{}, fmt.Errorf("factory: commit finding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FindingResult{}, fmt.Errorf("factory: commit finding: %w", err)
	}
	return FindingResult{FindingID: findingID}, nil
}

// DisposeFinding records the exactly-once typed disposition of one finding.
// Dismissal requires the finding's recorded evidence; every dispositioned
// finding carries a disposition receipt, and no finding is ever dropped
// silently. Repeating the same disposition replays; a differing second
// disposition conflicts without mutation.
func (k *Kernel) DisposeFinding(
	ctx context.Context, authenticated Identity, runID, findingID string, disposition contractsv1.FindingDisposition,
) error {
	if k == nil || ctx == nil || !validIdentity(authenticated) || runID == "" || findingID == "" {
		return ErrInvalidInput
	}
	dispositionText, err := dispositionText(disposition)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.db == nil {
		return ErrInvalidInput
	}
	tx, err := k.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("factory: begin disposition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var evidenceCount int
	err = tx.QueryRowContext(ctx, `SELECT evidence_count FROM factory_findings
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND finding_id=?`,
		authenticated.Tenant, authenticated.Principal, runID, findingID).Scan(&evidenceCount)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFoundOrDenied
	}
	if err != nil {
		return fmt.Errorf("factory: read finding: %w", err)
	}
	if disposition == contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE && evidenceCount == 0 {
		return ErrTransitionInvalid
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT disposition FROM factory_finding_dispositions
		WHERE tenant_id=? AND principal_id=? AND run_id=? AND finding_id=?`,
		authenticated.Tenant, authenticated.Principal, runID, findingID).Scan(&existing)
	if err == nil {
		if existing == dispositionText {
			return nil
		}
		return ErrTransitionInvalid
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("factory: read disposition: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO factory_finding_dispositions
		(tenant_id,principal_id,run_id,finding_id,disposition,receipt_id,receipt_reason_code,dispositioned_at_ms)
		VALUES (?,?,?,?,?,?,?,?)`,
		authenticated.Tenant, authenticated.Principal, runID, findingID, dispositionText,
		identity("ouroboros.stage05.disposition-receipt.v1", runID, findingID),
		dispositionReasonCode(disposition), k.clock.NowUnixMilli()); err != nil {
		return fmt.Errorf("factory: commit disposition: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("factory: commit disposition: %w", err)
	}
	return nil
}

func validFindingDraft(draft FindingDraft) error {
	if _, err := severityText(draft.Severity); err != nil {
		return err
	}
	if _, err := categoryText(draft.Category); err != nil {
		return err
	}
	if draft.Summary == "" || len(draft.Summary) > 2048 {
		return ErrInvalidInput
	}
	if len(draft.Evidence) > 16 {
		return ErrInvalidInput
	}
	for _, evidence := range draft.Evidence {
		if evidence.GetEvidenceId().GetValue() == "" || evidence.GetSourceRevisionId().GetValue() == "" {
			return ErrInvalidInput
		}
	}
	if !validPrincipalID(draft.ReviewerPrincipal) || !validPrincipalID(draft.ReviewerSession) ||
		!reviewerFamilyPattern.MatchString(draft.ReviewerFamily) {
		return ErrInvalidInput
	}
	return nil
}

// findingPayload is the vault-staged finding body: prose and evidence, never
// ledger columns.
type findingPayload struct {
	Summary  string               `json:"summary"`
	Evidence []findingEvidenceRef `json:"evidence"`
}

type findingEvidenceRef struct {
	EvidenceNamespace string `json:"evidence_namespace"`
	EvidenceValue     string `json:"evidence_value"`
	RevisionNamespace string `json:"revision_namespace"`
	RevisionValue     string `json:"revision_value"`
	AnchorAlgorithm   string `json:"anchor_algorithm,omitempty"`
	AnchorDigestHex   string `json:"anchor_digest_hex,omitempty"`
}

func marshalFindingPayload(draft FindingDraft) ([]byte, error) {
	payload := findingPayload{Summary: draft.Summary, Evidence: make([]findingEvidenceRef, 0, len(draft.Evidence))}
	for _, evidence := range draft.Evidence {
		payload.Evidence = append(payload.Evidence, findingEvidenceRef{
			EvidenceNamespace: evidence.GetEvidenceId().GetNamespace(),
			EvidenceValue:     evidence.GetEvidenceId().GetValue(),
			RevisionNamespace: evidence.GetSourceRevisionId().GetNamespace(),
			RevisionValue:     evidence.GetSourceRevisionId().GetValue(),
			AnchorAlgorithm:   evidence.GetAnchorDigest().GetAlgorithm(),
			AnchorDigestHex:   evidence.GetAnchorDigest().GetHex(),
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("factory: marshal finding payload: %w", err)
	}
	return encoded, nil
}

func unmarshalFindingPayload(encoded []byte) (findingPayload, error) {
	payload := findingPayload{}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return findingPayload{}, errors.Join(ErrPayloadUnavailable, err)
	}
	return payload, nil
}

func severityText(severity contractsv1.ReviewSeverity) (string, error) {
	switch severity {
	case contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO:
		return "INFO", nil
	case contractsv1.ReviewSeverity_REVIEW_SEVERITY_MINOR:
		return "MINOR", nil
	case contractsv1.ReviewSeverity_REVIEW_SEVERITY_MAJOR:
		return "MAJOR", nil
	case contractsv1.ReviewSeverity_REVIEW_SEVERITY_BLOCKER:
		return "BLOCKER", nil
	}
	return "", fmt.Errorf("%w: severity %d", ErrInvalidInput, int32(severity))
}

func severityFromText(value string) (contractsv1.ReviewSeverity, error) {
	switch value {
	case "INFO":
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO, nil
	case "MINOR":
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_MINOR, nil
	case "MAJOR":
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_MAJOR, nil
	case "BLOCKER":
		return contractsv1.ReviewSeverity_REVIEW_SEVERITY_BLOCKER, nil
	}
	return contractsv1.ReviewSeverity_REVIEW_SEVERITY_UNSPECIFIED, fmt.Errorf("%w: severity %q", ErrInvalidInput, value)
}

func categoryText(category contractsv1.ReviewCategory) (string, error) {
	switch category {
	case contractsv1.ReviewCategory_REVIEW_CATEGORY_CORRECTNESS:
		return "CORRECTNESS", nil
	case contractsv1.ReviewCategory_REVIEW_CATEGORY_SECURITY:
		return "SECURITY", nil
	case contractsv1.ReviewCategory_REVIEW_CATEGORY_DATA_INTEGRITY:
		return "DATA_INTEGRITY", nil
	case contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS:
		return "DOCS", nil
	case contractsv1.ReviewCategory_REVIEW_CATEGORY_TESTS:
		return "TESTS", nil
	}
	return "", fmt.Errorf("%w: category %d", ErrInvalidInput, int32(category))
}

func categoryFromText(value string) (contractsv1.ReviewCategory, error) {
	switch value {
	case "CORRECTNESS":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_CORRECTNESS, nil
	case "SECURITY":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_SECURITY, nil
	case "DATA_INTEGRITY":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_DATA_INTEGRITY, nil
	case "DOCS":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, nil
	case "TESTS":
		return contractsv1.ReviewCategory_REVIEW_CATEGORY_TESTS, nil
	}
	return contractsv1.ReviewCategory_REVIEW_CATEGORY_UNSPECIFIED, fmt.Errorf("%w: category %q", ErrInvalidInput, value)
}

func dispositionText(disposition contractsv1.FindingDisposition) (string, error) {
	switch disposition {
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_FIXED:
		return "FIXED", nil
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE:
		return "DISMISSED_WITH_EVIDENCE", nil
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_DEFERRED:
		return "DEFERRED", nil
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_BLOCKING:
		return "BLOCKING", nil
	}
	return "", fmt.Errorf("%w: disposition %d", ErrInvalidInput, int32(disposition))
}

func dispositionFromText(value string) (contractsv1.FindingDisposition, error) {
	switch value {
	case "FIXED":
		return contractsv1.FindingDisposition_FINDING_DISPOSITION_FIXED, nil
	case "DISMISSED_WITH_EVIDENCE":
		return contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE, nil
	case "DEFERRED":
		return contractsv1.FindingDisposition_FINDING_DISPOSITION_DEFERRED, nil
	case "BLOCKING":
		return contractsv1.FindingDisposition_FINDING_DISPOSITION_BLOCKING, nil
	}
	return contractsv1.FindingDisposition_FINDING_DISPOSITION_UNSPECIFIED, fmt.Errorf("%w: disposition %q", ErrInvalidInput, value)
}

// dispositionReasonCode derives the stable non-sensitive receipt reason for
// one typed disposition.
func dispositionReasonCode(disposition contractsv1.FindingDisposition) string {
	switch disposition {
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_FIXED:
		return "finding_fixed"
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE:
		return "finding_dismissed_with_evidence"
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_DEFERRED:
		return "finding_deferred"
	case contractsv1.FindingDisposition_FINDING_DISPOSITION_BLOCKING:
		return "finding_blocking"
	}
	return "finding_dispositioned"
}

// dispositionReceipt builds the required receipt one dispositioned finding
// carries; open findings never carry one.
func (k *Kernel) dispositionReceipt(runID, findingID, receiptID, reasonCode string, atMs int64) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: receiptID},
		Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
		ReasonCode:  reasonCode,
		OperationId: &contractsv1.Identifier{Namespace: "factory-finding", Value: findingID},
		Causal: &contractsv1.CausalContext{
			CorrelationId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
			CausationId:   &contractsv1.Identifier{Namespace: "factory-finding", Value: findingID},
			TraceId:       &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
		},
		RecordedAt:          timestamppb.New(unixMillis(atMs)),
		ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: k.policyDigestHex},
	}
}

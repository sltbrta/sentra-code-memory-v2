package factory

import (
	"context"
	"errors"
	"fmt"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

func admitReviewRun(t *testing.T, fixture *testKernel, key string) string {
	t.Helper()
	runID := admitHappy(t, fixture, key)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW)
	return runID
}

func reviewDraft(severity contractsv1.ReviewSeverity, category contractsv1.ReviewCategory, summary string, evidence int) FindingDraft {
	draft := FindingDraft{
		Severity:          severity,
		Category:          category,
		Summary:           summary,
		ReviewerPrincipal: "reviewer-1",
		ReviewerSession:   "review-session-1",
		ReviewerFamily:    "fresh-eyes.v1",
	}
	for index := 0; index < evidence; index++ {
		draft.Evidence = append(draft.Evidence, &contractsv1.EvidenceRef{
			EvidenceId:       &contractsv1.Identifier{Namespace: "evidence", Value: fmt.Sprintf("evidence-%d", index)},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "revision", Value: "revision-1"},
		})
	}
	return draft
}

func TestReviewReducerProducesTypedFindingsWithDispositions(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitReviewRun(t, fixture, "review-happy")

	first, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID,
		reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_MINOR, contractsv1.ReviewCategory_REVIEW_CATEGORY_TESTS, "missing table-driven case", 1))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID,
		reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_MINOR, contractsv1.ReviewCategory_REVIEW_CATEGORY_TESTS, "missing table-driven case", 1))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.FindingID != first.FindingID {
		t.Fatalf("finding replay = %#v, want original %q", replayed, first.FindingID)
	}
	second, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID,
		reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_MAJOR, contractsv1.ReviewCategory_REVIEW_CATEGORY_CORRECTNESS, "wrong fence comparison", 0))
	if err != nil {
		t.Fatal(err)
	}

	if err := fixture.kernel.DisposeFinding(context.Background(), testIdentity(), runID, first.FindingID,
		contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.DisposeFinding(context.Background(), testIdentity(), runID, first.FindingID,
		contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE); err != nil {
		t.Fatal("exact disposition replay must succeed", err)
	}
	if err := fixture.kernel.DisposeFinding(context.Background(), testIdentity(), runID, first.FindingID,
		contractsv1.FindingDisposition_FINDING_DISPOSITION_FIXED); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("differing second disposition error = %v, want ErrTransitionInvalid", err)
	}
	if err := fixture.kernel.DisposeFinding(context.Background(), testIdentity(), runID, second.FindingID,
		contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("evidence-free dismissal error = %v, want ErrTransitionInvalid", err)
	}
	if err := fixture.kernel.DisposeFinding(context.Background(), testIdentity(), runID, second.FindingID,
		contractsv1.FindingDisposition_FINDING_DISPOSITION_BLOCKING); err != nil {
		t.Fatal(err)
	}

	page, err := fixture.kernel.GetReviewFindings(context.Background(), testIdentity(), runID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(page.Findings))
	}
	var dismissed, blocking *contractsv1.ReviewFinding
	for _, finding := range page.Findings {
		switch finding.GetFindingId().GetValue() {
		case first.FindingID:
			dismissed = finding
		case second.FindingID:
			blocking = finding
		}
	}
	if dismissed.GetDisposition() != contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE ||
		dismissed.GetDispositionReceipt().GetStatus() != contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED ||
		len(dismissed.GetEvidence()) != 1 {
		t.Fatalf("dismissed finding = %v", dismissed)
	}
	if blocking.GetDisposition() != contractsv1.FindingDisposition_FINDING_DISPOSITION_BLOCKING ||
		blocking.GetDispositionReceipt() == nil {
		t.Fatalf("blocking finding = %v", blocking)
	}
	if dismissed.GetReviewer().GetPrincipalId().GetValue() != "reviewer-1" || dismissed.GetReviewerFamily() != "fresh-eyes.v1" {
		t.Fatalf("reviewer identity = %v", dismissed.GetReviewer())
	}
}

func TestReviewIdentityMustBeDisjointFromLeafInitiators(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitReviewRun(t, fixture, "review-disjoint")
	draft := reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO, contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, "docs note", 0)
	draft.ReviewerPrincipal = testPrincipal
	if _, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID, draft); !errors.Is(err, ErrReviewerConflict) {
		t.Fatalf("self-review error = %v, want ErrReviewerConflict", err)
	}
}

func TestFindingsRequireReviewStateAndBoundedShape(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "review-state")
	if _, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID,
		reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO, contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, "too early", 0)); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("finding before review state error = %v, want ErrNotFoundOrDenied", err)
	}
	runID = admitReviewRun(t, fixture, "review-shape")
	draft := reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_UNSPECIFIED, contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, "bad severity", 0)
	if _, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID, draft); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unspecified severity error = %v, want ErrInvalidInput", err)
	}
	draft = reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO, contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, "", 0)
	if _, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID, draft); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty summary error = %v, want ErrInvalidInput", err)
	}
}

func TestFindingsPaginateInCanonicalOrder(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitReviewRun(t, fixture, "review-pages")
	ids := make([]string, 0, 5)
	for index := 0; index < 5; index++ {
		fixture.clock.now++
		result, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID,
			reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO, contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, fmt.Sprintf("note %d", index), 0))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result.FindingID)
	}
	seen := make([]string, 0, 5)
	cursor := ""
	for {
		page, err := fixture.kernel.GetReviewFindings(context.Background(), testIdentity(), runID, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range page.Findings {
			seen = append(seen, finding.GetFindingId().GetValue())
			if finding.GetDisposition() != contractsv1.FindingDisposition_FINDING_DISPOSITION_OPEN ||
				finding.GetDispositionReceipt() != nil {
				t.Fatalf("open finding carried a disposition receipt: %v", finding)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paginated findings = %d, want 5", len(seen))
	}
	for index, id := range ids {
		if seen[index] != id {
			t.Fatalf("page order = %v, want canonical insertion order %v", seen, ids)
		}
	}
	if _, err := fixture.kernel.GetReviewFindings(context.Background(), testIdentity(), runID, "not-a-cursor", 2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("malformed cursor error = %v, want ErrInvalidInput", err)
	}
}

func TestFindingsHydrationFailsClosedOnTamperedPayload(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitReviewRun(t, fixture, "review-tamper")
	if _, err := fixture.kernel.RecordFinding(context.Background(), testIdentity(), runID,
		reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO, contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, "note", 0)); err != nil {
		t.Fatal(err)
	}
	fixture.payloads.tamper = true
	if _, err := fixture.kernel.GetReviewFindings(context.Background(), testIdentity(), runID, "", 10); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("tampered payload error = %v, want ErrPayloadUnavailable", err)
	}
}

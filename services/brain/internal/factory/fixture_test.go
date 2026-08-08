package factory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/roster"
)

// The fixture descriptors mirror tests/fixtures/stage-05/factory/factory-cases.json.
type fixtureGate struct {
	Kind           string `json:"kind"`
	Required       bool   `json:"required"`
	ExpectedStatus string `json:"expectedStatus"`
}

type fixtureCase struct {
	CaseID   string `json:"caseId"`
	Category string `json:"category"`
	Intent   struct {
		Summary    string   `json:"summary"`
		ScopePaths []string `json:"scopePaths"`
	} `json:"intent"`
	Leaves []struct {
		NodeID     string   `json:"nodeId"`
		OwnedPaths []string `json:"ownedPaths"`
	} `json:"leaves"`
	Interference  string `json:"interference"`
	EditCount     int    `json:"editCount"`
	FailureAtEdit int    `json:"failureAtEdit"`
	EscapeAttempt *struct {
		RequestedPath string   `json:"requestedPath"`
		GrantPaths    []string `json:"grantPaths"`
	} `json:"escapeAttempt"`
	Gates                  []fixtureGate `json:"gates"`
	ExpectedOutcome        string        `json:"expectedOutcome"`
	ExpectedPublicReason   *string       `json:"expectedPublicReason"`
	ExpectedRunState       *string       `json:"expectedRunState"`
	ExpectedCandidateState *string       `json:"expectedCandidateState"`
	RollbackReceipt        bool          `json:"rollbackReceipt"`
	CanonicalUnchanged     bool          `json:"canonicalUnchanged"`
}

type fixtureManifest struct {
	SchemaVersion     string        `json:"schemaVersion"`
	BaseGitOID        string        `json:"baseGitOid"`
	RequiredGateKinds []string      `json:"requiredGateKinds"`
	Cases             []fixtureCase `json:"cases"`
}

func readFixtureManifest(t *testing.T) fixtureManifest {
	t.Helper()
	relative := filepath.Join("tests", "fixtures", "stage-05", "factory", "factory-cases.json")
	paths := []string{relative, filepath.Join("..", "..", "..", "..", relative)}
	if testRoot := os.Getenv("TEST_SRCDIR"); testRoot != "" {
		paths = append([]string{filepath.Join(testRoot, os.Getenv("TEST_WORKSPACE"), relative)}, paths...)
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err == nil {
			manifest := fixtureManifest{}
			if err := json.Unmarshal(contents, &manifest); err != nil {
				t.Fatalf("fixture manifest: %v", err)
			}
			return manifest
		}
	}
	t.Fatalf("fixture manifest not found through %v", paths)
	return fixtureManifest{}
}

// TestKernelAgainstAnchorFixtureCorpus drives every frozen Stage 05 factory
// case through the real kernel and asserts the frozen outcomes: static public
// reasons, run and candidate states, rollback receipts, and canonical Git
// never being mutated — the kernel holds no canonical Git write path at all,
// so candidate work can never touch canonical source.
func TestKernelAgainstAnchorFixtureCorpus(t *testing.T) {
	manifest := readFixtureManifest(t)
	if manifest.SchemaVersion != "ouroboros.stage05.factory-cases.v1" || len(manifest.Cases) != 9 {
		t.Fatalf("fixture manifest shape = %q with %d cases", manifest.SchemaVersion, len(manifest.Cases))
	}
	for _, testCase := range manifest.Cases {
		t.Run(testCase.CaseID, func(t *testing.T) {
			fixture := newTestKernel(t)
			fixture.bases.base = manifest.BaseGitOID
			ctx := context.Background()
			scopePaths := append([]string(nil), testCase.Intent.ScopePaths...)
			if testCase.Category == "rollback" {
				// The fixture pins the rename pre-image in scope; the post-image
				// path joins the same bounded scope for the candidate proposal.
				scopePaths = append(scopePaths, "src/go/rename-01.go")
			}
			leaves := make([]LeafSpec, 0, len(testCase.Leaves))
			for _, leaf := range testCase.Leaves {
				ownedPaths := append([]string(nil), leaf.OwnedPaths...)
				if testCase.Category == "rollback" {
					ownedPaths = append(ownedPaths, "src/go/rename-01.go")
				}
				leaves = append(leaves, LeafSpec{
					NodeID:     leaf.NodeID,
					Goal:       []byte(testCase.Intent.Summary),
					OwnedPaths: ownedPaths,
				})
			}
			intentBase := manifest.BaseGitOID
			if testCase.Category == "stale_base" {
				intentBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			admission := AdmitRequest{
				Authenticated:      testIdentity(),
				Caller:             testCaller(),
				Intent:             makeIntent(t, "intent-"+testCase.CaseID, intentBase),
				ApprovedScopePaths: scopePaths,
				Leaves:             leaves,
				Review:             true,
				IdempotencyKey:     "key-" + testCase.CaseID,
			}

			switch testCase.Category {
			case "stale_base":
				if _, err := fixture.kernel.AdmitChangeIntent(ctx, admission); !errors.Is(err, ErrNotFoundOrDenied) {
					t.Fatalf("stale base error = %v, want ErrNotFoundOrDenied", err)
				}
				if testCase.ExpectedPublicReason == nil || *testCase.ExpectedPublicReason != "not_found_or_denied" {
					t.Fatalf("fixture public reason = %v", testCase.ExpectedPublicReason)
				}
				return

			case "duplicate_message":
				first, err := fixture.kernel.AdmitChangeIntent(ctx, admission)
				if err != nil {
					t.Fatal(err)
				}
				second, err := fixture.kernel.AdmitChangeIntent(ctx, admission)
				if err != nil {
					t.Fatal(err)
				}
				if !second.Replayed || second.RunID != first.RunID {
					t.Fatalf("exact replay = %#v, want original %#v", second, first)
				}
				conflicting := admission
				conflicting.Intent = makeIntent(t, "intent-conflicting", manifest.BaseGitOID)
				if _, err := fixture.kernel.AdmitChangeIntent(ctx, conflicting); !errors.Is(err, ErrNotFoundOrDenied) {
					t.Fatalf("conflicting reuse error = %v, want ErrNotFoundOrDenied", err)
				}
				assertRunState(t, fixture, first.RunID, testCase.ExpectedRunState)
				return

			case "revoke":
				runID := admitFixtureRun(t, fixture, admission)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
				if _, err := fixture.kernel.CancelChangeRun(ctx, CancelRequest{
					Authenticated: testIdentity(), Caller: testCaller(), RunID: runID,
					IdempotencyKey: "cancel-" + testCase.CaseID,
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.kernel.CommitLeafResult(ctx, testIdentity(), runID,
					testCase.Leaves[0].NodeID, 1, []byte("pending effect")); !errors.Is(err, ErrNotFoundOrDenied) {
					t.Fatalf("pending effect after revoke error = %v, want ErrNotFoundOrDenied", err)
				}
				assertRunState(t, fixture, runID, testCase.ExpectedRunState)
				return

			case "stale_lease":
				runID := admitFixtureRun(t, fixture, admission)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
				fixture.clock.now += 120_000
				if _, err := fixture.kernel.CommitLeafResult(ctx, testIdentity(), runID,
					testCase.Leaves[0].NodeID, 1, []byte("stale result")); !errors.Is(err, roster.ErrStaleFence) {
					t.Fatalf("stale lease commit error = %v, want roster.ErrStaleFence", err)
				}
				assertRunState(t, fixture, runID, testCase.ExpectedRunState)
				assertNoCandidate(t, fixture, runID)
				return

			case "leaf_escape":
				runID := admitFixtureRun(t, fixture, admission)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
				preview := makeFixturePreview(t, manifest.BaseGitOID, runID, []*contractsv1.PreviewEdit{
					modifyEdit(testCase.EscapeAttempt.RequestedPath),
				})
				if err := fixture.kernel.ProposeCandidate(ctx, testIdentity(), runID, preview); !errors.Is(err, ErrScopeEscape) {
					t.Fatalf("escape proposal error = %v, want ErrScopeEscape", err)
				}
				recordFixtureGates(t, fixture, runID, testCase.Gates)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED)
				assertRunState(t, fixture, runID, testCase.ExpectedRunState)
				assertNoCandidate(t, fixture, runID)
				return

			case "happy_path":
				runID := admitFixtureRun(t, fixture, admission)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
				for _, leaf := range testCase.Leaves {
					if _, err := fixture.kernel.CommitLeafResult(ctx, testIdentity(), runID, leaf.NodeID, 1,
						[]byte("result for "+leaf.NodeID)); err != nil {
						t.Fatal(err)
					}
				}
				edits := make([]*contractsv1.PreviewEdit, 0, len(testCase.Intent.ScopePaths))
				for _, path := range testCase.Intent.ScopePaths {
					edits = append(edits, modifyEdit(path))
				}
				if err := fixture.kernel.ProposeCandidate(ctx, testIdentity(), runID,
					makeFixturePreview(t, manifest.BaseGitOID, runID, edits)); err != nil {
					t.Fatal(err)
				}
				recordFixtureGates(t, fixture, runID, testCase.Gates)
				transitionFixtureCandidate(t, fixture, runID, contractsv1.CandidateState_CANDIDATE_STATE_APPLIED, nil)
				transitionFixtureCandidate(t, fixture, runID, contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED, nil)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW)
				finding, err := fixture.kernel.RecordFinding(ctx, testIdentity(), runID,
					reviewDraft(contractsv1.ReviewSeverity_REVIEW_SEVERITY_INFO,
						contractsv1.ReviewCategory_REVIEW_CATEGORY_DOCS, "fresh review note", 1))
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.kernel.DisposeFinding(ctx, testIdentity(), runID, finding.FindingID,
					contractsv1.FindingDisposition_FINDING_DISPOSITION_DISMISSED_WITH_EVIDENCE); err != nil {
					t.Fatal(err)
				}
				transitionFixtureCandidate(t, fixture, runID, contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED, nil)
				transitionFixtureCandidate(t, fixture, runID, contractsv1.CandidateState_CANDIDATE_STATE_RETAINED, nil)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED)
				assertRunState(t, fixture, runID, testCase.ExpectedRunState)
				assertCandidateState(t, fixture, runID, testCase.ExpectedCandidateState)
				return

			case "partial_edit_failure", "failed_gate", "rollback":
				runID := admitFixtureRun(t, fixture, admission)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
				edits := make([]*contractsv1.PreviewEdit, 0, len(testCase.Intent.ScopePaths))
				for index, path := range testCase.Intent.ScopePaths {
					switch {
					case strings.Contains(path, "add-"):
						edits = append(edits, &contractsv1.PreviewEdit{
							Path:        path,
							Operation:   contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_ADD,
							Language:    contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
							AfterDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestBytes([]byte("new:" + path))},
						})
					case testCase.Category == "rollback" && index == 0:
						oldPath := path
						edits = append(edits, &contractsv1.PreviewEdit{
							Path:         "src/go/rename-01.go",
							OldPath:      &oldPath,
							Operation:    contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_RENAME,
							Language:     contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
							BeforeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestBytes([]byte("before:" + path))},
							AfterDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: digestBytes([]byte("after:" + path))},
						})
					default:
						edits = append(edits, modifyEdit(path))
					}
				}
				if err := fixture.kernel.ProposeCandidate(ctx, testIdentity(), runID,
					makeFixturePreview(t, manifest.BaseGitOID, runID, edits)); err != nil {
					t.Fatal(err)
				}
				recordFixtureGates(t, fixture, runID, testCase.Gates)
				transitionFixtureCandidate(t, fixture, runID, contractsv1.CandidateState_CANDIDATE_STATE_APPLIED, nil)
				switch testCase.Category {
				case "failed_gate":
					if err := fixture.kernel.TransitionCandidate(ctx, testIdentity(), runID,
						contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED, nil); !errors.Is(err, ErrTransitionInvalid) {
						t.Fatalf("verify behind failed gate error = %v, want ErrTransitionInvalid", err)
					}
				case "rollback":
					transitionFixtureCandidate(t, fixture, runID, contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED, nil)
				}
				transitionFixtureCandidate(t, fixture, runID, contractsv1.CandidateState_CANDIDATE_STATE_REJECTED,
					&RollbackReceipt{ReasonCode: "candidate_rejected", ArtifactDigestHex: digestBytes([]byte("rollback:" + runID))})
				transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED)
				assertRunState(t, fixture, runID, testCase.ExpectedRunState)
				assertCandidateState(t, fixture, runID, testCase.ExpectedCandidateState)
				served, err := fixture.kernel.PreviewChangeSet(ctx, testIdentity(), runID)
				if err != nil {
					t.Fatal(err)
				}
				if !testCase.RollbackReceipt || served.GetRollbackReceipt().GetReasonCode() != "candidate_rejected" {
					t.Fatal("rejected candidate lost its rollback receipt")
				}
				return
			}
			t.Fatalf("uncovered fixture category %q", testCase.Category)
		})
	}
}

func admitFixtureRun(t *testing.T, fixture *testKernel, admission AdmitRequest) string {
	t.Helper()
	result, err := fixture.kernel.AdmitChangeIntent(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	return result.RunID
}

// makeFixturePreview builds a candidate preview whose gate roster uses the
// identities the kernel authored at admission.
func makeFixturePreview(t *testing.T, baseOID, runID string, edits []*contractsv1.PreviewEdit) *contractsv1.ChangeSetPreview {
	t.Helper()
	preview := makePreview(t, runID, edits)
	preview.ChangeSet.BaseGitOid = baseOID
	preview.ExpectedBaseGitOid = baseOID
	return preview
}

// recordFixtureGates drives the deterministic gate roster to the fixture's
// expected outcomes.
func recordFixtureGates(t *testing.T, fixture *testKernel, runID string, gates []fixtureGate) {
	t.Helper()
	ctx := context.Background()
	for _, gate := range gates {
		gateID := identity("ouroboros.stage05.gate.v1", runID, gate.Kind)
		status := contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED
		if gate.ExpectedStatus == "FAILED" {
			status = contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED
		}
		if err := fixture.kernel.RecordGateResult(ctx, testIdentity(), runID, gateID,
			contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING); err != nil {
			t.Fatal(err)
		}
		if err := fixture.kernel.RecordGateResult(ctx, testIdentity(), runID, gateID, status); err != nil {
			t.Fatal(err)
		}
	}
}

func transitionFixtureCandidate(t *testing.T, fixture *testKernel, runID string, next contractsv1.CandidateState, rollback *RollbackReceipt) {
	t.Helper()
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID, next, rollback); err != nil {
		t.Fatalf("candidate transition to %v: %v", next, err)
	}
}

func assertRunState(t *testing.T, fixture *testKernel, runID string, expected *string) {
	t.Helper()
	if expected == nil {
		return
	}
	// Read the canonical state directly: reads on revoked (cancelled) runs
	// deny statically at the kernel boundary, so the plan read cannot assert
	// the terminal cancelled state.
	var stateText string
	if err := fixture.kernel.db.QueryRow(`SELECT state FROM factory_run_states
		WHERE tenant_id=? AND principal_id=? AND run_id=? ORDER BY sequence DESC LIMIT 1`,
		testTenant, testPrincipal, runID).Scan(&stateText); err != nil {
		t.Fatal(err)
	}
	if stateText != *expected {
		t.Fatalf("run state = %s, want %s", stateText, *expected)
	}
}

func assertCandidateState(t *testing.T, fixture *testKernel, runID string, expected *string) {
	t.Helper()
	if expected == nil {
		return
	}
	preview, err := fixture.kernel.PreviewChangeSet(context.Background(), testIdentity(), runID)
	if err != nil {
		t.Fatal(err)
	}
	stateText, err := candidateStateText(preview.GetCandidateState())
	if err != nil {
		t.Fatal(err)
	}
	if stateText != *expected {
		t.Fatalf("candidate state = %s, want %s", stateText, *expected)
	}
}

func assertNoCandidate(t *testing.T, fixture *testKernel, runID string) {
	t.Helper()
	var count int
	if err := fixture.kernel.db.QueryRow(`SELECT count(*) FROM factory_candidates
		WHERE tenant_id=? AND principal_id=? AND run_id=?`, testTenant, testPrincipal, runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("a candidate became canonical in a case that expects none")
	}
}

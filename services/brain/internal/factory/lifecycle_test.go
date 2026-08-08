package factory

import (
	"context"
	"errors"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factory/roster"
)

func TestStaleFenceCommitNeverBecomesCanonical(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "stale-lease")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)

	// Advance the injected clock beyond the lease TTL; the fence is now stale.
	fixture.clock.now += 120_000
	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-a")); !errors.Is(err, roster.ErrStaleFence) {
		t.Fatalf("expired fence commit error = %v, want roster.ErrStaleFence", err)
	}
	var count int
	if err := fixture.kernel.db.QueryRow(`SELECT count(*) FROM factory_leaf_results
		WHERE tenant_id=? AND principal_id=? AND run_id=?`,
		testTenant, testPrincipal, runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("an expired-fence commit became canonical")
	}
}

func TestLeafCommitRequiresCurrentFenceAndRunningRun(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "leaf-commit")

	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-a")); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("commit before running error = %v, want ErrNotFoundOrDenied", err)
	}
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 99, []byte("result-a")); !errors.Is(err, roster.ErrStaleFence) {
		t.Fatalf("unknown fence error = %v, want roster.ErrStaleFence", err)
	}
	committed, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-a"))
	if err != nil {
		t.Fatal(err)
	}
	if committed.Replayed || committed.Lease.Fence != 1 {
		t.Fatalf("commit = %#v", committed)
	}
	replayed, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed {
		t.Fatal("exact leaf commit replay was not collapsed")
	}
	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-b")); !errors.Is(err, roster.ErrResultConflict) {
		t.Fatalf("conflicting leaf result error = %v, want roster.ErrResultConflict", err)
	}
}

func TestRunTransitionTableIsEnforced(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "transitions")
	if err := fixture.kernel.TransitionRun(context.Background(), testIdentity(), runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("planning->running error = %v, want ErrTransitionInvalid", err)
	}
	if err := fixture.kernel.TransitionRun(context.Background(), testIdentity(), runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("direct cancel error = %v, want ErrTransitionInvalid", err)
	}
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED)
	if err := fixture.kernel.TransitionRun(context.Background(), testIdentity(), runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("terminal transition error = %v, want ErrTransitionInvalid", err)
	}
}

func TestGateResultsAreDenseTerminalAndReplayable(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "gates")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
	plan, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), runID)
	if err != nil {
		t.Fatal(err)
	}
	gateID := plan.GetGates()[0].GetGateId().GetValue()
	if err := fixture.kernel.RecordGateResult(context.Background(), testIdentity(), runID, gateID,
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.RecordGateResult(context.Background(), testIdentity(), runID, gateID,
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
		t.Fatal("exact terminal replay must succeed", err)
	}
	if err := fixture.kernel.RecordGateResult(context.Background(), testIdentity(), runID, gateID,
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_FAILED); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("post-terminal flip error = %v, want ErrTransitionInvalid", err)
	}
	if err := fixture.kernel.RecordGateResult(context.Background(), testIdentity(), runID, "gate-absent",
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("unknown gate error = %v, want ErrNotFoundOrDenied", err)
	}
}

func TestCandidateAtomicityGatesRollbackAndCompletion(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "candidate-happy")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-b", 1, []byte("result-b")); err != nil {
		t.Fatal(err)
	}
	preview := makePreview(t, runID, []*contractsv1.PreviewEdit{
		modifyEdit("src/go/modify-00.go"),
		modifyEdit("src/go/modify-01.go"),
	})
	if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); err != nil {
		t.Fatal(err)
	}
	// Verification before every required gate passed must deny.
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID,
		contractsv1.CandidateState_CANDIDATE_STATE_APPLIED, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID,
		contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED, nil); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("verify behind pending gates error = %v, want ErrTransitionInvalid", err)
	}
	passAllGates(t, fixture, runID)
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID,
		contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED, nil); err != nil {
		t.Fatal(err)
	}
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW)
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID,
		contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID,
		contractsv1.CandidateState_CANDIDATE_STATE_RETAINED, nil); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.TransitionRun(context.Background(), testIdentity(), runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("completion before candidate-ready error = %v, want ErrTransitionInvalid", err)
	}
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED)

	served, err := fixture.kernel.PreviewChangeSet(context.Background(), testIdentity(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if served.GetCandidateState() != contractsv1.CandidateState_CANDIDATE_STATE_RETAINED {
		t.Fatalf("served candidate state = %v, want RETAINED", served.GetCandidateState())
	}
	for _, gate := range served.GetGates() {
		if gate.GetStatus() != contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED {
			t.Fatalf("served gate %s status = %v, want PASSED", gate.GetGateId().GetValue(), gate.GetStatus())
		}
	}
}

func TestRejectedCandidateCarriesRollbackReceiptAtomically(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "candidate-rollback")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
	preview := makePreview(t, runID, []*contractsv1.PreviewEdit{modifyEdit("src/go/modify-00.go")})
	if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); err != nil {
		t.Fatal(err)
	}
	// Rejection without a rollback receipt is structurally impossible.
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID,
		contractsv1.CandidateState_CANDIDATE_STATE_REJECTED, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("receiptless rejection error = %v, want ErrInvalidInput", err)
	}
	rollback := &RollbackReceipt{ReasonCode: "gate_failed", ArtifactDigestHex: testRouteHex}
	if err := fixture.kernel.TransitionCandidate(context.Background(), testIdentity(), runID,
		contractsv1.CandidateState_CANDIDATE_STATE_REJECTED, rollback); err != nil {
		t.Fatal(err)
	}
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED)
	served, err := fixture.kernel.PreviewChangeSet(context.Background(), testIdentity(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if served.GetCandidateState() != contractsv1.CandidateState_CANDIDATE_STATE_REJECTED ||
		served.GetRollbackReceipt().GetReasonCode() != "gate_failed" {
		t.Fatalf("served rejected preview = %v", served)
	}
}

func TestCandidateBaseBindingAndScopeEscapeDeny(t *testing.T) {
	t.Run("base mismatch", func(t *testing.T) {
		fixture := newTestKernel(t)
		runID := admitHappy(t, fixture, "candidate-base")
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
		preview := makePreview(t, runID, []*contractsv1.PreviewEdit{modifyEdit("src/go/modify-00.go")})
		preview.ExpectedBaseGitOid = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); !errors.Is(err, ErrPlanInvalid) {
			t.Fatalf("error = %v, want ErrPlanInvalid", err)
		}
	})
	t.Run("scope escape", func(t *testing.T) {
		fixture := newTestKernel(t)
		runID := admitHappy(t, fixture, "candidate-escape")
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
		preview := makePreview(t, runID, []*contractsv1.PreviewEdit{
			modifyEdit("src/go/modify-00.go"),
			modifyEdit("src/typescript/modify-00.ts"),
		})
		preview.Edits[1].Language = contractsv1.CodeLanguage_CODE_LANGUAGE_TYPESCRIPT
		preview.Obligations = append(preview.Obligations, &contractsv1.LanguageObligation{
			Language: contractsv1.CodeLanguage_CODE_LANGUAGE_TYPESCRIPT,
			Impact:   &contractsv1.ImpactReceipt{BaseGitOid: testBaseOID},
		})
		if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); !errors.Is(err, ErrScopeEscape) {
			t.Fatalf("error = %v, want ErrScopeEscape", err)
		}
		var count int
		if err := fixture.kernel.db.QueryRow(`SELECT count(*) FROM factory_candidates
			WHERE tenant_id=? AND principal_id=? AND run_id=?`, testTenant, testPrincipal, runID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal("an escaped candidate became canonical")
		}
	})
	t.Run("duplicate post-image paths", func(t *testing.T) {
		fixture := newTestKernel(t)
		runID := admitHappy(t, fixture, "candidate-dupe")
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
		preview := makePreview(t, runID, []*contractsv1.PreviewEdit{
			modifyEdit("src/go/modify-00.go"),
			modifyEdit("src/go/modify-00.go"),
		})
		if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); !errors.Is(err, ErrPlanInvalid) {
			t.Fatalf("error = %v, want ErrPlanInvalid", err)
		}
	})
	t.Run("rename pre-image consumed twice", func(t *testing.T) {
		fixture := newTestKernel(t)
		// Directory-scoped leaves keep the rename target in scope so the
		// pre-image uniqueness rule is what fires.
		result, err := fixture.kernel.AdmitChangeIntent(context.Background(), AdmitRequest{
			Authenticated:      testIdentity(),
			Caller:             testCaller(),
			Intent:             makeIntent(t, "intent-rename-dupe", testBaseOID),
			ApprovedScopePaths: []string{"src/go"},
			Leaves:             []LeafSpec{leafSpec("leaf-a", "src/go")},
			Review:             false,
			IdempotencyKey:     "key-rename-dupe",
		})
		if err != nil {
			t.Fatal(err)
		}
		runID := result.RunID
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
		transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
		oldPath := "src/go/modify-00.go"
		rename := &contractsv1.PreviewEdit{
			Path:         "src/go/renamed.go",
			OldPath:      &oldPath,
			Operation:    contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_RENAME,
			Language:     contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
			BeforeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestBytes([]byte("before"))},
			AfterDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: digestBytes([]byte("after"))},
		}
		preview := makePreview(t, runID, []*contractsv1.PreviewEdit{rename, modifyEdit("src/go/modify-00.go")})
		if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); !errors.Is(err, ErrPlanInvalid) {
			t.Fatalf("error = %v, want ErrPlanInvalid", err)
		}
	})
}

func TestCandidateProposalReplayCollapses(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "candidate-replay")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)
	preview := makePreview(t, runID, []*contractsv1.PreviewEdit{modifyEdit("src/go/modify-00.go")})
	if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); err != nil {
		t.Fatal("identical re-proposal must replay", err)
	}
	preview.ChangeSet.ChangeSetId.Value = "candidate-different"
	if err := fixture.kernel.ProposeCandidate(context.Background(), testIdentity(), runID, preview); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("differing re-proposal error = %v, want ErrNotFoundOrDenied", err)
	}
}

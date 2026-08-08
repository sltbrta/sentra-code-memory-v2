package factory

import (
	"context"
	"errors"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

func TestAdmitHappyPathOpensPlanningRun(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "admit-1")

	plan, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GetState() != contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING {
		t.Fatalf("state = %v, want PLANNING", plan.GetState())
	}
	if len(plan.GetNodes()) != 4 {
		t.Fatalf("nodes = %d, want 4 (orchestrator, two leaves, review)", len(plan.GetNodes()))
	}
	if len(plan.GetEdges()) != 3 {
		t.Fatalf("edges = %d, want 3", len(plan.GetEdges()))
	}
	if len(plan.GetGates()) != 4 {
		t.Fatalf("gates = %d, want 4", len(plan.GetGates()))
	}
	for _, gate := range plan.GetGates() {
		if !gate.GetRequired() || gate.GetStatus() != contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING {
			t.Fatalf("gate %v not required-pending", gate)
		}
	}
	for _, node := range plan.GetNodes() {
		switch node.GetKind() {
		case contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF:
			if node.GetLease().GetFence() != 1 || node.GetCapabilityGrant().GetRepositoryGitOid() != testBaseOID {
				t.Fatalf("leaf %s lease/grant malformed: %v", node.GetNodeId(), node)
			}
			for _, action := range node.GetCapabilityGrant().GetActions() {
				if action == "factory.dispatch" || action == "factory.task.create" {
					t.Fatalf("leaf %s carries dispatch authority", node.GetNodeId())
				}
			}
			if node.GetRoute().GetModelIdentity() != "model-static-1" {
				t.Fatalf("leaf %s route not recorded deterministically", node.GetNodeId())
			}
		default:
			if len(node.GetOwnedPaths()) != 0 || node.GetLease() != nil || node.GetCapabilityGrant() != nil || node.GetRoute() != nil {
				t.Fatalf("non-leaf %s carries scope, lease, grant, or route", node.GetNodeId())
			}
		}
	}
	if plan.GetIntent().GetRepositoryGitOid() != testBaseOID {
		t.Fatal("served plan lost the admitted intent")
	}
}

func TestAdmitExactReplayReturnsOriginalOutcome(t *testing.T) {
	fixture := newTestKernel(t)
	request := admitRequest(t, "admit-replay", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
	first, err := fixture.kernel.AdmitChangeIntent(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.kernel.AdmitChangeIntent(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.RunID != first.RunID {
		t.Fatalf("replay = %#v, want original run %q", second, first.RunID)
	}
}

func TestAdmitConflictingKeyReuseDeniesWithoutMutation(t *testing.T) {
	fixture := newTestKernel(t)
	request := admitRequest(t, "admit-conflict", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
	if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	conflicting := request
	conflicting.Intent = makeIntent(t, "intent-different", testBaseOID)
	if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), conflicting); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("conflicting reuse error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), "run-never-opened"); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("unknown run error = %v, want ErrNotFoundOrDenied", err)
	}
}

func TestAdmitSameIntentUnderNewKeyDeniesDuplicateRun(t *testing.T) {
	fixture := newTestKernel(t)
	request := admitRequest(t, "admit-dedupe", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
	if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	duplicate := request
	duplicate.IdempotencyKey = "admit-dedupe-new-key"
	if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), duplicate); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("duplicate intent error = %v, want ErrNotFoundOrDenied", err)
	}
}

func TestAdmitStaleBaseDeniesStatically(t *testing.T) {
	fixture := newTestKernel(t)
	request := admitRequest(t, "admit-stale-base", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
	request.Intent = makeIntent(t, "intent-stale", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("stale base error = %v, want ErrNotFoundOrDenied", err)
	}
	// A stale-base denial records nothing, so the same key can admit once the
	// base catches up.
	if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("still-stale base error = %v, want ErrNotFoundOrDenied", err)
	}
}

func TestAdmitAuthorizationFailuresDenyStatically(t *testing.T) {
	t.Run("caller mismatch", func(t *testing.T) {
		fixture := newTestKernel(t)
		request := admitRequest(t, "admit-caller", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
		request.Caller.Principal = "principal-2"
		if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
			t.Fatalf("error = %v, want ErrNotFoundOrDenied", err)
		}
	})
	t.Run("policy deny", func(t *testing.T) {
		fixture := newTestKernel(t)
		fixture.policy.allowed = false
		request := admitRequest(t, "admit-policy", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
		if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
			t.Fatalf("error = %v, want ErrNotFoundOrDenied", err)
		}
	})
	t.Run("expired approval", func(t *testing.T) {
		fixture := newTestKernel(t)
		fixture.clock.now = 10_000_000
		request := admitRequest(t, "admit-expired", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
		if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
			t.Fatalf("error = %v, want ErrNotFoundOrDenied", err)
		}
	})
	t.Run("rejected approval receipt", func(t *testing.T) {
		fixture := newTestKernel(t)
		request := admitRequest(t, "admit-rejected", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
		request.Intent.Approval.Receipt.Status = contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED
		if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
			t.Fatalf("error = %v, want ErrNotFoundOrDenied", err)
		}
	})
	t.Run("requested-by mismatch", func(t *testing.T) {
		fixture := newTestKernel(t)
		request := admitRequest(t, "admit-requestedby", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
		request.Intent.RequestedBy.PrincipalId.Value = "principal-2"
		if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
			t.Fatalf("error = %v, want ErrNotFoundOrDenied", err)
		}
	})
	t.Run("base resolver failure", func(t *testing.T) {
		fixture := newTestKernel(t)
		fixture.bases.err = errors.New("ingestion revoked")
		request := admitRequest(t, "admit-resolver", []LeafSpec{leafSpec("leaf-a", "src/go/modify-00.go")})
		if _, err := fixture.kernel.AdmitChangeIntent(context.Background(), request); !errors.Is(err, ErrNotFoundOrDenied) {
			t.Fatalf("error = %v, want ErrNotFoundOrDenied", err)
		}
	})
}

func TestCancelChangeRunIsTerminalReplayableAndDeniesPendingEffects(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "cancel-1")
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY)
	transitionRun(t, fixture, runID, contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING)

	cancel, err := fixture.kernel.CancelChangeRun(context.Background(), CancelRequest{
		Authenticated: testIdentity(), Caller: testCaller(), RunID: runID, IdempotencyKey: "cancel-key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancel.RunID != runID || cancel.Replayed {
		t.Fatalf("cancel = %#v", cancel)
	}
	replayed, err := fixture.kernel.CancelChangeRun(context.Background(), CancelRequest{
		Authenticated: testIdentity(), Caller: testCaller(), RunID: runID, IdempotencyKey: "cancel-key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed {
		t.Fatal("exact cancel replay did not return the original outcome")
	}
	if _, err := fixture.kernel.CancelChangeRun(context.Background(), CancelRequest{
		Authenticated: testIdentity(), Caller: testCaller(), RunID: runID, IdempotencyKey: "cancel-key-2",
	}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cancel of terminal run error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.CommitLeafResult(context.Background(), testIdentity(), runID, "leaf-a", 1, []byte("result")); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("pending effect after cancel error = %v, want ErrNotFoundOrDenied", err)
	}
	if err := fixture.kernel.TransitionRun(context.Background(), testIdentity(), runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("terminal transition error = %v, want ErrTransitionInvalid", err)
	}
	if _, err := fixture.kernel.CancelChangeRun(context.Background(), CancelRequest{
		Authenticated: testIdentity(), Caller: testCaller(), RunID: "run-absent", IdempotencyKey: "cancel-key-3",
	}); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cancel of unknown run error = %v, want ErrNotFoundOrDenied", err)
	}
	// A revoked run discloses nothing on any read.
	if _, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), runID); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cancelled plan read error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.PreviewChangeSet(context.Background(), testIdentity(), runID); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cancelled preview read error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.GetReviewFindings(context.Background(), testIdentity(), runID, "", 10); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cancelled findings read error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.CancelChangeRun(context.Background(), CancelRequest{
		Authenticated: testIdentity(), Caller: testCaller(), RunID: runID, IdempotencyKey: "cancel-key-1",
	}); err != nil {
		t.Fatal("replay after terminal-state denial must still return the original outcome", err)
	}
}

func TestCrossPrincipalReadsDenyStatically(t *testing.T) {
	fixture := newTestKernel(t)
	runID := admitHappy(t, fixture, "admit-scope")
	other := Identity{Tenant: testTenant, Principal: "principal-2", Session: testSession}
	if _, err := fixture.kernel.GetChangePlan(context.Background(), other, runID); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cross-principal plan read error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.PreviewChangeSet(context.Background(), other, runID); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cross-principal preview read error = %v, want ErrNotFoundOrDenied", err)
	}
	if _, err := fixture.kernel.GetReviewFindings(context.Background(), other, runID, "", 10); !errors.Is(err, ErrNotFoundOrDenied) {
		t.Fatalf("cross-principal findings read error = %v, want ErrNotFoundOrDenied", err)
	}
}

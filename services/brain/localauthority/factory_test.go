package localauthority

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// factorySurfaceTestPolicy is a static current-policy checker for the composed
// factory surface tests.
type factorySurfaceTestPolicy struct {
	allowed bool
}

func (p factorySurfaceTestPolicy) Check(
	_ context.Context, _ shared.MappedIdentityFact, _ shared.PolicyRequest,
) (shared.PolicyDecision, error) {
	return shared.PolicyDecision{Allowed: p.allowed, RevocationEpoch: 7}, nil
}

func factorySurfaceTestIntent(t *testing.T, identity Identity, intentID, baseOID string) *contractsv1.ChangeIntent {
	t.Helper()
	principalRef := &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: identity.Principal.Value},
		TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: identity.Tenant.Value},
		SessionId:   &contractsv1.Identifier{Namespace: "session", Value: identity.Session.Value},
	}
	return &contractsv1.ChangeIntent{
		IntentId:         &contractsv1.Identifier{Namespace: "intent", Value: intentID},
		RequestedBy:      principalRef,
		RepositoryGitOid: baseOID,
		ScopeDigest:      &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("c", 64)},
		SupportingEvidence: []*contractsv1.EvidenceRef{{
			EvidenceId:       &contractsv1.Identifier{Namespace: "artifact", Value: "artifact-factory-approval"},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "revision", Value: "revision-1"},
		}},
		Approval: &contractsv1.Approval{
			ApprovalId:  &contractsv1.Identifier{Namespace: "approval", Value: "approval-1"},
			Approver:    principalRef,
			ScopeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("c", 64)},
			ExpiresAt:   timestamppb.New(time.Now().UTC().Add(time.Hour)),
			Receipt: &contractsv1.Receipt{
				ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: "receipt-1"},
				Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
				ReasonCode:  "approved",
				OperationId: &contractsv1.Identifier{Namespace: "intent", Value: intentID},
				Causal: &contractsv1.CausalContext{
					CorrelationId: &contractsv1.Identifier{Namespace: "intent", Value: intentID},
					CausationId:   &contractsv1.Identifier{Namespace: "intent", Value: intentID},
					TraceId:       &contractsv1.Identifier{Namespace: "intent", Value: intentID},
				},
				RecordedAt:          timestamppb.New(time.Now().UTC().Add(-time.Hour)),
				ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("c", 64)},
			},
		},
	}
}

// TestFactorySurfaceComposesKernelOverDurableAuthority proves the composed
// Stage 05 factory surface over a real durable runtime: the kernel opens over
// migration 005 with the encrypted vault and the Stage 03 catalog base
// resolver, admits one intent pinned to the current committed base, denies a
// stale base statically, and replays durable facts across a surface restart.
func TestFactorySurfaceComposesKernelOverDurableAuthority(t *testing.T) {
	ctx := context.Background()
	repository := t.TempDir()
	writeRepositoryFiles(t, repository, map[string]string{
		"src/go/modify-00.go": "package main\n\nfunc marker() string { return \"stage\" }\n",
	})
	base := commitRepository(t, repository, "initial")

	config, keys := durableTestConfig(t.TempDir())
	config.Ingestion = testIngestionConfig(repository)
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.AddSource(ctx, AddSourceRequest{
		IngestionContext:  testIngestionContext(config, identity),
		ExpectedCommitOID: base, IdempotencyKey: "add-source",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.OpenFactorySurface(ctx, FactorySurfaceConfig{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("incomplete surface config = %v", err)
	}
	openSurface := func() *FactorySurface {
		t.Helper()
		surface, err := runtime.OpenFactorySurface(ctx, FactorySurfaceConfig{
			Policy:          factorySurfaceTestPolicy{allowed: true},
			LeaseTTLMillis:  60_000,
			RevocationEpoch: 7,
			PolicyDigestHex: strings.Repeat("d", 64),
		})
		if err != nil {
			t.Fatal(err)
		}
		return surface
	}
	surface := openSurface()
	scope := FactoryIdentity{
		Tenant: identity.Tenant.Value, Principal: identity.Principal.Value, Session: identity.Session.Value,
	}
	caller := FactoryCallerCrossCheck{
		Tenant: scope.Tenant, Principal: scope.Principal, Session: scope.Session,
	}
	leaves := []FactoryLeafSpec{{
		NodeID: "leaf-a", Goal: []byte("rename the marker accessor"),
		OwnedPaths: []string{"src/go/modify-00.go"}, HolderPrincipal: scope.Principal,
	}}
	intent := factorySurfaceTestIntent(t, identity, "intent-happy", base)
	admission := FactoryAdmitRequest{
		Authenticated:      scope,
		Caller:             caller,
		Intent:             intent,
		ApprovedScopePaths: []string{"src/go/modify-00.go"},
		Leaves:             leaves,
		Review:             true,
		IdempotencyKey:     "admit-happy",
	}
	admitted, err := surface.Kernel().AdmitChangeIntent(ctx, admission)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.RunID == "" || admitted.Replayed {
		t.Fatalf("admitted = %#v", admitted)
	}
	plan, err := surface.Kernel().GetChangePlan(ctx, scope, admitted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GetState() != contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING || len(plan.GetNodes()) != 3 ||
		len(plan.GetGates()) != 4 {
		t.Fatalf("plan = %v nodes=%d gates=%d", plan.GetState(), len(plan.GetNodes()), len(plan.GetGates()))
	}
	// The exact idempotent replay returns the original run without re-executing.
	replayed, err := surface.Kernel().AdmitChangeIntent(ctx, admission)
	if err != nil || !replayed.Replayed || replayed.RunID != admitted.RunID {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	// A stale base denies statically against the Stage 03 catalog resolution.
	staleBase := strings.Repeat("b", 40)
	if staleBase == base {
		t.Fatal("test collision")
	}
	if _, err := surface.Kernel().AdmitChangeIntent(ctx, FactoryAdmitRequest{
		Authenticated:      scope,
		Caller:             caller,
		Intent:             factorySurfaceTestIntent(t, identity, "intent-stale", staleBase),
		ApprovedScopePaths: []string{"src/go/modify-00.go"},
		Leaves:             leaves,
		Review:             true,
		IdempotencyKey:     "admit-stale",
	}); !errors.Is(err, ErrFactoryNotFoundOrDenied) {
		t.Fatalf("stale base = %v", err)
	}
	// A denied policy fails admission closed.
	deniedSurface, err := runtime.OpenFactorySurface(ctx, FactorySurfaceConfig{
		Policy:          factorySurfaceTestPolicy{allowed: false},
		LeaseTTLMillis:  60_000,
		RevocationEpoch: 7,
		PolicyDigestHex: strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deniedSurface.Kernel().AdmitChangeIntent(ctx, FactoryAdmitRequest{
		Authenticated:      scope,
		Caller:             caller,
		Intent:             factorySurfaceTestIntent(t, identity, "intent-denied", base),
		ApprovedScopePaths: []string{"src/go/modify-00.go"},
		Leaves:             leaves,
		Review:             true,
		IdempotencyKey:     "admit-denied",
	}); !errors.Is(err, ErrFactoryNotFoundOrDenied) {
		t.Fatalf("denied policy = %v", err)
	}
	if err := surface.Close(); err != nil {
		t.Fatal(err)
	}
	if err := deniedSurface.Close(); err != nil {
		t.Fatal(err)
	}
	// Durable run and plan facts survive a surface restart.
	restarted := openSurface()
	defer func() { _ = restarted.Close() }()
	restartedPlan, err := restarted.Kernel().GetChangePlan(ctx, scope, admitted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if restartedPlan.GetRunId().GetValue() != admitted.RunID || len(restartedPlan.GetNodes()) != 3 {
		t.Fatalf("restarted plan nodes = %d", len(restartedPlan.GetNodes()))
	}
	restartedReplay, err := restarted.Kernel().AdmitChangeIntent(ctx, admission)
	if err != nil || !restartedReplay.Replayed || restartedReplay.RunID != admitted.RunID {
		t.Fatalf("restart replay = %#v, %v", restartedReplay, err)
	}
}

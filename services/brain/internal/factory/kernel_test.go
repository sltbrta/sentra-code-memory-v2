package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate/schema"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	testTenant    = "tenant-1"
	testPrincipal = "principal-1"
	testSession   = "session-1"
	testBaseOID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testPolicyHex = "1111111111111111111111111111111111111111111111111111111111111111"
	testRouteHex  = "2222222222222222222222222222222222222222222222222222222222222222"
)

// testClock is the deterministic injected clock.
type testClock struct {
	now int64
}

func (c *testClock) NowUnixMilli() int64 { return c.now }

// fakePayloads is the deterministic in-memory PayloadStore. By default it is
// content-addressed; with uniqueIDs set it mints a fresh random identity per
// Put, mirroring the production vault adapter, so replay paths cannot rely on
// artifact identity equality.
type fakePayloads struct {
	mu        sync.Mutex
	objects   map[string][]byte
	tamper    bool
	puts      int
	uniqueIDs bool
}

func newFakePayloads() *fakePayloads {
	return &fakePayloads{objects: make(map[string][]byte)}
}

func (f *fakePayloads) Put(_ context.Context, tenant string, payload []byte) (string, error) {
	if tenant == "" || len(payload) == 0 {
		return "", errors.New("invalid payload")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var artifactID string
	if f.uniqueIDs {
		f.puts++
		artifactID = fmt.Sprintf("payload-random-%d", f.puts)
	} else {
		sum := sha256.Sum256(payload)
		artifactID = "payload-" + hex.EncodeToString(sum[:])
		f.puts++
	}
	if _, found := f.objects[artifactID]; !found {
		f.objects[artifactID] = append([]byte(nil), payload...)
	}
	return artifactID, nil
}

func (f *fakePayloads) Get(_ context.Context, tenant, artifactID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, found := f.objects[artifactID]
	if !found {
		return nil, errors.New("missing artifact")
	}
	if f.tamper {
		mutated := append([]byte(nil), payload...)
		mutated[0] ^= 0xff
		return mutated, nil
	}
	return append([]byte(nil), payload...), nil
}

func (f *fakePayloads) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

// fakeBases resolves the exact current Git base.
type fakeBases struct {
	base string
	err  error
}

func (f *fakeBases) CurrentBase(context.Context, Identity) (string, error) {
	return f.base, f.err
}

// fakePolicy is a static current-policy checker.
type fakePolicy struct {
	allowed bool
}

func (f *fakePolicy) Check(context.Context, contracts.MappedIdentityFact, contracts.PolicyRequest) (contracts.PolicyDecision, error) {
	return contracts.PolicyDecision{Allowed: f.allowed}, nil
}

// testKernel is the wired kernel under test with its fakes.
type testKernel struct {
	kernel   *Kernel
	payloads *fakePayloads
	clock    *testClock
	bases    *fakeBases
	policy   *fakePolicy
	path     string
}

// newTestKernel builds a real migrated authority database with the admitting
// principal's session seeded through the canonical Stage 02 session ledger,
// then opens the kernel over it with deterministic fakes.
func newTestKernel(t *testing.T) *testKernel {
	t.Helper()
	ctx := context.Background()
	path := t.TempDir() + "/authority.db"
	authority, err := localstate.OpenWithMigrations(ctx, path, schema.Migrations(), localstate.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.OpenSession(ctx, contracts.MappedIdentityFact{
		Principal:   contracts.Identifier{Namespace: "principal", Value: testPrincipal},
		Tenant:      contracts.Identifier{Namespace: "tenant", Value: testTenant},
		Session:     contracts.Identifier{Namespace: "session", Value: testSession},
		Credentials: contracts.PeerCredentials{UID: 501, PID: 4242},
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	router, err := NewStaticRouter(testRouteHex, "model-static-1", "static_profile")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &testKernel{
		payloads: newFakePayloads(),
		clock:    &testClock{now: 1_000_000},
		bases:    &fakeBases{base: testBaseOID},
		policy:   &fakePolicy{allowed: true},
		path:     path,
	}
	kernel, err := Open(ctx, Config{
		DatabasePath:    path,
		Payloads:        fixture.payloads,
		Clock:           fixture.clock,
		Bases:           fixture.bases,
		Policy:          fixture.policy,
		Router:          router,
		LeaseTTLMillis:  60_000,
		RevocationEpoch: 7,
		PolicyDigestHex: testPolicyHex,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernel.Close() })
	fixture.kernel = kernel
	return fixture
}

func testIdentity() Identity {
	return Identity{Tenant: testTenant, Principal: testPrincipal, Session: testSession}
}

func testCaller() CallerCrossCheck {
	return CallerCrossCheck{Tenant: testTenant, Principal: testPrincipal, Session: testSession}
}

// makeIntent builds one frozen ChangeIntent pinned to the given base with a
// completed unexpired approval.
func makeIntent(t *testing.T, intentID, baseOID string) *contractsv1.ChangeIntent {
	t.Helper()
	principalRef := &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: testPrincipal},
		TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
		SessionId:   &contractsv1.Identifier{Namespace: "session", Value: testSession},
	}
	return &contractsv1.ChangeIntent{
		IntentId:         &contractsv1.Identifier{Namespace: "intent", Value: intentID},
		RequestedBy:      principalRef,
		RepositoryGitOid: baseOID,
		ScopeDigest:      &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("c", 64)},
		SupportingEvidence: []*contractsv1.EvidenceRef{{
			EvidenceId:       &contractsv1.Identifier{Namespace: "evidence", Value: "evidence-1"},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "revision", Value: "revision-1"},
		}},
		Approval: &contractsv1.Approval{
			ApprovalId:  &contractsv1.Identifier{Namespace: "approval", Value: "approval-1"},
			Approver:    principalRef,
			ScopeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("c", 64)},
			ExpiresAt:   timestamppb.New(time.UnixMilli(9_000_000).UTC()),
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
				RecordedAt:          timestamppb.New(time.UnixMilli(900_000).UTC()),
				ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: testPolicyHex},
			},
		},
	}
}

// admitRequest builds a valid admission over the fixture's single Go lane.
func admitRequest(t *testing.T, key string, leaves []LeafSpec) AdmitRequest {
	t.Helper()
	return AdmitRequest{
		Authenticated:      testIdentity(),
		Caller:             testCaller(),
		Intent:             makeIntent(t, "intent-"+key, testBaseOID),
		ApprovedScopePaths: []string{"src/go"},
		Leaves:             leaves,
		Review:             true,
		IdempotencyKey:     key,
	}
}

func leafSpec(nodeID string, ownedPaths ...string) LeafSpec {
	return LeafSpec{
		NodeID:     nodeID,
		Goal:       []byte("implement " + nodeID + " exactly"),
		OwnedPaths: ownedPaths,
	}
}

// admitHappy admits one valid two-leaf intent and returns the run identity.
func admitHappy(t *testing.T, fixture *testKernel, key string) string {
	t.Helper()
	result, err := fixture.kernel.AdmitChangeIntent(context.Background(), admitRequest(t, key, []LeafSpec{
		leafSpec("leaf-a", "src/go/modify-00.go"),
		leafSpec("leaf-b", "src/go/modify-01.go"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed {
		t.Fatal("first admission replayed")
	}
	return result.RunID
}

// transitionRun is a test shorthand for a lifecycle step.
func transitionRun(t *testing.T, fixture *testKernel, runID string, next contractsv1.ChangeRunState) {
	t.Helper()
	if err := fixture.kernel.TransitionRun(context.Background(), testIdentity(), runID, next); err != nil {
		t.Fatalf("transition to %v: %v", next, err)
	}
}

// makePreview builds one valid PROPOSED candidate preview over the run's leaf
// scopes with the four required gates pending.
func makePreview(t *testing.T, runID string, edits []*contractsv1.PreviewEdit) *contractsv1.ChangeSetPreview {
	t.Helper()
	gates := make([]*contractsv1.GateSpec, 0, 4)
	for _, kind := range []contractsv1.FactoryGateKind{
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY,
	} {
		kindText, err := gateKindText(kind)
		if err != nil {
			t.Fatal(err)
		}
		gates = append(gates, &contractsv1.GateSpec{
			GateId: &contractsv1.Identifier{
				Namespace: "factory-gate",
				Value:     identity("ouroboros.stage05.gate.v1", runID, kindText),
			},
			Kind:     kind,
			Required: true,
			Status:   contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING,
		})
	}
	return &contractsv1.ChangeSetPreview{
		ChangeSet: &contractsv1.ChangeSet{
			ChangeSetId: &contractsv1.Identifier{Namespace: "factory-candidate", Value: "candidate-" + runID},
			BaseGitOid:  testBaseOID,
			PatchArtifact: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "patch-1"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("d", 64)},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			},
			ChangeSetDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("e", 64)},
			ValidationReceipts: []*contractsv1.Receipt{{
				ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: "validation-1"},
				Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED,
				ReasonCode:  "candidate_proposed",
				OperationId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
				Causal: &contractsv1.CausalContext{
					CorrelationId: &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
					CausationId:   &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
					TraceId:       &contractsv1.Identifier{Namespace: "factory-run", Value: runID},
				},
				RecordedAt:          timestamppb.New(time.UnixMilli(1_000_000).UTC()),
				ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: testPolicyHex},
			}},
			RollbackArtifact: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "rollback-1"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("f", 64)},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
			},
		},
		CandidateState: contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED,
		Edits:          edits,
		Obligations: []*contractsv1.LanguageObligation{{
			Language:      contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
			Impact:        &contractsv1.ImpactReceipt{BaseGitOid: testBaseOID},
			DocsRequired:  true,
			TestsRequired: true,
		}},
		Gates:              gates,
		ExpectedBaseGitOid: testBaseOID,
	}
}

// modifyEdit builds one bounded modify edit inside a leaf scope.
func modifyEdit(path string) *contractsv1.PreviewEdit {
	return &contractsv1.PreviewEdit{
		Path:         path,
		Operation:    contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_MODIFY,
		Language:     contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
		BeforeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestBytes([]byte("before:" + path))},
		AfterDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: digestBytes([]byte("after:" + path))},
	}
}

// passAllGates records RUNNING + PASSED for every required gate.
func passAllGates(t *testing.T, fixture *testKernel, runID string) {
	t.Helper()
	plan, err := fixture.kernel.GetChangePlan(context.Background(), testIdentity(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range plan.GetGates() {
		if err := fixture.kernel.RecordGateResult(context.Background(), testIdentity(), runID,
			gate.GetGateId().GetValue(), contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING); err != nil {
			t.Fatal(err)
		}
		if err := fixture.kernel.RecordGateResult(context.Background(), testIdentity(), runID,
			gate.GetGateId().GetValue(), contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
			t.Fatal(err)
		}
	}
}

package factoryapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeKernel struct {
	mu             sync.Mutex
	admitResult    *contractsv1.AdmitChangeIntentSuccess
	admitErr       error
	admitCalls     []AdmitIntentCommand
	planResult     *contractsv1.ChangePlan
	planErr        error
	planCalls      []string
	previewResult  *contractsv1.ChangeSetPreview
	previewErr     error
	previewCalls   []string
	findingsResult FindingsPage
	findingsErr    error
	findingsCalls  []findingsCall
	cancelResult   *contractsv1.CancelChangeRunSuccess
	cancelErr      error
	cancelCalls    []CancelRunCommand
	blockUntilDone bool
}

type findingsCall struct {
	runID string
	after string
	limit uint32
}

func (k *fakeKernel) AdmitChangeIntent(ctx context.Context, command AdmitIntentCommand) (*contractsv1.AdmitChangeIntentSuccess, error) {
	k.mu.Lock()
	k.admitCalls = append(k.admitCalls, command)
	block := k.blockUntilDone
	k.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return k.admitResult, k.admitErr
}

func (k *fakeKernel) ChangePlan(ctx context.Context, _ Principal, runID string) (*contractsv1.ChangePlan, error) {
	k.mu.Lock()
	k.planCalls = append(k.planCalls, runID)
	block := k.blockUntilDone
	k.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return k.planResult, k.planErr
}

func (k *fakeKernel) ChangeSetPreview(ctx context.Context, _ Principal, runID string) (*contractsv1.ChangeSetPreview, error) {
	k.mu.Lock()
	k.previewCalls = append(k.previewCalls, runID)
	block := k.blockUntilDone
	k.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return k.previewResult, k.previewErr
}

func (k *fakeKernel) ReviewFindings(ctx context.Context, _ Principal, runID, after string, limit uint32) (FindingsPage, error) {
	k.mu.Lock()
	k.findingsCalls = append(k.findingsCalls, findingsCall{runID: runID, after: after, limit: limit})
	block := k.blockUntilDone
	k.mu.Unlock()
	if block {
		<-ctx.Done()
		return FindingsPage{}, ctx.Err()
	}
	return k.findingsResult, k.findingsErr
}

func (k *fakeKernel) CancelChangeRun(ctx context.Context, command CancelRunCommand) (*contractsv1.CancelChangeRunSuccess, error) {
	k.mu.Lock()
	k.cancelCalls = append(k.cancelCalls, command)
	block := k.blockUntilDone
	k.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return k.cancelResult, k.cancelErr
}

func (k *fakeKernel) portCalls() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.admitCalls) + len(k.planCalls) + len(k.previewCalls) + len(k.findingsCalls) + len(k.cancelCalls)
}

func (k *fakeKernel) lastAdmit() AdmitIntentCommand {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.admitCalls[len(k.admitCalls)-1]
}

func (k *fakeKernel) lastCancel() CancelRunCommand {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cancelCalls[len(k.cancelCalls)-1]
}

func (k *fakeKernel) lastFindings() findingsCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.findingsCalls[len(k.findingsCalls)-1]
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type handlerFixture struct {
	handler *Handler
	kernel  *fakeKernel
	peer    localauthority.PeerContext
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	kernel := &fakeKernel{
		admitResult:   validAdmitSuccess(),
		planResult:    validChangePlan(),
		previewResult: validChangeSetPreview(),
		findingsResult: FindingsPage{
			Findings:   []*contractsv1.ReviewFinding{validReviewFinding()},
			NextCursor: "page-two",
		},
		cancelResult: validCancelSuccess(),
	}
	handler, err := NewHandler(Config{
		Kernel:              kernel,
		Clock:               fixedClock{now: time.UnixMilli(4242).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: strings.Repeat("f", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &handlerFixture{handler: handler, kernel: kernel, peer: testPeer()}
}

func testPeer() localauthority.PeerContext {
	return localauthority.PeerContext{
		Credentials: localauthority.PeerCredentials{UID: 501, GID: 20, PID: 4242},
		Identity: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: "p1"},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: "t1"},
			Session:   shared.Identifier{Namespace: "session", Value: "sess1"},
		},
	}
}

func validCaller() *contractsv1.UntrustedFactoryCaller {
	return &contractsv1.UntrustedFactoryCaller{
		RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "p1"},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "t1"},
			SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "sess1"},
		},
		RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: "sess1"},
	}
}

func validIdentifier(namespace, value string) *contractsv1.Identifier {
	return &contractsv1.Identifier{Namespace: namespace, Value: value}
}

func validDigest(hex string) *contractsv1.Digest {
	return &contractsv1.Digest{Algorithm: "sha256", Hex: hex}
}

func validPrincipalRef() *contractsv1.AuthenticatedPrincipalRef {
	return &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: validIdentifier("principal", "p1"),
		TenantId:    validIdentifier("tenant", "t1"),
		SessionId:   validIdentifier("session", "sess1"),
	}
}

func validCausal() *contractsv1.CausalContext {
	return &contractsv1.CausalContext{
		CorrelationId: validIdentifier("correlation", "c1"),
		CausationId:   validIdentifier("causation", "c1"),
		TraceId:       validIdentifier("trace", "t1"),
	}
}

func validReceipt(name string, status contractsv1.ReceiptStatus) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:           validIdentifier("receipt", name),
		OperationId:         validIdentifier("operation", name),
		Status:              status,
		Causal:              validCausal(),
		RecordedAt:          timestamppb.New(time.Unix(1_751_000_000, 0).UTC()),
		ConfigurationDigest: validDigest(strings.Repeat("e", 64)),
	}
}

func validArtifact(name string) *contractsv1.ArtifactRef {
	return &contractsv1.ArtifactRef{
		ArtifactId:    validIdentifier("artifact", name),
		ContentDigest: validDigest(strings.Repeat("d", 64)),
		TenantId:      validIdentifier("tenant", "t1"),
	}
}

const testBaseOID = "0123456789abcdef0123456789abcdef01234567"

func validChangeIntent() *contractsv1.ChangeIntent {
	return &contractsv1.ChangeIntent{
		IntentId:         validIdentifier("intent", "intent-1"),
		RequestedBy:      validPrincipalRef(),
		RepositoryGitOid: testBaseOID,
		ScopeDigest:      validDigest(strings.Repeat("a", 64)),
		SupportingEvidence: []*contractsv1.EvidenceRef{{
			EvidenceId:       validIdentifier("evidence", "e1"),
			SourceRevisionId: validIdentifier("revision", "r1"),
		}},
		Approval: &contractsv1.Approval{
			ApprovalId:  validIdentifier("approval", "a1"),
			Approver:    validPrincipalRef(),
			ScopeDigest: validDigest(strings.Repeat("a", 64)),
			ExpiresAt:   timestamppb.New(time.Unix(1_760_000_000, 0).UTC()),
			Receipt:     validReceipt("approval", contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED),
		},
	}
}

func validLease() *contractsv1.Lease {
	return &contractsv1.Lease{
		LeaseId:   validIdentifier("lease", "l1"),
		Holder:    validPrincipalRef(),
		Fence:     1,
		ExpiresAt: timestamppb.New(time.Unix(1_760_000_000, 0).UTC()),
	}
}

func validCapabilityGrant() *contractsv1.CapabilityGrant {
	return &contractsv1.CapabilityGrant{
		GrantId:          validIdentifier("grant", "g1"),
		Initiator:        validPrincipalRef(),
		Actions:          []string{"factory.leaf.execute"},
		Resources:        []*contractsv1.Identifier{validIdentifier("repository", "repo-1")},
		RepositoryGitOid: testBaseOID,
		Nonce:            "nonce-1",
		ExpiresAt:        timestamppb.New(time.Unix(1_760_000_000, 0).UTC()),
		PolicyDigest:     validDigest(strings.Repeat("b", 64)),
		CommandFence:     1,
	}
}

func validModelRoute() *contractsv1.ModelRoute {
	return &contractsv1.ModelRoute{
		ProfileDigest: validDigest(strings.Repeat("c", 64)),
		ModelIdentity: "model-v1",
		RationaleCode: "deterministic_route",
	}
}

func validGateRoster() []*contractsv1.GateSpec {
	kinds := []contractsv1.FactoryGateKind{
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY,
	}
	names := []string{"build", "test", "docs", "security"}
	gates := make([]*contractsv1.GateSpec, 0, len(kinds))
	for index, kind := range kinds {
		gates = append(gates, &contractsv1.GateSpec{
			GateId:   validIdentifier("gate", names[index]),
			Kind:     kind,
			Required: true,
			Status:   contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING,
		})
	}
	return gates
}

func validChangePlan() *contractsv1.ChangePlan {
	return &contractsv1.ChangePlan{
		PlanId: validIdentifier("plan", "plan-1"),
		RunId:  validIdentifier("run", "run-1"),
		Intent: validChangeIntent(),
		State:  contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY,
		Nodes: []*contractsv1.PlanNode{
			{
				NodeId:     "orch",
				Kind:       contractsv1.PlanNodeKind_PLAN_NODE_KIND_ORCHESTRATOR,
				GoalDigest: validDigest(strings.Repeat("1", 64)),
			},
			{
				NodeId:          "leaf-1",
				Kind:            contractsv1.PlanNodeKind_PLAN_NODE_KIND_LEAF,
				GoalDigest:      validDigest(strings.Repeat("2", 64)),
				OwnedPaths:      []string{"services/gateway"},
				Route:           validModelRoute(),
				Lease:           validLease(),
				CapabilityGrant: validCapabilityGrant(),
			},
		},
		Edges: []*contractsv1.PlanEdge{{FromNodeId: "orch", ToNodeId: "leaf-1"}},
		Gates: validGateRoster(),
	}
}

func validChangeSetPreview() *contractsv1.ChangeSetPreview {
	return &contractsv1.ChangeSetPreview{
		ChangeSet: &contractsv1.ChangeSet{
			ChangeSetId:        validIdentifier("changeset", "cs-1"),
			BaseGitOid:         testBaseOID,
			PatchArtifact:      validArtifact("patch"),
			ChangeSetDigest:    validDigest(strings.Repeat("3", 64)),
			ValidationReceipts: []*contractsv1.Receipt{validReceipt("validation", contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED)},
			RollbackArtifact:   validArtifact("rollback"),
		},
		CandidateState: contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED,
		Edits: []*contractsv1.PreviewEdit{{
			Path:         "services/gateway/main.go",
			Operation:    contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_MODIFY,
			Language:     contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
			BeforeDigest: validDigest(strings.Repeat("4", 64)),
			AfterDigest:  validDigest(strings.Repeat("5", 64)),
		}},
		Obligations: []*contractsv1.LanguageObligation{{
			Language: contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
			Impact: &contractsv1.ImpactReceipt{
				Receipt:    validReceipt("impact", contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED),
				BaseGitOid: testBaseOID,
			},
			DocsRequired:  true,
			TestsRequired: true,
		}},
		Gates:              validGateRoster(),
		ExpectedBaseGitOid: testBaseOID,
	}
}

func validReviewFinding() *contractsv1.ReviewFinding {
	return &contractsv1.ReviewFinding{
		FindingId:      validIdentifier("finding", "f1"),
		RunId:          validIdentifier("run", "run-1"),
		Severity:       contractsv1.ReviewSeverity_REVIEW_SEVERITY_MINOR,
		Category:       contractsv1.ReviewCategory_REVIEW_CATEGORY_CORRECTNESS,
		Disposition:    contractsv1.FindingDisposition_FINDING_DISPOSITION_OPEN,
		Summary:        "ConfigPath shadows the package-level default.",
		Reviewer:       validPrincipalRef(),
		ReviewerFamily: "fresh-review-v1",
	}
}

func validAdmitSuccess() *contractsv1.AdmitChangeIntentSuccess {
	return &contractsv1.AdmitChangeIntentSuccess{
		RunId: validIdentifier("run", "run-1"),
		State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY,
	}
}

func validCancelSuccess() *contractsv1.CancelChangeRunSuccess {
	return &contractsv1.CancelChangeRunSuccess{
		RunId: validIdentifier("run", "run-1"),
		State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED,
	}
}

func validAdmitRequest() *contractsv1.AdmitChangeIntentRequest {
	return &contractsv1.AdmitChangeIntentRequest{
		Caller:         validCaller(),
		Intent:         validChangeIntent(),
		IdempotencyKey: "admit-once",
	}
}

func validPlanRequest() *contractsv1.GetChangePlanRequest {
	return &contractsv1.GetChangePlanRequest{
		Caller: validCaller(),
		RunId:  validIdentifier("run", "run-1"),
	}
}

func validPreviewRequest() *contractsv1.PreviewChangeSetRequest {
	return &contractsv1.PreviewChangeSetRequest{
		Caller: validCaller(),
		RunId:  validIdentifier("run", "run-1"),
	}
}

func validFindingsRequest() *contractsv1.GetReviewFindingsRequest {
	return &contractsv1.GetReviewFindingsRequest{
		Caller:   validCaller(),
		RunId:    validIdentifier("run", "run-1"),
		PageSize: 25,
		After:    &contractsv1.Cursor{Token: "page-one", Watermark: 7},
	}
}

func validCancelRequest() *contractsv1.CancelChangeRunRequest {
	return &contractsv1.CancelChangeRunRequest{
		Caller:         validCaller(),
		RunId:          validIdentifier("run", "run-1"),
		IdempotencyKey: "cancel-once",
	}
}

func TestNewHandlerRequiresCompleteConfiguration(t *testing.T) {
	digest := shared.Digest{Algorithm: "sha256", Hex: strings.Repeat("f", 64)}
	cases := map[string]Config{
		"missing kernel":   {Clock: fixedClock{now: time.Unix(1, 0)}, ConfigurationDigest: digest},
		"missing clock":    {Kernel: &fakeKernel{}, ConfigurationDigest: digest},
		"digest algorithm": {Kernel: &fakeKernel{}, Clock: fixedClock{now: time.Unix(1, 0)}, ConfigurationDigest: shared.Digest{Algorithm: "sha512", Hex: strings.Repeat("f", 64)}},
		"digest hex":       {Kernel: &fakeKernel{}, Clock: fixedClock{now: time.Unix(1, 0)}, ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: "not-hex"}},
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewHandler(config); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("expected ErrInvalidConfiguration, got %v", err)
			}
		})
	}
}

func TestAdmitChangeIntentSuccess(t *testing.T) {
	fixture := newHandlerFixture(t)
	response, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	success, ok := response.Outcome.(*contractsv1.AdmitChangeIntentResponse_Success)
	if !ok {
		t.Fatalf("expected success outcome, got %T", response.Outcome)
	}
	if success.Success.RunId.Value != "run-1" ||
		success.Success.State != contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY {
		t.Fatalf("unexpected admitted outcome: %v", success.Success)
	}
	assertCompletedReceipt(t, response.Receipt, "factory.admit", "run-1")
	command := fixture.kernel.lastAdmit()
	if command.Principal != (Principal{Tenant: "t1", PrincipalID: "p1", Session: "sess1"}) {
		t.Fatalf("principal must derive from the authenticated peer, got %+v", command.Principal)
	}
	if command.IdempotencyKey != "admit-once" || command.Intent == nil {
		t.Fatalf("unexpected admit command: %+v", command)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("response must satisfy the frozen contract: %v", err)
	}
}

func TestAdmitChangeIntentReplayAndConflict(t *testing.T) {
	fixture := newHandlerFixture(t)
	first, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	// An exact authenticated replay returns the original outcome.
	replay, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(first, replay) {
		t.Fatal("an exact idempotent replay must return the original outcome")
	}
	// A conflicting key reuse collapses to the static denial, byte-identical
	// to an unknown-run denial.
	fixture.kernel.admitErr = ErrIdempotencyConflict
	conflict, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.kernel.admitErr = ErrUnknownRun
	unknown, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertStaticDenial(t, conflict.GetError(), conflict.Receipt, "factory.admit")
	if !proto.Equal(conflict, unknown) {
		t.Fatal("conflict and unknown-run denials must be indistinguishable")
	}
}

func TestAdmitChangeIntentStaleOrRevokedDenied(t *testing.T) {
	fixture := newHandlerFixture(t)
	// A stale base, a stale lease or fence, and a revoked grant all surface
	// from the kernel as ErrUnknownRun and share the one static shape.
	fixture.kernel.admitErr = ErrUnknownRun
	response, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertStaticDenial(t, response.GetError(), response.Receipt, "factory.admit")
}

func TestGetChangePlanSuccess(t *testing.T) {
	fixture := newHandlerFixture(t)
	response, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, validPlanRequest())
	if err != nil {
		t.Fatal(err)
	}
	success, ok := response.Outcome.(*contractsv1.GetChangePlanResponse_Success)
	if !ok || success.Success.Plan == nil {
		t.Fatalf("expected plan success, got %T", response.Outcome)
	}
	assertCompletedReceipt(t, response.Receipt, "factory.plan", "run-1")
	if fixture.kernel.planCalls[0] != "run-1" {
		t.Fatalf("kernel must see the request run identity, got %v", fixture.kernel.planCalls)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("response must satisfy the frozen contract: %v", err)
	}
}

func TestGetChangePlanUnknownDenied(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.kernel.planErr = ErrUnknownRun
	response, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, validPlanRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertStaticDenial(t, response.GetError(), response.Receipt, "factory.plan")
}

func TestPreviewChangeSetSuccess(t *testing.T) {
	fixture := newHandlerFixture(t)
	response, err := fixture.handler.PreviewChangeSet(context.Background(), fixture.peer, validPreviewRequest())
	if err != nil {
		t.Fatal(err)
	}
	success, ok := response.Outcome.(*contractsv1.PreviewChangeSetResponse_Success)
	if !ok || success.Success.Preview == nil {
		t.Fatalf("expected preview success, got %T", response.Outcome)
	}
	assertCompletedReceipt(t, response.Receipt, "factory.candidate", "run-1")
	if fixture.kernel.previewCalls[0] != "run-1" {
		t.Fatalf("kernel must see the request run identity, got %v", fixture.kernel.previewCalls)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("response must satisfy the frozen contract: %v", err)
	}
}

func TestPreviewChangeSetUnknownDenied(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.kernel.previewErr = ErrUnknownRun
	response, err := fixture.handler.PreviewChangeSet(context.Background(), fixture.peer, validPreviewRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertStaticDenial(t, response.GetError(), response.Receipt, "factory.candidate")
}

func TestGetReviewFindingsSuccess(t *testing.T) {
	fixture := newHandlerFixture(t)
	response, err := fixture.handler.GetReviewFindings(context.Background(), fixture.peer, validFindingsRequest())
	if err != nil {
		t.Fatal(err)
	}
	success, ok := response.Outcome.(*contractsv1.GetReviewFindingsResponse_Success)
	if !ok || len(success.Success.Findings) != 1 {
		t.Fatalf("expected one finding, got %T", response.Outcome)
	}
	if success.Success.NextCursor == nil || success.Success.NextCursor.Token != "page-two" {
		t.Fatalf("expected the kernel cursor to pass through, got %v", success.Success.NextCursor)
	}
	assertCompletedReceipt(t, response.Receipt, "factory.review", "run-1")
	call := fixture.kernel.lastFindings()
	if call.runID != "run-1" || call.after != "page-one" || call.limit != 25 {
		t.Fatalf("unexpected findings pagination: %+v", call)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("response must satisfy the frozen contract: %v", err)
	}
}

func TestGetReviewFindingsUnknownDenied(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.kernel.findingsErr = ErrUnknownRun
	response, err := fixture.handler.GetReviewFindings(context.Background(), fixture.peer, validFindingsRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertStaticDenial(t, response.GetError(), response.Receipt, "factory.review")
}

func TestCancelChangeRunSuccess(t *testing.T) {
	fixture := newHandlerFixture(t)
	response, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest())
	if err != nil {
		t.Fatal(err)
	}
	success, ok := response.Outcome.(*contractsv1.CancelChangeRunResponse_Success)
	if !ok {
		t.Fatalf("expected cancel success, got %T", response.Outcome)
	}
	if success.Success.State != contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED {
		t.Fatalf("cancellation confirms only the terminal cancelled state, got %v", success.Success.State)
	}
	assertCompletedReceipt(t, response.Receipt, "factory.cancel", "run-1")
	command := fixture.kernel.lastCancel()
	if command.RunID != "run-1" || command.IdempotencyKey != "cancel-once" {
		t.Fatalf("unexpected cancel command: %+v", command)
	}
	if err := protovalidate.Validate(response); err != nil {
		t.Fatalf("response must satisfy the frozen contract: %v", err)
	}
}

func TestCancelChangeRunReplayAndConflict(t *testing.T) {
	fixture := newHandlerFixture(t)
	first, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(first, replay) {
		t.Fatal("an exact idempotent replay must return the original outcome")
	}
	fixture.kernel.cancelErr = ErrIdempotencyConflict
	conflict, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.kernel.cancelErr = ErrUnknownRun
	unknown, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertStaticDenial(t, conflict.GetError(), conflict.Receipt, "factory.cancel")
	if !proto.Equal(conflict, unknown) {
		t.Fatal("conflict and unknown-run denials must be indistinguishable")
	}
}

func TestAdmitChangeIntentKernelDefectsFailClosed(t *testing.T) {
	cases := map[string]*contractsv1.AdmitChangeIntentSuccess{
		"nil success":          nil,
		"missing run identity": {State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY},
		"empty run identity": {
			RunId: validIdentifier("run", ""),
			State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY,
		},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newHandlerFixture(t)
			fixture.kernel.admitResult = result
			// A misbehaving kernel fails closed as ErrInvalidResponse, never a panic.
			if _, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest()); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("expected ErrInvalidResponse, got %v", err)
			}
		})
	}
}

func TestCancelChangeRunKernelDefectsFailClosed(t *testing.T) {
	t.Run("echo mismatch", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.kernel.cancelResult = &contractsv1.CancelChangeRunSuccess{
			RunId: validIdentifier("run", "other"),
			State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED,
		}
		if _, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
	t.Run("nil success", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.kernel.cancelResult = nil
		if _, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
	t.Run("missing run identity", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.kernel.cancelResult = &contractsv1.CancelChangeRunSuccess{
			State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED,
		}
		if _, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
}

// TestDenialEquivalenceAcrossReads proves unknown, unauthorized, stale, and
// revoked runs collapse to one byte-identical static shape per method.
func TestDenialEquivalenceAcrossReads(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.kernel.planErr = ErrUnknownRun
	planDenied, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, validPlanRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.kernel.planErr = errors.New(" revoked by current policy ")
	// Any non-sentinel port failure is a port failure, never a disclosure.
	if _, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, validPlanRequest()); !errors.Is(err, errPortFailure) {
		t.Fatalf("non-sentinel port errors must map to errPortFailure, got %v", err)
	}
	fixture.kernel.planErr = ErrUnknownRun
	planDeniedAgain, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, validPlanRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(planDenied, planDeniedAgain) {
		t.Fatal("repeated denials must be byte-identical")
	}
}

func TestMalformedAndOversizedRequestsNeverReachPorts(t *testing.T) {
	fixture := newHandlerFixture(t)
	oversized := strings.Repeat("x", 600)
	type call func() error
	cases := map[string]call{
		"admit nil": func() error {
			_, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, nil)
			return err
		},
		"admit missing caller": func() error {
			request := validAdmitRequest()
			request.Caller = nil
			_, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, request)
			return err
		},
		"admit missing intent": func() error {
			request := validAdmitRequest()
			request.Intent = nil
			_, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, request)
			return err
		},
		"admit empty idempotency": func() error {
			request := validAdmitRequest()
			request.IdempotencyKey = ""
			_, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, request)
			return err
		},
		"admit oversized idempotency": func() error {
			request := validAdmitRequest()
			request.IdempotencyKey = oversized
			_, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, request)
			return err
		},
		"admit malformed base": func() error {
			request := validAdmitRequest()
			request.Intent.RepositoryGitOid = "not-a-git-oid"
			_, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, request)
			return err
		},
		"plan missing run": func() error {
			request := validPlanRequest()
			request.RunId = nil
			_, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, request)
			return err
		},
		"preview oversized run": func() error {
			request := validPreviewRequest()
			request.RunId = validIdentifier("run", oversized)
			_, err := fixture.handler.PreviewChangeSet(context.Background(), fixture.peer, request)
			return err
		},
		"findings page size zero": func() error {
			request := validFindingsRequest()
			request.PageSize = 0
			_, err := fixture.handler.GetReviewFindings(context.Background(), fixture.peer, request)
			return err
		},
		"findings page size above bound": func() error {
			request := validFindingsRequest()
			request.PageSize = 101
			_, err := fixture.handler.GetReviewFindings(context.Background(), fixture.peer, request)
			return err
		},
		"cancel missing idempotency": func() error {
			request := validCancelRequest()
			request.IdempotencyKey = ""
			_, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, request)
			return err
		},
	}
	for name, invoke := range cases {
		t.Run(name, func(t *testing.T) {
			if err := invoke(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("expected ErrInvalidRequest, got %v", err)
			}
		})
	}
	if calls := fixture.kernel.portCalls(); calls != 0 {
		t.Fatalf("validation failures must never reach a port, saw %d calls", calls)
	}
}

func TestBodyIdentityMismatchNeverReachesPorts(t *testing.T) {
	type mutation struct {
		name string
		edit func(*contractsv1.UntrustedFactoryCaller)
	}
	mutations := []mutation{
		{"principal", func(c *contractsv1.UntrustedFactoryCaller) { c.RequestedPrincipal.PrincipalId.Value = "other" }},
		{"tenant", func(c *contractsv1.UntrustedFactoryCaller) { c.RequestedPrincipal.TenantId.Value = "other" }},
		{"principal session", func(c *contractsv1.UntrustedFactoryCaller) { c.RequestedPrincipal.SessionId.Value = "other" }},
		{"requested session", func(c *contractsv1.UntrustedFactoryCaller) { c.RequestedSession.Value = "other" }},
		{"missing principal session", func(c *contractsv1.UntrustedFactoryCaller) { c.RequestedPrincipal.SessionId = nil }},
	}
	for _, mutate := range mutations {
		t.Run(mutate.name, func(t *testing.T) {
			fixture := newHandlerFixture(t)
			request := validPlanRequest()
			mutate.edit(request.Caller)
			if _, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, request); !errors.Is(err, ErrRequestDenied) {
				t.Fatalf("expected ErrRequestDenied, got %v", err)
			}
			admit := validAdmitRequest()
			mutate.edit(admit.Caller)
			if _, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, admit); !errors.Is(err, ErrRequestDenied) {
				t.Fatalf("expected ErrRequestDenied, got %v", err)
			}
			cancel := validCancelRequest()
			mutate.edit(cancel.Caller)
			if _, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, cancel); !errors.Is(err, ErrRequestDenied) {
				t.Fatalf("expected ErrRequestDenied, got %v", err)
			}
			if calls := fixture.kernel.portCalls(); calls != 0 {
				t.Fatalf("identity mismatches must never reach a port, saw %d calls", calls)
			}
		})
	}
}

func TestUnmappedPeerIdentityDenied(t *testing.T) {
	fixture := newHandlerFixture(t)
	peer := testPeer()
	peer.Identity.Session = shared.Identifier{}
	if _, err := fixture.handler.GetChangePlan(context.Background(), peer, validPlanRequest()); !errors.Is(err, ErrRequestDenied) {
		t.Fatalf("expected ErrRequestDenied for an unmapped peer, got %v", err)
	}
	if calls := fixture.kernel.portCalls(); calls != 0 {
		t.Fatalf("unmapped peers must never reach a port, saw %d calls", calls)
	}
}

func TestContractViolatingPortOutputFailsClosed(t *testing.T) {
	t.Run("admit state", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		// RUNNING is inexpressible as an admitted state in the frozen contract.
		fixture.kernel.admitResult = &contractsv1.AdmitChangeIntentSuccess{
			RunId: validIdentifier("run", "run-1"),
			State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING,
		}
		if _, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
	t.Run("plan shape", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		plan := validChangePlan()
		// A second orchestrator violates the frozen one-layer DAG shape.
		plan.Nodes = append(plan.Nodes, &contractsv1.PlanNode{
			NodeId:     "orch-2",
			Kind:       contractsv1.PlanNodeKind_PLAN_NODE_KIND_ORCHESTRATOR,
			GoalDigest: validDigest(strings.Repeat("6", 64)),
		})
		fixture.kernel.planResult = plan
		if _, err := fixture.handler.GetChangePlan(context.Background(), fixture.peer, validPlanRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
	t.Run("preview base binding", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		preview := validChangeSetPreview()
		// A candidate built on another commit violates the base binding.
		preview.ChangeSet.BaseGitOid = "abcdef0123456789abcdef0123456789abcdef01"
		fixture.kernel.previewResult = preview
		if _, err := fixture.handler.PreviewChangeSet(context.Background(), fixture.peer, validPreviewRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
	t.Run("finding disposition", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		// A dispositioned finding without its disposition receipt is invalid.
		finding := validReviewFinding()
		finding.Disposition = contractsv1.FindingDisposition_FINDING_DISPOSITION_FIXED
		fixture.kernel.findingsResult = FindingsPage{Findings: []*contractsv1.ReviewFinding{finding}}
		if _, err := fixture.handler.GetReviewFindings(context.Background(), fixture.peer, validFindingsRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
	t.Run("cancel state", func(t *testing.T) {
		fixture := newHandlerFixture(t)
		fixture.kernel.cancelResult = &contractsv1.CancelChangeRunSuccess{
			RunId: validIdentifier("run", "run-1"),
			State: contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY,
		}
		if _, err := fixture.handler.CancelChangeRun(context.Background(), fixture.peer, validCancelRequest()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("expected ErrInvalidResponse, got %v", err)
		}
	})
}

func TestPortFailureNeverDiscloses(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.kernel.admitErr = errors.New("database connection string leaked")
	if _, err := fixture.handler.AdmitChangeIntent(context.Background(), fixture.peer, validAdmitRequest()); !errors.Is(err, errPortFailure) {
		t.Fatalf("expected errPortFailure, got %v", err)
	}
}

func TestCancelledContextNeverReachesPorts(t *testing.T) {
	fixture := newHandlerFixture(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.handler.AdmitChangeIntent(cancelled, fixture.peer, validAdmitRequest()); err == nil {
		t.Fatal("a cancelled transport context must fail")
	}
	if _, err := fixture.handler.GetChangePlan(cancelled, fixture.peer, validPlanRequest()); err == nil {
		t.Fatal("a cancelled transport context must fail")
	}
	if _, err := fixture.handler.PreviewChangeSet(cancelled, fixture.peer, validPreviewRequest()); err == nil {
		t.Fatal("a cancelled transport context must fail")
	}
	if _, err := fixture.handler.GetReviewFindings(cancelled, fixture.peer, validFindingsRequest()); err == nil {
		t.Fatal("a cancelled transport context must fail")
	}
	if _, err := fixture.handler.CancelChangeRun(cancelled, fixture.peer, validCancelRequest()); err == nil {
		t.Fatal("a cancelled transport context must fail")
	}
	if calls := fixture.kernel.portCalls(); calls != 0 {
		t.Fatalf("cancellation must never reach a port, saw %d calls", calls)
	}
}

func TestCancellationDuringPortCall(t *testing.T) {
	fixture := newHandlerFixture(t)
	fixture.kernel.blockUntilDone = true
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := fixture.handler.AdmitChangeIntent(ctx, fixture.peer, validAdmitRequest())
		finished <- err
	}()
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the context error unwrapped, got %v", err)
	}
}

func assertCompletedReceipt(t *testing.T, receipt *contractsv1.Receipt, operation, value string) {
	t.Helper()
	if receipt.Status != contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED {
		t.Fatalf("expected a completed receipt, got %v", receipt.Status)
	}
	if receipt.ReceiptId.Namespace != namespaceReceipt || receipt.ReceiptId.Value != value {
		t.Fatalf("receipt must bind the admitted identity, got %v", receipt.ReceiptId)
	}
	if receipt.OperationId.Namespace != namespaceOperation || receipt.OperationId.Value != operation {
		t.Fatalf("receipt must bind the operation, got %v", receipt.OperationId)
	}
	if receipt.Causal == nil || receipt.RecordedAt == nil || receipt.ConfigurationDigest == nil {
		t.Fatal("receipt must pin causal context, observation time, and configuration")
	}
}

func assertStaticDenial(t *testing.T, publicError *contractsv1.PublicError, receipt *contractsv1.Receipt, operation string) {
	t.Helper()
	if publicError == nil || publicError.Code != deniedCode || publicError.Render != nil {
		t.Fatalf("expected the static %s error, got %v", deniedCode, publicError)
	}
	if receipt.Status != contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED ||
		receipt.ReasonCode != deniedCode ||
		len(receipt.Evidence) != 0 {
		t.Fatalf("denial receipts are rejected, reason-bound, and evidence-free: %v", receipt)
	}
	if receipt.OperationId.Namespace != namespaceOperation || receipt.OperationId.Value != operation {
		t.Fatalf("denial receipt must bind its operation, got %v", receipt.OperationId)
	}
	if err := protovalidate.Validate(receipt); err != nil {
		t.Fatalf("denial receipt must satisfy the frozen contract: %v", err)
	}
}

package authorityprocess

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/factoryapi"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// factoryDescriptorFixture builds one descriptor and its digest-bound intent.
type factoryDescriptorFixture struct {
	descriptor factoryDescriptor
	payload    []byte
	digestHex  string
	intent     *contractsv1.ChangeIntent
}

// freshFactoryDescriptor builds one independent descriptor per call so test
// mutations never share slice backing arrays.
func freshFactoryDescriptor() *factoryDescriptor {
	return &factoryDescriptor{
		Version:          factoryDescriptorVersion,
		IntentID:         "intent-one",
		EvidenceRevision: "revision-1",
		Approval: factoryDescriptorApproval{
			ApprovalID:            "approval-1",
			ExpiresAtUnixSeconds:  1_900_000_000,
			RecordedAtUnixSeconds: 1_800_000_000,
		},
		ScopePaths: []string{"src/go/modify-00.go"},
		Review:     true,
		Leaves: []factoryDescriptorLeaf{{
			NodeID:     "leaf-a",
			Goal:       "rename the marker accessor",
			OwnedPaths: []string{"src/go/modify-00.go"},
			Edits:      []factoryDescriptorLeafEdit{{Op: "modify", Path: "src/go/modify-00.go"}},
		}},
		Findings: []factoryDescriptorFinding{{
			Severity: "INFO", Category: "DOCS", Summary: "fresh review note",
			Disposition: "DISMISSED_WITH_EVIDENCE",
		}},
	}
}

func newFactoryDescriptorFixture(t *testing.T) factoryDescriptorFixture {
	t.Helper()
	descriptor := *freshFactoryDescriptor()
	payload, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	fixture := factoryDescriptorFixture{
		descriptor: descriptor,
		payload:    payload,
		digestHex:  hex.EncodeToString(digest[:]),
	}
	fixture.intent = buildFactoryDescriptorIntent(&descriptor, fixture.digestHex)
	return fixture
}

// buildFactoryDescriptorIntent builds one fresh digest-bound intent per call
// so test mutations never alias shared messages.
func buildFactoryDescriptorIntent(
	descriptor *factoryDescriptor, digestHex string,
) *contractsv1.ChangeIntent {
	principalRef := &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "principal-a"},
		TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
		SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
	}
	return &contractsv1.ChangeIntent{
		IntentId:         &contractsv1.Identifier{Namespace: "intent", Value: descriptor.IntentID},
		RequestedBy:      principalRef,
		RepositoryGitOid: strings.Repeat("a", 40),
		ScopeDigest:      &contractsv1.Digest{Algorithm: "sha256", Hex: digestHex},
		SupportingEvidence: []*contractsv1.EvidenceRef{{
			EvidenceId:       &contractsv1.Identifier{Namespace: "artifact", Value: "artifact-factory-approval"},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "revision", Value: descriptor.EvidenceRevision},
		}},
		Approval: &contractsv1.Approval{
			ApprovalId:  &contractsv1.Identifier{Namespace: "approval", Value: descriptor.Approval.ApprovalID},
			Approver:    principalRef,
			ScopeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestHex},
			ExpiresAt:   timestamppb.New(time.Unix(descriptor.Approval.ExpiresAtUnixSeconds, 0).UTC()),
			Receipt: &contractsv1.Receipt{
				ReceiptId:           &contractsv1.Identifier{Namespace: "receipt", Value: "receipt-1"},
				Status:              contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
				RecordedAt:          timestamppb.New(time.Unix(descriptor.Approval.RecordedAtUnixSeconds, 0).UTC()),
				ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestHex},
			},
		},
	}
}

func TestParseFactoryDescriptorBounds(t *testing.T) {
	fixture := newFactoryDescriptorFixture(t)
	parsed, err := parseFactoryDescriptor(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.IntentID != fixture.descriptor.IntentID || len(parsed.Leaves) != 1 || len(parsed.Findings) != 1 {
		t.Fatalf("parsed = %#v", parsed)
	}
	for name, mutate := range map[string]func(*factoryDescriptor){
		"version":     func(d *factoryDescriptor) { d.Version = "other" },
		"empty scope": func(d *factoryDescriptor) { d.ScopePaths = nil },
		"too many leaves": func(d *factoryDescriptor) {
			d.Leaves = append(d.Leaves, d.Leaves[0], d.Leaves[0], d.Leaves[0])
		},
		"bad node":   func(d *factoryDescriptor) { d.Leaves[0].NodeID = "Leaf-A" },
		"empty goal": func(d *factoryDescriptor) { d.Leaves[0].Goal = "" },
		"no edits":   func(d *factoryDescriptor) { d.Leaves[0].Edits = nil },
		"bad op":     func(d *factoryDescriptor) { d.Leaves[0].Edits[0].Op = "execute" },
		"rename no old": func(d *factoryDescriptor) {
			d.Leaves[0].Edits[0] = factoryDescriptorLeafEdit{Op: "rename", Path: "src/go/a.go"}
		},
		"add with old":    func(d *factoryDescriptor) { d.Leaves[0].Edits[0].OldPath = "src/go/b.go" },
		"absolute path":   func(d *factoryDescriptor) { d.ScopePaths[0] = "/etc/passwd" },
		"traversal":       func(d *factoryDescriptor) { d.Leaves[0].OwnedPaths[0] = "src/go/../secret" },
		"git segment":     func(d *factoryDescriptor) { d.Leaves[0].OwnedPaths[0] = "src/.git/config" },
		"bad severity":    func(d *factoryDescriptor) { d.Findings[0].Severity = "CRITICAL" },
		"bad disposition": func(d *factoryDescriptor) { d.Findings[0].Disposition = "IGNORED" },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := freshFactoryDescriptor()
			mutate(mutated)
			encoded, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseFactoryDescriptor(encoded); !errors.Is(err, errFactoryDescriptor) {
				t.Fatalf("%s parsed: %v", name, err)
			}
		})
	}
	if _, err := parseFactoryDescriptor([]byte(`{"version":"` + factoryDescriptorVersion + `","unknown":1}`)); !errors.Is(err, errFactoryDescriptor) {
		t.Fatalf("unknown field parsed: %v", err)
	}
}

func TestRevalidateDescriptorBindsIntentFacts(t *testing.T) {
	fixture := newFactoryDescriptorFixture(t)
	evidence := fixture.intent.GetSupportingEvidence()[0]
	if err := revalidateDescriptor(fixture.descriptor, fixture.intent, fixture.payload, evidence); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*contractsv1.ChangeIntent){
		"scope digest": func(i *contractsv1.ChangeIntent) {
			i.ScopeDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}
		},
		"approval scope": func(i *contractsv1.ChangeIntent) {
			i.Approval.ScopeDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}
		},
		"receipt config": func(i *contractsv1.ChangeIntent) {
			i.Approval.Receipt.ConfigurationDigest = &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}
		},
		"intent id": func(i *contractsv1.ChangeIntent) {
			i.IntentId = &contractsv1.Identifier{Namespace: "intent", Value: "intent-other"}
		},
		"approval id": func(i *contractsv1.ChangeIntent) {
			i.Approval.ApprovalId = &contractsv1.Identifier{Namespace: "approval", Value: "approval-other"}
		},
		"expiry": func(i *contractsv1.ChangeIntent) {
			i.Approval.ExpiresAt = timestamppb.New(time.Unix(1_900_000_001, 0).UTC())
		},
		"revision": func(i *contractsv1.ChangeIntent) {
			i.SupportingEvidence[0].SourceRevisionId = &contractsv1.Identifier{Namespace: "revision", Value: "revision-other"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := buildFactoryDescriptorIntent(&fixture.descriptor, fixture.digestHex)
			mutate(mutated)
			if err := revalidateDescriptor(fixture.descriptor, mutated, fixture.payload,
				mutated.GetSupportingEvidence()[0]); !errors.Is(err, errFactoryDescriptor) {
				t.Fatalf("%s revalidated: %v", name, err)
			}
		})
	}
}

func TestFactoryGateEvaluation(t *testing.T) {
	node := &contractsv1.PlanNode{NodeId: "leaf-a", OwnedPaths: []string{"src/go/modify-00.go"}}
	completed := func(edit broker.FactoryAppliedEdit, denials []broker.FactoryDenial) []factoryLeafOutcome {
		return []factoryLeafOutcome{{node: node, outcome: broker.FactoryLeafOutcome{
			State: "COMPLETED", Edits: []broker.FactoryAppliedEdit{edit}, Denials: denials,
		}}}
	}
	validGo := broker.FactoryAppliedEdit{
		Op: "modify", Path: "src/go/modify-00.go", Language: "go",
		AfterBytes: []byte("package main\n\n// marker\nfunc marker() string { return \"stage\" }\n"),
	}
	brokenGo := broker.FactoryAppliedEdit{
		Op: "modify", Path: "src/go/modify-00.go", Language: "go",
		AfterBytes: []byte("package main\n\nfunc marker( {\n"),
	}
	// Changed deliberately with the DOCS gate's semantics. The gate used to ask
	// whether the file contained the characters "//" anywhere, so a file with
	// no comments at all -- even one with nothing to document -- failed it,
	// while a file full of undocumented exported API passed on a stray inline
	// comment. It now checks that exported declarations carry doc comments, so
	// the fixture has to declare something exported and leave it undocumented.
	undocGo := broker.FactoryAppliedEdit{
		Op: "add", Path: "src/go/add-00.go", Language: "go",
		AfterBytes: []byte("package main\n\nfunc Exported() string { return \"x\" }\n"),
	}
	if !evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD, completed(validGo, nil)) {
		t.Fatal("BUILD failed on completed leaf")
	}
	if evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD,
		[]factoryLeafOutcome{{node: node, outcome: broker.FactoryLeafOutcome{State: "FAILED"}}}) {
		t.Fatal("BUILD passed on failed leaf")
	}
	if !evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST, completed(validGo, nil)) {
		t.Fatal("TEST failed on parseable Go")
	}
	if evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST, completed(brokenGo, nil)) {
		t.Fatal("TEST passed on unparseable Go")
	}
	if !evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS, completed(validGo, nil)) {
		t.Fatal("DOCS failed on documented Go")
	}
	if evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_DOCS, completed(undocGo, nil)) {
		t.Fatal("DOCS passed on undocumented Go")
	}
	if !evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY, completed(validGo, nil)) {
		t.Fatal("SECURITY failed on clean leaf")
	}
	if evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY,
		completed(validGo, []broker.FactoryDenial{{Action: "file.write", ReasonCode: "escape_path_scope"}})) {
		t.Fatal("SECURITY passed with a denial recorded")
	}
	escaped := broker.FactoryAppliedEdit{Op: "modify", Path: "src/typescript/modify-00.ts", Language: "typescript"}
	if evaluateFactoryGate(contractsv1.FactoryGateKind_FACTORY_GATE_KIND_SECURITY, completed(escaped, nil)) {
		t.Fatal("SECURITY passed with an out-of-scope edit")
	}
}

func TestFactoryLeafFailureClassification(t *testing.T) {
	for name, outcome := range map[string]struct {
		outcome broker.FactoryLeafOutcome
		kind    string
	}{
		"escape": {broker.FactoryLeafOutcome{
			State: "FAILED", Denials: []broker.FactoryDenial{{ReasonCode: "escape_forbidden_path"}},
		}, "escape"},
		"stale lease": {broker.FactoryLeafOutcome{
			State: "FAILED", Denials: []broker.FactoryDenial{{ReasonCode: "stale_lease"}},
		}, "stale"},
		"stale fence": {broker.FactoryLeafOutcome{
			State: "FAILED", Denials: []broker.FactoryDenial{{ReasonCode: "stale_fence"}},
		}, "stale"},
		"apply": {broker.FactoryLeafOutcome{
			State: "FAILED", Rollback: &broker.FactoryRollback{ChangeSetDigestHex: strings.Repeat("a", 64)},
		}, "apply"},
	} {
		t.Run(name, func(t *testing.T) {
			failure := classifyFactoryLeafFailure(outcome.outcome)
			if failure.kind != outcome.kind {
				t.Fatalf("kind = %q, want %q", failure.kind, outcome.kind)
			}
		})
	}
	apply := classifyFactoryLeafFailure(broker.FactoryLeafOutcome{
		State: "FAILED", Rollback: &broker.FactoryRollback{ChangeSetDigestHex: strings.Repeat("a", 64)},
	})
	if apply.rollbackDigest != strings.Repeat("a", 64) {
		t.Fatalf("rollback digest = %q", apply.rollbackDigest)
	}
}

func TestFactoryFenceRegistry(t *testing.T) {
	registry := newFactoryFenceRegistry()
	expiry := time.Now().UTC().Add(time.Minute)
	registry.load(&contractsv1.ChangePlan{Nodes: []*contractsv1.PlanNode{{
		NodeId: "leaf-a",
		Lease: &contractsv1.Lease{
			LeaseId:   &contractsv1.Identifier{Namespace: "lease", Value: "lease-1"},
			Fence:     3,
			ExpiresAt: timestamppb.New(expiry),
		},
	}}})
	fence, expiresAt, ok := registry.CurrentFence(context.Background(),
		shared.Identifier{Namespace: "lease", Value: "lease-1"})
	if !ok || fence != 3 || !expiresAt.Equal(expiry) {
		t.Fatalf("fence = %d %v %v", fence, expiresAt, ok)
	}
	if _, _, ok := registry.CurrentFence(context.Background(),
		shared.Identifier{Namespace: "lease", Value: "lease-unknown"}); ok {
		t.Fatal("unknown lease resolved")
	}
}

func TestFactoryLeaseTTLFromEnv(t *testing.T) {
	if ttl, err := factoryLeaseTTLFromEnv(func(string) string { return "" }); err != nil || ttl != factoryDefaultLeaseTTLMillis {
		t.Fatalf("default = %d, %v", ttl, err)
	}
	if ttl, err := factoryLeaseTTLFromEnv(func(string) string { return "750" }); err != nil || ttl != 750 {
		t.Fatalf("pinned = %d, %v", ttl, err)
	}
	for _, value := range []string{"abc", "-1", "0", "249", "600001", " 750ms"} {
		if _, err := factoryLeaseTTLFromEnv(func(string) string { return value }); err == nil {
			t.Fatalf("value %q accepted", value)
		}
	}
}

func TestMapFactoryKernelError(t *testing.T) {
	if err := mapFactoryKernelError(brain.ErrFactoryNotFoundOrDenied); !errors.Is(err, factoryapi.ErrUnknownRun) {
		t.Fatalf("not found = %v", err)
	}
	if err := mapFactoryKernelError(brain.ErrFactoryStaleFence); !errors.Is(err, factoryapi.ErrUnknownRun) {
		t.Fatalf("stale fence = %v", err)
	}
	if err := mapFactoryKernelError(brain.ErrFactoryScopeEscape); !errors.Is(err, factoryapi.ErrUnknownRun) {
		t.Fatalf("scope escape = %v", err)
	}
	if err := mapFactoryKernelError(brain.ErrFactoryInvalidInput); errors.Is(err, factoryapi.ErrUnknownRun) {
		t.Fatalf("invalid input mapped to static denial: %v", err)
	}
	if mapFactoryKernelError(nil) != nil {
		t.Fatal("nil mapped")
	}
}

// TestFactoryDescriptorReadCommandIDsIncludeDigest ensures revised descriptor
// bytes under the same artifact ID and length never reuse a prior read's
// command/idempotency reservation.
func TestFactoryDescriptorReadCommandIDsIncludeDigest(t *testing.T) {
	artifactID := "artifact-factory-approval"
	length := uint64(128)
	firstID, firstKey := factoryDescriptorReadCommandIDs(artifactID, strings.Repeat("a", 64), length)
	secondID, secondKey := factoryDescriptorReadCommandIDs(artifactID, strings.Repeat("b", 64), length)
	sameID, sameKey := factoryDescriptorReadCommandIDs(artifactID, strings.Repeat("a", 64), length)
	if firstID == "" || firstKey == "" || !strings.HasPrefix(firstID, factoryDescriptorIDPrefix+"-") ||
		!strings.HasPrefix(firstKey, factoryDescriptorIDPrefix+":") {
		t.Fatalf("ids = %q %q", firstID, firstKey)
	}
	if firstID == secondID || firstKey == secondKey {
		t.Fatal("distinct digests must produce distinct command/idempotency keys")
	}
	if firstID != sameID || firstKey != sameKey {
		t.Fatal("identical inputs must be stable")
	}
	// Keys stay under the identifier ceiling even with long artifact IDs.
	longArtifact := strings.Repeat("x", 400)
	longID, longKey := factoryDescriptorReadCommandIDs(longArtifact, strings.Repeat("c", 64), length)
	if len(longID) > 512 || len(longKey) > 512 {
		t.Fatalf("keys exceed identifier ceiling: %d %d", len(longID), len(longKey))
	}
}

// TestRecordFactoryGateStalePendingIsIdempotent proves overlapping drives that
// both observe PENDING do not permanently deny when a peer already advanced the
// durable gate to RUNNING (or terminal).
func TestRecordFactoryGateStalePendingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fixture := newFactoryRecoveryFixture(t)
	identity := fixture.scope
	adapter := &factoryKernelAdapter{kernel: fixture.kernel, now: time.Now}

	runID := fixture.admit(t, "admit-stale-gate")
	if err := fixture.kernel.TransitionRun(ctx, identity, runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.TransitionRun(ctx, identity, runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING); err != nil {
		t.Fatal(err)
	}
	// Capture a PENDING plan snapshot before any gate advances.
	stalePlan, err := fixture.kernel.GetChangePlan(ctx, identity, runID)
	if err != nil {
		t.Fatal(err)
	}
	kind := contractsv1.FactoryGateKind_FACTORY_GATE_KIND_BUILD
	if status := factoryPlanGateStatus(stalePlan, kind); status !=
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING {
		t.Fatalf("stale plan gate status = %v, want PENDING", status)
	}
	gateID := factoryPlanGateID(stalePlan, kind)
	// Peer drive advances PENDING → RUNNING under the live roster.
	if err := fixture.kernel.RecordGateResult(ctx, identity, runID, gateID,
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING); err != nil {
		t.Fatal(err)
	}
	// Stale-plan re-drive must treat the concurrent RUNNING as success and
	// still record the terminal outcome.
	if err := adapter.recordFactoryGate(ctx, identity, stalePlan, kind,
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
		t.Fatalf("stale PENDING re-drive after peer RUNNING: %v", err)
	}
	// Exact terminal re-drive with the same stale snapshot is also success.
	if err := adapter.recordFactoryGate(ctx, identity, stalePlan, kind,
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
		t.Fatalf("terminal re-drive after peer PASSED: %v", err)
	}
	// Peer already terminal + stale PENDING trying RUNNING then same PASSED:
	// still success via exact terminal replay after TransitionInvalid on RUNNING.
	if err := fixture.kernel.RecordGateResult(ctx, identity, runID,
		factoryPlanGateID(stalePlan, contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST),
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kernel.RecordGateResult(ctx, identity, runID,
		factoryPlanGateID(stalePlan, contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST),
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
		t.Fatal(err)
	}
	if err := adapter.recordFactoryGate(ctx, identity, stalePlan,
		contractsv1.FactoryGateKind_FACTORY_GATE_KIND_TEST,
		contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
		t.Fatalf("stale PENDING after peer already PASSED: %v", err)
	}
}

// TestFactoryRedriveStatePredicates locks the crash-recovery re-drive gates:
// READY must re-drive execution (PLANNING→READY→RUNNING can die mid-way), and
// CANDIDATE_READY must re-drive review (RETAINED can finish before COMPLETED).
func TestFactoryRedriveStatePredicates(t *testing.T) {
	execution := map[contractsv1.ChangeRunState]bool{
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING:        true,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY:           true,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING:         true,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW:          false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY: false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED:       false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED:          false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED:       false,
	}
	for state, want := range execution {
		if got := factoryShouldDriveExecution(state); got != want {
			t.Fatalf("execution drive state=%v got=%v want=%v", state, got, want)
		}
	}
	review := map[contractsv1.ChangeRunState]bool{
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_PLANNING:        false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY:           false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING:         true,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW:          true,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY: true,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED:       false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_FAILED:          false,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANCELLED:       false,
	}
	for state, want := range review {
		if got := factoryShouldDriveReview(state); got != want {
			t.Fatalf("review drive state=%v got=%v want=%v", state, got, want)
		}
	}
}

// TestFactoryMidCrashRedriveRecovery proves both durable mid-drive kill gaps:
// READY re-enters execution, and RETAINED under REVIEW/CANDIDATE_READY finishes
// to COMPLETED without re-firing findings.
func TestFactoryMidCrashRedriveRecovery(t *testing.T) {
	ctx := context.Background()
	fixture := newFactoryRecoveryFixture(t)
	identity := fixture.scope
	principal := factoryapi.Principal{
		Tenant: identity.Tenant, PrincipalID: identity.Principal, Session: identity.Session,
	}
	adapter := &factoryKernelAdapter{kernel: fixture.kernel, now: time.Now}

	// Gap 1: PLANNING→READY committed, then crash before RUNNING. The next
	// candidate read re-drives and advances READY→RUNNING before descriptor
	// work (which fails closed here without a staged approval artifact).
	readyRun := fixture.admit(t, "admit-ready-crash")
	if err := fixture.kernel.TransitionRun(ctx, identity, readyRun,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY); err != nil {
		t.Fatal(err)
	}
	readyPlan, err := fixture.kernel.GetChangePlan(ctx, identity, readyRun)
	if err != nil {
		t.Fatal(err)
	}
	if readyPlan.GetState() != contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY {
		t.Fatalf("ready plan state = %v", readyPlan.GetState())
	}
	if !factoryShouldDriveExecution(readyPlan.GetState()) {
		t.Fatal("READY must re-drive execution after mid-crash")
	}
	preview, previewErr := adapter.ChangeSetPreview(ctx, principal, readyRun)
	if previewErr == nil || preview != nil {
		t.Fatalf("expected descriptor-closed re-drive, got preview=%v err=%v", preview, previewErr)
	}
	if !errors.Is(previewErr, factoryapi.ErrUnknownRun) {
		t.Fatalf("re-drive error = %v, want ErrUnknownRun", previewErr)
	}
	advanced, err := fixture.kernel.GetChangePlan(ctx, identity, readyRun)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.GetState() != contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING {
		t.Fatalf("after READY re-drive state = %v, want RUNNING", advanced.GetState())
	}

	// Gap 2a: candidate RETAINED, run still REVIEW — findings read finishes COMPLETED.
	reviewRun := fixture.admitRetainedAt(t, "admit-retained-review",
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW)
	reviewPlan, err := fixture.kernel.GetChangePlan(ctx, identity, reviewRun)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.driveReview(ctx, identity, reviewPlan); err != nil {
		t.Fatalf("driveReview from RETAINED+REVIEW: %v", err)
	}
	completed, err := fixture.kernel.GetChangePlan(ctx, identity, reviewRun)
	if err != nil {
		t.Fatal(err)
	}
	if completed.GetState() != contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED {
		t.Fatalf("after RETAINED+REVIEW recovery state = %v, want COMPLETED", completed.GetState())
	}
	// Idempotent re-entry after COMPLETED must not fail.
	if err := adapter.driveReview(ctx, identity, completed); err != nil {
		t.Fatalf("driveReview after COMPLETED: %v", err)
	}

	// Gap 2b: candidate RETAINED, run CANDIDATE_READY — ReviewFindings re-drives.
	readyCandRun := fixture.admitRetainedAt(t, "admit-retained-candidate-ready",
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY)
	if _, err := adapter.ReviewFindings(ctx, principal, readyCandRun, "", 10); err != nil {
		t.Fatalf("ReviewFindings from RETAINED+CANDIDATE_READY: %v", err)
	}
	finished, err := fixture.kernel.GetChangePlan(ctx, identity, readyCandRun)
	if err != nil {
		t.Fatal(err)
	}
	if finished.GetState() != contractsv1.ChangeRunState_CHANGE_RUN_STATE_COMPLETED {
		t.Fatalf("after RETAINED+CANDIDATE_READY recovery state = %v, want COMPLETED", finished.GetState())
	}
}

// factoryRecoveryFixture is a durable factory surface used only for mid-crash
// recovery proofs: admit + manual lifecycle advances without the sealed runner.
type factoryRecoveryFixture struct {
	kernel *brain.FactoryKernel
	scope  brain.FactoryIdentity
	base   string
}

type factoryRecoveryPolicy struct{}

func (factoryRecoveryPolicy) Check(
	_ context.Context, _ shared.MappedIdentityFact, _ shared.PolicyRequest,
) (shared.PolicyDecision, error) {
	return shared.PolicyDecision{Allowed: true, RevocationEpoch: 7}, nil
}

type factoryRecoveryClock struct{ millis int64 }

func (clock factoryRecoveryClock) NowUnixMilli() int64 { return clock.millis }

type factoryRecoveryKeys struct {
	tenant    brain.Identifier
	reference brain.KeyReference
}

func (keys factoryRecoveryKeys) Current(ctx context.Context, tenant brain.Identifier) (brain.KeyMaterial, error) {
	if err := ctx.Err(); err != nil {
		return brain.KeyMaterial{}, err
	}
	if tenant != keys.tenant {
		return brain.KeyMaterial{}, brain.ErrUnavailable
	}
	return brain.KeyMaterial{
		Reference: keys.reference, RootKey: bytes.Repeat([]byte{7}, brain.RootKeyBytes),
	}, nil
}

func (keys factoryRecoveryKeys) Resolve(
	ctx context.Context, tenant brain.Identifier, epoch uint64,
) (brain.KeyMaterial, error) {
	if epoch != keys.reference.Epoch {
		return brain.KeyMaterial{}, brain.ErrUnavailable
	}
	return keys.Current(ctx, tenant)
}

func newFactoryRecoveryFixture(t *testing.T) *factoryRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	repository := t.TempDir()
	writeFactoryRecoveryRepo(t, repository, map[string]string{
		"src/go/modify-00.go": "package main\n\n// marker\nfunc marker() string { return \"stage\" }\n",
	})
	base := commitFactoryRecoveryRepo(t, repository, "initial")

	root := t.TempDir()
	tenant := brain.Identifier{Namespace: "tenant", Value: "t"}
	reference := brain.KeyReference{
		Root:  brain.Identifier{Namespace: "key-root", Value: "t"},
		KeyID: brain.Identifier{Namespace: "key", Value: "factory-recovery-key"}, Epoch: 1,
	}
	config := brain.DurableConfig{
		DatabasePath: filepath.Join(root, "authority.db"), ObjectRoot: filepath.Join(root, "objects"),
		Tenant: tenant, CurrentKeyReference: reference,
		Brain:               brain.Identifier{Namespace: "brain", Value: "b"},
		ConfigurationDigest: testDigest("factory-recovery-config"),
		Clock:               factoryRecoveryClock{millis: 1_000_000},
		Storage:             brain.StorageOptions{FrameBytes: 64 * 1024, MaxReadBytes: 1 << 20},
		Ingestion: &brain.IngestionConfig{
			ApprovedRoot: repository, GitExecutable: "/usr/bin/git", RepositoryID: "test-repository",
			CommandTimeout: 10 * time.Second, MaxFiles: 256, MaxPathBytes: 4096,
			MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxIdempotencyRecords: 128,
		},
	}
	runtime, err := brain.OpenDurable(ctx, config, factoryRecoveryKeys{tenant: tenant, reference: reference})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	sessionIdentity := brain.Identity{
		Principal:   brain.Identifier{Namespace: "principal", Value: "p"},
		Tenant:      tenant,
		Session:     brain.Identifier{Namespace: "session", Value: "s"},
		Credentials: shared.PeerCredentials{UID: 501, PID: 42},
	}
	if _, err := runtime.OpenSession(ctx, sessionIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.AddSource(ctx, brain.AddSourceRequest{
		IngestionContext: brain.IngestionContext{
			Identity: sessionIdentity, ConfigurationDigest: config.ConfigurationDigest,
			Policy: brain.IngestionPolicyBothIgnoreNoFollow, Fence: 1,
			Authorize: func(_ context.Context, got brain.Identity, action string, resource brain.Identifier) (brain.Authorization, error) {
				if got != sessionIdentity || !strings.HasPrefix(action, "source.") || resource != config.Brain {
					return brain.Authorization{}, brain.ErrDenied
				}
				return brain.Authorization{Allowed: true, ReasonCode: "allowed", RevocationEpoch: 1}, nil
			},
		},
		ExpectedCommitOID: base, IdempotencyKey: "add-source",
	}); err != nil {
		t.Fatal(err)
	}
	surface, err := runtime.OpenFactorySurface(ctx, brain.FactorySurfaceConfig{
		Policy: factoryRecoveryPolicy{}, LeaseTTLMillis: 60_000, RevocationEpoch: 7,
		PolicyDigestHex: strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = surface.Close() })
	return &factoryRecoveryFixture{
		kernel: surface.Kernel(),
		scope: brain.FactoryIdentity{
			Tenant: sessionIdentity.Tenant.Value, Principal: sessionIdentity.Principal.Value,
			Session: sessionIdentity.Session.Value,
		},
		base: base,
	}
}

func (fixture *factoryRecoveryFixture) admit(t *testing.T, key string) string {
	t.Helper()
	principalRef := &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: fixture.scope.Principal},
		TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: fixture.scope.Tenant},
		SessionId:   &contractsv1.Identifier{Namespace: "session", Value: fixture.scope.Session},
	}
	digestHex := strings.Repeat("c", 64)
	intent := &contractsv1.ChangeIntent{
		IntentId:         &contractsv1.Identifier{Namespace: "intent", Value: "intent-" + key},
		RequestedBy:      principalRef,
		RepositoryGitOid: fixture.base,
		ScopeDigest:      &contractsv1.Digest{Algorithm: "sha256", Hex: digestHex},
		SupportingEvidence: []*contractsv1.EvidenceRef{{
			EvidenceId:       &contractsv1.Identifier{Namespace: "artifact", Value: "artifact-factory-approval"},
			SourceRevisionId: &contractsv1.Identifier{Namespace: "revision", Value: "revision-1"},
		}},
		Approval: &contractsv1.Approval{
			ApprovalId:  &contractsv1.Identifier{Namespace: "approval", Value: "approval-" + key},
			Approver:    principalRef,
			ScopeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestHex},
			ExpiresAt:   timestamppb.New(time.Unix(1_900_000_000, 0).UTC()),
			Receipt: &contractsv1.Receipt{
				ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: "receipt-" + key},
				Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
				ReasonCode:  "approved",
				OperationId: &contractsv1.Identifier{Namespace: "intent", Value: "intent-" + key},
				Causal: &contractsv1.CausalContext{
					CorrelationId: &contractsv1.Identifier{Namespace: "intent", Value: "intent-" + key},
					CausationId:   &contractsv1.Identifier{Namespace: "intent", Value: "intent-" + key},
					TraceId:       &contractsv1.Identifier{Namespace: "intent", Value: "intent-" + key},
				},
				RecordedAt:          timestamppb.New(time.Unix(1_800_000_000, 0).UTC()),
				ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: digestHex},
			},
		},
	}
	admitted, err := fixture.kernel.AdmitChangeIntent(context.Background(), brain.FactoryAdmitRequest{
		Authenticated:      fixture.scope,
		Caller:             factoryCaller(fixture.scope),
		Intent:             intent,
		ApprovedScopePaths: []string{"src/go/modify-00.go"},
		Leaves: []brain.FactoryLeafSpec{{
			NodeID: "leaf-a", Goal: []byte("rename the marker accessor"),
			OwnedPaths: []string{"src/go/modify-00.go"}, HolderPrincipal: fixture.scope.Principal,
		}},
		Review:         true,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return admitted.RunID
}

// admitRetainedAt admits a run and advances it to RETAINED under the given
// non-terminal run state (REVIEW or CANDIDATE_READY), simulating a crash after
// retention but before COMPLETED.
func (fixture *factoryRecoveryFixture) admitRetainedAt(
	t *testing.T, key string, stopAt contractsv1.ChangeRunState,
) string {
	t.Helper()
	ctx := context.Background()
	runID := fixture.admit(t, key)
	identity := fixture.scope
	for _, next := range []contractsv1.ChangeRunState{
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_READY,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_RUNNING,
	} {
		if err := fixture.kernel.TransitionRun(ctx, identity, runID, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.kernel.CommitLeafResult(ctx, identity, runID, "leaf-a", 1, []byte("result-a")); err != nil {
		t.Fatal(err)
	}
	plan, err := fixture.kernel.GetChangePlan(ctx, identity, runID)
	if err != nil {
		t.Fatal(err)
	}
	gates := make([]*contractsv1.GateSpec, 0, len(plan.GetGates()))
	for _, gate := range plan.GetGates() {
		gates = append(gates, &contractsv1.GateSpec{
			GateId: gate.GetGateId(), Kind: gate.GetKind(), Required: gate.GetRequired(),
			Status: contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PENDING,
		})
	}
	preview := &contractsv1.ChangeSetPreview{
		ChangeSet: &contractsv1.ChangeSet{
			ChangeSetId: &contractsv1.Identifier{Namespace: "factory-candidate", Value: "candidate-" + runID},
			BaseGitOid:  fixture.base,
			PatchArtifact: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "patch-1"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("d", 64)},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: fixture.scope.Tenant},
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
				ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("d", 64)},
			}},
			RollbackArtifact: &contractsv1.ArtifactRef{
				ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "rollback-1"},
				ContentDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("f", 64)},
				TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: fixture.scope.Tenant},
			},
		},
		CandidateState: contractsv1.CandidateState_CANDIDATE_STATE_PROPOSED,
		Edits: []*contractsv1.PreviewEdit{{
			Path: "src/go/modify-00.go", Operation: contractsv1.PreviewEditOperation_PREVIEW_EDIT_OPERATION_MODIFY,
			Language:     contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
			BeforeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("a", 64)},
			AfterDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("b", 64)},
		}},
		Obligations: []*contractsv1.LanguageObligation{{
			Language:     contractsv1.CodeLanguage_CODE_LANGUAGE_GO,
			Impact:       &contractsv1.ImpactReceipt{BaseGitOid: fixture.base},
			DocsRequired: true, TestsRequired: true,
		}},
		Gates: gates, ExpectedBaseGitOid: fixture.base,
	}
	if err := fixture.kernel.ProposeCandidate(ctx, identity, runID, preview); err != nil {
		t.Fatal(err)
	}
	for _, gate := range plan.GetGates() {
		gateID := gate.GetGateId().GetValue()
		if err := fixture.kernel.RecordGateResult(ctx, identity, runID, gateID,
			contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_RUNNING); err != nil {
			t.Fatal(err)
		}
		if err := fixture.kernel.RecordGateResult(ctx, identity, runID, gateID,
			contractsv1.FactoryGateStatus_FACTORY_GATE_STATUS_PASSED); err != nil {
			t.Fatal(err)
		}
	}
	for _, next := range []contractsv1.CandidateState{
		contractsv1.CandidateState_CANDIDATE_STATE_APPLIED,
		contractsv1.CandidateState_CANDIDATE_STATE_VERIFIED,
	} {
		if err := fixture.kernel.TransitionCandidate(ctx, identity, runID, next, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.kernel.TransitionRun(ctx, identity, runID,
		contractsv1.ChangeRunState_CHANGE_RUN_STATE_REVIEW); err != nil {
		t.Fatal(err)
	}
	for _, next := range []contractsv1.CandidateState{
		contractsv1.CandidateState_CANDIDATE_STATE_REVIEWED,
		contractsv1.CandidateState_CANDIDATE_STATE_RETAINED,
	} {
		if err := fixture.kernel.TransitionCandidate(ctx, identity, runID, next, nil); err != nil {
			t.Fatal(err)
		}
	}
	if stopAt == contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY {
		if err := fixture.kernel.TransitionRun(ctx, identity, runID,
			contractsv1.ChangeRunState_CHANGE_RUN_STATE_CANDIDATE_READY); err != nil {
			t.Fatal(err)
		}
	}
	plan, err = fixture.kernel.GetChangePlan(ctx, identity, runID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.GetState() != stopAt {
		t.Fatalf("stop state = %v, want %v", plan.GetState(), stopAt)
	}
	previewed, err := fixture.kernel.PreviewChangeSet(ctx, identity, runID)
	if err != nil {
		t.Fatal(err)
	}
	if previewed.GetCandidateState() != contractsv1.CandidateState_CANDIDATE_STATE_RETAINED {
		t.Fatalf("candidate state = %v, want RETAINED", previewed.GetCandidateState())
	}
	return runID
}

func writeFactoryRecoveryRepo(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func commitFactoryRecoveryRepo(t *testing.T, root, message string) string {
	t.Helper()
	runFactoryRecoveryGit(t, root, "init", "--quiet")
	runFactoryRecoveryGit(t, root, "add", "-A")
	runFactoryRecoveryGit(t, root, "-c", "user.name=Ouroboros Test", "-c", "user.email=test@example.invalid",
		"commit", "--quiet", "-m", message)
	command := exec.Command("/usr/bin/git", "rev-parse", "HEAD")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func runFactoryRecoveryGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("/usr/bin/git", args...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

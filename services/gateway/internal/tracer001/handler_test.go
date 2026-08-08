package tracer001

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakePath struct {
	mu       sync.Mutex
	calls    []StepCommand
	result   *PathSuccess
	err      error
	denyB    bool
	block    bool
	lastStep Step
}

func (p *fakePath) Advance(ctx context.Context, command StepCommand) (*PathSuccess, error) {
	p.mu.Lock()
	p.calls = append(p.calls, command)
	p.lastStep = command.Step
	block := p.block
	denyB := p.denyB
	result := p.result
	err := p.err
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if denyB && command.Principal.PrincipalID == "principal-b" {
		return nil, ErrUnknownScope
	}
	if err != nil {
		return nil, err
	}
	if result == nil {
		return validSuccess(command.Step, command.Principal), nil
	}
	return result, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func testPeer(principal string) localauthority.PeerContext {
	return localauthority.PeerContext{
		Credentials: localauthority.PeerCredentials{UID: 501, GID: 20, PID: 42},
		Identity: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: principal},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: "tracer-tenant"},
			Session:   shared.Identifier{Namespace: "session", Value: "sess-1"},
		},
	}
}

func callerFor(principal string) *contractsv1.AuthenticatedPrincipalRef {
	return &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: id("principal", principal),
		TenantId:    id("tenant", "tracer-tenant"),
		SessionId:   id("session", "sess-1"),
	}
}

func id(ns, value string) *contractsv1.Identifier {
	return &contractsv1.Identifier{Namespace: ns, Value: value}
}

func digest(hex string) *contractsv1.Digest {
	return &contractsv1.Digest{Algorithm: "sha256", Hex: hex}
}

func sha(char byte) string {
	return strings.Repeat(string(char), 64)
}

func baseRequest(principal string) PathRequest {
	return PathRequest{
		Caller:             callerFor(principal),
		RequestedSession:   id("session", "sess-1"),
		RunID:              id("run", "tracer-run-1"),
		ManifestDigest:     digest(sha('a')),
		ConfigDigest:       digest(sha('b')),
		IdempotencyKey:     "once",
		QueryText:          "What does the supporting span return?",
		SourceID:           id("source", "fixture"),
		GenerationID:       id("generation", "gen-1"),
		ActiveVariant:      contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_AUTHORIZED,
		BaseGitOID:         "0123456789abcdef0123456789abcdef01234567",
		ScopeDigest:        digest(sha('c')),
		EffectApprovalHex:  sha('d'),
		ChangeSetDigestHex: sha('e'),
	}
}

func validReceipt(name string) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   id("receipt", name),
		OperationId: id("operation", name),
		Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
		Causal: &contractsv1.CausalContext{
			CorrelationId: id("correlation", "sess-1"),
			CausationId:   id("session", "sess-1"),
			TraceId:       id("trace", "sess-1"),
			Watermark:     1,
		},
		RecordedAt:          timestamppb.New(time.Unix(1_751_000_000, 0).UTC()),
		ConfigurationDigest: digest(sha('f')),
	}
}

func validStep(step contractsv1.TracerStep) *contractsv1.TracerStepReceipt {
	return &contractsv1.TracerStepReceipt{
		Step:         step,
		Receipt:      validReceipt("step"),
		InputDigest:  digest(sha('1')),
		OutputDigest: digest(sha('2')),
	}
}

func validRun(principal string, state contractsv1.TracerRunState) *contractsv1.TracerRun {
	return &contractsv1.TracerRun{
		RunId:            id("run", "tracer-run-1"),
		ManifestDigest:   digest(sha('a')),
		State:            state,
		ActiveVariant:    contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_AUTHORIZED,
		ActorPrincipalId: id("principal", principal),
		ConfigDigest:     digest(sha('b')),
		Receipt:          validReceipt("run"),
	}
}

func validDraftPR() *contractsv1.DraftPrReceipt {
	return &contractsv1.DraftPrReceipt{
		ActionId:               id("action", "draft-1"),
		Phase:                  contractsv1.DraftPrPhase_DRAFT_PR_PHASE_PR,
		HeadRef:                "refs/heads/ouroboros/tracer-001/" + strings.Repeat("a", 24),
		BaseRef:                "refs/heads/main",
		BaseCommitOid:          "0123456789abcdef0123456789abcdef01234567",
		HeadCommitOid:          "abcdef0123456789abcdef0123456789abcdef01",
		RepositoryFullName:     "example/tracer-demo",
		ProviderPrId:           "42",
		IsDraft:                true,
		PublicationTupleDigest: digest(sha('3')),
		ContentDigest:          digest(sha('4')),
		EffectApprovalDigest:   digest(sha('d')),
		ChangeSetDigest:        digest(sha('e')),
		Receipt:                validReceipt("draft-pr"),
	}
}

func validOutcome() *contractsv1.OutcomeFact {
	return &contractsv1.OutcomeFact{
		FactId:               id("fact", "outcome-1"),
		AuthorityClass:       contractsv1.AuthorityClass_AUTHORITY_CLASS_MACHINE_OBSERVATION,
		OutcomeBundleDigest:  digest(sha('5')),
		DraftPrReceiptDigest: digest(sha('6')),
		RawTraceSeparated:    true,
		Receipt:              validReceipt("outcome"),
	}
}

func validSuccess(step Step, principal Principal) *PathSuccess {
	runState := contractsv1.TracerRunState_TRACER_RUN_STATE_READY
	tracerStep := contractsv1.TracerStep_TRACER_STEP_INGEST
	success := &PathSuccess{
		Run:  validRun(principal.PrincipalID, runState),
		Step: validStep(tracerStep),
	}
	switch step {
	case StepSession:
		success.Run.State = contractsv1.TracerRunState_TRACER_RUN_STATE_MANIFEST_PINNED
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_PIN_FIXTURE
	case StepIngest:
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_INGEST
	case StepAsk:
		success.Run.State = contractsv1.TracerRunState_TRACER_RUN_STATE_ANSWERED
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_ASK
	case StepIntent:
		success.Run.State = contractsv1.TracerRunState_TRACER_RUN_STATE_INTENT_APPROVED
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_ADMIT_INTENT
	case StepPlan:
		success.Run.State = contractsv1.TracerRunState_TRACER_RUN_STATE_PLANNED
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_PLAN_DAG
	case StepReview:
		success.Run.State = contractsv1.TracerRunState_TRACER_RUN_STATE_REVIEWING
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_REVIEW_AND_DRAFT_PR
	case StepDraftPR:
		success.Run.State = contractsv1.TracerRunState_TRACER_RUN_STATE_DRAFT_PR_CREATED
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_REVIEW_AND_DRAFT_PR
		success.DraftPR = validDraftPR()
	case StepOutcome:
		success.Run.State = contractsv1.TracerRunState_TRACER_RUN_STATE_COMPLETE
		success.Step.Step = contractsv1.TracerStep_TRACER_STEP_OUTCOME_REINGEST
		success.Outcome = validOutcome()
	}
	return success
}

func newFixture(t *testing.T) (*Handler, *fakePath) {
	t.Helper()
	path := &fakePath{}
	handler, err := NewHandler(Config{
		Path:                path,
		Clock:               fixedClock{now: time.UnixMilli(4242).UTC()},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: sha('f')},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, path
}

func TestAdvanceAllStepsPrincipalA(t *testing.T) {
	handler, path := newFixture(t)
	peer := testPeer("principal-a")
	req := baseRequest("principal-a")
	steps := []Step{
		StepSession, StepIngest, StepAsk, StepIntent,
		StepPlan, StepReview, StepDraftPR, StepOutcome,
	}
	for _, step := range steps {
		// Session does not require run fields; rebuild a valid request each time.
		request := req
		if step == StepSession {
			request = PathRequest{
				Caller:           callerFor("principal-a"),
				RequestedSession: id("session", "sess-1"),
				IdempotencyKey:   "session-once",
			}
		}
		response, err := handler.Advance(context.Background(), peer, step, request)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if response.Error != nil || response.Receipt.Status != contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED {
			t.Fatalf("%s: expected success, got denial %#v", step, response.Error)
		}
		if response.Run == nil || response.Step == nil {
			t.Fatalf("%s: missing run/step", step)
		}
		if step == StepDraftPR && response.DraftPR == nil {
			t.Fatal("draft-pr missing DraftPR")
		}
		if step == StepOutcome && response.Outcome == nil {
			t.Fatal("outcome missing Outcome")
		}
	}
	if len(path.calls) != len(steps) {
		t.Fatalf("calls=%d want %d", len(path.calls), len(steps))
	}
}

func TestPrincipalBStaticDenial(t *testing.T) {
	handler, path := newFixture(t)
	path.denyB = true
	peer := testPeer("principal-b")
	req := baseRequest("principal-b")
	denied := make([]*PathResponse, 0, 4)
	for _, step := range []Step{StepIngest, StepAsk, StepIntent, StepDraftPR} {
		response, err := handler.Advance(context.Background(), peer, step, req)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if response.Error == nil || response.Error.Code != deniedCode {
			t.Fatalf("%s: expected static denial", step)
		}
		if response.Receipt.ReasonCode != deniedCode ||
			response.Receipt.Status != contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED {
			t.Fatalf("%s: denial receipt shape", step)
		}
		if response.Run != nil || response.Step != nil || response.DraftPR != nil || response.Outcome != nil {
			t.Fatalf("%s: denial must not carry success payloads", step)
		}
		denied = append(denied, response)
	}
	// Inaccessible ≡ absent: all denials share the same public code.
	for i := 1; i < len(denied); i++ {
		if denied[i].Error.Code != denied[0].Error.Code {
			t.Fatal("denial codes diverge")
		}
	}
}

func TestPeerMismatchDeniedBeforePort(t *testing.T) {
	handler, path := newFixture(t)
	peer := testPeer("principal-a")
	req := baseRequest("principal-b")
	_, err := handler.Advance(context.Background(), peer, StepAsk, req)
	if !errors.Is(err, ErrRequestDenied) {
		t.Fatalf("want ErrRequestDenied, got %v", err)
	}
	if len(path.calls) != 0 {
		t.Fatal("port invoked on mismatch")
	}
}

func TestMissingFieldsInvalidRequest(t *testing.T) {
	handler, path := newFixture(t)
	peer := testPeer("principal-a")
	req := baseRequest("principal-a")
	req.QueryText = ""
	_, err := handler.Advance(context.Background(), peer, StepAsk, req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
	if len(path.calls) != 0 {
		t.Fatal("port invoked on invalid request")
	}
}

func TestIdempotencyConflictIsStaticDenial(t *testing.T) {
	handler, path := newFixture(t)
	path.err = ErrIdempotencyConflict
	peer := testPeer("principal-a")
	response, err := handler.Advance(context.Background(), peer, StepIngest, baseRequest("principal-a"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != deniedCode {
		t.Fatal("expected static denial for conflict")
	}
}

func TestCancellationBeforePort(t *testing.T) {
	handler, _ := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := handler.Advance(ctx, testPeer("principal-a"), StepPlan, baseRequest("principal-a"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestContractViolatingPortOutputFailsClosed(t *testing.T) {
	handler, path := newFixture(t)
	path.result = &PathSuccess{
		Run:  validRun("principal-a", contractsv1.TracerRunState_TRACER_RUN_STATE_READY),
		Step: &contractsv1.TracerStepReceipt{}, // missing required receipt → invalid
	}
	_, err := handler.Advance(context.Background(), testPeer("principal-a"), StepPlan, baseRequest("principal-a"))
	if err == nil {
		t.Fatal("expected fail closed on contract-violating port output")
	}
	if !errors.Is(err, ErrInvalidResponse) && !errors.Is(err, ErrInvalidRequest) {
		// Empty step fails protovalidate inside buildSuccess as ErrInvalidResponse.
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewHandlerRequiresPorts(t *testing.T) {
	_, err := NewHandler(Config{})
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("want ErrInvalidConfiguration, got %v", err)
	}
}

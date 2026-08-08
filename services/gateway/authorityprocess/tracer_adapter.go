// Package authorityprocess composes the Stage 06 Tracer 001 path into the production
// local-authority command: gateway handlers, the L2 DAG compiler, the draft-PR
// broker (FakeAPI by default), and sanitized outcome admission. The default is
// deterministic and hermetic; live GitHub is opt-in via env (see HANDOVER).
package authorityprocess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	factorytracer "github.com/sltbrta/sentra-code-memory-v2/services/brain/factorytracer"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/outcomes"
	"github.com/sltbrta/sentra-code-memory-v2/services/broker/github"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localbootstrap"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/tracer001"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Pinned L1 synthetic fixture facts (tests/fixtures/stage-06/tracer/).
const (
	tracerPinnedManifestDigest = "c8a9a9610450a3aab5fb7dfd7b6daf714607847b65e2b776bc7c43a983e37c56"
	tracerPinnedBaseGitOID     = "02354ff3b1740905347f538de22ac20f96b25668"
	tracerPinnedHeadGitOID     = "b7e1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9"
	tracerPinnedConfigDigest   = "d72a4a9d18b8ef0d6b261591397dc41dd5f20c8df69542ea2ecd016fb17ef9a9"
	tracerPinnedPolicyDigest   = "7b2039fd876a66dd4d88e35876602e4636189f428b5d6a32466d51cc3512d02e"
	tracerRepoOwner            = "ouroboros"
	tracerRepoName             = "tracer-001-dogfood"
	tracerAuthorizedPrincipal  = "principal-a"
	tracerDeniedPrincipal      = "principal-b"
)

// composeTracerAuthority builds the Stage 06 Tracer 001 surface on the same
// authenticated gateway: deterministic synthetic path over L1 digests, L2 DAG
// compiler, draft-PR broker (FakeAPI unless live env is set), and outcome
// admission. Composition failure rejects startup.
func composeTracerAuthority(
	_ context.Context,
	config *localbootstrap.Config,
	_ dependencies,
) (gateway.TracerAuthority, func() error, error) {
	if config == nil {
		return nil, nil, errInvalidConfig
	}
	api, err := tracerGitHubAPI()
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	// Seed the approved base so the two-phase broker can reconcile refs.
	if fake, ok := api.(*github.FakeAPI); ok {
		fake.SeedRef(tracerRepoOwner, tracerRepoName, "refs/heads/main", tracerPinnedBaseGitOID)
	}
	broker, err := github.NewBroker(github.Config{
		API:    api,
		Policy: tracerAllowPolicy{},
		Clock:  func() time.Time { return time.Now().UTC() },
		Token:  "process-synthetic-token",
	})
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	path := newTracerPath(tracerPathConfig{
		configHex:    config.ConfigurationDigest(),
		policyHex:    config.PolicyDigest(),
		github:       broker,
		outcomes:     outcomes.New(),
		clock:        func() time.Time { return time.Now().UTC() },
		authorized:   tracerAuthorizedPrincipal,
		denied:       tracerDeniedPrincipal,
		manifestHex:  tracerPinnedManifestDigest,
		configPinHex: tracerPinnedConfigDigest,
		policyPinHex: tracerPinnedPolicyDigest,
		baseGitOID:   tracerPinnedBaseGitOID,
		headGitOID:   tracerPinnedHeadGitOID,
		handoff:      tracerSyntheticHandoff(),
		repoOwner:    tracerRepoOwner,
		repoName:     tracerRepoName,
	})
	handler, err := tracer001.NewHandler(tracer001.Config{
		Path:  path,
		Clock: tracerClock{now: path.clock},
		ConfigurationDigest: shared.Digest{
			Algorithm: "sha256", Hex: config.ConfigurationDigest(),
		},
	})
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	return tracerAuthorityAdapter{handler: handler}, func() error { return nil }, nil
}

// tracerGitHubAPI selects FakeAPI (default, deterministic) or live REST when
// OUROBOROS_TRACER_LIVE_GITHUB=1 and a fine-grained PAT is present.
func tracerGitHubAPI() (github.API, error) {
	if os.Getenv("OUROBOROS_TRACER_LIVE_GITHUB") == "1" {
		token := github.ResolveToken()
		if token == "" {
			return nil, fmt.Errorf("live github requested without token")
		}
		return github.NewRESTAPI(nil, token), nil
	}
	return github.NewFakeAPI(), nil
}

type tracerAllowPolicy struct{}

func (tracerAllowPolicy) Check(
	_ context.Context, _ shared.MappedIdentityFact, request shared.PolicyRequest,
) (shared.PolicyDecision, error) {
	return shared.PolicyDecision{Allowed: true, RevocationEpoch: request.RevocationEpoch}, nil
}

type tracerClock struct {
	now func() time.Time
}

func (clock tracerClock) Now() time.Time {
	if clock.now == nil {
		return time.Now().UTC()
	}
	return clock.now().UTC()
}

// tracerAuthorityAdapter mounts the tracer001 handler behind the JSON transport.
type tracerAuthorityAdapter struct {
	handler *tracer001.Handler
}

func (adapter tracerAuthorityAdapter) Advance(
	ctx context.Context, peer gateway.PeerContext, step string, body []byte,
) ([]byte, error) {
	if adapter.handler == nil || ctx == nil {
		return nil, gateway.ErrPeerDenied
	}
	request, err := decodeTracerWireRequest(body)
	if err != nil {
		return nil, err
	}
	var response *tracer001.PathResponse
	switch step {
	case "Session":
		response, err = adapter.handler.Session(ctx, peer, request)
	case "Ingest":
		response, err = adapter.handler.Ingest(ctx, peer, request)
	case "Ask":
		response, err = adapter.handler.Ask(ctx, peer, request)
	case "Intent":
		response, err = adapter.handler.Intent(ctx, peer, request)
	case "Plan":
		response, err = adapter.handler.Plan(ctx, peer, request)
	case "Review":
		response, err = adapter.handler.Review(ctx, peer, request)
	case "DraftPr":
		response, err = adapter.handler.DraftPR(ctx, peer, request)
	case "Outcome":
		response, err = adapter.handler.Outcome(ctx, peer, request)
	default:
		return nil, gateway.ErrPeerDenied
	}
	if err != nil {
		if errors.Is(err, tracer001.ErrRequestDenied) {
			return nil, gateway.ErrPeerDenied
		}
		if errors.Is(err, tracer001.ErrInvalidRequest) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, gateway.ErrPeerDenied
	}
	encoded, err := encodeTracerWireResponse(step, response)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// --- wire JSON (TUI composition facade) ---

type tracerWireCaller struct {
	Principal tracerWireID `json:"principal"`
	Tenant    tracerWireID `json:"tenant"`
	Session   tracerWireID `json:"session"`
}

type tracerWireID struct {
	Namespace string `json:"namespace"`
	Value     string `json:"value"`
}

type tracerWireDigest struct {
	Algorithm string `json:"algorithm"`
	Hex       string `json:"hex"`
}

type tracerWireRequest struct {
	Caller               tracerWireCaller  `json:"caller"`
	RunID                *tracerWireID     `json:"run_id"`
	ManifestDigest       *tracerWireDigest `json:"manifest_digest"`
	ConfigDigest         *tracerWireDigest `json:"config_digest"`
	IdempotencyKey       string            `json:"idempotency_key"`
	QueryText            string            `json:"query_text"`
	SourceID             *tracerWireID     `json:"source_id"`
	GenerationID         *tracerWireID     `json:"generation_id"`
	ActiveVariant        string            `json:"active_variant"`
	BaseGitOID           string            `json:"base_git_oid"`
	ScopeDigest          *tracerWireDigest `json:"scope_digest"`
	EffectApprovalDigest *tracerWireDigest `json:"effect_approval_digest"`
	ChangeSetDigest      *tracerWireDigest `json:"change_set_digest"`
}

func decodeTracerWireRequest(body []byte) (tracer001.PathRequest, error) {
	var wire tracerWireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return tracer001.PathRequest{}, err
	}
	request := tracer001.PathRequest{
		Caller: &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: wireIDProto(wire.Caller.Principal),
			TenantId:    wireIDProto(wire.Caller.Tenant),
			SessionId:   wireIDProto(wire.Caller.Session),
		},
		RequestedSession: wireIDProto(wire.Caller.Session),
		RunID:            optionalWireID(wire.RunID),
		ManifestDigest:   optionalWireDigest(wire.ManifestDigest),
		ConfigDigest:     optionalWireDigest(wire.ConfigDigest),
		IdempotencyKey:   wire.IdempotencyKey,
		QueryText:        wire.QueryText,
		SourceID:         optionalWireID(wire.SourceID),
		GenerationID:     optionalWireID(wire.GenerationID),
		ActiveVariant:    parseTracerVariant(wire.ActiveVariant),
		BaseGitOID:       wire.BaseGitOID,
		ScopeDigest:      optionalWireDigest(wire.ScopeDigest),
	}
	if wire.EffectApprovalDigest != nil {
		request.EffectApprovalHex = wire.EffectApprovalDigest.Hex
	}
	if wire.ChangeSetDigest != nil {
		request.ChangeSetDigestHex = wire.ChangeSetDigest.Hex
	}
	return request, nil
}

func wireIDProto(id tracerWireID) *contractsv1.Identifier {
	if id.Namespace == "" || id.Value == "" {
		return nil
	}
	return &contractsv1.Identifier{Namespace: id.Namespace, Value: id.Value}
}

func optionalWireID(id *tracerWireID) *contractsv1.Identifier {
	if id == nil {
		return nil
	}
	return wireIDProto(*id)
}

func optionalWireDigest(digest *tracerWireDigest) *contractsv1.Digest {
	if digest == nil {
		return nil
	}
	return &contractsv1.Digest{Algorithm: digest.Algorithm, Hex: digest.Hex}
}

func parseTracerVariant(value string) contractsv1.TracerVariantKind {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "authorized":
		return contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_AUTHORIZED
	case "absent":
		return contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_ABSENT
	case "stale":
		return contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_STALE
	case "unauthorized":
		return contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_UNAUTHORIZED
	case "revoked":
		return contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_REVOKED
	default:
		return contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_UNSPECIFIED
	}
}

type tracerWireResponse struct {
	Receipt *tracerWireReceipt     `json:"receipt"`
	Error   *tracerWirePublicError `json:"error,omitempty"`
	Run     *tracerWireRun         `json:"run,omitempty"`
	Step    *tracerWireStep        `json:"step,omitempty"`
	DraftPR *tracerWireDraftPR     `json:"draft_pr,omitempty"`
	Outcome *tracerWireOutcome     `json:"outcome,omitempty"`
	Lines   []string               `json:"lines,omitempty"`
}

type tracerWireReceipt struct {
	ID         string `json:"id"`
	Operation  string `json:"operation"`
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code"`
	Watermark  uint64 `json:"watermark"`
}

type tracerWirePublicError struct {
	Code string `json:"code"`
}

type tracerWireRun struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Variant string `json:"variant,omitempty"`
}

type tracerWireStep struct {
	Step       string `json:"step"`
	ReasonCode string `json:"reason_code,omitempty"`
}

type tracerWireDraftPR struct {
	HeadRef      string `json:"head_ref"`
	ProviderPRID string `json:"provider_pr_id,omitempty"`
	IsDraft      bool   `json:"is_draft"`
}

type tracerWireOutcome struct {
	FactID            string `json:"fact_id"`
	RawTraceSeparated bool   `json:"raw_trace_separated"`
}

func encodeTracerWireResponse(step string, response *tracer001.PathResponse) ([]byte, error) {
	if response == nil || response.Receipt == nil {
		return nil, errors.New("tracer response incomplete")
	}
	receipt := response.Receipt
	status := "completed"
	if receipt.GetStatus() == contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED {
		status = "rejected"
	}
	wire := tracerWireResponse{
		Receipt: &tracerWireReceipt{
			ID:         formatID(receipt.GetReceiptId()),
			Operation:  formatID(receipt.GetOperationId()),
			Status:     status,
			ReasonCode: receipt.GetReasonCode(),
			Watermark:  receipt.GetCausal().GetWatermark(),
		},
	}
	if response.Error != nil {
		wire.Error = &tracerWirePublicError{Code: response.Error.GetCode()}
	}
	if response.Run != nil {
		wire.Run = &tracerWireRun{
			ID:      formatID(response.Run.GetRunId()),
			State:   stripEnumPrefix(response.Run.GetState().String(), "TRACER_RUN_STATE_"),
			Variant: stripEnumPrefix(response.Run.GetActiveVariant().String(), "TRACER_VARIANT_KIND_"),
		}
	}
	if response.Step != nil {
		wire.Step = &tracerWireStep{
			Step:       stripEnumPrefix(response.Step.GetStep().String(), "TRACER_STEP_"),
			ReasonCode: response.Step.GetReasonCode(),
		}
	}
	if response.DraftPR != nil {
		wire.DraftPR = &tracerWireDraftPR{
			HeadRef:      response.DraftPR.GetHeadRef(),
			ProviderPRID: response.DraftPR.GetProviderPrId(),
			IsDraft:      response.DraftPR.GetIsDraft(),
		}
	}
	if response.Outcome != nil {
		wire.Outcome = &tracerWireOutcome{
			FactID:            formatID(response.Outcome.GetFactId()),
			RawTraceSeparated: response.Outcome.GetRawTraceSeparated(),
		}
	}
	wire.Lines = tracerDefaultLines(step, wire)
	return json.Marshal(wire)
}

func formatID(id *contractsv1.Identifier) string {
	if id == nil || id.GetNamespace() == "" || id.GetValue() == "" {
		return ""
	}
	return id.GetNamespace() + ":" + id.GetValue()
}

func stripEnumPrefix(value, prefix string) string {
	return strings.TrimPrefix(value, prefix)
}

func tracerDefaultLines(step string, wire tracerWireResponse) []string {
	if wire.Error != nil || (wire.Receipt != nil && wire.Receipt.Status == "rejected") {
		return nil
	}
	runID := "run:?"
	state := "unknown"
	if wire.Run != nil {
		runID = wire.Run.ID
		state = wire.Run.State
	}
	switch step {
	case "Session":
		return []string{fmt.Sprintf("Session opened — run=%s state=%s", runID, state)}
	case "Ingest":
		return []string{fmt.Sprintf("Fixture ingested — run=%s state=%s", runID, state)}
	case "Ask":
		line := fmt.Sprintf("Ask complete — run=%s state=%s", runID, state)
		if wire.Step != nil && wire.Step.ReasonCode != "" {
			line += " reason=" + wire.Step.ReasonCode
		}
		return []string{line}
	case "Intent":
		return []string{fmt.Sprintf("Intent admitted — run=%s state=%s", runID, state)}
	case "Plan":
		return []string{fmt.Sprintf("Plan ready — run=%s state=%s", runID, state)}
	case "Review":
		return []string{fmt.Sprintf("Review complete — run=%s state=%s", runID, state)}
	case "DraftPr":
		if wire.DraftPR == nil {
			return []string{fmt.Sprintf("Draft PR authorized — run=%s state=%s", runID, state)}
		}
		line := fmt.Sprintf("Draft PR authorized — head=%s draft=%t", wire.DraftPR.HeadRef, wire.DraftPR.IsDraft)
		if wire.DraftPR.ProviderPRID != "" {
			line += " pr=" + wire.DraftPR.ProviderPRID
		}
		return []string{line}
	case "Outcome":
		if wire.Outcome == nil {
			return []string{fmt.Sprintf("Outcome reingested — run=%s state=%s", runID, state)}
		}
		return []string{fmt.Sprintf(
			"Outcome reingested — fact=%s raw_trace_separated=%t state=%s",
			wire.Outcome.FactID, wire.Outcome.RawTraceSeparated, state,
		)}
	default:
		return []string{fmt.Sprintf("Tracer step complete — run=%s state=%s", runID, state)}
	}
}

// --- Path composition ---

type tracerPathConfig struct {
	configHex    string
	policyHex    string
	github       *github.Broker
	outcomes     *outcomes.Admissions
	clock        func() time.Time
	authorized   string
	denied       string
	manifestHex  string
	configPinHex string
	policyPinHex string
	baseGitOID   string
	headGitOID   string
	handoff      factorytracer.IntentHandoff
	repoOwner    string
	repoName     string
}

type tracerPath struct {
	tracerPathConfig
	mu   sync.Mutex
	runs map[string]*tracerRunState
}

type tracerRunState struct {
	id             string
	principal      string
	tenant         string
	session        string
	state          contractsv1.TracerRunState
	variant        contractsv1.TracerVariantKind
	watermark      uint64
	workflow       *factorytracer.CompiledWorkflow
	draft          *contractsv1.DraftPrReceipt
	outcome        *contractsv1.OutcomeFact
	idempotency    map[string]string // key -> response fingerprint (step name)
	effectApproval string
	changeSetHex   string
}

func newTracerPath(config tracerPathConfig) *tracerPath {
	if config.clock == nil {
		config.clock = func() time.Time { return time.Now().UTC() }
	}
	return &tracerPath{
		tracerPathConfig: config,
		runs:             make(map[string]*tracerRunState),
	}
}

func (path *tracerPath) Advance(ctx context.Context, command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	if path == nil || ctx == nil {
		return nil, tracer001.ErrUnknownScope
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Principal B is always inaccessible ≡ absent on the composed path.
	if command.Principal.PrincipalID == path.denied {
		return nil, tracer001.ErrUnknownScope
	}
	switch command.Step {
	case tracer001.StepSession:
		return path.stepSession(command)
	case tracer001.StepIngest:
		return path.stepIngest(command)
	case tracer001.StepAsk:
		return path.stepAsk(command)
	case tracer001.StepIntent:
		return path.stepIntent(command)
	case tracer001.StepPlan:
		return path.stepPlan(command)
	case tracer001.StepReview:
		return path.stepReview(command)
	case tracer001.StepDraftPR:
		return path.stepDraftPR(ctx, command)
	case tracer001.StepOutcome:
		return path.stepOutcome(command)
	default:
		return nil, tracer001.ErrUnknownScope
	}
}

func (path *tracerPath) stepSession(command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	runID := deterministicRunID(command.Principal, command.Request.IdempotencyKey)
	if existing, ok := path.runs[runID]; ok {
		existing.watermark++
		return path.success(existing, contractsv1.TracerStep_TRACER_STEP_PIN_FIXTURE, "")
	}
	run := &tracerRunState{
		id:          runID,
		principal:   command.Principal.PrincipalID,
		tenant:      command.Principal.Tenant,
		session:     command.Principal.Session,
		state:       contractsv1.TracerRunState_TRACER_RUN_STATE_MANIFEST_PINNED,
		variant:     contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_AUTHORIZED,
		watermark:   1,
		idempotency: map[string]string{command.Request.IdempotencyKey: "session"},
	}
	path.runs[runID] = run
	return path.success(run, contractsv1.TracerStep_TRACER_STEP_PIN_FIXTURE, "")
}

func (path *tracerPath) stepIngest(command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	run, err := path.requireRun(command)
	if err != nil {
		return nil, err
	}
	if command.Request.ManifestDigest == nil ||
		command.Request.ManifestDigest.GetAlgorithm() != "sha256" ||
		command.Request.ManifestDigest.GetHex() != path.manifestHex {
		return nil, tracer001.ErrUnknownScope
	}
	if err := path.rememberIdempotency(run, command.Request.IdempotencyKey, "ingest"); err != nil {
		return nil, err
	}
	run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_READY
	run.watermark++
	return path.success(run, contractsv1.TracerStep_TRACER_STEP_INGEST, "")
}

func (path *tracerPath) stepAsk(command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	run, err := path.requireRun(command)
	if err != nil {
		return nil, err
	}
	if run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_READY &&
		run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_ANSWERED &&
		run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_ABSTAINED {
		return nil, tracer001.ErrUnknownScope
	}
	if err := path.rememberIdempotency(run, command.Request.IdempotencyKey, "ask"); err != nil {
		return nil, err
	}
	variant := command.Request.ActiveVariant
	if variant == contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_UNSPECIFIED {
		variant = contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_AUTHORIZED
	}
	run.variant = variant
	run.watermark++
	reason := ""
	switch variant {
	case contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_AUTHORIZED:
		run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_ANSWERED
	case contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_ABSENT:
		run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_ABSTAINED
		reason = "SPAN_ABSENT"
	case contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_STALE:
		run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_ABSTAINED
		reason = "SPAN_STALE"
	case contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_UNAUTHORIZED:
		run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_ABSTAINED
		reason = "SPAN_UNAUTHORIZED"
	case contractsv1.TracerVariantKind_TRACER_VARIANT_KIND_REVOKED:
		run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_ABSTAINED
		reason = "SPAN_REVOKED"
	default:
		return nil, tracer001.ErrUnknownScope
	}
	return path.success(run, contractsv1.TracerStep_TRACER_STEP_ASK, reason)
}

func (path *tracerPath) stepIntent(command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	run, err := path.requireRun(command)
	if err != nil {
		return nil, err
	}
	// Intent requires a positive cited answer on the authorized arm.
	if run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_ANSWERED {
		return nil, tracer001.ErrUnknownScope
	}
	if command.Request.BaseGitOID != path.baseGitOID {
		return nil, tracer001.ErrUnknownScope
	}
	if err := path.rememberIdempotency(run, command.Request.IdempotencyKey, "intent"); err != nil {
		return nil, err
	}
	planID := "plan-" + run.id[:tracerMin(16, len(run.id))]
	workflow, err := factorytracer.CompileFromHandoff(
		path.handoff,
		command.Principal.Tenant,
		command.Principal.PrincipalID,
		command.Principal.Session,
		run.id,
		planID,
		path.policyPinHex,
		true,
	)
	if err != nil {
		return nil, tracer001.ErrUnknownScope
	}
	run.workflow = workflow
	run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_INTENT_APPROVED
	run.watermark++
	return path.success(run, contractsv1.TracerStep_TRACER_STEP_ADMIT_INTENT, "")
}

func (path *tracerPath) stepPlan(command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	run, err := path.requireRun(command)
	if err != nil {
		return nil, err
	}
	if run.workflow == nil {
		return nil, tracer001.ErrUnknownScope
	}
	if err := factorytracer.ValidateNoRedispatch(run.workflow); err != nil {
		return nil, tracer001.ErrUnknownScope
	}
	if err := factorytracer.ValidateSealedActions(run.workflow); err != nil {
		return nil, tracer001.ErrUnknownScope
	}
	run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_PLANNED
	run.watermark++
	return path.success(run, contractsv1.TracerStep_TRACER_STEP_PLAN_DAG, "")
}

func (path *tracerPath) stepReview(command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	run, err := path.requireRun(command)
	if err != nil {
		return nil, err
	}
	if run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_PLANNED &&
		run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_REVIEWING &&
		run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_EFFECT_PENDING {
		return nil, tracer001.ErrUnknownScope
	}
	run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_EFFECT_PENDING
	run.watermark++
	return path.success(run, contractsv1.TracerStep_TRACER_STEP_REVIEW_AND_DRAFT_PR, "")
}

func (path *tracerPath) stepDraftPR(ctx context.Context, command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	run, err := path.requireRun(command)
	if err != nil {
		return nil, err
	}
	if run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_EFFECT_PENDING &&
		run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_DRAFT_PR_CREATED {
		return nil, tracer001.ErrUnknownScope
	}
	if command.Request.EffectApprovalHex == "" || command.Request.ChangeSetDigestHex == "" {
		return nil, tracer001.ErrUnknownScope
	}
	if err := path.rememberIdempotency(run, command.Request.IdempotencyKey, "draft-pr"); err != nil {
		return nil, err
	}
	// Exact replay of a completed draft-PR step returns the original receipt.
	if run.draft != nil && run.state == contractsv1.TracerRunState_TRACER_RUN_STATE_DRAFT_PR_CREATED {
		run.watermark++
		success, err := path.success(run, contractsv1.TracerStep_TRACER_STEP_REVIEW_AND_DRAFT_PR, "")
		if err != nil {
			return nil, err
		}
		success.DraftPR = run.draft
		return success, nil
	}
	effectApproval := shared.Digest{Algorithm: "sha256", Hex: command.Request.EffectApprovalHex}
	changeSet := shared.Digest{Algorithm: "sha256", Hex: command.Request.ChangeSetDigestHex}
	policyDigest := shared.Digest{Algorithm: "sha256", Hex: path.policyPinHex}
	configDigest := shared.Digest{Algorithm: "sha256", Hex: path.configPinHex}
	req := github.PublishRequest{
		Authenticated: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: command.Principal.PrincipalID},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: command.Principal.Tenant},
			Session:   shared.Identifier{Namespace: "session", Value: command.Principal.Session},
		},
		Tuple: github.PublicationTuple{
			TenantID:             command.Principal.Tenant,
			RepositoryOwner:      path.repoOwner,
			RepositoryName:       path.repoName,
			BaseRef:              "main",
			BaseCommitOID:        path.baseGitOID,
			HeadCommitOID:        path.headGitOID,
			ChangeSetDigest:      changeSet,
			EffectApprovalDigest: effectApproval,
			PolicyDigest:         policyDigest,
			ConfigDigest:         configDigest,
		},
		Content: github.PRContent{
			Title: "tracer-001: rename MarkerLabel",
			Body:  "Deterministic Tracer 001 draft. No raw traces.",
		},
		Grant: github.EffectGrant{
			GrantID:            shared.Identifier{Namespace: "grant", Value: "tracer-draft-" + run.id[:tracerMin(12, len(run.id))]},
			Initiator:          shared.Identifier{Namespace: "principal", Value: command.Principal.PrincipalID},
			Tenant:             shared.Identifier{Namespace: "tenant", Value: command.Principal.Tenant},
			Actions:            []string{github.ActionBranchPublish, github.ActionDraftPRCreate},
			RepositoryFullName: path.repoOwner + "/" + path.repoName,
			BaseCommitOID:      path.baseGitOID,
			HeadCommitOID:      path.headGitOID,
			RevocationEpoch:    1,
			ExpiresAt:          path.clock().Add(time.Hour),
			PolicyDigest:       policyDigest,
			Nonce:              "nonce-" + command.Request.IdempotencyKey,
		},
		IdempotencyKey: command.Request.IdempotencyKey,
		ActionID:       "action-" + run.id[:tracerMin(16, len(run.id))],
	}
	// FakeAPI is in-process and never re-enters the path; live REST is
	// opt-in and still serialised under this lock for the synthetic path.
	receipt, publishErr := path.github.Publish(ctx, req)
	if publishErr != nil {
		return nil, tracer001.ErrUnknownScope
	}
	draft := &contractsv1.DraftPrReceipt{
		ActionId:               &contractsv1.Identifier{Namespace: "action", Value: receipt.ActionID},
		Phase:                  contractsv1.DraftPrPhase_DRAFT_PR_PHASE_PR,
		HeadRef:                receipt.HeadRef,
		BaseRef:                "refs/heads/main",
		BaseCommitOid:          receipt.BaseCommitOID,
		HeadCommitOid:          receipt.HeadCommitOID,
		RepositoryFullName:     receipt.RepositoryFullName,
		ProviderPrId:           receipt.ProviderPRID,
		IsDraft:                true,
		PublicationTupleDigest: digestProto(receipt.PublicationTupleDigest),
		ContentDigest:          digestProto(receipt.ContentDigest),
		EffectApprovalDigest:   digestProto(receipt.EffectApprovalDigest),
		ChangeSetDigest:        digestProto(receipt.ChangeSetDigest),
		Receipt:                path.makeReceipt("draft-pr", run, contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
	}
	run.draft = draft
	run.effectApproval = command.Request.EffectApprovalHex
	run.changeSetHex = command.Request.ChangeSetDigestHex
	run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_DRAFT_PR_CREATED
	run.watermark++
	success, err := path.success(run, contractsv1.TracerStep_TRACER_STEP_REVIEW_AND_DRAFT_PR, "")
	if err != nil {
		return nil, err
	}
	success.DraftPR = draft
	return success, nil
}

func (path *tracerPath) stepOutcome(command tracer001.StepCommand) (*tracer001.PathSuccess, error) {
	path.mu.Lock()
	defer path.mu.Unlock()
	run, err := path.requireRun(command)
	if err != nil {
		return nil, err
	}
	if run.outcome != nil && run.state == contractsv1.TracerRunState_TRACER_RUN_STATE_COMPLETE {
		run.watermark++
		success, err := path.success(run, contractsv1.TracerStep_TRACER_STEP_OUTCOME_REINGEST, "")
		if err != nil {
			return nil, err
		}
		success.Outcome = run.outcome
		return success, nil
	}
	if run.draft == nil || run.state != contractsv1.TracerRunState_TRACER_RUN_STATE_DRAFT_PR_CREATED {
		return nil, tracer001.ErrUnknownScope
	}
	if err := path.rememberIdempotency(run, command.Request.IdempotencyKey, "outcome"); err != nil {
		return nil, err
	}
	bundle := []byte(`{"kind":"draft_pr_outcome","provider_pr_id":"` + run.draft.GetProviderPrId() + `","is_draft":true}`)
	draftDigest := shared.Digest{
		Algorithm: "sha256",
		Hex:       sha256Hex([]byte(run.draft.GetProviderPrId() + "|" + run.draft.GetHeadRef())),
	}
	factID := "outcome-" + run.id[:tracerMin(16, len(run.id))]
	admitted, admitErr := path.outcomes.Admit(outcomes.AdmitRequest{
		Tenant:               run.tenant,
		Principal:            run.principal,
		FactID:               factID,
		AuthorityClass:       outcomes.AuthorityMachineObservation,
		OutcomeBundle:        bundle,
		DraftPrReceiptDigest: draftDigest,
		RawTraceSeparated:    true,
		IdempotencyKey:       command.Request.IdempotencyKey,
	})
	if admitErr != nil {
		return nil, tracer001.ErrUnknownScope
	}
	outcome := &contractsv1.OutcomeFact{
		FactId:               &contractsv1.Identifier{Namespace: "fact", Value: admitted.FactID},
		AuthorityClass:       contractsv1.AuthorityClass_AUTHORITY_CLASS_MACHINE_OBSERVATION,
		OutcomeBundleDigest:  digestProto(admitted.OutcomeBundleDigest),
		DraftPrReceiptDigest: digestProto(admitted.DraftPrReceiptDigest),
		RawTraceSeparated:    true,
		Receipt:              path.makeReceipt("outcome", run, contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
	}
	run.outcome = outcome
	run.state = contractsv1.TracerRunState_TRACER_RUN_STATE_COMPLETE
	run.watermark++
	success, err := path.success(run, contractsv1.TracerStep_TRACER_STEP_OUTCOME_REINGEST, "")
	if err != nil {
		return nil, err
	}
	success.Outcome = outcome
	return success, nil
}

func (path *tracerPath) requireRun(command tracer001.StepCommand) (*tracerRunState, error) {
	if command.Request.RunID == nil || command.Request.RunID.GetNamespace() != "run" {
		return nil, tracer001.ErrUnknownScope
	}
	run, ok := path.runs[command.Request.RunID.GetValue()]
	if !ok {
		return nil, tracer001.ErrUnknownScope
	}
	if run.principal != command.Principal.PrincipalID ||
		run.tenant != command.Principal.Tenant ||
		run.session != command.Principal.Session {
		return nil, tracer001.ErrUnknownScope
	}
	return run, nil
}

func (path *tracerPath) rememberIdempotency(run *tracerRunState, key, step string) error {
	if key == "" {
		return nil
	}
	if previous, ok := run.idempotency[key]; ok && previous != step {
		return tracer001.ErrIdempotencyConflict
	}
	run.idempotency[key] = step
	return nil
}

func (path *tracerPath) success(
	run *tracerRunState, step contractsv1.TracerStep, reason string,
) (*tracer001.PathSuccess, error) {
	return &tracer001.PathSuccess{
		Run: &contractsv1.TracerRun{
			RunId:            &contractsv1.Identifier{Namespace: "run", Value: run.id},
			ManifestDigest:   &contractsv1.Digest{Algorithm: "sha256", Hex: path.manifestHex},
			State:            run.state,
			ActiveVariant:    run.variant,
			ActorPrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: run.principal},
			ConfigDigest:     &contractsv1.Digest{Algorithm: "sha256", Hex: path.configPinHex},
			DraftPr:          run.draft,
			Outcome:          run.outcome,
			Receipt:          path.makeReceipt("run", run, contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		},
		Step: &contractsv1.TracerStepReceipt{
			Step:         step,
			Receipt:      path.makeReceipt("step", run, contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, reason),
			ReasonCode:   reason,
			InputDigest:  &contractsv1.Digest{Algorithm: "sha256", Hex: sha256Hex([]byte("input|" + run.id + "|" + step.String()))},
			OutputDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: sha256Hex([]byte("output|" + run.id + "|" + step.String() + "|" + reason))},
		},
	}, nil
}

func (path *tracerPath) makeReceipt(
	name string, run *tracerRunState, status contractsv1.ReceiptStatus, reason string,
) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: name + "-" + run.id[:tracerMin(16, len(run.id))] + fmt.Sprintf("-%d", run.watermark)},
		OperationId: &contractsv1.Identifier{Namespace: "operation", Value: "tracer." + name},
		Status:      status,
		ReasonCode:  reason,
		Causal: &contractsv1.CausalContext{
			CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: run.session},
			CausationId:   &contractsv1.Identifier{Namespace: "session", Value: run.session},
			TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: run.id},
			Watermark:     run.watermark,
		},
		RecordedAt: timestamppb.New(path.clock()),
		ConfigurationDigest: &contractsv1.Digest{
			Algorithm: "sha256", Hex: path.configHex,
		},
	}
}

func deterministicRunID(principal tracer001.Principal, key string) string {
	sum := sha256.Sum256([]byte(principal.Tenant + "\x00" + principal.PrincipalID + "\x00" + principal.Session + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func digestProto(digest shared.Digest) *contractsv1.Digest {
	return &contractsv1.Digest{Algorithm: digest.Algorithm, Hex: digest.Hex}
}

func tracerSyntheticHandoff() factorytracer.IntentHandoff {
	return factorytracer.IntentHandoff{
		SchemaVersion:  "tracer-001/change-intent/v1",
		BaseGitOID:     tracerPinnedBaseGitOID,
		DynamicLeafMin: 1,
		DynamicLeafMax: 3,
		ExpectedN:      2,
		ScopePaths:     []string{"src/marker/marker.go", "src/marker/marker_test.go"},
		Leaves: []factorytracer.IntentLeaf{
			{NodeID: "leaf-impl", OwnedPaths: []string{"src/marker/marker.go"}},
			{NodeID: "leaf-test", OwnedPaths: []string{"src/marker/marker_test.go"}},
		},
		RequiredGateKinds: []string{"BUILD", "TEST", "DOCS", "SECURITY"},
		Summary:           "Rename MarkerLabel to AuthorizedMarkerLabel and update its unit test.",
	}
}

func tracerMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

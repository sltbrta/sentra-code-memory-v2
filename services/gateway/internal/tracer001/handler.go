package tracer001

import (
	"context"
	"errors"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handler implements the eight Tracer 001 public-path steps behind Path.
// It holds no state between calls and is safe for concurrent use.
type Handler struct {
	path          Path
	clock         Clock
	configuration shared.Digest
}

// Config binds a Handler to its ports. Every port is required.
type Config struct {
	Path                Path
	Clock               Clock
	ConfigurationDigest shared.Digest
}

// NewHandler validates configuration; a misconfigured handler fails at
// construction, never at request time.
func NewHandler(config Config) (*Handler, error) {
	if config.Path == nil || config.Clock == nil {
		return nil, fmt.Errorf("%w: handler requires path and clock", ErrInvalidConfiguration)
	}
	if config.ConfigurationDigest.Algorithm != "sha256" ||
		!isLowerHexSHA256(config.ConfigurationDigest.Hex) {
		return nil, fmt.Errorf("%w: configuration digest", ErrInvalidConfiguration)
	}
	return &Handler{
		path:          config.Path,
		clock:         config.Clock,
		configuration: config.ConfigurationDigest,
	}, nil
}

// Advance runs one public path step: validate → identity → path → revalidate.
func (h *Handler) Advance(
	ctx context.Context,
	peer localauthority.PeerContext,
	step Step,
	request PathRequest,
) (*PathResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequestShape(step, request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller, request.RequestedSession)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	success, err := h.path.Advance(ctx, StepCommand{
		Principal: principal,
		Step:      step,
		Request:   request,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownScope) || errors.Is(err, ErrIdempotencyConflict) {
			return h.denied(step, identity)
		}
		return nil, errPortFailure
	}
	return h.buildSuccess(step, identity, success)
}

func (h *Handler) buildSuccess(
	step Step,
	identity shared.MappedIdentityFact,
	success *PathSuccess,
) (*PathResponse, error) {
	if success == nil || success.Step == nil || success.Run == nil {
		return nil, ErrInvalidResponse
	}
	if err := validateResponseMessage(success.Step); err != nil {
		return nil, err
	}
	if err := validateResponseMessage(success.Run); err != nil {
		return nil, err
	}
	if step == StepDraftPR {
		if success.DraftPR == nil {
			return nil, ErrInvalidResponse
		}
		if err := validateResponseMessage(success.DraftPR); err != nil {
			return nil, err
		}
	}
	if step == StepOutcome {
		if success.Outcome == nil {
			return nil, ErrInvalidResponse
		}
		if err := validateResponseMessage(success.Outcome); err != nil {
			return nil, err
		}
	}
	receiptValue := success.Run.GetRunId().GetValue()
	if receiptValue == "" {
		receiptValue = operationID(step)
	}
	response := &PathResponse{
		Receipt: h.receipt(step, receiptValue, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Run:     success.Run,
		Step:    success.Step,
		DraftPR: success.DraftPR,
		Outcome: success.Outcome,
	}
	if err := validateResponseMessage(response.Receipt); err != nil {
		return nil, err
	}
	return response, nil
}

func (h *Handler) denied(step Step, identity shared.MappedIdentityFact) (*PathResponse, error) {
	response := &PathResponse{
		Receipt: h.receipt(step, operationID(step), identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Error: staticPublicError(),
	}
	if err := validateResponseMessage(response.Receipt); err != nil {
		return nil, err
	}
	return response, nil
}

func (h *Handler) receipt(
	step Step,
	receiptValue string,
	identity shared.MappedIdentityFact,
	status contractsv1.ReceiptStatus,
	reason string,
) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: namespaceReceipt, Value: receiptValue},
		OperationId: &contractsv1.Identifier{Namespace: namespaceOperation, Value: operationID(step)},
		Status:      status,
		ReasonCode:  reason,
		Causal:      sessionCausal(identity),
		RecordedAt:  timestamppb.New(h.clock.Now().UTC()),
		ConfigurationDigest: &contractsv1.Digest{
			Algorithm: h.configuration.Algorithm,
			Hex:       h.configuration.Hex,
		},
	}
}

// Session opens the tracer path session for the authenticated peer.
func (h *Handler) Session(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepSession, request)
}

// Ingest admits the pinned fixture and records readiness.
func (h *Handler) Ingest(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepIngest, request)
}

// Ask issues the bounded grounded question for one cited span.
func (h *Handler) Ask(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepAsk, request)
}

// Intent admits one approved scope-limited ChangeIntent.
func (h *Handler) Intent(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepIntent, request)
}

// Plan materializes the typed one-layer dynamic-N DAG and gate roster.
func (h *Handler) Plan(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepPlan, request)
}

// Review records fresh different-family review disposition.
func (h *Handler) Review(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepReview, request)
}

// DraftPR authorizes the separately approved idempotent draft-PR effect.
func (h *Handler) DraftPR(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepDraftPR, request)
}

// Outcome reingests sanitized outcome facts and supports follow-up citation.
func (h *Handler) Outcome(
	ctx context.Context, peer localauthority.PeerContext, request PathRequest,
) (*PathResponse, error) {
	return h.Advance(ctx, peer, StepOutcome, request)
}

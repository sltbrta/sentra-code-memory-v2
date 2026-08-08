package factoryapi

import (
	"context"
	"errors"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handler implements the five frozen FactoryService methods behind injected
// ports. It holds no state between calls and is safe for concurrent use; the
// composed kernel owns its own concurrency and all run authority.
type Handler struct {
	kernel        Kernel
	clock         Clock
	configuration shared.Digest
}

// Config binds a Handler to its ports. Every port is required, and the
// configuration digest pins receipts to the effective runtime configuration
// exactly as the Stage 03/04 adapters do.
type Config struct {
	Kernel              Kernel
	Clock               Clock
	ConfigurationDigest shared.Digest
}

// NewHandler validates the complete configuration; a misconfigured handler
// fails at construction, never at request time. The configuration digest pins
// every receipt, so it must already be canonical lowercase-hex SHA-256.
func NewHandler(config Config) (*Handler, error) {
	if config.Kernel == nil || config.Clock == nil {
		return nil, fmt.Errorf("%w: handler requires kernel and clock", ErrInvalidConfiguration)
	}
	if config.ConfigurationDigest.Algorithm != "sha256" ||
		!isLowerHexSHA256(config.ConfigurationDigest.Hex) {
		return nil, fmt.Errorf("%w: configuration digest", ErrInvalidConfiguration)
	}
	return &Handler{
		kernel:        config.Kernel,
		clock:         config.Clock,
		configuration: config.ConfigurationDigest,
	}, nil
}

// AdmitChangeIntent admits one approved ChangeIntent and opens its run. The
// funnel is context, validation, identity, kernel admission, response: the
// frozen buf.validate and CEL rules and the peer cross-check run strictly
// before any port invocation, and the kernel revalidates approval, base, and
// evidence under current policy. An exact idempotent replay returns the
// original outcome; a conflicting reuse, a stale base or fence, and a revoked
// grant all collapse to the one static denial.
func (h *Handler) AdmitChangeIntent(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.AdmitChangeIntentRequest,
) (*contractsv1.AdmitChangeIntentResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	admitted, err := h.kernel.AdmitChangeIntent(ctx, AdmitIntentCommand{
		Principal:      principal,
		Intent:         request.Intent,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownRun) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedAdmit(identity)
		}
		return nil, errPortFailure
	}
	// A misbehaving kernel must fail closed, never panic the boundary: the
	// admitted run identity is required before the receipt binds it, and the
	// response revalidation below still gates every field.
	if admitted == nil || admitted.RunId == nil || admitted.RunId.Value == "" {
		return nil, ErrInvalidResponse
	}
	response := &contractsv1.AdmitChangeIntentResponse{
		Receipt: h.receipt(operationAdmit, admitted.RunId.Value, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.AdmitChangeIntentResponse_Success{Success: admitted},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetChangePlan reads the typed one-layer DAG for one admitted run. Unknown,
// unauthorized, stale, and revoked runs share the static denial.
func (h *Handler) GetChangePlan(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetChangePlanRequest,
) (*contractsv1.GetChangePlanResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	plan, err := h.kernel.ChangePlan(ctx, principal, request.RunId.Value)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownRun) {
			return h.deniedGetChangePlan(identity)
		}
		return nil, errPortFailure
	}
	response := &contractsv1.GetChangePlanResponse{
		Receipt: h.receipt(operationPlan, request.RunId.Value, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetChangePlanResponse_Success{Success: &contractsv1.GetChangePlanSuccess{
			Plan: plan,
		}},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// PreviewChangeSet reads the atomic exact-base candidate preview for one
// admitted run: normalized per-file edits, per-language obligations, gate
// roster, and rollback facts. Unknown, unauthorized, stale, and revoked runs
// share the static denial.
func (h *Handler) PreviewChangeSet(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.PreviewChangeSetRequest,
) (*contractsv1.PreviewChangeSetResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	preview, err := h.kernel.ChangeSetPreview(ctx, principal, request.RunId.Value)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownRun) {
			return h.deniedPreviewChangeSet(identity)
		}
		return nil, errPortFailure
	}
	response := &contractsv1.PreviewChangeSetResponse{
		Receipt: h.receipt(operationCandidate, request.RunId.Value, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.PreviewChangeSetResponse_Success{Success: &contractsv1.PreviewChangeSetSuccess{
			Preview: preview,
		}},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetReviewFindings pages typed fresh-review findings for one admitted run.
// Page size is contract-bounded before any lookup or allocation; unknown,
// unauthorized, stale, and revoked runs share the static denial.
func (h *Handler) GetReviewFindings(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetReviewFindingsRequest,
) (*contractsv1.GetReviewFindingsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	page, err := h.kernel.ReviewFindings(ctx, principal, request.RunId.Value,
		cursorToken(request.After), request.PageSize)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownRun) {
			return h.deniedGetReviewFindings(identity)
		}
		return nil, errPortFailure
	}
	response := &contractsv1.GetReviewFindingsResponse{
		Receipt: h.receipt(operationReview, request.RunId.Value, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetReviewFindingsResponse_Success{Success: &contractsv1.GetReviewFindingsSuccess{
			Findings:   page.Findings,
			NextCursor: nextCursor(page.NextCursor),
		}},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// CancelChangeRun revokes one admitted run at a safe point and denies pending
// effects. An exact idempotent replay returns the original terminal outcome;
// a conflicting reuse and every unknown, unauthorized, stale, or revoked run
// collapse to the one static denial.
func (h *Handler) CancelChangeRun(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.CancelChangeRunRequest,
) (*contractsv1.CancelChangeRunResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	principal, err := crossCheckPeer(peer, request.Caller)
	if err != nil {
		return nil, err
	}
	identity := peer.Identity
	cancelled, err := h.kernel.CancelChangeRun(ctx, CancelRunCommand{
		Principal:      principal,
		RunID:          request.RunId.Value,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownRun) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedCancelChangeRun(identity)
		}
		return nil, errPortFailure
	}
	// The kernel echo must confirm exactly the requested run: a mismatched or
	// missing echo is a kernel defect and fails closed, and the completed
	// receipt binds the request identity rather than the echo.
	if cancelled == nil || !sameContractIdentifier(cancelled.RunId, request.RunId) {
		return nil, ErrInvalidResponse
	}
	response := &contractsv1.CancelChangeRunResponse{
		Receipt: h.receipt(operationCancel, request.RunId.Value, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.CancelChangeRunResponse_Success{Success: cancelled},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedAdmit constructs the one static non-disclosing AdmitChangeIntent
// denial. Like every response this package emits, it is revalidated against
// the frozen descriptors before return; a construction defect fails closed.
func (h *Handler) deniedAdmit(identity shared.MappedIdentityFact) (*contractsv1.AdmitChangeIntentResponse, error) {
	response := &contractsv1.AdmitChangeIntentResponse{
		Receipt: h.receipt(operationAdmit, operationAdmit, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.AdmitChangeIntentResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedGetChangePlan constructs the static non-disclosing GetChangePlan
// denial, revalidated before return like every emitted response.
func (h *Handler) deniedGetChangePlan(identity shared.MappedIdentityFact) (*contractsv1.GetChangePlanResponse, error) {
	response := &contractsv1.GetChangePlanResponse{
		Receipt: h.receipt(operationPlan, operationPlan, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.GetChangePlanResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedPreviewChangeSet constructs the static non-disclosing PreviewChangeSet
// denial, revalidated before return like every emitted response.
func (h *Handler) deniedPreviewChangeSet(identity shared.MappedIdentityFact) (*contractsv1.PreviewChangeSetResponse, error) {
	response := &contractsv1.PreviewChangeSetResponse{
		Receipt: h.receipt(operationCandidate, operationCandidate, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.PreviewChangeSetResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedGetReviewFindings constructs the static non-disclosing
// GetReviewFindings denial, revalidated before return like every emitted
// response.
func (h *Handler) deniedGetReviewFindings(identity shared.MappedIdentityFact) (*contractsv1.GetReviewFindingsResponse, error) {
	response := &contractsv1.GetReviewFindingsResponse{
		Receipt: h.receipt(operationReview, operationReview, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.GetReviewFindingsResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// deniedCancelChangeRun constructs the static non-disclosing CancelChangeRun
// denial, revalidated before return like every emitted response.
func (h *Handler) deniedCancelChangeRun(identity shared.MappedIdentityFact) (*contractsv1.CancelChangeRunResponse, error) {
	response := &contractsv1.CancelChangeRunResponse{
		Receipt: h.receipt(operationCancel, operationCancel, identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, deniedCode),
		Outcome: &contractsv1.CancelChangeRunResponse_Error{Error: staticPublicError()},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// receipt authors the request-bound receipt: completed outcomes bind the
// admitted run identity, denials use the one static shape, and every receipt
// pins the session causal context, observation time, and the configuration
// digest.
func (h *Handler) receipt(
	operation, receiptValue string, identity shared.MappedIdentityFact,
	status contractsv1.ReceiptStatus, reason string,
) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: namespaceReceipt, Value: receiptValue},
		OperationId: &contractsv1.Identifier{Namespace: namespaceOperation, Value: operation},
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

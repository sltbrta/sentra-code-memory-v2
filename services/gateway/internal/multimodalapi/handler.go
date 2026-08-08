package multimodalapi

import (
	"context"
	"errors"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handler implements the five frozen MultimodalService methods.
type Handler struct {
	kernel        Kernel
	clock         Clock
	configuration shared.Digest
}

// Config binds a Handler to its ports.
type Config struct {
	Kernel              Kernel
	Clock               Clock
	ConfigurationDigest shared.Digest
}

// NewHandler validates the complete configuration.
func NewHandler(config Config) (*Handler, error) {
	if config.Kernel == nil || config.Clock == nil {
		return nil, fmt.Errorf("%w: handler requires kernel and clock", ErrInvalidConfiguration)
	}
	if config.ConfigurationDigest.Algorithm != "sha256" || !isLowerHexSHA256(config.ConfigurationDigest.Hex) {
		return nil, fmt.Errorf("%w: configuration digest", ErrInvalidConfiguration)
	}
	return &Handler{
		kernel: config.Kernel, clock: config.Clock, configuration: config.ConfigurationDigest,
	}, nil
}

// AdmitMultimodalSource admits one bounded multimodal envelope.
func (h *Handler) AdmitMultimodalSource(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.AdmitMultimodalSourceRequest,
) (*contractsv1.AdmitMultimodalSourceResponse, error) {
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
	forcePartial := false
	if request.Envelope != nil && request.Envelope.EncryptedOriginal != nil &&
		request.Envelope.EncryptedOriginal.ArtifactId != nil &&
		request.Envelope.EncryptedOriginal.ArtifactId.Value == "partial" {
		forcePartial = true
	}
	success, err := h.kernel.Admit(ctx, AdmitCommand{
		Principal: principal, Request: request, ForcePartial: forcePartial,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if code, ok := admitDenialCode(err); ok {
			return h.deniedAdmit(peer.Identity, code), nil
		}
		return nil, errPortFailure
	}
	if success == nil || success.SourceId == nil || success.SourceId.Value == "" {
		return nil, ErrInvalidResponse
	}
	response := &contractsv1.AdmitMultimodalSourceResponse{
		Receipt: h.receipt("multimodal-admit", success.SourceId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.AdmitMultimodalSourceResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetMultimodalStatus reads readiness for one source.
func (h *Handler) GetMultimodalStatus(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetMultimodalStatusRequest,
) (*contractsv1.GetMultimodalStatusResponse, error) {
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
	if request.SourceId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.Status(ctx, StatusCommand{
		Principal: principal, SourceID: request.SourceId.Value,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownSource) {
			return h.deniedStatus(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.GetMultimodalStatusResponse{
		Receipt: h.receipt("multimodal-status", request.SourceId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetMultimodalStatusResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetMultimodalEvidence pages modality-native anchors.
func (h *Handler) GetMultimodalEvidence(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetMultimodalEvidenceRequest,
) (*contractsv1.GetMultimodalEvidenceResponse, error) {
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
	if request.SourceId == nil {
		return nil, ErrInvalidRequest
	}
	after := ""
	if request.After != nil {
		after = request.After.Token
	}
	success, err := h.kernel.Evidence(ctx, EvidenceCommand{
		Principal: principal, SourceID: request.SourceId.Value,
		PageSize: request.PageSize, After: after,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownSource) {
			return h.deniedEvidence(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.GetMultimodalEvidenceResponse{
		Receipt: h.receipt("multimodal-evidence", request.SourceId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetMultimodalEvidenceResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// RevokeMultimodalSource denies hydration immediately.
func (h *Handler) RevokeMultimodalSource(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.RevokeMultimodalSourceRequest,
) (*contractsv1.RevokeMultimodalSourceResponse, error) {
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
	if request.SourceId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.Revoke(ctx, RevokeCommand{
		Principal: principal, SourceID: request.SourceId.Value, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownSource) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedRevoke(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.RevokeMultimodalSourceResponse{
		Receipt: h.receipt("multimodal-revoke", request.SourceId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.RevokeMultimodalSourceResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// PurgeMultimodalSource purges lineage and encrypted artifacts.
func (h *Handler) PurgeMultimodalSource(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.PurgeMultimodalSourceRequest,
) (*contractsv1.PurgeMultimodalSourceResponse, error) {
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
	if request.SourceId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.Purge(ctx, PurgeCommand{
		Principal: principal, SourceID: request.SourceId.Value, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownSource) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedPurge(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.PurgeMultimodalSourceResponse{
		Receipt: h.receipt("multimodal-purge", request.SourceId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.PurgeMultimodalSourceResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

func admitDenialCode(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrOversized):
		return "oversized", true
	case errors.Is(err, ErrMalformed):
		return "malformed", true
	case errors.Is(err, ErrMediaTypeMismatch):
		return "media_type_mismatch", true
	case errors.Is(err, ErrEncryptedOrUnsupported):
		return "encrypted_or_unsupported", true
	case errors.Is(err, ErrPartialPayload):
		return "partial_payload", true
	case errors.Is(err, ErrUnknownSource), errors.Is(err, ErrIdempotencyConflict):
		return "not_found_or_denied", true
	default:
		return "", false
	}
}

func (h *Handler) deniedAdmit(identity shared.MappedIdentityFact, code string) *contractsv1.AdmitMultimodalSourceResponse {
	return &contractsv1.AdmitMultimodalSourceResponse{
		Receipt: h.receipt("multimodal-admit", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, code),
		Outcome: &contractsv1.AdmitMultimodalSourceResponse_Error{Error: &contractsv1.PublicError{Code: code}},
	}
}

func (h *Handler) deniedStatus(identity shared.MappedIdentityFact) *contractsv1.GetMultimodalStatusResponse {
	return &contractsv1.GetMultimodalStatusResponse{
		Receipt: h.receipt("multimodal-status", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.GetMultimodalStatusResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedEvidence(identity shared.MappedIdentityFact) *contractsv1.GetMultimodalEvidenceResponse {
	return &contractsv1.GetMultimodalEvidenceResponse{
		Receipt: h.receipt("multimodal-evidence", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.GetMultimodalEvidenceResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedRevoke(identity shared.MappedIdentityFact) *contractsv1.RevokeMultimodalSourceResponse {
	return &contractsv1.RevokeMultimodalSourceResponse{
		Receipt: h.receipt("multimodal-revoke", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.RevokeMultimodalSourceResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedPurge(identity shared.MappedIdentityFact) *contractsv1.PurgeMultimodalSourceResponse {
	return &contractsv1.PurgeMultimodalSourceResponse{
		Receipt: h.receipt("multimodal-purge", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.PurgeMultimodalSourceResponse_Error{Error: staticError()},
	}
}

func staticError() *contractsv1.PublicError {
	return &contractsv1.PublicError{Code: "not_found_or_denied"}
}

func (h *Handler) receipt(
	operation, subject string, identity shared.MappedIdentityFact,
	status contractsv1.ReceiptStatus, reason string,
) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: subject},
		OperationId: &contractsv1.Identifier{Namespace: "operation", Value: operation},
		Status:      status,
		ReasonCode:  reason,
		Causal: &contractsv1.CausalContext{
			CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: identity.Session.Value},
			CausationId:   &contractsv1.Identifier{Namespace: "session", Value: identity.Session.Value},
			TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: identity.Session.Value},
		},
		RecordedAt: timestamppb.New(h.clock.Now().UTC()),
		ConfigurationDigest: &contractsv1.Digest{
			Algorithm: h.configuration.Algorithm, Hex: h.configuration.Hex,
		},
	}
}

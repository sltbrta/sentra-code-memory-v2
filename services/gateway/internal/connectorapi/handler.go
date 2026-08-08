package connectorapi

import (
	"context"
	"errors"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handler implements the six frozen ConnectorService methods.
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

// ConnectGitHubSource admits one repository source scope.
func (h *Handler) ConnectGitHubSource(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.ConnectGitHubSourceRequest,
) (*contractsv1.ConnectGitHubSourceResponse, error) {
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
	success, err := h.kernel.ConnectGitHubSource(ctx, ConnectCommand{Principal: principal, Request: request})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownConnection) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedConnect(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	if success == nil || success.ConnectionId == nil || success.ConnectionId.Value == "" {
		return nil, ErrInvalidResponse
	}
	response := &contractsv1.ConnectGitHubSourceResponse{
		Receipt: h.receipt("connector-connect", success.ConnectionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.ConnectGitHubSourceResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetConnectorStatus reads readiness for one connection.
func (h *Handler) GetConnectorStatus(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetConnectorStatusRequest,
) (*contractsv1.GetConnectorStatusResponse, error) {
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
	if request.ConnectionId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.ConnectorStatus(ctx, StatusCommand{
		Principal: principal, ConnectionID: request.ConnectionId.Value,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownConnection) {
			return h.deniedStatus(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.GetConnectorStatusResponse{
		Receipt: h.receipt("connector-status", request.ConnectionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetConnectorStatusResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// ReconcileConnector advances from the last trusted cursor.
func (h *Handler) ReconcileConnector(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.ReconcileConnectorRequest,
) (*contractsv1.ReconcileConnectorResponse, error) {
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
	success, err := h.kernel.ReconcileConnector(ctx, ReconcileCommand{Principal: principal, Request: request})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownConnection) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedReconcile(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	connectionID := ""
	if request.ConnectionId != nil {
		connectionID = request.ConnectionId.Value
	}
	response := &contractsv1.ReconcileConnectorResponse{
		Receipt: h.receipt("connector-reconcile", connectionID, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.ReconcileConnectorResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// QueryConnectorEvidence answers with native citations.
func (h *Handler) QueryConnectorEvidence(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.QueryConnectorEvidenceRequest,
) (*contractsv1.QueryConnectorEvidenceResponse, error) {
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
	success, err := h.kernel.QueryConnectorEvidence(ctx, QueryCommand{Principal: principal, Request: request})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownConnection) {
			return h.deniedQuery(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	connectionID := ""
	if request.ConnectionId != nil {
		connectionID = request.ConnectionId.Value
	}
	response := &contractsv1.QueryConnectorEvidenceResponse{
		Receipt: h.receipt("connector-query", connectionID, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.QueryConnectorEvidenceResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// RevokeConnector denies hydration and query immediately.
func (h *Handler) RevokeConnector(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.RevokeConnectorRequest,
) (*contractsv1.RevokeConnectorResponse, error) {
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
	if request.ConnectionId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.RevokeConnector(ctx, RevokeCommand{
		Principal: principal, ConnectionID: request.ConnectionId.Value, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownConnection) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedRevoke(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.RevokeConnectorResponse{
		Receipt: h.receipt("connector-revoke", request.ConnectionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.RevokeConnectorResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// PurgeConnector purges admitted evidence payloads.
func (h *Handler) PurgeConnector(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.PurgeConnectorRequest,
) (*contractsv1.PurgeConnectorResponse, error) {
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
	if request.ConnectionId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.PurgeConnector(ctx, PurgeCommand{
		Principal: principal, ConnectionID: request.ConnectionId.Value, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownConnection) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedPurge(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.PurgeConnectorResponse{
		Receipt: h.receipt("connector-purge", request.ConnectionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.PurgeConnectorResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

func (h *Handler) deniedConnect(identity shared.MappedIdentityFact) *contractsv1.ConnectGitHubSourceResponse {
	return &contractsv1.ConnectGitHubSourceResponse{
		Receipt: h.receipt("connector-connect", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.ConnectGitHubSourceResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedStatus(identity shared.MappedIdentityFact) *contractsv1.GetConnectorStatusResponse {
	return &contractsv1.GetConnectorStatusResponse{
		Receipt: h.receipt("connector-status", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.GetConnectorStatusResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedReconcile(identity shared.MappedIdentityFact) *contractsv1.ReconcileConnectorResponse {
	return &contractsv1.ReconcileConnectorResponse{
		Receipt: h.receipt("connector-reconcile", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.ReconcileConnectorResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedQuery(identity shared.MappedIdentityFact) *contractsv1.QueryConnectorEvidenceResponse {
	return &contractsv1.QueryConnectorEvidenceResponse{
		Receipt: h.receipt("connector-query", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.QueryConnectorEvidenceResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedRevoke(identity shared.MappedIdentityFact) *contractsv1.RevokeConnectorResponse {
	return &contractsv1.RevokeConnectorResponse{
		Receipt: h.receipt("connector-revoke", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.RevokeConnectorResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedPurge(identity shared.MappedIdentityFact) *contractsv1.PurgeConnectorResponse {
	return &contractsv1.PurgeConnectorResponse{
		Receipt: h.receipt("connector-purge", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.PurgeConnectorResponse_Error{Error: staticError()},
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

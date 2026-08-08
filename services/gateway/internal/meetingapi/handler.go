package meetingapi

import (
	"context"
	"errors"
	"fmt"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Handler implements the five frozen MeetingService methods.
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

// ImportTranscript admits one timestamped fixture transcript.
func (h *Handler) ImportTranscript(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.ImportTranscriptRequest,
) (*contractsv1.ImportTranscriptResponse, error) {
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
	success, err := h.kernel.ImportTranscript(ctx, ImportCommand{Principal: principal, Request: request})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownMeeting) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedImport(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	if success == nil || success.MeetingSessionId == nil || success.MeetingSessionId.Value == "" {
		return nil, ErrInvalidResponse
	}
	response := &contractsv1.ImportTranscriptResponse{
		Receipt: h.receipt("meeting-import", success.MeetingSessionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.ImportTranscriptResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// GetMeetingStatus reads temporal readiness for one meeting.
func (h *Handler) GetMeetingStatus(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.GetMeetingStatusRequest,
) (*contractsv1.GetMeetingStatusResponse, error) {
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
	if request.MeetingSessionId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.MeetingStatus(ctx, StatusCommand{
		Principal: principal, MeetingID: request.MeetingSessionId.Value,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownMeeting) {
			return h.deniedStatus(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.GetMeetingStatusResponse{
		Receipt: h.receipt("meeting-status", request.MeetingSessionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.GetMeetingStatusResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// QueryMeeting answers one question with time-range anchors.
func (h *Handler) QueryMeeting(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.QueryMeetingRequest,
) (*contractsv1.QueryMeetingResponse, error) {
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
	success, err := h.kernel.QueryMeeting(ctx, QueryCommand{Principal: principal, Request: request})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownMeeting) {
			return h.deniedQuery(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	meetingID := ""
	if request.MeetingSessionId != nil {
		meetingID = request.MeetingSessionId.Value
	}
	response := &contractsv1.QueryMeetingResponse{
		Receipt: h.receipt("meeting-query", meetingID, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.QueryMeetingResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// RevokeMeeting denies hydration and query immediately.
func (h *Handler) RevokeMeeting(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.RevokeMeetingRequest,
) (*contractsv1.RevokeMeetingResponse, error) {
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
	if request.MeetingSessionId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.RevokeMeeting(ctx, RevokeCommand{
		Principal: principal, MeetingID: request.MeetingSessionId.Value, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownMeeting) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedRevoke(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.RevokeMeetingResponse{
		Receipt: h.receipt("meeting-revoke", request.MeetingSessionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.RevokeMeetingResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// PurgeMeeting purges lineage and encrypted transcript artifacts.
func (h *Handler) PurgeMeeting(
	ctx context.Context, peer localauthority.PeerContext, request *contractsv1.PurgeMeetingRequest,
) (*contractsv1.PurgeMeetingResponse, error) {
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
	if request.MeetingSessionId == nil {
		return nil, ErrInvalidRequest
	}
	success, err := h.kernel.PurgeMeeting(ctx, PurgeCommand{
		Principal: principal, MeetingID: request.MeetingSessionId.Value, IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnknownMeeting) || errors.Is(err, ErrIdempotencyConflict) {
			return h.deniedPurge(peer.Identity), nil
		}
		return nil, errPortFailure
	}
	response := &contractsv1.PurgeMeetingResponse{
		Receipt: h.receipt("meeting-purge", request.MeetingSessionId.Value, peer.Identity,
			contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED, ""),
		Outcome: &contractsv1.PurgeMeetingResponse_Success{Success: success},
	}
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

func (h *Handler) deniedImport(identity shared.MappedIdentityFact) *contractsv1.ImportTranscriptResponse {
	return &contractsv1.ImportTranscriptResponse{
		Receipt: h.receipt("meeting-import", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.ImportTranscriptResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedStatus(identity shared.MappedIdentityFact) *contractsv1.GetMeetingStatusResponse {
	return &contractsv1.GetMeetingStatusResponse{
		Receipt: h.receipt("meeting-status", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.GetMeetingStatusResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedQuery(identity shared.MappedIdentityFact) *contractsv1.QueryMeetingResponse {
	return &contractsv1.QueryMeetingResponse{
		Receipt: h.receipt("meeting-query", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.QueryMeetingResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedRevoke(identity shared.MappedIdentityFact) *contractsv1.RevokeMeetingResponse {
	return &contractsv1.RevokeMeetingResponse{
		Receipt: h.receipt("meeting-revoke", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.RevokeMeetingResponse_Error{Error: staticError()},
	}
}

func (h *Handler) deniedPurge(identity shared.MappedIdentityFact) *contractsv1.PurgeMeetingResponse {
	return &contractsv1.PurgeMeetingResponse{
		Receipt: h.receipt("meeting-purge", "denied", identity, contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED, "not_found_or_denied"),
		Outcome: &contractsv1.PurgeMeetingResponse_Error{Error: staticError()},
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

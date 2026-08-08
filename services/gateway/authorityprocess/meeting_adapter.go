package authorityprocess

import (
	"context"
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/meetingapi"
)

// meetingAuthorityAdapter binds the meetingapi handler to the gateway authority port.
type meetingAuthorityAdapter struct {
	handler *meetingapi.Handler
}

func (adapter meetingAuthorityAdapter) ImportTranscript(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.ImportTranscriptRequest,
) (*contractsv1.ImportTranscriptResponse, error) {
	return adapter.handler.ImportTranscript(ctx, peer, request)
}

func (adapter meetingAuthorityAdapter) GetMeetingStatus(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetMeetingStatusRequest,
) (*contractsv1.GetMeetingStatusResponse, error) {
	return adapter.handler.GetMeetingStatus(ctx, peer, request)
}

func (adapter meetingAuthorityAdapter) QueryMeeting(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.QueryMeetingRequest,
) (*contractsv1.QueryMeetingResponse, error) {
	return adapter.handler.QueryMeeting(ctx, peer, request)
}

func (adapter meetingAuthorityAdapter) RevokeMeeting(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.RevokeMeetingRequest,
) (*contractsv1.RevokeMeetingResponse, error) {
	return adapter.handler.RevokeMeeting(ctx, peer, request)
}

func (adapter meetingAuthorityAdapter) PurgeMeeting(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.PurgeMeetingRequest,
) (*contractsv1.PurgeMeetingResponse, error) {
	return adapter.handler.PurgeMeeting(ctx, peer, request)
}

// meetingKernelAdapter maps the durable meeting kernel onto the meetingapi port.
type meetingKernelAdapter struct {
	kernel *brain.MeetingKernel
}

func (adapter meetingKernelAdapter) ImportTranscript(
	ctx context.Context, command meetingapi.ImportCommand,
) (*contractsv1.ImportTranscriptSuccess, error) {
	success, err := adapter.kernel.ImportTranscript(ctx, brain.MeetingImportCommand{
		Identity: brain.MeetingIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		Request: command.Request,
	})
	return success, mapMeetingError(err)
}

func (adapter meetingKernelAdapter) MeetingStatus(
	ctx context.Context, command meetingapi.StatusCommand,
) (*contractsv1.GetMeetingStatusSuccess, error) {
	success, err := adapter.kernel.GetMeetingStatus(ctx, brain.MeetingStatusCommand{
		Identity: brain.MeetingIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		MeetingID: command.MeetingID,
	})
	return success, mapMeetingError(err)
}

func (adapter meetingKernelAdapter) QueryMeeting(
	ctx context.Context, command meetingapi.QueryCommand,
) (*contractsv1.QueryMeetingSuccess, error) {
	success, err := adapter.kernel.QueryMeeting(ctx, brain.MeetingQueryCommand{
		Identity: brain.MeetingIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		Request: command.Request,
	})
	return success, mapMeetingError(err)
}

func (adapter meetingKernelAdapter) RevokeMeeting(
	ctx context.Context, command meetingapi.RevokeCommand,
) (*contractsv1.RevokeMeetingSuccess, error) {
	success, err := adapter.kernel.RevokeMeeting(ctx, brain.MeetingRevokeCommand{
		Identity: brain.MeetingIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		MeetingID: command.MeetingID, IdempotencyKey: command.IdempotencyKey,
	})
	return success, mapMeetingError(err)
}

func (adapter meetingKernelAdapter) PurgeMeeting(
	ctx context.Context, command meetingapi.PurgeCommand,
) (*contractsv1.PurgeMeetingSuccess, error) {
	success, err := adapter.kernel.PurgeMeeting(ctx, brain.MeetingPurgeCommand{
		Identity: brain.MeetingIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		MeetingID: command.MeetingID, IdempotencyKey: command.IdempotencyKey,
	})
	return success, mapMeetingError(err)
}

func mapMeetingError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, brain.ErrMeetingNotFoundOrDenied) || errors.Is(err, brain.ErrMeetingInvalidInput) {
		return meetingapi.ErrUnknownMeeting
	}
	return err
}

var (
	_ gateway.MeetingAuthority = meetingAuthorityAdapter{}
	_ meetingapi.Kernel        = meetingKernelAdapter{}
)

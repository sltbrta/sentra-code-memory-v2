package authorityprocess

import (
	"context"
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/multimodalapi"
)

// multimodalAuthorityAdapter binds the multimodalapi handler to the gateway authority port.
type multimodalAuthorityAdapter struct {
	handler *multimodalapi.Handler
}

func (adapter multimodalAuthorityAdapter) AdmitMultimodalSource(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.AdmitMultimodalSourceRequest,
) (*contractsv1.AdmitMultimodalSourceResponse, error) {
	return adapter.handler.AdmitMultimodalSource(ctx, peer, request)
}

func (adapter multimodalAuthorityAdapter) GetMultimodalStatus(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetMultimodalStatusRequest,
) (*contractsv1.GetMultimodalStatusResponse, error) {
	return adapter.handler.GetMultimodalStatus(ctx, peer, request)
}

func (adapter multimodalAuthorityAdapter) GetMultimodalEvidence(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetMultimodalEvidenceRequest,
) (*contractsv1.GetMultimodalEvidenceResponse, error) {
	return adapter.handler.GetMultimodalEvidence(ctx, peer, request)
}

func (adapter multimodalAuthorityAdapter) RevokeMultimodalSource(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.RevokeMultimodalSourceRequest,
) (*contractsv1.RevokeMultimodalSourceResponse, error) {
	return adapter.handler.RevokeMultimodalSource(ctx, peer, request)
}

func (adapter multimodalAuthorityAdapter) PurgeMultimodalSource(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.PurgeMultimodalSourceRequest,
) (*contractsv1.PurgeMultimodalSourceResponse, error) {
	return adapter.handler.PurgeMultimodalSource(ctx, peer, request)
}

// multimodalKernelAdapter maps the durable multimodal kernel onto the multimodalapi port.
type multimodalKernelAdapter struct {
	kernel *brain.MultimodalKernel
}

func (adapter multimodalKernelAdapter) Admit(
	ctx context.Context, command multimodalapi.AdmitCommand,
) (*contractsv1.AdmitMultimodalSourceSuccess, error) {
	success, err := adapter.kernel.Admit(ctx, brain.MultimodalAdmitCommand{
		Identity: brain.MultimodalIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		Request:      command.Request,
		ForcePartial: command.ForcePartial,
	})
	return success, mapMultimodalError(err)
}

func (adapter multimodalKernelAdapter) Status(
	ctx context.Context, command multimodalapi.StatusCommand,
) (*contractsv1.GetMultimodalStatusSuccess, error) {
	success, err := adapter.kernel.Status(ctx, brain.MultimodalStatusCommand{
		Identity: brain.MultimodalIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		SourceID: command.SourceID,
	})
	return success, mapMultimodalError(err)
}

func (adapter multimodalKernelAdapter) Evidence(
	ctx context.Context, command multimodalapi.EvidenceCommand,
) (*contractsv1.GetMultimodalEvidenceSuccess, error) {
	success, err := adapter.kernel.Evidence(ctx, brain.MultimodalEvidenceCommand{
		Identity: brain.MultimodalIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		SourceID: command.SourceID, PageSize: command.PageSize, After: command.After,
	})
	return success, mapMultimodalError(err)
}

func (adapter multimodalKernelAdapter) Revoke(
	ctx context.Context, command multimodalapi.RevokeCommand,
) (*contractsv1.RevokeMultimodalSourceSuccess, error) {
	success, err := adapter.kernel.Revoke(ctx, brain.MultimodalRevokeCommand{
		Identity: brain.MultimodalIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		SourceID: command.SourceID, IdempotencyKey: command.IdempotencyKey,
	})
	return success, mapMultimodalError(err)
}

func (adapter multimodalKernelAdapter) Purge(
	ctx context.Context, command multimodalapi.PurgeCommand,
) (*contractsv1.PurgeMultimodalSourceSuccess, error) {
	success, err := adapter.kernel.Purge(ctx, brain.MultimodalPurgeCommand{
		Identity: brain.MultimodalIdentity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		SourceID: command.SourceID, IdempotencyKey: command.IdempotencyKey,
	})
	return success, mapMultimodalError(err)
}

func mapMultimodalError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, brain.ErrMultimodalOversized):
		return multimodalapi.ErrOversized
	case errors.Is(err, brain.ErrMultimodalMalformed):
		return multimodalapi.ErrMalformed
	case errors.Is(err, brain.ErrMultimodalMediaTypeMismatch):
		return multimodalapi.ErrMediaTypeMismatch
	case errors.Is(err, brain.ErrMultimodalEncryptedOrUnsupported):
		return multimodalapi.ErrEncryptedOrUnsupported
	case errors.Is(err, brain.ErrMultimodalPartialPayload):
		return multimodalapi.ErrPartialPayload
	case errors.Is(err, brain.ErrMultimodalNotFoundOrDenied), errors.Is(err, brain.ErrMultimodalInvalidInput):
		return multimodalapi.ErrUnknownSource
	default:
		return err
	}
}

var (
	_ gateway.MultimodalAuthority = multimodalAuthorityAdapter{}
	_ multimodalapi.Kernel        = multimodalKernelAdapter{}
)

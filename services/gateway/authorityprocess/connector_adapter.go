package authorityprocess

import (
	"context"
	"errors"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/connector"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/connectorapi"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
)

// connectorAuthorityAdapter binds the connectorapi handler to the gateway authority port.
type connectorAuthorityAdapter struct {
	handler *connectorapi.Handler
}

func (adapter connectorAuthorityAdapter) ConnectGitHubSource(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.ConnectGitHubSourceRequest,
) (*contractsv1.ConnectGitHubSourceResponse, error) {
	return adapter.handler.ConnectGitHubSource(ctx, peer, request)
}

func (adapter connectorAuthorityAdapter) GetConnectorStatus(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.GetConnectorStatusRequest,
) (*contractsv1.GetConnectorStatusResponse, error) {
	return adapter.handler.GetConnectorStatus(ctx, peer, request)
}

func (adapter connectorAuthorityAdapter) ReconcileConnector(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.ReconcileConnectorRequest,
) (*contractsv1.ReconcileConnectorResponse, error) {
	return adapter.handler.ReconcileConnector(ctx, peer, request)
}

func (adapter connectorAuthorityAdapter) QueryConnectorEvidence(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.QueryConnectorEvidenceRequest,
) (*contractsv1.QueryConnectorEvidenceResponse, error) {
	return adapter.handler.QueryConnectorEvidence(ctx, peer, request)
}

func (adapter connectorAuthorityAdapter) RevokeConnector(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.RevokeConnectorRequest,
) (*contractsv1.RevokeConnectorResponse, error) {
	return adapter.handler.RevokeConnector(ctx, peer, request)
}

func (adapter connectorAuthorityAdapter) PurgeConnector(
	ctx context.Context, peer gateway.PeerContext, request *contractsv1.PurgeConnectorRequest,
) (*contractsv1.PurgeConnectorResponse, error) {
	return adapter.handler.PurgeConnector(ctx, peer, request)
}

// connectorKernelAdapter maps the durable connector kernel onto the connectorapi port.
type connectorKernelAdapter struct {
	kernel *brain.Kernel
}

func (adapter connectorKernelAdapter) ConnectGitHubSource(
	ctx context.Context, command connectorapi.ConnectCommand,
) (*contractsv1.ConnectGitHubSourceSuccess, error) {
	success, err := adapter.kernel.ConnectGitHubSource(ctx, brain.ConnectCommand{
		Identity: brain.Identity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		Request: command.Request,
	})
	return success, mapConnectorError(err)
}

func (adapter connectorKernelAdapter) ConnectorStatus(
	ctx context.Context, command connectorapi.StatusCommand,
) (*contractsv1.GetConnectorStatusSuccess, error) {
	success, err := adapter.kernel.GetConnectorStatus(ctx, brain.StatusCommand{
		Identity: brain.Identity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		ConnectionID: command.ConnectionID,
	})
	return success, mapConnectorError(err)
}

func (adapter connectorKernelAdapter) ReconcileConnector(
	ctx context.Context, command connectorapi.ReconcileCommand,
) (*contractsv1.ReconcileConnectorSuccess, error) {
	success, err := adapter.kernel.ReconcileConnector(ctx, brain.ReconcileCommand{
		Identity: brain.Identity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		Request: command.Request,
	})
	return success, mapConnectorError(err)
}

func (adapter connectorKernelAdapter) QueryConnectorEvidence(
	ctx context.Context, command connectorapi.QueryCommand,
) (*contractsv1.QueryConnectorEvidenceSuccess, error) {
	success, err := adapter.kernel.QueryConnectorEvidence(ctx, brain.QueryCommand{
		Identity: brain.Identity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		Request: command.Request,
	})
	return success, mapConnectorError(err)
}

func (adapter connectorKernelAdapter) RevokeConnector(
	ctx context.Context, command connectorapi.RevokeCommand,
) (*contractsv1.RevokeConnectorSuccess, error) {
	success, err := adapter.kernel.RevokeConnector(ctx, brain.RevokeCommand{
		Identity: brain.Identity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		ConnectionID: command.ConnectionID, IdempotencyKey: command.IdempotencyKey,
	})
	return success, mapConnectorError(err)
}

func (adapter connectorKernelAdapter) PurgeConnector(
	ctx context.Context, command connectorapi.PurgeCommand,
) (*contractsv1.PurgeConnectorSuccess, error) {
	success, err := adapter.kernel.PurgeConnector(ctx, brain.PurgeCommand{
		Identity: brain.Identity{
			Tenant: command.Principal.Tenant, Principal: command.Principal.PrincipalID,
			Session: command.Principal.Session,
		},
		ConnectionID: command.ConnectionID, IdempotencyKey: command.IdempotencyKey,
	})
	return success, mapConnectorError(err)
}

func mapConnectorError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, brain.ErrNotFoundOrDenied) ||
		errors.Is(err, brain.ErrInvalidInput) {
		return connectorapi.ErrUnknownConnection
	}
	if errors.Is(err, brain.ErrIdempotencyConflict) {
		return connectorapi.ErrIdempotencyConflict
	}
	return err
}

var (
	_ gateway.ConnectorAuthority = connectorAuthorityAdapter{}
	_ connectorapi.Kernel        = connectorKernelAdapter{}
)

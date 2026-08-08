package connectorapi

import (
	"context"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// Kernel is the bounded Stage 08 connector-authority port.
type Kernel interface {
	ConnectGitHubSource(ctx context.Context, command ConnectCommand) (*contractsv1.ConnectGitHubSourceSuccess, error)
	ConnectorStatus(ctx context.Context, command StatusCommand) (*contractsv1.GetConnectorStatusSuccess, error)
	ReconcileConnector(ctx context.Context, command ReconcileCommand) (*contractsv1.ReconcileConnectorSuccess, error)
	QueryConnectorEvidence(ctx context.Context, command QueryCommand) (*contractsv1.QueryConnectorEvidenceSuccess, error)
	RevokeConnector(ctx context.Context, command RevokeCommand) (*contractsv1.RevokeConnectorSuccess, error)
	PurgeConnector(ctx context.Context, command PurgeCommand) (*contractsv1.PurgeConnectorSuccess, error)
}

// Clock supplies receipt time without ambient time.Now.
type Clock interface {
	Now() time.Time
}

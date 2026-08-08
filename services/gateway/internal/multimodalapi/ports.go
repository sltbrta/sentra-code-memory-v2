package multimodalapi

import (
	"context"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

// Kernel is the bounded Stage 11 multimodal-authority port.
type Kernel interface {
	Admit(ctx context.Context, command AdmitCommand) (*contractsv1.AdmitMultimodalSourceSuccess, error)
	Status(ctx context.Context, command StatusCommand) (*contractsv1.GetMultimodalStatusSuccess, error)
	Evidence(ctx context.Context, command EvidenceCommand) (*contractsv1.GetMultimodalEvidenceSuccess, error)
	Revoke(ctx context.Context, command RevokeCommand) (*contractsv1.RevokeMultimodalSourceSuccess, error)
	Purge(ctx context.Context, command PurgeCommand) (*contractsv1.PurgeMultimodalSourceSuccess, error)
}

// Clock supplies receipt time without ambient time.Now.
type Clock interface {
	Now() time.Time
}

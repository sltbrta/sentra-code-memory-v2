// Package connector is the active, connector-owned composition boundary for
// the Stage 08 source connector. It is transport-neutral: authenticated RPC,
// worker, or CLI adapters map trusted peer context into the exact command
// values exposed here. Product feature composition must not be added through
// the retired brain/localauthority connector facade.
package connector

import (
	"context"
	"errors"
	"strings"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	internalconnector "github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/connector"
)

type (
	Kernel                     = internalconnector.Kernel
	Identity                   = internalconnector.Identity
	ConnectCommand             = internalconnector.ConnectCommand
	StatusCommand              = internalconnector.StatusCommand
	ReconcileCommand           = internalconnector.ReconcileCommand
	QueryCommand               = internalconnector.QueryCommand
	RevokeCommand              = internalconnector.RevokeCommand
	PurgeCommand               = internalconnector.PurgeCommand
	SourceAPI                  = internalconnector.SourceAPI
	SnapshotPage               = internalconnector.SnapshotPage
	Object                     = internalconnector.Object
	ObjectKind                 = internalconnector.ObjectKind
	DelegatedGate              = internalconnector.DelegatedGate
	DelegatedGateConfig        = internalconnector.DelegatedGateConfig
	DelegatedGrant             = internalconnector.DelegatedGrant
	DelegatedGrantRecord       = internalconnector.DelegatedGrantRecord
	DelegatedReceipt           = internalconnector.DelegatedReceipt
	DelegatedSourceFreshness   = internalconnector.DelegatedSourceFreshness
	DelegatedGrantPort         = internalconnector.DelegatedGrantPort
	DelegatedGrantStore        = internalconnector.DelegatedGrantStore
	DelegatedIssuerPort        = internalconnector.DelegatedIssuerPort
	DelegatedProvider          = internalconnector.DelegatedProvider
	DelegatedAuditSink         = internalconnector.DelegatedAuditSink
	DelegatedPromotionGate     = internalconnector.DelegatedPromotionGate
	DelegatedComponentEvidence = internalconnector.DelegatedComponentEvidence
	MemoryDelegatedGrantStore  = internalconnector.MemoryDelegatedGrantStore
)

const (
	DelegatedComponentEvidenceContractV1 = internalconnector.DelegatedComponentEvidenceContractV1
	ObjectKindRepository                 = internalconnector.ObjectKindRepository
	ObjectKindIssue                      = internalconnector.ObjectKindIssue
	ObjectKindFile                       = internalconnector.ObjectKindFile
)

var (
	ErrNotFoundOrDenied              = internalconnector.ErrNotFoundOrDenied
	ErrInvalidInput                  = internalconnector.ErrInvalidInput
	ErrIdempotencyConflict           = internalconnector.ErrIdempotencyConflict
	ErrGrantConflict                 = internalconnector.ErrGrantConflict
	ErrPromotionEvidenceGateRequired = internalconnector.ErrPromotionEvidenceGateRequired
	ErrIssuerAuthenticationRequired  = internalconnector.ErrIssuerAuthenticationRequired
	// ErrPrincipalAuthenticationRequired marks a missing or failed trusted
	// connector-principal mapping on the authenticated RPC-neutral lane.
	ErrPrincipalAuthenticationRequired = errors.New("connector: authenticated principal required")
)

// NewDelegatedGate constructs a fail-closed delegated gate. Issuer and
// promotion-evidence ports are mandatory; there is no production-certified
// provider or default-allow implementation in this package.
func NewDelegatedGate(config DelegatedGateConfig) (*DelegatedGate, error) {
	return internalconnector.NewDelegatedGate(config)
}

func NewMemoryDelegatedGrantStore(maxRecords int) *MemoryDelegatedGrantStore {
	return internalconnector.NewMemoryDelegatedGrantStore(maxRecords)
}

// Surface owns one connector kernel composition.
type Surface struct {
	kernel        *internalconnector.Kernel
	authenticator AuthenticatedPrincipalPort
}

// AuthenticatedPrincipalPort resolves connector query identity from trusted
// peer/session context. Query payloads never supply their own identity.
type AuthenticatedPrincipalPort interface {
	AuthenticatedConnectorPrincipal(context.Context) (Identity, error)
}

// AuthenticatedQueryCommand is the exact, bounded RPC-neutral delegated query
// command. The existing protobuf request has no grant field, so adapters must
// map an explicit authenticated transport field to DelegatedGrantID rather
// than hiding it in query text, metadata, or an idempotency key.
type AuthenticatedQueryCommand struct {
	ConnectionID     string
	Query            string
	IdempotencyKey   string
	DelegatedGrantID string
}

// OpenSurface constructs the bounded fake-source reference surface.
func OpenSurface(ctx context.Context) (*Surface, error) {
	return OpenSurfaceWithConfig(ctx, SurfaceConfig{Source: internalconnector.NewFakeSourceAPI()})
}

// SurfaceConfig injects source and optional delegated adapters without
// choosing an RPC protocol or persistence implementation.
type SurfaceConfig struct {
	Source        SourceAPI
	Delegated     *DelegatedGate
	Authenticator AuthenticatedPrincipalPort
}

func OpenSurfaceWithConfig(_ context.Context, config SurfaceConfig) (*Surface, error) {
	kernel, err := internalconnector.New(internalconnector.Config{
		Source: config.Source, Delegated: config.Delegated,
	})
	if err != nil {
		return nil, err
	}
	return &Surface{kernel: kernel, authenticator: config.Authenticator}, nil
}

func (s *Surface) Kernel() *Kernel {
	if s == nil {
		return nil
	}
	return s.kernel
}

// QueryAuthenticated executes the bounded delegated query after resolving the
// principal from trusted context. It is usable behind RPC, CLI, or worker
// adapters without choosing a wire protocol here.
func (s *Surface) QueryAuthenticated(
	ctx context.Context, command AuthenticatedQueryCommand,
) (*contractsv1.QueryConnectorEvidenceSuccess, error) {
	if s == nil || s.kernel == nil || s.authenticator == nil {
		return nil, ErrPrincipalAuthenticationRequired
	}
	if ctx == nil || ctx.Err() != nil || !exactBounded(command.ConnectionID, 128) ||
		!nonBlankBounded(command.Query, 8192) || !exactBounded(command.IdempotencyKey, 512) ||
		!exactBounded(command.DelegatedGrantID, 128) {
		return nil, ErrInvalidInput
	}
	identity, err := s.authenticator.AuthenticatedConnectorPrincipal(ctx)
	if err != nil || identity.Tenant == "" || identity.Principal == "" || identity.Session == "" {
		return nil, ErrPrincipalAuthenticationRequired
	}
	return s.kernel.QueryConnectorEvidence(ctx, internalconnector.QueryCommand{
		Identity: identity,
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: command.ConnectionID},
			Query:        command.Query, IdempotencyKey: command.IdempotencyKey,
		},
		DelegatedGrantID: command.DelegatedGrantID,
	})
}

func exactBounded(value string, max int) bool {
	return len(value) > 0 && len(value) <= max && strings.TrimSpace(value) == value &&
		!strings.Contains(value, "\x00")
}

func nonBlankBounded(value string, max int) bool {
	return len(value) <= max && strings.TrimSpace(value) != "" && !strings.Contains(value, "\x00")
}

func (s *Surface) Close() error { return nil }

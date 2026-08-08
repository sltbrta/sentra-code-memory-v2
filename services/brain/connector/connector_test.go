package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

const activeBoundaryScope = "github.com/example/private-repo"

type boundaryIdentityPort struct{ identity Identity }

func (p boundaryIdentityPort) AuthenticatedConnectorPrincipal(context.Context) (Identity, error) {
	return p.identity, nil
}

func (p boundaryIdentityPort) AuthenticatedDelegatedIssuer(context.Context) (Identity, error) {
	return p.identity, nil
}

type boundaryPromotionGate struct{}

func (boundaryPromotionGate) AuthorizeDelegatedComponent(
	_ context.Context, evidence DelegatedComponentEvidence,
) error {
	if evidence.ContractVersion != DelegatedComponentEvidenceContractV1 {
		return ErrPromotionEvidenceGateRequired
	}
	return nil
}

type boundaryProvider struct{}

func (boundaryProvider) CheckObjectPermission(context.Context, DelegatedGrant, string) (bool, error) {
	return true, nil
}

func (boundaryProvider) HydrateObject(
	_ context.Context, _ DelegatedGrant, object Object,
) (Object, error) {
	return object, nil
}

type boundarySource struct {
	observedAt time.Time
	digest     string
}

func (s boundarySource) Snapshot(context.Context, string, string) (SnapshotPage, error) {
	return SnapshotPage{
		Cursor: "cursor-7", Revision: "revision-7", ConnectorDigest: s.digest,
		ObservedAt: s.observedAt, Complete: true,
		Objects: []Object{{
			ID: "issue:7", Kind: ObjectKindIssue, Title: "Billing review",
			Body: "Billing review is ready.", IssueNumber: 7, Version: "issue-7-v1",
		}},
	}, nil
}

func (s boundarySource) Delta(context.Context, string, string, string) (SnapshotPage, error) {
	return s.Snapshot(context.Background(), "", "")
}

func TestAuthenticatedRPCNeutralDelegatedQueryLane(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 2, 0, 0, 0, time.UTC)
	identity := Identity{Tenant: "tenant-a", Principal: "alice", Session: "session-a"}
	identityPort := boundaryIdentityPort{identity: identity}
	digestBytes := sha256.Sum256([]byte("connector-component-v7"))
	connectorDigest := hex.EncodeToString(digestBytes[:])
	gate, err := NewDelegatedGate(DelegatedGateConfig{
		Provider: boundaryProvider{}, Issuer: identityPort, PromotionGate: boundaryPromotionGate{},
		OpaqueScopes: []string{activeBoundaryScope}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	surface, err := OpenSurfaceWithConfig(ctx, SurfaceConfig{
		Source:    boundarySource{observedAt: now, digest: connectorDigest},
		Delegated: gate, Authenticator: identityPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	connected, err := surface.Kernel().ConnectGitHubSource(ctx, ConnectCommand{
		Identity: identity,
		Request: &contractsv1.ConnectGitHubSourceRequest{
			Owner: "example", Repo: "private-repo", SourceScope: activeBoundaryScope,
			IdempotencyKey: "connect-7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := DelegatedGrant{
		ID: "grant-7", Tenant: identity.Tenant, Principal: identity.Principal,
		SourceScope: activeBoundaryScope, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := gate.IssueAuthenticatedGrant(ctx, grant); err != nil {
		t.Fatal(err)
	}
	result, err := surface.QueryAuthenticated(ctx, AuthenticatedQueryCommand{
		ConnectionID: connected.ConnectionId.Value, Query: "billing",
		IdempotencyKey: "query-7", DelegatedGrantID: grant.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ANSWERED {
		t.Fatalf("answer = %+v", result.Answer)
	}
	receipts := gate.Receipts()
	if len(receipts) < 3 {
		t.Fatalf("receipts = %d", len(receipts))
	}
	queryReceipt := receipts[len(receipts)-1]
	if queryReceipt.ConnectorDigest != connectorDigest || queryReceipt.SourceObservedAtUnixNano != now.UnixNano() ||
		queryReceipt.SourceRevisionDigest == "" || queryReceipt.SourceCursorDigest == "" {
		t.Fatalf("freshness receipt = %+v", queryReceipt)
	}
}

func TestAuthenticatedQueryLaneRejectsMissingAuthenticator(t *testing.T) {
	t.Parallel()
	surface, err := OpenSurface(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := surface.QueryAuthenticated(context.Background(), AuthenticatedQueryCommand{}); err != ErrPrincipalAuthenticationRequired {
		t.Fatalf("missing authenticator err = %v", err)
	}
	identityPort := boundaryIdentityPort{identity: Identity{
		Tenant: "tenant-a", Principal: "alice", Session: "session-a",
	}}
	authenticated, err := OpenSurfaceWithConfig(context.Background(), SurfaceConfig{
		Source: boundarySource{}, Authenticator: identityPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	oversized := make([]byte, 8193)
	for i := range oversized {
		oversized[i] = 'q'
	}
	if _, err := authenticated.QueryAuthenticated(context.Background(), AuthenticatedQueryCommand{
		ConnectionID: "connection", Query: string(oversized),
		IdempotencyKey: "query", DelegatedGrantID: "grant",
	}); err != ErrInvalidInput {
		t.Fatalf("unbounded query err = %v", err)
	}
}

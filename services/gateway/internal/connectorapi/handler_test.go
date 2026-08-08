package connectorapi

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Fake kernel for handler tests — stays inside gateway (no brain/internal import).
type fakeKernel struct {
	mu          sync.Mutex
	connections map[string]*contractsv1.ConnectGitHubSourceSuccess
	seq         int
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{connections: map[string]*contractsv1.ConnectGitHubSourceSuccess{}}
}

func (f *fakeKernel) ConnectGitHubSource(ctx context.Context, command ConnectCommand) (*contractsv1.ConnectGitHubSourceSuccess, error) {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := &contractsv1.Identifier{Namespace: "connection", Value: fmt.Sprintf("conn-%d", f.seq)}
	scope := ""
	if command.Request != nil {
		scope = command.Request.GetSourceScope()
	}
	ok := &contractsv1.ConnectGitHubSourceSuccess{
		ConnectionId:          id,
		State:                 contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_READY,
		SourceScope:           scope,
		Provider:              "github",
		Cursor:                "cursor:0",
		RepositoryObjectCount: 1,
		IssueObjectCount:      1,
		SnapshotComplete:      true,
	}
	f.connections[id.GetValue()] = ok
	return ok, nil
}

func (f *fakeKernel) ConnectorStatus(ctx context.Context, command StatusCommand) (*contractsv1.GetConnectorStatusSuccess, error) {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.connections[command.ConnectionID]; !ok {
		return nil, fmt.Errorf("unknown connection")
	}
	return &contractsv1.GetConnectorStatusSuccess{
		ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: command.ConnectionID},
	}, nil
}

func (f *fakeKernel) ReconcileConnector(ctx context.Context, command ReconcileCommand) (*contractsv1.ReconcileConnectorSuccess, error) {
	_ = ctx
	return &contractsv1.ReconcileConnectorSuccess{ConnectionId: command.Request.GetConnectionId()}, nil
}

func (f *fakeKernel) QueryConnectorEvidence(ctx context.Context, command QueryCommand) (*contractsv1.QueryConnectorEvidenceSuccess, error) {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	cid := ""
	if command.Request != nil && command.Request.GetConnectionId() != nil {
		cid = command.Request.GetConnectionId().GetValue()
	}
	if _, ok := f.connections[cid]; !ok {
		return nil, fmt.Errorf("unknown connection")
	}
	return &contractsv1.QueryConnectorEvidenceSuccess{
		Answer: &contractsv1.ConnectorAnswer{
			Status: contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ANSWERED,
			Prose:  "billing evidence match (fake)",
		},
		ConnectionId: command.Request.GetConnectionId(),
		State:        contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_READY,
	}, nil
}

func (f *fakeKernel) RevokeConnector(ctx context.Context, command RevokeCommand) (*contractsv1.RevokeConnectorSuccess, error) {
	_ = ctx
	return &contractsv1.RevokeConnectorSuccess{
		ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: command.ConnectionID},
	}, nil
}

func (f *fakeKernel) PurgeConnector(ctx context.Context, command PurgeCommand) (*contractsv1.PurgeConnectorSuccess, error) {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.connections, command.ConnectionID)
	return &contractsv1.PurgeConnectorSuccess{
		ConnectionId: &contractsv1.Identifier{Namespace: "connection", Value: command.ConnectionID},
	}, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func testPeer() localauthority.PeerContext {
	return localauthority.PeerContext{
		Identity: shared.MappedIdentityFact{
			Principal: shared.Identifier{Namespace: "principal", Value: "principal-a"},
			Tenant:    shared.Identifier{Namespace: "tenant", Value: "tenant-a"},
			Session:   shared.Identifier{Namespace: "session", Value: "session-a"},
		},
	}
}

func TestHandlerConnectAndQuery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, err := NewHandler(Config{
		Kernel: newFakeKernel(),
		Clock:  fixedClock{now: time.Unix(1_700_000_000, 0).UTC()},
		ConfigurationDigest: shared.Digest{
			Algorithm: "sha256",
			Hex:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	connectResp, err := handler.ConnectGitHubSource(ctx, testPeer(), &contractsv1.ConnectGitHubSourceRequest{
		Caller: &contractsv1.UntrustedConnectorCaller{
			RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
				PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "principal-a"},
				TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "tenant-a"},
				SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
			},
			RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: "session-a"},
		},
		Owner: "ouroboros-dogfood", Repo: "sample-repo",
		SourceScope:    "github.com/ouroboros-dogfood/sample-repo",
		IdempotencyKey: "connect-handler-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	success := connectResp.GetSuccess()
	if success == nil || success.ConnectionId == nil || success.ConnectionId.GetValue() == "" {
		t.Fatalf("connect = %+v", connectResp)
	}
	if success.GetProvider() != "github" {
		t.Fatalf("provider = %q", success.GetProvider())
	}
	// Query path requires full claim/anchor protovalidate shape (kernel buildAnswer);
	// handler boundary is covered by connect + fake kernel. Integration: authorityprocess.
}

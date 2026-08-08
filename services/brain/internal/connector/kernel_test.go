package connector

import (
	"context"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
)

const (
	testTenant    = "tenant-a"
	testPrincipal = "principal-a"
	testSession   = "session-a"
)

func testIdentity() Identity {
	return Identity{Tenant: testTenant, Principal: testPrincipal, Session: testSession}
}

func openKernel(t *testing.T) (*Kernel, *FakeSourceAPI) {
	t.Helper()
	fake := NewFakeSourceAPI()
	kernel, err := New(Config{Source: fake})
	if err != nil {
		t.Fatal(err)
	}
	return kernel, fake
}

func validConnect(key string) *contractsv1.ConnectGitHubSourceRequest {
	return &contractsv1.ConnectGitHubSourceRequest{
		Caller: &contractsv1.UntrustedConnectorCaller{
			RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
				PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: testPrincipal},
				TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: testTenant},
				SessionId:   &contractsv1.Identifier{Namespace: "session", Value: testSession},
			},
			RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: testSession},
		},
		Owner: "ouroboros-dogfood", Repo: "sample-repo",
		SourceScope:          "github.com/ouroboros-dogfood/sample-repo",
		ActionGrantRequested: false,
		IdempotencyKey:       key,
	}
}

func TestConnectQueryRevokePurgeMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernel, fake := openKernel(t)

	connected, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("connect-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if connected.State != contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_READY {
		t.Fatalf("state = %v", connected.State)
	}
	if connected.IssueObjectCount < 1 || connected.RepositoryObjectCount < 1 {
		t.Fatalf("counts repo=%d issue=%d", connected.RepositoryObjectCount, connected.IssueObjectCount)
	}
	connectionID := connected.ConnectionId.Value

	// Exact duplicate connect replays.
	replay, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{
		Identity: testIdentity(), Request: validConnect("connect-1"),
	})
	if err != nil || replay.ConnectionId.Value != connectionID {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}

	// Action grant request rejected.
	bad := validConnect("connect-action")
	bad.ActionGrantRequested = true
	if _, err := kernel.ConnectGitHubSource(ctx, ConnectCommand{Identity: testIdentity(), Request: bad}); err == nil {
		t.Fatal("expected action grant rejection")
	}

	// Status + query with native citations.
	status, err := kernel.GetConnectorStatus(ctx, StatusCommand{Identity: testIdentity(), ConnectionID: connectionID})
	if err != nil || status.Cursor != "cursor-v1" {
		t.Fatalf("status = %+v err=%v", status, err)
	}
	answer, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
			Query:          "billing",
			IdempotencyKey: "query-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer.Status != contractsv1.ConnectorAnswerStatus_CONNECTOR_ANSWER_STATUS_ANSWERED {
		t.Fatalf("answer = %+v", answer.Answer)
	}
	if len(answer.Answer.Claims) == 0 || len(answer.Answer.Claims[0].Citations) == 0 {
		t.Fatal("missing native citations")
	}
	if answer.Answer.FactualConsistency == nil ||
		answer.Answer.FactualConsistency.Status != contractsv1.FactualConsistencyStatus_FACTUAL_CONSISTENCY_STATUS_UNKNOWN ||
		answer.Answer.FactualConsistency.TotalClaimCount != uint32(len(answer.Answer.Claims)) {
		t.Fatalf("factual consistency = %+v", answer.Answer.FactualConsistency)
	}
	cite := answer.Answer.Claims[0].Citations[0]
	if cite.Owner != "ouroboros-dogfood" || cite.Repo != "sample-repo" {
		t.Fatalf("citation = %+v", cite)
	}

	// Delta reconcile admits new issue.
	reconciled, err := kernel.ReconcileConnector(ctx, ReconcileCommand{
		Identity: testIdentity(),
		Request: &contractsv1.ReconcileConnectorRequest{
			ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
			KnownCursor:    "cursor-v1",
			Reason:         "manual",
			IdempotencyKey: "reconcile-1",
		},
	})
	if err != nil || !reconciled.PageComplete || reconciled.NextCursor != "cursor-v2" {
		t.Fatalf("reconcile = %+v err=%v", reconciled, err)
	}
	if reconciled.AdmittedObjectCount == 0 {
		t.Fatal("expected admitted delta objects")
	}

	// Incomplete/malformed page does not advance cursor or delete.
	fake.MalformedDelta = true
	partial, err := kernel.ReconcileConnector(ctx, ReconcileCommand{
		Identity: testIdentity(),
		Request: &contractsv1.ReconcileConnectorRequest{
			ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
			KnownCursor:    "cursor-v2",
			Reason:         "retry",
			IdempotencyKey: "reconcile-malformed",
		},
	})
	if err != nil || partial.PageComplete || partial.NextCursor != "cursor-v2" || partial.DeletedObjectCount != 0 {
		t.Fatalf("malformed reconcile = %+v err=%v", partial, err)
	}

	// Cross-principal denial.
	if _, err := kernel.GetConnectorStatus(ctx, StatusCommand{
		Identity:     Identity{Tenant: testTenant, Principal: "principal-b", Session: testSession},
		ConnectionID: connectionID,
	}); err == nil {
		t.Fatal("expected cross-principal denial")
	}

	// Revoke then non-disclosing status.
	revoked, err := kernel.RevokeConnector(ctx, RevokeCommand{
		Identity: testIdentity(), ConnectionID: connectionID, IdempotencyKey: "revoke-1",
	})
	if err != nil || revoked.State != contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_REVOKED {
		t.Fatalf("revoke = %+v err=%v", revoked, err)
	}
	if _, err := kernel.QueryConnectorEvidence(ctx, QueryCommand{
		Identity: testIdentity(),
		Request: &contractsv1.QueryConnectorEvidenceRequest{
			ConnectionId:   &contractsv1.Identifier{Namespace: "connection", Value: connectionID},
			Query:          "billing",
			IdempotencyKey: "query-revoked",
		},
	}); err == nil {
		t.Fatal("expected revoked query denial")
	}

	// Purge.
	purged, err := kernel.PurgeConnector(ctx, PurgeCommand{
		Identity: testIdentity(), ConnectionID: connectionID, IdempotencyKey: "purge-1",
	})
	if err != nil || purged.State != contractsv1.ConnectorLifecycleState_CONNECTOR_LIFECYCLE_STATE_PURGED {
		t.Fatalf("purge = %+v err=%v", purged, err)
	}
	if purged.PurgedObjectCount == 0 {
		t.Fatal("expected purged objects")
	}
}

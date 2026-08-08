package authorityprocess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeRuntime struct {
	result     brain.Result
	status     brain.Status
	seen       brain.ExecuteRequest
	executions int
	executeErr error
	returnRaw  bool
}

func (runtime *fakeRuntime) OpenSession(_ context.Context, identity brain.Identity) (brain.Result, error) {
	runtime.status.Identity = identity
	return runtime.result, nil
}

func (runtime *fakeRuntime) Execute(ctx context.Context, request brain.ExecuteRequest) (brain.Result, error) {
	runtime.executions++
	runtime.seen = request
	if runtime.returnRaw {
		return runtime.result, runtime.executeErr
	}
	action := request.Command.Type
	decision, err := request.Authorize(ctx, request.Identity, action, brain.Identifier{Namespace: "evidence", Value: request.Artifact.ID.Value})
	if err != nil || !decision.Allowed {
		return brain.Result{}, brain.ErrDenied
	}
	if runtime.executeErr != nil {
		return brain.Result{}, runtime.executeErr
	}
	result := runtime.result
	result.Authorization = decision
	result.Artifact = request.Artifact
	return result, nil
}

func (runtime *fakeRuntime) ReadStatus(_ context.Context, identity brain.Identity) (brain.Status, error) {
	status := runtime.status
	status.Identity = identity
	return status, nil
}

func TestAdapterMapsPeerAndExecutesCurrentAuthorization(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := testBroker(t)
	for _, tuple := range []string{
		"brain:b#tenant@tenant:t",
		"brain:b#owner@user:p",
		"evidence:a#brain@brain:b",
	} {
		if err := policy.AddRelationship(tuple); err != nil {
			t.Fatal(err)
		}
	}
	plaintext := []byte("temporary plaintext")
	result := testResult()
	result.Bytes = plaintext
	runtime := &fakeRuntime{result: result, status: testStatus()}
	adapter := &authorityAdapter{runtime: runtime, broker: policy, keyEpoch: 1, now: func() time.Time { return now }}
	request := testExecuteRequest(t, now)
	registerRequestGrant(t, policy, identity, request, now)
	response, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, request)
	if err != nil || response.Receipt == nil || response.Authorization == nil || response.Authorization.AclEpoch != 0 {
		t.Fatalf("response = %#v, %v", response, err)
	}
	if runtime.seen.Identity != identity || runtime.seen.Artifact.Tenant != identity.Tenant || runtime.seen.Artifact.KeyEpoch != 1 {
		t.Fatalf("runtime request = %#v", runtime.seen)
	}
	for index, value := range plaintext {
		if value != 0 {
			t.Fatalf("plaintext byte %d was not cleared", index)
		}
	}
}

func TestAdapterRendersCanonicalRuntimeRejectionWithoutArtifactFacts(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := testBroker(t)
	result := testResult()
	result.Receipt.Status = "rejected"
	result.Receipt.ReasonCode = "not_found_or_denied"
	result.Authorization = brain.Authorization{
		Allowed: false, ReasonCode: "not_found_or_denied", RevocationEpoch: 3,
	}
	result.Artifact = brain.Artifact{ID: brain.Identifier{Namespace: "artifact", Value: "a"}, Generation: 7}
	runtime := &fakeRuntime{result: result, returnRaw: true}
	adapter := &authorityAdapter{
		runtime: runtime, broker: policy, keyEpoch: 1, now: func() time.Time { return now },
	}
	request := testExecuteRequest(t, now)
	response, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Receipt == nil || response.Receipt.Status != contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED ||
		response.Authorization == nil || response.Authorization.AclEpoch != 3 {
		t.Fatalf("rejection receipts = %#v", response)
	}
	if response.Error == nil || response.Error.Code != "request-denied" || response.Error.Render == nil ||
		response.Error.Render.Title != "Request denied" ||
		response.Error.Render.Detail != "The requested local authority operation was not permitted." {
		t.Fatalf("public rejection = %#v", response.Error)
	}
	if response.Artifact != nil || response.Generation != 0 || response.FrameDigest != nil || response.NextCursor != nil {
		t.Fatalf("rejection exposed artifact facts = %#v", response)
	}
}

func TestReceiptStatusRejectsUnknownInternalValues(t *testing.T) {
	tests := map[string]contractsv1.ReceiptStatus{
		"accepted":  contractsv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED,
		"rejected":  contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED,
		"deferred":  contractsv1.ReceiptStatus_RECEIPT_STATUS_DEFERRED,
		"partial":   contractsv1.ReceiptStatus_RECEIPT_STATUS_PARTIAL,
		"completed": contractsv1.ReceiptStatus_RECEIPT_STATUS_COMPLETED,
		"":          contractsv1.ReceiptStatus_RECEIPT_STATUS_UNSPECIFIED,
		"malformed": contractsv1.ReceiptStatus_RECEIPT_STATUS_UNSPECIFIED,
	}
	for value, want := range tests {
		t.Run(value, func(t *testing.T) {
			if got := receiptStatus(value); got != want {
				t.Fatalf("receiptStatus(%q) = %v, want %v", value, got, want)
			}
		})
	}
}

func TestAdapterDeniesBodyMismatchAndAbsentPolicyUniformly(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := testBroker(t)
	runtime := &fakeRuntime{result: testResult()}
	adapter := &authorityAdapter{runtime: runtime, broker: policy, keyEpoch: 1, now: func() time.Time { return now }}
	mismatch := testExecuteRequest(t, now)
	registerRequestGrant(t, policy, identity, mismatch, now)
	mismatch.Command.Actor.SessionId.Value = "other"
	_, mismatchErr := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, mismatch)
	_, absentErr := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, testExecuteRequest(t, now))
	if !errors.Is(mismatchErr, errRequestDenied) || !errors.Is(absentErr, errRequestDenied) || mismatchErr.Error() != absentErr.Error() {
		t.Fatalf("mismatch = %v, absent = %v", mismatchErr, absentErr)
	}
}

func TestAdapterRejectsAcceptedBodySubstitutionBeforeSecondEffect(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := authorizedTestBroker(t)
	runtime := &fakeRuntime{result: testResult(), executeErr: brain.ErrDenied}
	adapter := &authorityAdapter{runtime: runtime, broker: policy, keyEpoch: 1, now: func() time.Time { return now }}
	original := testExecuteRequest(t, now)
	registerRequestGrant(t, policy, identity, original, now)
	if _, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, original); !errors.Is(err, errRequestDenied) {
		t.Fatalf("accepted simulation error = %v", err)
	}
	substituted := testExecuteRequest(t, now)
	substituted.Command.PayloadDigest = original.Command.PayloadDigest
	substituted.GetArtifactRead().Artifact.ArtifactId.Value = "b"
	if _, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, substituted); !errors.Is(err, errRequestDenied) {
		t.Fatalf("substitution error = %v", err)
	}
	if runtime.executions != 1 {
		t.Fatalf("runtime effects = %d, want 1", runtime.executions)
	}
}

func TestAdapterRejectsCompletedReadSubstitutionBeforeHydration(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := authorizedTestBroker(t)
	runtime := &fakeRuntime{result: testResult()}
	adapter := &authorityAdapter{runtime: runtime, broker: policy, keyEpoch: 1, now: func() time.Time { return now }}
	original := testExecuteRequest(t, now)
	registerRequestGrant(t, policy, identity, original, now)
	if _, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, original); err != nil {
		t.Fatal(err)
	}
	substituted := testExecuteRequest(t, now)
	substituted.Command.PayloadDigest = original.Command.PayloadDigest
	substituted.GetArtifactRead().Artifact.ArtifactId.Value = "b"
	if _, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, substituted); !errors.Is(err, errRequestDenied) {
		t.Fatalf("substitution error = %v", err)
	}
	if runtime.executions != 1 {
		t.Fatalf("hydration calls = %d, want 1", runtime.executions)
	}
}

func TestOperationFingerprintRejectsChangedTypedFacts(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := testBroker(t)
	tests := map[string]struct {
		request func() *contractsv1.ExecuteAuthorityCommandRequest
		mutate  func(*contractsv1.ExecuteAuthorityCommandRequest)
	}{
		"admit declared length": {
			request: func() *contractsv1.ExecuteAuthorityCommandRequest { return admitRequest(t, now, identity) },
			mutate:  func(request *contractsv1.ExecuteAuthorityCommandRequest) { request.GetArtifactAdmit().DeclaredLength++ },
		},
		"read offset": {
			request: func() *contractsv1.ExecuteAuthorityCommandRequest { return testExecuteRequest(t, now) },
			mutate:  func(request *contractsv1.ExecuteAuthorityCommandRequest) { request.GetArtifactRead().Offset++ },
		},
		"delete purge": {
			request: func() *contractsv1.ExecuteAuthorityCommandRequest { return deleteRequest(t, now, identity) },
			mutate: func(request *contractsv1.ExecuteAuthorityCommandRequest) {
				request.GetArtifactDelete().PurgeAfterTombstone = !request.GetArtifactDelete().PurgeAfterTombstone
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := test.request()
			test.mutate(request)
			if _, err := executeRequest(identity, request, policy, 1, func() time.Time { return now }); !errors.Is(err, errRequestDenied) {
				t.Fatalf("changed operation error = %v", err)
			}
		})
	}
}

func TestExecuteRequestRejectsTypeAndFenceMismatch(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := testBroker(t)
	t.Run("oneof type", func(t *testing.T) {
		request := testExecuteRequest(t, now)
		request.Command.CommandType = "artifact.admit"
		if _, err := executeRequest(identity, request, policy, 1, func() time.Time { return now }); !errors.Is(err, errRequestDenied) {
			t.Fatalf("type mismatch error = %v", err)
		}
	})
	t.Run("grant fence", func(t *testing.T) {
		request := testExecuteRequest(t, now)
		request.Grant.CommandFence++
		if _, err := executeRequest(identity, request, policy, 1, func() time.Time { return now }); !errors.Is(err, errRequestDenied) {
			t.Fatalf("fence mismatch error = %v", err)
		}
	})
}

func TestOperationFingerprintPermitsExactRetryAfterNewSession(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, originalIdentity := testBroker(t)
	original := testExecuteRequest(t, now)
	originalDomain, err := executeRequest(originalIdentity, original, policy, 1, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	retryIdentity := originalIdentity
	retryIdentity.Session.Value = "new-session"
	retry := testExecuteRequest(t, now)
	retry.Command.Actor.SessionId.Value = retryIdentity.Session.Value
	retry.Grant.Initiator.SessionId.Value = retryIdentity.Session.Value
	retry.Command.PayloadDigest = original.Command.PayloadDigest
	retryDomain, err := executeRequest(retryIdentity, retry, policy, 1, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if retryDomain.Command.PayloadDigest != originalDomain.Command.PayloadDigest {
		t.Fatalf("retry digest = %#v, want %#v", retryDomain.Command.PayloadDigest, originalDomain.Command.PayloadDigest)
	}
}

func TestAdapterMetersActualReadAndAdmitUsage(t *testing.T) {
	now := time.Unix(1_000, 0)
	tests := map[string]struct {
		request func(*testing.T, shared.MappedIdentityFact) *contractsv1.ExecuteAuthorityCommandRequest
		limits  []*contractsv1.ResourceLimit
	}{
		"read bytes": {
			request: func(t *testing.T, _ shared.MappedIdentityFact) *contractsv1.ExecuteAuthorityCommandRequest {
				return testExecuteRequest(t, now)
			},
			limits: []*contractsv1.ResourceLimit{{Name: "bytes", Maximum: 1}},
		},
		"admit bytes": {
			request: func(t *testing.T, identity shared.MappedIdentityFact) *contractsv1.ExecuteAuthorityCommandRequest {
				return admitRequest(t, now, identity)
			},
			limits: []*contractsv1.ResourceLimit{{Name: "bytes", Maximum: 6}},
		},
		"admit frames": {
			request: func(t *testing.T, identity shared.MappedIdentityFact) *contractsv1.ExecuteAuthorityCommandRequest {
				return admitRequest(t, now, identity)
			},
			limits: []*contractsv1.ResourceLimit{{Name: "frames", Maximum: 1}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			policy, identity := authorizedTestBroker(t)
			runtime := &fakeRuntime{result: testResult()}
			request := test.request(t, identity)
			request.Grant.Limits = test.limits
			registerRequestGrant(t, policy, identity, request, now)
			adapter := &authorityAdapter{runtime: runtime, broker: policy, keyEpoch: 1, now: func() time.Time { return now }}
			if _, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, request); !errors.Is(err, errRequestDenied) {
				t.Fatalf("over-limit error = %v", err)
			}
		})
	}
}

func TestExecuteRequestRejectsUnknownDuplicateAndInapplicableLimits(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := testBroker(t)
	tests := map[string]struct {
		request *contractsv1.ExecuteAuthorityCommandRequest
		limits  []*contractsv1.ResourceLimit
	}{
		"unknown": {request: testExecuteRequest(t, now), limits: []*contractsv1.ResourceLimit{{Name: "tokens", Maximum: 1}}},
		"duplicate": {request: testExecuteRequest(t, now), limits: []*contractsv1.ResourceLimit{
			{Name: "bytes", Maximum: 8}, {Name: "bytes", Maximum: 9},
		}},
		"inapplicable": {request: deleteRequest(t, now, identity), limits: []*contractsv1.ResourceLimit{{Name: "bytes", Maximum: 1}}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			test.request.Grant.Limits = test.limits
			if _, err := executeRequest(identity, test.request, policy, 1, func() time.Time { return now }); !errors.Is(err, errRequestDenied) {
				t.Fatalf("limit error = %v", err)
			}
		})
	}
}

func TestExecuteRequestRejectsUnsupportedOrChangedGrantFacts(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := testBroker(t)
	mutations := map[string]func(*contractsv1.CapabilityGrant){
		"grant namespace": func(grant *contractsv1.CapabilityGrant) { grant.GrantId.Namespace = "other" },
		"task scope": func(grant *contractsv1.CapabilityGrant) {
			grant.TaskId = &contractsv1.Identifier{Namespace: "task", Value: "task-1"}
		},
		"workflow scope": func(grant *contractsv1.CapabilityGrant) {
			grant.WorkflowId = &contractsv1.Identifier{Namespace: "workflow", Value: "workflow-1"}
		},
		"repository scope": func(grant *contractsv1.CapabilityGrant) { grant.RepositoryGitOid = "abc123" },
		"path scope":       func(grant *contractsv1.CapabilityGrant) { grant.AllowedPaths = []string{"src"} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			request := testExecuteRequest(t, now)
			mutate(request.Grant)
			if _, err := executeRequest(identity, request, policy, 1, func() time.Time { return now }); !errors.Is(err, errRequestDenied) {
				t.Fatalf("unbound grant fact error = %v", err)
			}
		})
	}
}

func TestAdapterRejectsRequestGrantMutationAgainstIssuedRegistry(t *testing.T) {
	now := time.Unix(1_000, 0)
	policy, identity := authorizedTestBroker(t)
	runtime := &fakeRuntime{result: testResult()}
	adapter := &authorityAdapter{runtime: runtime, broker: policy, keyEpoch: 1, now: func() time.Time { return now }}
	request := testExecuteRequest(t, now)
	registerRequestGrant(t, policy, identity, request, now)
	request.Grant.PolicyDigest = protoDigest(testDigest("different-policy"))
	if _, err := adapter.Execute(context.Background(), gateway.PeerContext{Identity: identity}, request); !errors.Is(err, errRequestDenied) {
		t.Fatalf("issued grant mutation error = %v", err)
	}
}

func TestPeerMapperRejectsWrongOSIdentity(t *testing.T) {
	policy, identity := testBroker(t)
	mapped, err := (peerMapper{broker: policy}).MapPeer(gateway.PeerCredentials{UID: 501, PID: 42})
	if err != nil || mapped != identity {
		t.Fatalf("mapped = %#v, %v", mapped, err)
	}
	if _, err := (peerMapper{broker: policy}).MapPeer(gateway.PeerCredentials{UID: 502, PID: 42}); err == nil {
		t.Fatal("wrong uid mapped")
	}
}

func TestReadStatusUsesCurrentBrokerRevocationEpoch(t *testing.T) {
	policy, identity := testBroker(t)
	if err := policy.SetRevocationEpoch(identity.Tenant.Value, 3); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{status: testStatus()}
	adapter := &authorityAdapter{runtime: runtime, broker: policy}
	response, err := adapter.ReadStatus(context.Background(), gateway.PeerContext{Identity: identity}, &contractsv1.ReadStatusRequest{
		RequestedSession: identifierProto(identity.Session),
	})
	if err != nil || response.RevocationEpoch != 3 {
		t.Fatalf("status = %#v, %v", response, err)
	}
}

func testBroker(t *testing.T) (*broker.Broker, shared.MappedIdentityFact) {
	t.Helper()
	policy, err := broker.New(broker.Config{
		UID:       501,
		Principal: broker.Identifier{Namespace: "principal", Value: "p"},
		Tenant:    broker.Identifier{Namespace: "tenant", Value: "t"},
		Session:   broker.Identifier{Namespace: "session", Value: "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := policy.MapPeer(broker.PeerCredentials{UID: 501, PID: 42})
	if err != nil {
		t.Fatal(err)
	}
	return policy, identity
}

func testExecuteRequest(t *testing.T, now time.Time) *contractsv1.ExecuteAuthorityCommandRequest {
	t.Helper()
	actor := &contractsv1.AuthenticatedPrincipalRef{
		PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: "p"},
		TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: "t"},
		SessionId:   &contractsv1.Identifier{Namespace: "session", Value: "s"},
	}
	artifact := &contractsv1.ArtifactRef{
		ArtifactId:    &contractsv1.Identifier{Namespace: "artifact", Value: "a"},
		TenantId:      &contractsv1.Identifier{Namespace: "tenant", Value: "t"},
		ContentDigest: protoDigest(testDigest("content")),
	}
	request := &contractsv1.ExecuteAuthorityCommandRequest{
		Command: &contractsv1.CommandEnvelope{
			CommandId:   &contractsv1.Identifier{Namespace: "command", Value: "c"},
			CommandType: "artifact.read", Actor: actor, SubmittedAt: timestamppb.New(now),
			IdempotencyKey: "key", PayloadDigest: protoDigest(testDigest("placeholder")),
			Causal: &contractsv1.CausalContext{
				CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: "c"},
				CausationId:   &contractsv1.Identifier{Namespace: "cause", Value: "c"},
				TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: "c"}, Fence: 7,
			},
		},
		Grant: &contractsv1.CapabilityGrant{
			GrantId: &contractsv1.Identifier{Namespace: "grant", Value: "g"}, Initiator: actor,
			Actions: []string{"artifact.read"}, Resources: []*contractsv1.Identifier{{Namespace: "evidence", Value: "a"}},
			Nonce: "n", ExpiresAt: timestamppb.New(now.Add(time.Minute)),
			PolicyDigest: protoDigest(testDigest("policy")), CommandFence: 7,
		},
		ArtifactCommand: &contractsv1.ExecuteAuthorityCommandRequest_ArtifactRead{
			ArtifactRead: &contractsv1.ArtifactReadCommand{Artifact: artifact, Generation: 1, Length: 7},
		},
	}
	sealRequest(t, shared.MappedIdentityFact{
		Principal: shared.Identifier{Namespace: "principal", Value: "p"},
		Tenant:    shared.Identifier{Namespace: "tenant", Value: "t"},
		Session:   shared.Identifier{Namespace: "session", Value: "s"},
	}, request)
	return request
}

func admitRequest(t *testing.T, now time.Time, identity shared.MappedIdentityFact) *contractsv1.ExecuteAuthorityCommandRequest {
	t.Helper()
	request := testExecuteRequest(t, now)
	request.Command.CommandType = "artifact.admit"
	request.Grant.Actions = []string{"artifact.admit"}
	request.ArtifactCommand = &contractsv1.ExecuteAuthorityCommandRequest_ArtifactAdmit{
		ArtifactAdmit: &contractsv1.ArtifactAdmitCommand{
			Artifact: request.GetArtifactRead().Artifact, DeclaredLength: 7, FrameCount: 2,
		},
	}
	sealRequest(t, identity, request)
	return request
}

func deleteRequest(t *testing.T, now time.Time, identity shared.MappedIdentityFact) *contractsv1.ExecuteAuthorityCommandRequest {
	t.Helper()
	request := testExecuteRequest(t, now)
	request.Command.CommandType = "artifact.delete"
	request.Grant.Actions = []string{"artifact.delete"}
	request.ArtifactCommand = &contractsv1.ExecuteAuthorityCommandRequest_ArtifactDelete{
		ArtifactDelete: &contractsv1.ArtifactDeleteCommand{
			Artifact: request.GetArtifactRead().Artifact, ExpectedGeneration: 1, PurgeAfterTombstone: true,
		},
	}
	sealRequest(t, identity, request)
	return request
}

func sealRequest(t *testing.T, identity shared.MappedIdentityFact, request *contractsv1.ExecuteAuthorityCommandRequest) {
	t.Helper()
	fingerprint, err := OperationFingerprint(identity, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Command.PayloadDigest = protoDigest(fingerprint)
}

func authorizedTestBroker(t *testing.T) (*broker.Broker, shared.MappedIdentityFact) {
	t.Helper()
	policy, identity := testBroker(t)
	for _, tuple := range []string{
		"brain:b#tenant@tenant:t",
		"brain:b#owner@user:p",
		"evidence:a#brain@brain:b",
	} {
		if err := policy.AddRelationship(tuple); err != nil {
			t.Fatal(err)
		}
	}
	return policy, identity
}

func registerRequestGrant(
	t *testing.T,
	policy *broker.Broker,
	identity shared.MappedIdentityFact,
	request *contractsv1.ExecuteAuthorityCommandRequest,
	now time.Time,
) {
	t.Helper()
	_, _, _, _, availableUsage, err := domainOperation(request, 1)
	if err != nil {
		t.Fatal(err)
	}
	grant, _, err := domainGrant(request.Grant, identity, availableUsage)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.RegisterGrant(grant, now); err != nil {
		t.Fatal(err)
	}
}

func testResult() brain.Result {
	return brain.Result{
		Receipt: shared.Receipt{
			OperationID: shared.Identifier{Namespace: "command", Value: "c"},
			Status:      "completed", ReasonCode: "artifact_read",
		},
		RecordedAtMilli: 1_000_000, ConfigurationDigest: testDigest("config"),
	}
}

func testStatus() brain.Status {
	return brain.Status{
		Receipt: shared.Receipt{
			OperationID: shared.Identifier{Namespace: "status", Value: "s"},
			Status:      "completed", ReasonCode: "status_read",
		},
		ObservedAtMilli: 1_000_000, ConfigurationDigest: testDigest("config"),
	}
}

func testDigest(value string) shared.Digest {
	digest := sha256.Sum256([]byte(value))
	return shared.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(digest[:])}
}

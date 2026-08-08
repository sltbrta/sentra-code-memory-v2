package localauthority

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeFactoryAuthority records the five factory requests and returns static
// contract-valid denial responses.
type fakeFactoryAuthority struct {
	mu       sync.Mutex
	requests map[string]proto.Message
	err      error
}

func newFakeFactoryAuthority() *fakeFactoryAuthority {
	return &fakeFactoryAuthority{requests: make(map[string]proto.Message)}
}

func (fake *fakeFactoryAuthority) record(name string, message proto.Message) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.requests[name] = message
}

func (fake *fakeFactoryAuthority) request(name string) proto.Message {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.requests[name]
}

func (fake *fakeFactoryAuthority) AdmitChangeIntent(
	_ context.Context, _ PeerContext, request *contractsv1.AdmitChangeIntentRequest,
) (*contractsv1.AdmitChangeIntentResponse, error) {
	fake.record("admit", request)
	if fake.err != nil {
		return nil, fake.err
	}
	return &contractsv1.AdmitChangeIntentResponse{
		Receipt: deniedFactoryReceipt("admit"),
		Outcome: &contractsv1.AdmitChangeIntentResponse_Error{Error: deniedFactoryPublicError()},
	}, nil
}

func (fake *fakeFactoryAuthority) GetChangePlan(
	_ context.Context, _ PeerContext, request *contractsv1.GetChangePlanRequest,
) (*contractsv1.GetChangePlanResponse, error) {
	fake.record("plan", request)
	if fake.err != nil {
		return nil, fake.err
	}
	return &contractsv1.GetChangePlanResponse{
		Receipt: deniedFactoryReceipt("plan"),
		Outcome: &contractsv1.GetChangePlanResponse_Error{Error: deniedFactoryPublicError()},
	}, nil
}

func (fake *fakeFactoryAuthority) PreviewChangeSet(
	_ context.Context, _ PeerContext, request *contractsv1.PreviewChangeSetRequest,
) (*contractsv1.PreviewChangeSetResponse, error) {
	fake.record("candidate", request)
	if fake.err != nil {
		return nil, fake.err
	}
	return &contractsv1.PreviewChangeSetResponse{
		Receipt: deniedFactoryReceipt("candidate"),
		Outcome: &contractsv1.PreviewChangeSetResponse_Error{Error: deniedFactoryPublicError()},
	}, nil
}

func (fake *fakeFactoryAuthority) GetReviewFindings(
	_ context.Context, _ PeerContext, request *contractsv1.GetReviewFindingsRequest,
) (*contractsv1.GetReviewFindingsResponse, error) {
	fake.record("review", request)
	if fake.err != nil {
		return nil, fake.err
	}
	return &contractsv1.GetReviewFindingsResponse{
		Receipt: deniedFactoryReceipt("review"),
		Outcome: &contractsv1.GetReviewFindingsResponse_Error{Error: deniedFactoryPublicError()},
	}, nil
}

func (fake *fakeFactoryAuthority) CancelChangeRun(
	_ context.Context, _ PeerContext, request *contractsv1.CancelChangeRunRequest,
) (*contractsv1.CancelChangeRunResponse, error) {
	fake.record("cancel", request)
	if fake.err != nil {
		return nil, fake.err
	}
	return &contractsv1.CancelChangeRunResponse{
		Receipt: deniedFactoryReceipt("cancel"),
		Outcome: &contractsv1.CancelChangeRunResponse_Error{Error: deniedFactoryPublicError()},
	}, nil
}

func deniedFactoryPublicError() *contractsv1.PublicError {
	return &contractsv1.PublicError{Code: "not_found_or_denied"}
}

func deniedFactoryReceipt(operation string) *contractsv1.Receipt {
	return &contractsv1.Receipt{
		ReceiptId:   &contractsv1.Identifier{Namespace: "receipt", Value: operation},
		OperationId: &contractsv1.Identifier{Namespace: "operation", Value: operation},
		Status:      contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED,
		ReasonCode:  "not_found_or_denied",
		Causal: &contractsv1.CausalContext{
			CorrelationId: &contractsv1.Identifier{Namespace: "correlation", Value: "c"},
			CausationId:   &contractsv1.Identifier{Namespace: "cause", Value: "c"},
			TraceId:       &contractsv1.Identifier{Namespace: "trace", Value: "c"},
		},
		RecordedAt:          timestamppb.New(time.Unix(1_000_000, 0).UTC()),
		ConfigurationDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("a", 64)},
	}
}

func factoryCallerFor(identity mappedIdentityFactAlias) *contractsv1.UntrustedFactoryCaller {
	return &contractsv1.UntrustedFactoryCaller{
		RequestedPrincipal: &contractsv1.AuthenticatedPrincipalRef{
			PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: identity.principal},
			TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: identity.tenant},
			SessionId:   &contractsv1.Identifier{Namespace: "session", Value: identity.session},
		},
		RequestedSession: &contractsv1.Identifier{Namespace: "session", Value: identity.session},
	}
}

// mappedIdentityFactAlias mirrors the test peer identity values.
type mappedIdentityFactAlias struct {
	principal string
	tenant    string
	session   string
}

func testFactoryIdentity() mappedIdentityFactAlias {
	identity := mappedIdentity(testCredentials())
	return mappedIdentityFactAlias{
		principal: identity.Principal.Value, tenant: identity.Tenant.Value, session: identity.Session.Value,
	}
}

func validAdmitRequest(identity mappedIdentityFactAlias) *contractsv1.AdmitChangeIntentRequest {
	return &contractsv1.AdmitChangeIntentRequest{
		Caller: factoryCallerFor(identity),
		Intent: &contractsv1.ChangeIntent{
			IntentId: &contractsv1.Identifier{Namespace: "intent", Value: "intent-1"},
			RequestedBy: &contractsv1.AuthenticatedPrincipalRef{
				PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: identity.principal},
				TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: identity.tenant},
				SessionId:   &contractsv1.Identifier{Namespace: "session", Value: identity.session},
			},
			RepositoryGitOid: strings.Repeat("a", 40),
			ScopeDigest:      &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("b", 64)},
			SupportingEvidence: []*contractsv1.EvidenceRef{{
				EvidenceId:       &contractsv1.Identifier{Namespace: "artifact", Value: "artifact-1"},
				SourceRevisionId: &contractsv1.Identifier{Namespace: "revision", Value: "revision-1"},
			}},
			Approval: &contractsv1.Approval{
				ApprovalId: &contractsv1.Identifier{Namespace: "approval", Value: "approval-1"},
				Approver: &contractsv1.AuthenticatedPrincipalRef{
					PrincipalId: &contractsv1.Identifier{Namespace: "principal", Value: identity.principal},
					TenantId:    &contractsv1.Identifier{Namespace: "tenant", Value: identity.tenant},
					SessionId:   &contractsv1.Identifier{Namespace: "session", Value: identity.session},
				},
				ScopeDigest: &contractsv1.Digest{Algorithm: "sha256", Hex: strings.Repeat("b", 64)},
				ExpiresAt:   timestamppb.Now(),
				Receipt:     deniedFactoryReceipt("approval"),
			},
		},
		IdempotencyKey: "admit-1",
	}
}

func validFactoryRunRequest(identity mappedIdentityFactAlias) *contractsv1.GetChangePlanRequest {
	return &contractsv1.GetChangePlanRequest{
		Caller: factoryCallerFor(identity),
		RunId:  &contractsv1.Identifier{Namespace: "factory-run", Value: strings.Repeat("a", 64)},
	}
}

func startFactoryTestServer(t *testing.T, authority FactoryAuthority) (*Server, string, func()) {
	t.Helper()
	socketPath := secureSocketDirectory(t) + "/gateway.sock"
	config := testConfig(socketPath, &fakeAuthority{})
	config.FactoryAuthority = authority
	config.authenticatePeer = func(net.Conn) (PeerContext, error) {
		credentials := testCredentials()
		return PeerContext{Credentials: credentials, Identity: mappedIdentity(credentials)}, nil
	}
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx) }()
	waitForSocket(t, socketPath)
	return server, socketPath, func() {
		cancel()
		if err := <-result; err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	}
}

func TestFactoryGatewayPreservesFiveTypedMethods(t *testing.T) {
	t.Parallel()
	authority := newFakeFactoryAuthority()
	_, socketPath, stop := startFactoryTestServer(t, authority)
	defer stop()
	identity := testFactoryIdentity()

	admit := validAdmitRequest(identity)
	plan := validFactoryRunRequest(identity)
	candidate := &contractsv1.PreviewChangeSetRequest{
		Caller: factoryCallerFor(identity),
		RunId:  &contractsv1.Identifier{Namespace: "factory-run", Value: strings.Repeat("a", 64)},
	}
	review := &contractsv1.GetReviewFindingsRequest{
		Caller:   factoryCallerFor(identity),
		RunId:    &contractsv1.Identifier{Namespace: "factory-run", Value: strings.Repeat("a", 64)},
		PageSize: 10,
	}
	cancel := &contractsv1.CancelChangeRunRequest{
		Caller:         factoryCallerFor(identity),
		RunId:          &contractsv1.Identifier{Namespace: "factory-run", Value: strings.Repeat("a", 64)},
		IdempotencyKey: "cancel-1",
	}
	tests := []struct {
		name     string
		path     string
		request  proto.Message
		response proto.Message
	}{
		{"admit", admitChangeIntentProcedure, admit, &contractsv1.AdmitChangeIntentResponse{
			Receipt: deniedFactoryReceipt("admit"),
			Outcome: &contractsv1.AdmitChangeIntentResponse_Error{Error: deniedFactoryPublicError()},
		}},
		{"plan", getChangePlanProcedure, plan, &contractsv1.GetChangePlanResponse{
			Receipt: deniedFactoryReceipt("plan"),
			Outcome: &contractsv1.GetChangePlanResponse_Error{Error: deniedFactoryPublicError()},
		}},
		{"candidate", previewChangeSetProcedure, candidate, &contractsv1.PreviewChangeSetResponse{
			Receipt: deniedFactoryReceipt("candidate"),
			Outcome: &contractsv1.PreviewChangeSetResponse_Error{Error: deniedFactoryPublicError()},
		}},
		{"review", getReviewFindingsProcedure, review, &contractsv1.GetReviewFindingsResponse{
			Receipt: deniedFactoryReceipt("review"),
			Outcome: &contractsv1.GetReviewFindingsResponse_Error{Error: deniedFactoryPublicError()},
		}},
		{"cancel", cancelChangeRunProcedure, cancel, &contractsv1.CancelChangeRunResponse{
			Receipt: deniedFactoryReceipt("cancel"),
			Outcome: &contractsv1.CancelChangeRunResponse_Error{Error: deniedFactoryPublicError()},
		}},
	}
	client := unixClient(socketPath)
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			status, contentType, body := post(t, client, testCase.path, "application/proto", marshal(t, testCase.request))
			if status != http.StatusOK || contentType != "application/proto" {
				t.Fatalf("status=%d content_type=%q body=%q", status, contentType, body)
			}
			decoded := testCase.response.ProtoReflect().Type().New().Interface()
			if err := proto.Unmarshal(body, decoded); err != nil || !proto.Equal(decoded, testCase.response) {
				t.Fatalf("response was not preserved: %v", err)
			}
			if called := authority.request(testCase.name); !proto.Equal(called, testCase.request) {
				t.Fatal("request was not preserved")
			}
		})
	}
}

func TestFactoryGatewayRejectsInvalidInputBeforeAuthority(t *testing.T) {
	t.Parallel()
	identity := testFactoryIdentity()
	principalMismatch := validFactoryRunRequest(identity)
	principalMismatch.Caller.RequestedPrincipal.PrincipalId.Value = "other"
	sessionMismatch := validFactoryRunRequest(identity)
	sessionMismatch.Caller.RequestedSession.Value = "other"
	invalidSchema := validFactoryRunRequest(identity)
	invalidSchema.RunId = nil

	tests := []struct {
		name       string
		payload    []byte
		wantStatus int
	}{
		{"malformed protobuf", []byte{0x0a, 0xff}, http.StatusBadRequest},
		{"validation failure", marshal(t, invalidSchema), http.StatusBadRequest},
		{"principal mismatch", marshal(t, principalMismatch), http.StatusForbidden},
		{"session mismatch", marshal(t, sessionMismatch), http.StatusForbidden},
	}
	authority := newFakeFactoryAuthority()
	_, socketPath, stop := startFactoryTestServer(t, authority)
	defer stop()
	client := unixClient(socketPath)
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			status, _, _ := post(t, client, getChangePlanProcedure, "application/proto", testCase.payload)
			if status != testCase.wantStatus {
				t.Fatalf("status=%d, want %d", status, testCase.wantStatus)
			}
		})
	}
	if called := authority.request("plan"); called != nil {
		t.Fatal("authority was invoked for a rejected request")
	}
}

func TestFactoryGatewayMapsAuthorityErrorToStaticDenial(t *testing.T) {
	t.Parallel()
	authority := newFakeFactoryAuthority()
	authority.err = errors.New("upstream detail must not leak")
	_, socketPath, stop := startFactoryTestServer(t, authority)
	defer stop()
	identity := testFactoryIdentity()
	status, _, body := post(t, unixClient(socketPath), getChangePlanProcedure, "application/proto",
		marshal(t, validFactoryRunRequest(identity)))
	if status != http.StatusForbidden || strings.Contains(string(body), "upstream detail") {
		t.Fatalf("status=%d body=%q", status, body)
	}
}

func TestFactoryRoutesRequireConfiguredAuthority(t *testing.T) {
	t.Parallel()
	_, socketPath, stop := startFactoryTestServer(t, nil)
	defer stop()
	identity := testFactoryIdentity()
	status, _, body := post(t, unixClient(socketPath), getChangePlanProcedure, "application/proto",
		marshal(t, validFactoryRunRequest(identity)))
	if status != http.StatusNotFound || !strings.Contains(string(body), "procedure-not-found") {
		t.Fatalf("status=%d body=%q", status, body)
	}
}

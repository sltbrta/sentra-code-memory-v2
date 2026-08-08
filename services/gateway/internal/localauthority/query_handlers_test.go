package localauthority

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
)

func TestQueryGatewayPreservesFourTypedMethods(t *testing.T) {
	t.Parallel()
	authority := newFakeQueryAuthority()
	_, socketPath, stop := startQueryTestServer(t, authority)
	defer stop()
	identity := mappedIdentity(testCredentials())

	tests := []struct {
		name     string
		path     string
		request  proto.Message
		response proto.Message
	}{
		{"ask", askProcedure, validAskRequest(identity), deniedAskResponse()},
		{"list sources", listSourcesProcedure, validListSourcesRequest(identity), deniedListSourcesResponse()},
		{"get history", getHistoryProcedure, validGetHistoryRequest(identity), deniedGetHistoryResponse()},
		{"get status", getStatusProcedure, validGetStatusRequest(identity), deniedGetStatusResponse()},
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

func TestQueryGatewayRejectsInvalidInputBeforeAuthority(t *testing.T) {
	t.Parallel()
	identity := mappedIdentity(testCredentials())

	principalMismatch := validAskRequest(identity)
	principalMismatch.Caller.RequestedPrincipal.PrincipalId.Value = "other"
	tenantMismatch := validAskRequest(identity)
	tenantMismatch.Caller.RequestedPrincipal.TenantId.Value = "other"
	principalSessionMismatch := validAskRequest(identity)
	principalSessionMismatch.Caller.RequestedPrincipal.SessionId.Value = "other"
	requestedSessionMismatch := validAskRequest(identity)
	requestedSessionMismatch.Caller.RequestedSession.Value = "other"
	invalidSchema := validAskRequest(identity)
	invalidSchema.Freshness = contractsv1.FreshnessRequirement_FRESHNESS_REQUIREMENT_UNSPECIFIED

	tests := []struct {
		name       string
		payload    []byte
		wantStatus int
	}{
		{"malformed protobuf", []byte{0x0a, 0xff}, http.StatusBadRequest},
		{"validation failure", marshal(t, invalidSchema), http.StatusBadRequest},
		{"schema-incompatible protobuf payload", marshal(t, validOpenRequest(identity)), http.StatusBadRequest},
		{"principal mismatch", marshal(t, principalMismatch), http.StatusForbidden},
		{"tenant mismatch", marshal(t, tenantMismatch), http.StatusForbidden},
		{"principal session mismatch", marshal(t, principalSessionMismatch), http.StatusForbidden},
		{"requested session mismatch", marshal(t, requestedSessionMismatch), http.StatusForbidden},
		{"oversized body", bytes.Repeat([]byte{1}, maxRequestBytes+1), http.StatusRequestEntityTooLarge},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			authority := newFakeQueryAuthority()
			_, socketPath, stop := startQueryTestServer(t, authority)
			defer stop()
			status, _, _ := post(t, unixClient(socketPath), askProcedure, "application/proto", testCase.payload)
			if status != testCase.wantStatus {
				t.Fatalf("status=%d, want %d", status, testCase.wantStatus)
			}
			if authority.callCount() != 0 {
				t.Fatal("invalid request invoked authority")
			}
		})
	}
}

func TestQueryGatewayRejectsInvalidAuthorityResponse(t *testing.T) {
	t.Parallel()
	authority := newFakeQueryAuthority()
	authority.invalidResponse = "ask"
	_, socketPath, stop := startQueryTestServer(t, authority)
	defer stop()

	request := validAskRequest(mappedIdentity(testCredentials()))
	status, _, body := post(t, unixClient(socketPath), askProcedure, "application/proto", marshal(t, request))
	if status != http.StatusInternalServerError || !bytes.Equal(body, []byte(`{"code":"response-invalid"}`)) {
		t.Fatalf("status=%d body=%q", status, body)
	}
	if authority.callCount() != 1 {
		t.Fatalf("authority calls=%d, want 1", authority.callCount())
	}
}

func TestQueryGatewayMapsAuthorityErrorToStaticDenial(t *testing.T) {
	t.Parallel()
	authority := newFakeQueryAuthority()
	authority.askError = errors.New("sensitive source path")
	_, socketPath, stop := startQueryTestServer(t, authority)
	defer stop()

	request := validAskRequest(mappedIdentity(testCredentials()))
	status, _, body := post(t, unixClient(socketPath), askProcedure, "application/proto", marshal(t, request))
	if status != http.StatusForbidden || !bytes.Equal(body, []byte(`{"code":"request-denied"}`)) ||
		bytes.Contains(body, []byte("sensitive")) {
		t.Fatalf("status=%d body=%q", status, body)
	}
	if authority.callCount() != 1 {
		t.Fatalf("authority calls=%d, want 1", authority.callCount())
	}
}

func TestQueryRoutesRequireConfiguredAuthorityAndRejectUnknownRoute(t *testing.T) {
	t.Parallel()
	identity := mappedIdentity(testCredentials())
	for name, configured := range map[string]bool{"missing query authority": false, "unknown route": true} {
		t.Run(name, func(t *testing.T) {
			authority := newFakeQueryAuthority()
			var query QueryAuthority
			path := askProcedure
			if configured {
				query = authority
				path = "/ouroboros.contracts.v1.QueryService/Unknown"
			}
			_, socketPath, stop := startQueryTestServerWithAuthority(t, query)
			defer stop()
			status, _, body := post(t, unixClient(socketPath), path, "application/proto", marshal(t, validAskRequest(identity)))
			if status != http.StatusNotFound || !bytes.Equal(body, []byte(`{"code":"procedure-not-found"}`)) {
				t.Fatalf("status=%d body=%q", status, body)
			}
			if authority.callCount() != 0 {
				t.Fatal("unknown route invoked authority")
			}
		})
	}
	t.Run("typed nil query authority", func(t *testing.T) {
		var authority *fakeQueryAuthority
		_, socketPath, stop := startQueryTestServerWithAuthority(t, authority)
		defer stop()
		status, _, body := post(t, unixClient(socketPath), askProcedure, "application/proto", marshal(t, validAskRequest(identity)))
		if status != http.StatusNotFound || !bytes.Equal(body, []byte(`{"code":"procedure-not-found"}`)) {
			t.Fatalf("status=%d body=%q", status, body)
		}
	})
}

type fakeQueryAuthority struct {
	mu              sync.Mutex
	requests        map[string]proto.Message
	calls           int
	askError        error
	invalidResponse string
}

func newFakeQueryAuthority() *fakeQueryAuthority {
	return &fakeQueryAuthority{requests: make(map[string]proto.Message)}
}

func (authority *fakeQueryAuthority) Ask(_ context.Context, _ PeerContext, request *contractsv1.AskRequest) (*contractsv1.AskResponse, error) {
	authority.record("ask", request)
	if authority.askError != nil {
		return nil, authority.askError
	}
	if authority.invalid("ask") {
		return &contractsv1.AskResponse{}, nil
	}
	return deniedAskResponse(), nil
}

func (authority *fakeQueryAuthority) ListSources(_ context.Context, _ PeerContext, request *contractsv1.ListSourcesRequest) (*contractsv1.ListSourcesResponse, error) {
	authority.record("list sources", request)
	if authority.invalid("list sources") {
		return &contractsv1.ListSourcesResponse{}, nil
	}
	return deniedListSourcesResponse(), nil
}

func (authority *fakeQueryAuthority) GetHistory(_ context.Context, _ PeerContext, request *contractsv1.GetHistoryRequest) (*contractsv1.GetHistoryResponse, error) {
	authority.record("get history", request)
	if authority.invalid("get history") {
		return &contractsv1.GetHistoryResponse{}, nil
	}
	return deniedGetHistoryResponse(), nil
}

func (authority *fakeQueryAuthority) GetStatus(_ context.Context, _ PeerContext, request *contractsv1.GetStatusRequest) (*contractsv1.GetStatusResponse, error) {
	authority.record("get status", request)
	if authority.invalid("get status") {
		return &contractsv1.GetStatusResponse{}, nil
	}
	return deniedGetStatusResponse(), nil
}

func (authority *fakeQueryAuthority) record(name string, request proto.Message) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.calls++
	authority.requests[name] = proto.Clone(request)
}

func (authority *fakeQueryAuthority) invalid(name string) bool {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.invalidResponse == name
}

func (authority *fakeQueryAuthority) request(name string) proto.Message {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.requests[name]
}

func (authority *fakeQueryAuthority) callCount() int {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.calls
}

func validQueryCaller(identity shared.MappedIdentityFact) *contractsv1.UntrustedQueryCaller {
	return &contractsv1.UntrustedQueryCaller{
		RequestedPrincipal: principalRef(identity),
		RequestedSession:   identifier(identity.Session.Namespace, identity.Session.Value),
	}
}

func validAskRequest(identity shared.MappedIdentityFact) *contractsv1.AskRequest {
	return &contractsv1.AskRequest{
		Caller:   validQueryCaller(identity),
		SourceId: identifier("source", "one"), GenerationId: identifier("generation", "one"),
		Query:          "What does the anchor return?",
		Freshness:      contractsv1.FreshnessRequirement_FRESHNESS_REQUIREMENT_BEST_EFFORT,
		IdempotencyKey: "ask-1",
	}
}

func validListSourcesRequest(identity shared.MappedIdentityFact) *contractsv1.ListSourcesRequest {
	return &contractsv1.ListSourcesRequest{Caller: validQueryCaller(identity), PageSize: 25}
}

func validGetHistoryRequest(identity shared.MappedIdentityFact) *contractsv1.GetHistoryRequest {
	return &contractsv1.GetHistoryRequest{Caller: validQueryCaller(identity), PageSize: 25}
}

func validGetStatusRequest(identity shared.MappedIdentityFact) *contractsv1.GetStatusRequest {
	return &contractsv1.GetStatusRequest{Caller: validQueryCaller(identity), SourceId: identifier("source", "one")}
}

func deniedQueryReceipt(name string) *contractsv1.Receipt {
	receipt := fixtureReceipt(name)
	receipt.Status = contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED
	receipt.ReasonCode = "not_found_or_denied"
	return receipt
}

func deniedAskResponse() *contractsv1.AskResponse {
	return &contractsv1.AskResponse{Receipt: deniedQueryReceipt("ask"), Outcome: &contractsv1.AskResponse_Error{Error: deniedPublicError()}}
}

func deniedListSourcesResponse() *contractsv1.ListSourcesResponse {
	return &contractsv1.ListSourcesResponse{Receipt: deniedQueryReceipt("sources"), Outcome: &contractsv1.ListSourcesResponse_Error{Error: deniedPublicError()}}
}

func deniedGetHistoryResponse() *contractsv1.GetHistoryResponse {
	return &contractsv1.GetHistoryResponse{Receipt: deniedQueryReceipt("history"), Outcome: &contractsv1.GetHistoryResponse_Error{Error: deniedPublicError()}}
}

func deniedGetStatusResponse() *contractsv1.GetStatusResponse {
	return &contractsv1.GetStatusResponse{Receipt: deniedQueryReceipt("status"), Outcome: &contractsv1.GetStatusResponse_Error{Error: deniedPublicError()}}
}

func startQueryTestServer(t *testing.T, authority QueryAuthority) (*Server, string, func()) {
	t.Helper()
	return startQueryTestServerWithAuthority(t, authority)
}

func startQueryTestServerWithAuthority(t *testing.T, authority QueryAuthority) (*Server, string, func()) {
	t.Helper()
	socketPath := secureSocketDirectory(t) + "/gateway.sock"
	config := testConfig(socketPath, &fakeAuthority{})
	config.QueryAuthority = authority
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

var _ = strings.Repeat

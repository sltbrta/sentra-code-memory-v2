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

	"buf.build/go/protovalidate"
	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
)

func TestGeneratedContractsUseCanonicalProtovalidateDescriptors(t *testing.T) {
	t.Parallel()
	if err := protovalidate.Validate(&contractsv1.AddSourceRequest{}); err == nil {
		t.Fatal("malformed generated ingestion request passed Protovalidate")
	}
}

func TestIngestionGatewayPreservesFiveTypedMethods(t *testing.T) {
	t.Parallel()
	authority := newFakeIngestionAuthority()
	_, socketPath, stop := startIngestionTestServer(t, authority)
	defer stop()
	identity := mappedIdentity(testCredentials())

	tests := []struct {
		name     string
		path     string
		request  proto.Message
		response proto.Message
	}{
		{"add source", addSourceProcedure, validAddSourceRequest(identity), deniedAddSourceResponse()},
		{"get source status", getSourceStatusProcedure, validGetSourceStatusRequest(identity), deniedGetSourceStatusResponse()},
		{"search code", searchCodeProcedure, validSearchCodeRequest(identity), deniedSearchCodeResponse()},
		{"reconcile source", reconcileSourceProcedure, validReconcileSourceRequest(identity), deniedReconcileSourceResponse()},
		{"revoke source", revokeSourceProcedure, validRevokeSourceRequest(identity), deniedRevokeSourceResponse()},
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

func TestIngestionGatewayRejectsInvalidInputBeforeAuthority(t *testing.T) {
	t.Parallel()
	identity := mappedIdentity(testCredentials())

	principalMismatch := validAddSourceRequest(identity)
	principalMismatch.Caller.RequestedPrincipal.PrincipalId.Value = "other"
	tenantMismatch := validAddSourceRequest(identity)
	tenantMismatch.Caller.RequestedPrincipal.TenantId.Value = "other"
	principalSessionMismatch := validAddSourceRequest(identity)
	principalSessionMismatch.Caller.RequestedPrincipal.SessionId.Value = "other"
	requestedSessionMismatch := validAddSourceRequest(identity)
	requestedSessionMismatch.Caller.RequestedSession.Value = "other"
	invalidSchema := validAddSourceRequest(identity)
	invalidSchema.ExpectedCommitOid = "not-an-oid"

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
			authority := newFakeIngestionAuthority()
			_, socketPath, stop := startIngestionTestServer(t, authority)
			defer stop()
			status, _, _ := post(t, unixClient(socketPath), addSourceProcedure, "application/proto", testCase.payload)
			if status != testCase.wantStatus {
				t.Fatalf("status=%d, want %d", status, testCase.wantStatus)
			}
			if authority.callCount() != 0 {
				t.Fatal("invalid request invoked authority")
			}
		})
	}
}

func TestIngestionGatewayRejectsInvalidAuthorityResponse(t *testing.T) {
	t.Parallel()
	authority := newFakeIngestionAuthority()
	authority.invalidResponse = "search code"
	_, socketPath, stop := startIngestionTestServer(t, authority)
	defer stop()

	request := validSearchCodeRequest(mappedIdentity(testCredentials()))
	status, _, body := post(t, unixClient(socketPath), searchCodeProcedure, "application/proto", marshal(t, request))
	if status != http.StatusInternalServerError || !bytes.Equal(body, []byte(`{"code":"response-invalid"}`)) {
		t.Fatalf("status=%d body=%q", status, body)
	}
	if authority.callCount() != 1 {
		t.Fatalf("authority calls=%d, want 1", authority.callCount())
	}
}

func TestIngestionGatewayMapsAuthorityErrorToStaticDenial(t *testing.T) {
	t.Parallel()
	authority := newFakeIngestionAuthority()
	authority.addError = errors.New("sensitive source path")
	_, socketPath, stop := startIngestionTestServer(t, authority)
	defer stop()

	request := validAddSourceRequest(mappedIdentity(testCredentials()))
	client := unixClient(socketPath)
	for invocation := 0; invocation < 2; invocation++ {
		status, _, body := post(t, client, addSourceProcedure, "application/proto", marshal(t, request))
		if status != http.StatusForbidden || !bytes.Equal(body, []byte(`{"code":"request-denied"}`)) ||
			bytes.Contains(body, []byte("sensitive")) {
			t.Fatalf("status=%d body=%q", status, body)
		}
	}
	if authority.callCount() != 2 {
		t.Fatalf("authority calls=%d, want 2", authority.callCount())
	}
}

func TestIngestionRoutesRequireConfiguredAuthorityAndRejectUnknownRoute(t *testing.T) {
	t.Parallel()
	identity := mappedIdentity(testCredentials())
	for name, configured := range map[string]bool{"missing ingestion authority": false, "unknown route": true} {
		t.Run(name, func(t *testing.T) {
			authority := newFakeIngestionAuthority()
			var ingestion IngestionAuthority
			path := addSourceProcedure
			if configured {
				ingestion = authority
				path = "/ouroboros.contracts.v1.IngestionService/Unknown"
			}
			_, socketPath, stop := startIngestionTestServerWithAuthority(t, ingestion)
			defer stop()
			status, _, body := post(t, unixClient(socketPath), path, "application/proto", marshal(t, validAddSourceRequest(identity)))
			if status != http.StatusNotFound || !bytes.Equal(body, []byte(`{"code":"procedure-not-found"}`)) {
				t.Fatalf("status=%d body=%q", status, body)
			}
			if authority.callCount() != 0 {
				t.Fatal("unknown route invoked authority")
			}
		})
	}
	t.Run("typed nil ingestion authority", func(t *testing.T) {
		var authority *fakeIngestionAuthority
		_, socketPath, stop := startIngestionTestServerWithAuthority(t, authority)
		defer stop()
		status, _, body := post(t, unixClient(socketPath), addSourceProcedure, "application/proto", marshal(t, validAddSourceRequest(identity)))
		if status != http.StatusNotFound || !bytes.Equal(body, []byte(`{"code":"procedure-not-found"}`)) {
			t.Fatalf("status=%d body=%q", status, body)
		}
	})
}

type fakeIngestionAuthority struct {
	mu              sync.Mutex
	requests        map[string]proto.Message
	calls           int
	addError        error
	invalidResponse string
}

func newFakeIngestionAuthority() *fakeIngestionAuthority {
	return &fakeIngestionAuthority{requests: make(map[string]proto.Message)}
}

func (authority *fakeIngestionAuthority) AddSource(_ context.Context, _ PeerContext, request *contractsv1.AddSourceRequest) (*contractsv1.AddSourceResponse, error) {
	authority.record("add source", request)
	if authority.addError != nil {
		return nil, authority.addError
	}
	if authority.invalid("add source") {
		return &contractsv1.AddSourceResponse{}, nil
	}
	return deniedAddSourceResponse(), nil
}

func (authority *fakeIngestionAuthority) GetSourceStatus(_ context.Context, _ PeerContext, request *contractsv1.GetSourceStatusRequest) (*contractsv1.GetSourceStatusResponse, error) {
	authority.record("get source status", request)
	if authority.invalid("get source status") {
		return &contractsv1.GetSourceStatusResponse{}, nil
	}
	return deniedGetSourceStatusResponse(), nil
}

func (authority *fakeIngestionAuthority) SearchCode(_ context.Context, _ PeerContext, request *contractsv1.SearchCodeRequest) (*contractsv1.SearchCodeResponse, error) {
	authority.record("search code", request)
	if authority.invalid("search code") {
		return &contractsv1.SearchCodeResponse{}, nil
	}
	return deniedSearchCodeResponse(), nil
}

func (authority *fakeIngestionAuthority) ReconcileSource(_ context.Context, _ PeerContext, request *contractsv1.ReconcileSourceRequest) (*contractsv1.ReconcileSourceResponse, error) {
	authority.record("reconcile source", request)
	if authority.invalid("reconcile source") {
		return &contractsv1.ReconcileSourceResponse{}, nil
	}
	return deniedReconcileSourceResponse(), nil
}

func (authority *fakeIngestionAuthority) RevokeSource(_ context.Context, _ PeerContext, request *contractsv1.RevokeSourceRequest) (*contractsv1.RevokeSourceResponse, error) {
	authority.record("revoke source", request)
	if authority.invalid("revoke source") {
		return &contractsv1.RevokeSourceResponse{}, nil
	}
	return deniedRevokeSourceResponse(), nil
}

func (authority *fakeIngestionAuthority) record(name string, request proto.Message) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.calls++
	authority.requests[name] = proto.Clone(request)
}

func (authority *fakeIngestionAuthority) invalid(name string) bool {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.invalidResponse == name
}

func (authority *fakeIngestionAuthority) request(name string) proto.Message {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.requests[name]
}

func (authority *fakeIngestionAuthority) callCount() int {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.calls
}

func validIngestionCaller(identity shared.MappedIdentityFact) *contractsv1.UntrustedIngestionCaller {
	return &contractsv1.UntrustedIngestionCaller{
		RequestedPrincipal: principalRef(identity),
		RequestedSession:   identifier(identity.Session.Namespace, identity.Session.Value),
	}
}

func validAddSourceRequest(identity shared.MappedIdentityFact) *contractsv1.AddSourceRequest {
	return &contractsv1.AddSourceRequest{
		Caller:                      validIngestionCaller(identity),
		ExpectedCommitOid:           strings.Repeat("a", 40),
		Policy:                      &contractsv1.SourcePolicy{SymlinkPolicy: contractsv1.SymlinkPolicy_SYMLINK_POLICY_RECORD_WITHOUT_FOLLOW},
		ExpectedConfigurationDigest: digest(),
		IdempotencyKey:              "add-1",
	}
}

func validGetSourceStatusRequest(identity shared.MappedIdentityFact) *contractsv1.GetSourceStatusRequest {
	return &contractsv1.GetSourceStatusRequest{Caller: validIngestionCaller(identity), SourceId: identifier("source", "one")}
}

func validSearchCodeRequest(identity shared.MappedIdentityFact) *contractsv1.SearchCodeRequest {
	return &contractsv1.SearchCodeRequest{
		Caller: validIngestionCaller(identity), SourceId: identifier("source", "one"),
		GenerationId: identifier("generation", "one"), Query: "Symbol",
		Kind: contractsv1.SearchKind_SEARCH_KIND_EXACT, PageSize: 25,
	}
}

func validReconcileSourceRequest(identity shared.MappedIdentityFact) *contractsv1.ReconcileSourceRequest {
	return &contractsv1.ReconcileSourceRequest{
		Caller: validIngestionCaller(identity), SourceId: identifier("source", "one"),
		ExpectedGenerationId: identifier("generation", "one"), ExpectedCommitOid: strings.Repeat("a", 40),
		TargetCommitOid: strings.Repeat("b", 40), IdempotencyKey: "reconcile-1",
	}
}

func validRevokeSourceRequest(identity shared.MappedIdentityFact) *contractsv1.RevokeSourceRequest {
	return &contractsv1.RevokeSourceRequest{
		Caller: validIngestionCaller(identity), SourceId: identifier("source", "one"),
		ExpectedGenerationId: identifier("generation", "one"), IdempotencyKey: "revoke-1",
	}
}

func deniedIngestionReceipt(name string) *contractsv1.Receipt {
	receipt := fixtureReceipt(name)
	receipt.Status = contractsv1.ReceiptStatus_RECEIPT_STATUS_REJECTED
	receipt.ReasonCode = "not_found_or_denied"
	return receipt
}

func deniedAddSourceResponse() *contractsv1.AddSourceResponse {
	return &contractsv1.AddSourceResponse{Receipt: deniedIngestionReceipt("add"), Outcome: &contractsv1.AddSourceResponse_Error{Error: deniedPublicError()}}
}

func deniedGetSourceStatusResponse() *contractsv1.GetSourceStatusResponse {
	return &contractsv1.GetSourceStatusResponse{Receipt: deniedIngestionReceipt("status"), Outcome: &contractsv1.GetSourceStatusResponse_Error{Error: deniedPublicError()}}
}

func deniedSearchCodeResponse() *contractsv1.SearchCodeResponse {
	return &contractsv1.SearchCodeResponse{Receipt: deniedIngestionReceipt("search"), Outcome: &contractsv1.SearchCodeResponse_Error{Error: deniedPublicError()}}
}

func deniedReconcileSourceResponse() *contractsv1.ReconcileSourceResponse {
	return &contractsv1.ReconcileSourceResponse{Receipt: deniedIngestionReceipt("reconcile"), Outcome: &contractsv1.ReconcileSourceResponse_Error{Error: deniedPublicError()}}
}

func deniedRevokeSourceResponse() *contractsv1.RevokeSourceResponse {
	return &contractsv1.RevokeSourceResponse{Receipt: deniedIngestionReceipt("revoke"), Outcome: &contractsv1.RevokeSourceResponse_Error{Error: deniedPublicError()}}
}

func deniedPublicError() *contractsv1.PublicError {
	return &contractsv1.PublicError{Code: "not_found_or_denied"}
}

func startIngestionTestServer(t *testing.T, authority IngestionAuthority) (*Server, string, func()) {
	t.Helper()
	return startIngestionTestServerWithAuthority(t, authority)
}

func startIngestionTestServerWithAuthority(t *testing.T, authority IngestionAuthority) (*Server, string, func()) {
	t.Helper()
	socketPath := secureSocketDirectory(t) + "/gateway.sock"
	config := testConfig(socketPath, &fakeAuthority{})
	config.IngestionAuthority = authority
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

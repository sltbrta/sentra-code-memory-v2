package localauthority

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	contractsv1 "github.com/sltbrta/sentra-code-memory-v2/packages/contracts/gen/go/ouroboros/contracts/v1"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestServerUsesOwnerOnlyUnixSocketCleansUpAndRestarts(t *testing.T) {
	t.Parallel()
	directory := secureSocketDirectory(t)
	socketPath := filepath.Join(directory, "gateway.sock")
	for iteration := 0; iteration < 2; iteration++ {
		server, err := NewServer(testConfig(socketPath, &fakeAuthority{}))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- server.Serve(ctx) }()
		waitForSocket(t, socketPath)
		info, err := os.Lstat(socketPath)
		if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("socket is not owner-only: %v, %v", info, err)
		}
		if server.Network() != "unix" {
			t.Fatalf("network = %q", server.Network())
		}
		cancel()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("socket remained after shutdown: %v", err)
		}
	}
}

func TestServerRejectsUnsafeAndReplacedPaths(t *testing.T) {
	t.Parallel()
	root := secureTempRoot(t)
	insecure := filepath.Join(root, "insecure")
	if err := os.Mkdir(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	// Mkdir honors the process umask; force the intended insecure mode so this
	// test remains meaningful on hosts with a restrictive default umask.
	if err := os.Chmod(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(insecure, "leaf")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(insecure, symlink); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeSocketPath := filepath.Join(nested, "gateway.sock")
	if _, err := NewServer(testConfig(safeSocketPath, &fakeAuthority{})); err != nil {
		t.Fatalf("NewServer(%q) rejected secure nested socket path: %v", safeSocketPath, err)
	}
	for _, socketPath := range []string{
		"relative.sock", filepath.Join(insecure, "gateway.sock"),
		filepath.Join(symlink, "gateway.sock"), regular,
	} {
		if _, err := NewServer(testConfig(socketPath, &fakeAuthority{})); !errors.Is(err, ErrUnsafeSocket) {
			t.Errorf("NewServer(%q) error = %v", socketPath, err)
		}
	}

	directory := filepath.Join(root, "replace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "gateway.sock")
	server, err := NewServer(testConfig(socketPath, &fakeAuthority{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(insecure, directory); err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(context.Background()); !errors.Is(err, ErrUnsafeSocket) {
		t.Fatalf("Serve after directory replacement error = %v", err)
	}
}

func TestSecureAncestorPolicyAllowsOnlyRootStickyWritableDirectories(t *testing.T) {
	t.Parallel()
	const currentUID = uint32(501)
	cases := []struct {
		name  string
		mode  os.FileMode
		owner uint32
		want  bool
	}{
		{"root sticky writable", os.ModeDir | os.ModeSticky | 0o777, 0, true},
		{"root non-sticky writable", os.ModeDir | 0o777, 0, false},
		{"foreign sticky writable", os.ModeDir | os.ModeSticky | 0o777, 502, false},
		{"root read-only ancestor", os.ModeDir | 0o755, 0, true},
		{"current owner private", os.ModeDir | 0o700, currentUID, true},
		{"foreign non-writable", os.ModeDir | 0o755, 502, false},
		{"sticky symlink", os.ModeSymlink | os.ModeSticky | 0o777, 0, false},
		{"sticky regular file", os.ModeSticky | 0o777, 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := allowsSecureAncestor(testCase.mode, testCase.owner, currentUID); got != testCase.want {
				t.Fatalf("allowsSecureAncestor(%v, %d) = %v, want %v", testCase.mode, testCase.owner, got, testCase.want)
			}
		})
	}
}

func TestPeerDenialHappensBeforeRequestDecode(t *testing.T) {
	t.Parallel()
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	reads := &countingConnection{Conn: serverSide}
	_, err := authenticateConnection(reads, func(net.Conn) (PeerContext, error) {
		return PeerContext{}, ErrPeerDenied
	})
	if !errors.Is(err, ErrPeerDenied) || reads.ReadCount() != 0 {
		t.Fatalf("authentication error=%v reads=%d", err, reads.ReadCount())
	}
}

func TestGatewayPreservesTypedMessagesAndRejectsBoundaryCases(t *testing.T) {
	t.Parallel()
	authority := &fakeAuthority{}
	_, socketPath, stop := startTestServer(t, authority, 64)
	defer stop()
	client := unixClient(socketPath)
	peer := mappedIdentity(testCredentials())

	openRequest := validOpenRequest(peer)
	body := marshal(t, openRequest)
	status, contentType, responseBody := post(t, client, openSessionProcedure, "application/proto", body)
	if status != http.StatusOK || contentType != "application/proto" {
		t.Fatalf("open response status=%d type=%q", status, contentType)
	}
	openResponse := &contractsv1.OpenLocalSessionResponse{}
	if err := proto.Unmarshal(responseBody, openResponse); err != nil || !proto.Equal(openResponse, authority.openResponse) {
		t.Fatalf("open response was not preserved: %v", err)
	}
	if !proto.Equal(authority.openRequest, openRequest) {
		t.Fatal("open request was not preserved")
	}

	executeRequest := validExecuteRequest(peer)
	status, _, responseBody = post(t, client, executeCommandProcedure, "application/proto", marshal(t, executeRequest))
	executeResponse := &contractsv1.ExecuteAuthorityCommandResponse{}
	if status != http.StatusOK || proto.Unmarshal(responseBody, executeResponse) != nil ||
		!proto.Equal(executeResponse, authority.executeResponse) || !proto.Equal(authority.executeRequest, executeRequest) {
		t.Fatal("execute request or response was not preserved")
	}

	statusRequest := validStatusRequest(peer)
	status, _, responseBody = post(t, client, readStatusProcedure, "application/proto", marshal(t, statusRequest))
	statusResponse := &contractsv1.ReadStatusResponse{}
	if status != http.StatusOK || proto.Unmarshal(responseBody, statusResponse) != nil ||
		!proto.Equal(statusResponse, authority.statusResponse) || !proto.Equal(authority.statusRequest, statusRequest) {
		t.Fatal("status request or response was not preserved")
	}

	wrong := validOpenRequest(peer)
	wrong.RequestedPrincipal.PrincipalId.Value = "other"
	invalidGrant := validExecuteRequest(peer)
	invalidGrant.Grant.Nonce = ""
	cases := []struct {
		name        string
		path        string
		contentType string
		body        []byte
		wantStatus  int
	}{
		{"malformed", openSessionProcedure, "application/proto", []byte{0x0a, 0xff}, http.StatusBadRequest},
		{"missing identity", openSessionProcedure, "application/proto", marshal(t, &contractsv1.OpenLocalSessionRequest{IdempotencyKey: "x"}), http.StatusBadRequest},
		{"principal mismatch", openSessionProcedure, "application/proto", marshal(t, wrong), http.StatusForbidden},
		{"invalid grant", executeCommandProcedure, "application/proto", marshal(t, invalidGrant), http.StatusBadRequest},
		{"wrong content type", openSessionProcedure, "application/json", body, http.StatusUnsupportedMediaType},
		{"oversize", openSessionProcedure, "application/proto", bytes.Repeat([]byte{1}, maxRequestBytes+1), http.StatusRequestEntityTooLarge},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, _, body := post(t, client, testCase.path, testCase.contentType, testCase.body)
			if status != testCase.wantStatus || bytes.Contains(body, []byte("other")) {
				t.Fatalf("status=%d body=%q", status, body)
			}
		})
	}
}

func TestGatewayRejectsInvalidAuthorityResponse(t *testing.T) {
	t.Parallel()
	authority := &fakeAuthority{invalidOpenResponse: true}
	_, socketPath, stop := startTestServer(t, authority, 64)
	defer stop()
	status, _, body := post(t, unixClient(socketPath), openSessionProcedure, "application/proto", marshal(t, validOpenRequest(mappedIdentity(testCredentials()))))
	if status != http.StatusInternalServerError || !bytes.Equal(body, []byte(`{"code":"response-invalid"}`)) {
		t.Fatalf("invalid authority response status=%d body=%q", status, body)
	}
}

func TestArtifactReadRejectsRangeOverflowBeforeAuthority(t *testing.T) {
	t.Parallel()
	authority := &fakeAuthority{}
	_, socketPath, stop := startTestServer(t, authority, 64)
	defer stop()
	request := validExecuteRequest(mappedIdentity(testCredentials()))
	request.GetArtifactRead().Offset = ^uint64(0)
	request.GetArtifactRead().Length = 2
	status, _, _ := post(t, unixClient(socketPath), executeCommandProcedure, "application/proto", marshal(t, request))
	if status != http.StatusBadRequest || authority.executeRequest != nil {
		t.Fatalf("overflow status=%d authority_called=%v", status, authority.executeRequest != nil)
	}
}

func TestValidPathsRejectsTraversalAndWidening(t *testing.T) {
	t.Parallel()
	if !validPaths([]string{"src", "docs/reference"}) {
		t.Fatal("normalized relative path prefixes were rejected")
	}
	for _, value := range []string{".", "..", "../secret", "src/../secret", "/absolute", "src/", `src\secret`, "src/*", "src/?", "src/[ab]"} {
		if validPaths([]string{value}) {
			t.Errorf("validPaths(%q) accepted traversal or widening", value)
		}
	}
}

func TestInvalidPathConsumesConnectionRequestBudget(t *testing.T) {
	t.Parallel()
	authority := &fakeAuthority{}
	server, socketPath, stop := startTestServer(t, authority, 1)
	defer stop()
	client := unixClient(socketPath)
	status, _, _ := post(t, client, "/invalid", "application/proto", []byte{1})
	if status != http.StatusNotFound {
		t.Fatalf("invalid path status = %d", status)
	}
	status, _, _ = post(t, client, openSessionProcedure, "application/proto", marshal(t, validOpenRequest(mappedIdentity(testCredentials()))))
	if status != http.StatusForbidden || authority.openRequest != nil || server == nil {
		t.Fatalf("post-budget status=%d authority_called=%v", status, authority.openRequest != nil)
	}
}

func TestBoundedListenerRejectsConnectionBeyondLimit(t *testing.T) {
	t.Parallel()
	firstServer, firstClient := net.Pipe()
	secondServer, secondClient := net.Pipe()
	defer firstClient.Close()
	defer secondClient.Close()
	listener := &sequenceListener{connections: []net.Conn{firstServer, secondServer}}
	bounded := newBoundedListener(listener, 1)
	accepted, err := bounded.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second accept error = %v", err)
	}
	if _, err := secondClient.Write([]byte{1}); err == nil {
		t.Fatal("connection beyond active limit remained open")
	}
	if err := accepted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityFailuresAreExternallyIndistinguishable(t *testing.T) {
	t.Parallel()
	responses := make([][]byte, 0, 2)
	for _, internalError := range []error{errors.New("artifact absent"), errors.New("policy denied")} {
		authority := &fakeAuthority{openError: internalError}
		_, socketPath, stop := startTestServer(t, authority, 64)
		status, _, body := post(t, unixClient(socketPath), openSessionProcedure, "application/proto", marshal(t, validOpenRequest(mappedIdentity(testCredentials()))))
		stop()
		if status != http.StatusForbidden {
			t.Fatalf("authority denial status = %d", status)
		}
		responses = append(responses, body)
	}
	if !bytes.Equal(responses[0], responses[1]) {
		t.Fatalf("absent and denied responses differ: %q != %q", responses[0], responses[1])
	}
}

type fakeAuthority struct {
	mu                  sync.Mutex
	openError           error
	invalidOpenResponse bool
	openRequest         *contractsv1.OpenLocalSessionRequest
	openResponse        *contractsv1.OpenLocalSessionResponse
	executeRequest      *contractsv1.ExecuteAuthorityCommandRequest
	executeResponse     *contractsv1.ExecuteAuthorityCommandResponse
	statusRequest       *contractsv1.ReadStatusRequest
	statusResponse      *contractsv1.ReadStatusResponse
}

func (authority *fakeAuthority) OpenSession(_ context.Context, _ PeerContext, request *contractsv1.OpenLocalSessionRequest) (*contractsv1.OpenLocalSessionResponse, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.openError != nil {
		return nil, authority.openError
	}
	authority.openRequest = proto.Clone(request).(*contractsv1.OpenLocalSessionRequest)
	if authority.invalidOpenResponse {
		return &contractsv1.OpenLocalSessionResponse{}, nil
	}
	authority.openResponse = &contractsv1.OpenLocalSessionResponse{Session: proto.Clone(request.RequestedPrincipal).(*contractsv1.AuthenticatedPrincipalRef), Receipt: fixtureReceipt("open")}
	return proto.Clone(authority.openResponse).(*contractsv1.OpenLocalSessionResponse), nil
}

func (authority *fakeAuthority) Execute(_ context.Context, _ PeerContext, request *contractsv1.ExecuteAuthorityCommandRequest) (*contractsv1.ExecuteAuthorityCommandResponse, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.executeRequest = proto.Clone(request).(*contractsv1.ExecuteAuthorityCommandRequest)
	artifact := request.GetArtifactRead().Artifact
	authority.executeResponse = &contractsv1.ExecuteAuthorityCommandResponse{Receipt: fixtureReceipt("execute"), Artifact: proto.Clone(artifact).(*contractsv1.ArtifactRef), Generation: 8}
	return proto.Clone(authority.executeResponse).(*contractsv1.ExecuteAuthorityCommandResponse), nil
}

func (authority *fakeAuthority) ReadStatus(_ context.Context, _ PeerContext, request *contractsv1.ReadStatusRequest) (*contractsv1.ReadStatusResponse, error) {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.statusRequest = proto.Clone(request).(*contractsv1.ReadStatusRequest)
	identity := mappedIdentity(testCredentials())
	authority.statusResponse = &contractsv1.ReadStatusResponse{Session: principalRef(identity), Watermark: 11, RevocationEpoch: 4, ObservedAt: timestamppb.New(time.Unix(100, 0)), Receipt: fixtureReceipt("status"), Render: &contractsv1.RenderModel{Title: "Ready", Detail: "Replay complete."}}
	return proto.Clone(authority.statusResponse).(*contractsv1.ReadStatusResponse), nil
}

func validOpenRequest(identity shared.MappedIdentityFact) *contractsv1.OpenLocalSessionRequest {
	return &contractsv1.OpenLocalSessionRequest{RequestedPrincipal: principalRef(identity), IdempotencyKey: "open-1", ResumeFrom: &contractsv1.Cursor{Watermark: 2, Token: "next"}}
}

func validExecuteRequest(identity shared.MappedIdentityFact) *contractsv1.ExecuteAuthorityCommandRequest {
	artifact := &contractsv1.ArtifactRef{ArtifactId: identifier("artifact", "a1"), TenantId: identifier("tenant", "local"), ContentDigest: digest()}
	return &contractsv1.ExecuteAuthorityCommandRequest{
		Command:         &contractsv1.CommandEnvelope{CommandId: identifier("command", "c1"), CommandType: "artifact.read.v1", Actor: principalRef(identity), SubmittedAt: timestamppb.New(time.Unix(90, 0)), IdempotencyKey: "execute-1", Causal: causal(), PayloadDigest: digest()},
		Grant:           &contractsv1.CapabilityGrant{GrantId: identifier("grant", "g1"), Initiator: principalRef(identity), Actions: []string{"artifact.read"}, Resources: []*contractsv1.Identifier{identifier("artifact", "a1")}, Nonce: "nonce-1", ExpiresAt: timestamppb.New(time.Unix(200, 0)), PolicyDigest: digest(), CommandFence: 1},
		ArtifactCommand: &contractsv1.ExecuteAuthorityCommandRequest_ArtifactRead{ArtifactRead: &contractsv1.ArtifactReadCommand{Artifact: artifact, Generation: 7, Length: 32}},
	}
}

func validStatusRequest(identity shared.MappedIdentityFact) *contractsv1.ReadStatusRequest {
	return &contractsv1.ReadStatusRequest{RequestedSession: identifier(identity.Session.Namespace, identity.Session.Value), After: &contractsv1.Cursor{Watermark: 3}}
}

func fixtureReceipt(value string) *contractsv1.Receipt {
	return &contractsv1.Receipt{ReceiptId: identifier("receipt", value), Status: contractsv1.ReceiptStatus_RECEIPT_STATUS_ACCEPTED, ReasonCode: value + "-accepted", OperationId: identifier("operation", value), Causal: causal(), RecordedAt: timestamppb.New(time.Unix(100, 0)), ConfigurationDigest: digest()}
}

func causal() *contractsv1.CausalContext {
	return &contractsv1.CausalContext{CorrelationId: identifier("correlation", "c1"), CausationId: identifier("causation", "c1"), TraceId: identifier("trace", "t1"), Fence: 1}
}

func digest() *contractsv1.Digest { return &contractsv1.Digest{Algorithm: "sha256", Hex: "aa"} }
func identifier(namespace, value string) *contractsv1.Identifier {
	return &contractsv1.Identifier{Namespace: namespace, Value: value}
}

func principalRef(identity shared.MappedIdentityFact) *contractsv1.AuthenticatedPrincipalRef {
	return &contractsv1.AuthenticatedPrincipalRef{PrincipalId: identifier(identity.Principal.Namespace, identity.Principal.Value), TenantId: identifier(identity.Tenant.Namespace, identity.Tenant.Value), SessionId: identifier(identity.Session.Namespace, identity.Session.Value)}
}

func testConfig(socketPath string, authority Authority) Config {
	return Config{SocketPath: socketPath, Authority: authority, PeerMapper: PeerMapperFunc(func(credentials PeerCredentials) (shared.MappedIdentityFact, error) {
		return mappedIdentity(credentials), nil
	}), ExpectedUID: uint32(os.Getuid()), MaxRequestsPerConnect: 64, MaxActiveConnections: 8}
}

func mappedIdentity(credentials PeerCredentials) shared.MappedIdentityFact {
	return shared.MappedIdentityFact{Principal: shared.Identifier{Namespace: "principal", Value: "local-user"}, Tenant: shared.Identifier{Namespace: "tenant", Value: "local"}, Session: shared.Identifier{Namespace: "session", Value: "session-1"}, Credentials: shared.PeerCredentials{UID: credentials.UID, GID: credentials.GID, PID: credentials.PID}}
}

func startTestServer(t *testing.T, authority Authority, requestLimit uint32) (*Server, string, func()) {
	t.Helper()
	socketPath := filepath.Join(secureSocketDirectory(t), "gateway.sock")
	config := testConfig(socketPath, authority)
	config.MaxRequestsPerConnect = requestLimit
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

func secureSocketDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(secureTempRoot(t), "authority")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func secureTempRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "ob-gw-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return root
}

func testCredentials() PeerCredentials {
	return PeerCredentials{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), PID: uint32(os.Getpid())}
}

func marshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func waitForSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(socketPath); err == nil &&
			info.Mode()&os.ModeSocket != 0 && info.Mode().Perm() == 0o600 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("socket did not become ready")
}

func unixClient(socketPath string) *http.Client {
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}}
}

func post(t *testing.T, client *http.Client, path, contentType string, payload []byte) (int, string, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://local"+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", contentType)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header.Get("Content-Type"), body
}

type countingConnection struct {
	net.Conn
	mu    sync.Mutex
	reads int
}

func (connection *countingConnection) Read(buffer []byte) (int, error) {
	connection.mu.Lock()
	connection.reads++
	connection.mu.Unlock()
	return connection.Conn.Read(buffer)
}
func (connection *countingConnection) ReadCount() int {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.reads
}

type sequenceListener struct {
	connections []net.Conn
	index       int
}

func (listener *sequenceListener) Accept() (net.Conn, error) {
	if listener.index >= len(listener.connections) {
		return nil, net.ErrClosed
	}
	connection := listener.connections[listener.index]
	listener.index++
	return connection, nil
}
func (*sequenceListener) Close() error   { return nil }
func (*sequenceListener) Addr() net.Addr { return testAddress("sequence") }

type testAddress string

func (address testAddress) Network() string { return "test" }
func (address testAddress) String() string  { return string(address) }

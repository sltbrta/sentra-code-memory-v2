package localauthority

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

var (
	// ErrUnsafeSocket indicates that the configured local IPC path is not owner-only.
	ErrUnsafeSocket = errors.New("unsafe local authority socket")
	// ErrPeerDenied is returned before request bytes are read for an unauthenticated peer.
	ErrPeerDenied = errors.New("local peer denied")
)

type peerAuthenticator func(net.Conn) (PeerContext, error)

// Config defines the complete local transport authority. SocketPath must be an
// absolute path beneath an existing owner-only directory. Authority is required.
type Config struct {
	// SocketPath is the absolute owner-only Unix socket path.
	SocketPath string
	// Authority is the canonical typed local authority implementation.
	Authority Authority
	// IngestionAuthority enables the five Stage 3 ingestion routes when set.
	IngestionAuthority IngestionAuthority
	// QueryAuthority enables the four Stage 4 query routes when set.
	QueryAuthority QueryAuthority
	// FactoryAuthority enables the five Stage 5 factory routes when set.
	FactoryAuthority FactoryAuthority
	// TracerAuthority enables the eight Stage 6 Tracer 001 JSON routes when set.
	TracerAuthority TracerAuthority
	// MeetingAuthority enables the five Stage 7 meeting routes when set.
	MeetingAuthority MeetingAuthority
	// ConnectorAuthority enables the six Stage 8 connector routes when set.
	ConnectorAuthority ConnectorAuthority
	// MultimodalAuthority enables the five Stage 11 multimodal routes when set.
	MultimodalAuthority MultimodalAuthority
	// PeerMapper maps authenticated operating-system credentials to identity.
	PeerMapper PeerMapper
	// ExpectedUID is the only local user allowed to connect.
	ExpectedUID uint32
	// MaxRequestsPerConnect bounds all requests, including invalid routes.
	MaxRequestsPerConnect uint32
	// MaxActiveConnections bounds accepted local client connections.
	MaxActiveConnections uint32
	authenticatePeer     peerAuthenticator
}

// Server serves the frozen local-authority procedures and, when configured,
// the five frozen Stage 3 ingestion procedures, the four frozen Stage 4 query
// procedures, the five frozen Stage 5 factory procedures, the eight Stage 6
// Tracer 001 JSON composition-facade procedures, the five frozen Stage 7
// meeting procedures, the six frozen Stage 8 connector procedures, and the
// five frozen Stage 11 multimodal procedures.
type Server struct {
	config Config
	http   *http.Server
	router http.Handler
}

type peerContextKey struct{}
type requestPeerKey struct{}

// NewServer validates the fail-closed Unix transport configuration.
func NewServer(config Config) (*Server, error) {
	if config.Authority == nil || config.PeerMapper == nil || config.MaxRequestsPerConnect == 0 ||
		config.MaxActiveConnections == 0 {
		return nil, fmt.Errorf("%w: incomplete configuration", ErrUnsafeSocket)
	}
	if err := validateSocketPath(config.SocketPath); err != nil {
		return nil, err
	}
	if config.authenticatePeer == nil {
		config.authenticatePeer = systemPeerAuthenticator(config.ExpectedUID, config.PeerMapper)
	}
	hasIngestionAuthority := !isNilInterface(config.IngestionAuthority)
	if !hasIngestionAuthority {
		config.IngestionAuthority = nil
	}
	hasQueryAuthority := !isNilInterface(config.QueryAuthority)
	if !hasQueryAuthority {
		config.QueryAuthority = nil
	}
	hasFactoryAuthority := !isNilInterface(config.FactoryAuthority)
	if !hasFactoryAuthority {
		config.FactoryAuthority = nil
	}
	hasTracerAuthority := !isNilInterface(config.TracerAuthority)
	if !hasTracerAuthority {
		config.TracerAuthority = nil
	}
	hasMeetingAuthority := !isNilInterface(config.MeetingAuthority)
	if !hasMeetingAuthority {
		config.MeetingAuthority = nil
	}
	hasConnectorAuthority := !isNilInterface(config.ConnectorAuthority)
	if !hasConnectorAuthority {
		config.ConnectorAuthority = nil
	}
	hasMultimodalAuthority := !isNilInterface(config.MultimodalAuthority)
	if !hasMultimodalAuthority {
		config.MultimodalAuthority = nil
	}
	server := &Server{config: config}
	mux := http.NewServeMux()
	mux.HandleFunc(openSessionProcedure, server.handleOpenSession)
	mux.HandleFunc(executeCommandProcedure, server.handleExecute)
	mux.HandleFunc(readStatusProcedure, server.handleStatus)
	if hasIngestionAuthority {
		mux.HandleFunc(addSourceProcedure, server.handleAddSource)
		mux.HandleFunc(getSourceStatusProcedure, server.handleGetSourceStatus)
		mux.HandleFunc(searchCodeProcedure, server.handleSearchCode)
		mux.HandleFunc(reconcileSourceProcedure, server.handleReconcileSource)
		mux.HandleFunc(revokeSourceProcedure, server.handleRevokeSource)
	}
	if hasQueryAuthority {
		mux.HandleFunc(askProcedure, server.handleAsk)
		mux.HandleFunc(listSourcesProcedure, server.handleListSources)
		mux.HandleFunc(getHistoryProcedure, server.handleGetHistory)
		mux.HandleFunc(getStatusProcedure, server.handleGetStatus)
	}
	if hasFactoryAuthority {
		mux.HandleFunc(admitChangeIntentProcedure, server.handleAdmitChangeIntent)
		mux.HandleFunc(getChangePlanProcedure, server.handleGetChangePlan)
		mux.HandleFunc(previewChangeSetProcedure, server.handlePreviewChangeSet)
		mux.HandleFunc(getReviewFindingsProcedure, server.handleGetReviewFindings)
		mux.HandleFunc(cancelChangeRunProcedure, server.handleCancelChangeRun)
	}
	if hasTracerAuthority {
		mux.HandleFunc(tracerSessionProcedure, server.handleTracerSession)
		mux.HandleFunc(tracerIngestProcedure, server.handleTracerIngest)
		mux.HandleFunc(tracerAskProcedure, server.handleTracerAsk)
		mux.HandleFunc(tracerIntentProcedure, server.handleTracerIntent)
		mux.HandleFunc(tracerPlanProcedure, server.handleTracerPlan)
		mux.HandleFunc(tracerReviewProcedure, server.handleTracerReview)
		mux.HandleFunc(tracerDraftPRProcedure, server.handleTracerDraftPR)
		mux.HandleFunc(tracerOutcomeProcedure, server.handleTracerOutcome)
	}
	if hasMeetingAuthority {
		mux.HandleFunc(importTranscriptProcedure, server.handleImportTranscript)
		mux.HandleFunc(getMeetingStatusProcedure, server.handleGetMeetingStatus)
		mux.HandleFunc(queryMeetingProcedure, server.handleQueryMeeting)
		mux.HandleFunc(revokeMeetingProcedure, server.handleRevokeMeeting)
		mux.HandleFunc(purgeMeetingProcedure, server.handlePurgeMeeting)
	}
	if hasConnectorAuthority {
		mux.HandleFunc(connectGitHubProcedure, server.handleConnectGitHubSource)
		mux.HandleFunc(getConnectorStatusProcedure, server.handleGetConnectorStatus)
		mux.HandleFunc(reconcileConnectorProcedure, server.handleReconcileConnector)
		mux.HandleFunc(queryConnectorProcedure, server.handleQueryConnectorEvidence)
		mux.HandleFunc(revokeConnectorProcedure, server.handleRevokeConnector)
		mux.HandleFunc(purgeConnectorProcedure, server.handlePurgeConnector)
	}
	if hasMultimodalAuthority {
		mux.HandleFunc(admitMultimodalProcedure, server.handleAdmitMultimodalSource)
		mux.HandleFunc(getMultimodalStatusProcedure, server.handleGetMultimodalStatus)
		mux.HandleFunc(getMultimodalEvidenceProcedure, server.handleGetMultimodalEvidence)
		mux.HandleFunc(revokeMultimodalProcedure, server.handleRevokeMultimodalSource)
		mux.HandleFunc(purgeMultimodalProcedure, server.handlePurgeMultimodalSource)
	}
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeStaticHTTPError(writer, http.StatusNotFound, "procedure-not-found")
	})
	server.router = mux
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 * 1024,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			if authenticated, ok := connection.(*authenticatedConnection); ok {
				return context.WithValue(ctx, peerContextKey{}, &connectionContext{
					peer:  authenticated.peer,
					limit: config.MaxRequestsPerConnect,
				})
			}
			return ctx
		},
	}
	return server, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Network reports the only supported transport and is stable for diagnostics.
func (*Server) Network() string { return "unix" }

// ServeHTTP applies the per-connection request bound before method, content,
// or path routing so invalid traffic cannot bypass the same finite budget.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	connection, ok := request.Context().Value(peerContextKey{}).(*connectionContext)
	if !ok || connection == nil || connection.requests.Add(1) > connection.limit {
		writeStaticHTTPError(writer, http.StatusForbidden, "request-denied")
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), requestPeerKey{}, connection.peer))
	s.router.ServeHTTP(writer, request)
}

// Serve listens on the configured owner-only socket until cancellation.
func (s *Server) Serve(ctx context.Context) error {
	if err := validateSocketPath(s.config.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.config.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on local authority socket: %w", err)
	}
	if err := os.Chmod(s.config.SocketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.config.SocketPath)
		return fmt.Errorf("secure local authority socket: %w", err)
	}
	bounded := newBoundedListener(listener, s.config.MaxActiveConnections)
	authenticated := newAuthenticatedListener(bounded, s.config.authenticatePeer)
	serveResult := make(chan error, 1)
	go func() { serveResult <- s.http.Serve(authenticated) }()

	var serveErr error
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		shutdownErr := s.http.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			_ = s.http.Close()
			serveErr = fmt.Errorf("shutdown local authority server: %w", shutdownErr)
		}
		if result := <-serveResult; result != nil && !errors.Is(result, http.ErrServerClosed) && serveErr == nil {
			serveErr = fmt.Errorf("serve local authority socket: %w", result)
		}
	case result := <-serveResult:
		if result != nil && !errors.Is(result, http.ErrServerClosed) {
			serveErr = fmt.Errorf("serve local authority socket: %w", result)
		}
	}
	if removeErr := removeOwnedSocket(s.config.SocketPath); removeErr != nil && serveErr == nil {
		serveErr = removeErr
	}
	return serveErr
}

type connectionContext struct {
	peer     PeerContext
	requests atomic.Uint32
	limit    uint32
}

func peerForRequest(request *http.Request) (PeerContext, bool) {
	peer, ok := request.Context().Value(requestPeerKey{}).(PeerContext)
	return peer, ok
}

type boundedListener struct {
	net.Listener
	active chan struct{}
}

func newBoundedListener(listener net.Listener, maximum uint32) net.Listener {
	return &boundedListener{Listener: listener, active: make(chan struct{}, maximum)}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.active <- struct{}{}:
			return &boundedConnection{Conn: connection, release: func() { <-l.active }}, nil
		default:
			_ = connection.Close()
		}
	}
}

type boundedConnection struct {
	net.Conn
	release func()
	closed  atomic.Bool
}

func (connection *boundedConnection) Close() error {
	err := connection.Conn.Close()
	if connection.closed.CompareAndSwap(false, true) {
		connection.release()
	}
	return err
}

type authenticatedListener struct {
	net.Listener
	authenticate peerAuthenticator
}

func newAuthenticatedListener(listener net.Listener, authenticate peerAuthenticator) net.Listener {
	return &authenticatedListener{Listener: listener, authenticate: authenticate}
}

func (l *authenticatedListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		authenticated, err := authenticateConnection(connection, l.authenticate)
		if err == nil {
			return authenticated, nil
		}
	}
}

func authenticateConnection(connection net.Conn, authenticate peerAuthenticator) (net.Conn, error) {
	peer, err := authenticate(connection)
	if err != nil {
		_ = connection.Close()
		return nil, ErrPeerDenied
	}
	return &authenticatedConnection{Conn: connection, peer: peer}, nil
}

type authenticatedConnection struct {
	net.Conn
	peer PeerContext
}

func validateSocketPath(socketPath string) error {
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return fmt.Errorf("%w: path must be absolute and clean", ErrUnsafeSocket)
	}
	if len(socketPath) > 103 {
		return fmt.Errorf("%w: path exceeds local socket limit", ErrUnsafeSocket)
	}
	directory := filepath.Dir(socketPath)
	if err := validateAncestors(directory); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: socket directory unavailable", ErrUnsafeSocket)
	}
	if info.Mode().Perm() != 0o700 || ownerUID(info) != uint32(os.Getuid()) {
		return fmt.Errorf("%w: socket directory is not owner-only", ErrUnsafeSocket)
	}
	if _, err := os.Lstat(socketPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: socket path already exists", ErrUnsafeSocket)
	}
	return nil
}

func validateAncestors(directory string) error {
	current := string(filepath.Separator)
	relative := strings.TrimPrefix(directory, current)
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: socket ancestor unavailable", ErrUnsafeSocket)
		}
		if !allowsSecureAncestor(info.Mode(), ownerUID(info), uint32(os.Getuid())) {
			return fmt.Errorf("%w: socket ancestor is writable or foreign", ErrUnsafeSocket)
		}
	}
	return nil
}

func allowsSecureAncestor(mode os.FileMode, owner, current uint32) bool {
	if !mode.IsDir() || mode&os.ModeSymlink != 0 {
		return false
	}
	if mode.Perm()&0o022 != 0 {
		return owner == 0 && mode&os.ModeSticky != 0
	}
	return owner == 0 || owner == current
}

type osFileInfo interface {
	Sys() any
}

func removeOwnedSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 || ownerUID(info) != uint32(os.Getuid()) {
		return fmt.Errorf("%w: refusing socket cleanup", ErrUnsafeSocket)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove local authority socket: %w", err)
	}
	return nil
}

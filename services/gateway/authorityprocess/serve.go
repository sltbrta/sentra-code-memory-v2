package authorityprocess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	brainconnector "github.com/sltbrta/sentra-code-memory-v2/services/brain/connector"
	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/connectorapi"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/factoryapi"
	gateway "github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localbootstrap"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/meetingapi"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/multimodalapi"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/queryapi"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type dependencies struct {
	runtime *brain.Runtime
	broker  *broker.Broker
}

func serveWithDependencies(ctx context.Context, config *localbootstrap.Config, deps dependencies) error {
	if ctx == nil || config == nil || deps.runtime == nil || deps.broker == nil {
		return errInvalidConfig
	}
	authority := &authorityAdapter{
		runtime: deps.runtime, broker: deps.broker, keyEpoch: config.KeyEpoch(), now: time.Now,
		configuration: brain.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()},
		brain:         brain.Identifier{Namespace: "brain", Value: config.Brain()}, repositoryID: bootstrapRepositoryID(config),
		approvedRoot: config.ApprovedSourceRoot(), gitExecutable: "/usr/bin/git", commandTimeout: 5 * time.Second,
	}
	queryAuthority, closeQuery, err := composeQueryAuthority(ctx, config, deps)
	if err != nil {
		return errInvalidConfig
	}
	defer func() { _ = closeQuery() }()
	factoryAuthority, closeFactory, err := composeFactoryAuthority(ctx, config, deps)
	if err != nil {
		return errInvalidConfig
	}
	defer func() { _ = closeFactory() }()
	tracerAuthority, closeTracer, err := composeTracerAuthority(ctx, config, deps)
	if err != nil {
		return errInvalidConfig
	}
	defer func() { _ = closeTracer() }()
	meetingAuthority, closeMeeting, err := composeMeetingAuthority(ctx, config, deps)
	if err != nil {
		return errInvalidConfig
	}
	defer func() { _ = closeMeeting() }()
	connectorAuthority, closeConnector, err := composeConnectorAuthority(ctx, config, deps)
	if err != nil {
		return errInvalidConfig
	}
	defer func() { _ = closeConnector() }()
	multimodalAuthority, closeMultimodal, err := composeMultimodalAuthority(ctx, config, deps)
	if err != nil {
		return errInvalidConfig
	}
	defer func() { _ = closeMultimodal() }()
	server, err := gateway.NewServer(gateway.Config{
		SocketPath: config.SocketPath(), Authority: authority, IngestionAuthority: authority,
		QueryAuthority: queryAuthority, FactoryAuthority: factoryAuthority,
		TracerAuthority: tracerAuthority, MeetingAuthority: meetingAuthority,
		ConnectorAuthority: connectorAuthority, MultimodalAuthority: multimodalAuthority,
		PeerMapper:  peerMapper{broker: deps.broker},
		ExpectedUID: uint32(os.Getuid()), MaxActiveConnections: config.MaxConnections(),
		MaxRequestsPerConnect: config.MaxRequests(),
	})
	if err != nil {
		return errInvalidConfig
	}
	if err := server.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return errInvalidConfig
	}
	return nil
}

// composeQueryAuthority builds the Stage 04 query surface on the same
// authenticated gateway as the ingestion procedures: the grounded-query
// engine over the durable corpus, the migration 004 conversation store, the
// migration 003 source catalog, and the current-relationship broker. Startup
// recovery marks interrupted assistant turns failed before the surface
// serves, and every composition failure rejects startup. This frozen local
// Stage 04 compatibility path is retired from organization-brain serving and
// therefore opts out explicitly until it has a canonical propagation-receipt
// provider.
func composeQueryAuthority(
	ctx context.Context,
	config *localbootstrap.Config,
	deps dependencies,
) (gateway.QueryAuthority, func() error, error) {
	synthesizer, err := querySynthesizerFromEnv(os.Getenv)
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	brainID := brain.Identifier{Namespace: "brain", Value: config.Brain()}
	identity := brain.Identity{
		Principal:   brain.Identifier{Namespace: "principal", Value: config.Principal()},
		Tenant:      brain.Identifier{Namespace: "tenant", Value: config.Tenant()},
		Session:     brain.Identifier{Namespace: "session", Value: config.Session()},
		Credentials: shared.PeerCredentials{UID: uint32(os.Getuid())},
	}
	authorizer := func(ctx context.Context, identity brain.Identity, action string, _ string) (bool, uint64, error) {
		decision, err := deps.broker.AuthorizeSource(ctx, identity, action, broker.Identifier(brainID))
		if err != nil {
			return false, decision.RevocationEpoch, nil
		}
		return decision.Allowed, decision.RevocationEpoch, nil
	}
	surface, err := deps.runtime.OpenLegacyQuerySurfaceWithoutEvidenceAdmission(ctx, identity, authorizer, synthesizer)
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	if err := surface.RecoverInterrupted(ctx); err != nil {
		_ = surface.Close()
		return nil, nil, errInvalidConfig
	}
	sourceID := deps.runtime.ConfiguredIngestionSourceID()
	handler, err := queryapi.NewHandler(queryapi.Config{
		Engine:        queryEngineAdapter{engine: surface.Engine()},
		Conversations: queryConversationsAdapter{store: surface.Conversations()},
		Sources: queryCatalogAdapter{
			surface: surface, broker: deps.broker, brain: brainID, source: sourceID,
		},
		Authorizer:          queryAuthorizerAdapter{broker: deps.broker, brain: brainID, source: sourceID},
		Clock:               queryClock{},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()},
	})
	if err != nil {
		_ = surface.Close()
		return nil, nil, errInvalidConfig
	}
	return queryAuthorityAdapter{handler: handler}, surface.Close, nil
}

// composeFactoryAuthority builds the Stage 05 bounded-factory surface on the
// same authenticated gateway as the ingestion and query procedures: the
// deterministic kernel over the durable authority and encrypted payload vault,
// the sealed runner over isolated exact-base candidates, and the production
// broker for every current-policy checkpoint. Startup reaps the process-local
// candidate projection — durable run, plan, lease, candidate, and finding
// facts are kernel-owned and survive — and every composition failure rejects
// startup.
//
// composeTracerAuthority (tracer_adapter.go) mounts Stage 06 Tracer 001 as a
// JSON composition facade on the same socket: synthetic L1 digests, L2 DAG
// compiler, draft-PR broker (FakeAPI by default), and outcome admission.
func composeFactoryAuthority(
	ctx context.Context,
	config *localbootstrap.Config,
	deps dependencies,
) (gateway.FactoryAuthority, func() error, error) {
	leaseTTL, err := factoryLeaseTTLFromEnv(os.Getenv)
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	brainID := brain.Identifier{Namespace: "brain", Value: config.Brain()}
	// PID is required by the durable Execute identity boundary: the factory
	// surface revalidates approval descriptors through real artifact.read
	// calls, so the composed identity must be a complete peer fact.
	identity := brain.Identity{
		Principal: brain.Identifier{Namespace: "principal", Value: config.Principal()},
		Tenant:    brain.Identifier{Namespace: "tenant", Value: config.Tenant()},
		Session:   brain.Identifier{Namespace: "session", Value: config.Session()},
		Credentials: shared.PeerCredentials{
			UID: uint32(os.Getuid()), PID: uint32(os.Getpid()),
		},
	}
	policy := factoryPolicyAdapter{broker: deps.broker, brain: broker.Identifier(brainID)}
	epoch, err := deps.broker.RevocationEpoch(identity.Tenant)
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	// The candidate store is a process-local rebuildable projection beneath the
	// owner-only state root: it is reaped at startup so a crashed predecessor
	// never leaves orphaned isolated candidates, and durable replay facts stay
	// with the kernel ledger.
	candidateRoot := filepath.Join(config.StateRoot(), "factory-candidates")
	if err := os.RemoveAll(candidateRoot); err != nil {
		return nil, nil, errInvalidConfig
	}
	if err := os.MkdirAll(candidateRoot, 0o700); err != nil {
		return nil, nil, errInvalidConfig
	}
	fences := newFactoryFenceRegistry()
	factoryRunner, err := broker.OpenFactoryRunner(broker.FactoryRunnerConfig{
		CanonicalRoot:  config.ApprovedSourceRoot(),
		CandidateRoot:  candidateRoot,
		GitExecutable:  "/usr/bin/git",
		CommandTimeout: 5 * time.Second,
		MaxFiles:       10_000,
		MaxFileBytes:   1 << 20,
		MaxTotalBytes:  16 << 20,
		Policy:         policy,
		Fences:         fences,
		Clock:          func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	surface, err := deps.runtime.OpenFactorySurface(ctx, brain.FactorySurfaceConfig{
		Policy:          policy,
		LeaseTTLMillis:  leaseTTL,
		RevocationEpoch: epoch,
		PolicyDigestHex: config.PolicyDigest(),
	})
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	handler, err := factoryapi.NewHandler(factoryapi.Config{
		Kernel: &factoryKernelAdapter{
			kernel: surface.Kernel(), runner: factoryRunner, runtime: deps.runtime,
			broker: deps.broker, config: config, identity: identity, keyEpoch: config.KeyEpoch(),
			now: time.Now, fences: fences, configHex: config.ConfigurationDigest(),
		},
		Clock:               queryClock{},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()},
	})
	if err != nil {
		_ = surface.Close()
		return nil, nil, errInvalidConfig
	}
	return factoryAuthorityAdapter{handler: handler}, surface.Close, nil
}

// composeMeetingAuthority builds the Stage 07 meeting-transcript surface on the
// same authenticated gateway: the deterministic meeting kernel over migration
// 006 and the encrypted ArtifactVault payload port shared with conversation.
func composeMeetingAuthority(
	ctx context.Context,
	config *localbootstrap.Config,
	deps dependencies,
) (gateway.MeetingAuthority, func() error, error) {
	surface, err := deps.runtime.OpenMeetingSurface(ctx)
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	handler, err := meetingapi.NewHandler(meetingapi.Config{
		Kernel:              meetingKernelAdapter{kernel: surface.Kernel()},
		Clock:               queryClock{},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()},
	})
	if err != nil {
		_ = surface.Close()
		return nil, nil, errInvalidConfig
	}
	return meetingAuthorityAdapter{handler: handler}, surface.Close, nil
}

// composeMultimodalAuthority builds the Stage 11 multimodal surface on the
// same authenticated gateway: the deterministic multimodal kernel over migration
// 007 and the encrypted ArtifactVault payload port shared with conversation.
func composeMultimodalAuthority(
	ctx context.Context,
	config *localbootstrap.Config,
	deps dependencies,
) (gateway.MultimodalAuthority, func() error, error) {
	surface, err := deps.runtime.OpenMultimodalSurface(ctx)
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	handler, err := multimodalapi.NewHandler(multimodalapi.Config{
		Kernel:              multimodalKernelAdapter{kernel: surface.Kernel()},
		Clock:               queryClock{},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()},
	})
	if err != nil {
		_ = surface.Close()
		return nil, nil, errInvalidConfig
	}
	return multimodalAuthorityAdapter{handler: handler}, surface.Close, nil
}

// composeConnectorAuthority builds the Stage 08 GitHub source-connector surface
// on the authenticated gateway: deterministic fake provider by default, optional
// live REST when GITHUB_TOKEN and OUROBOROS_CONNECTOR_LIVE=1 are set.
func composeConnectorAuthority(
	ctx context.Context,
	config *localbootstrap.Config,
	deps dependencies,
) (gateway.ConnectorAuthority, func() error, error) {
	_ = deps
	surface, err := brainconnector.OpenSurface(ctx)
	if err != nil {
		return nil, nil, errInvalidConfig
	}
	handler, err := connectorapi.NewHandler(connectorapi.Config{
		Kernel:              connectorKernelAdapter{kernel: surface.Kernel()},
		Clock:               queryClock{},
		ConfigurationDigest: shared.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()},
	})
	if err != nil {
		_ = surface.Close()
		return nil, nil, errInvalidConfig
	}
	return connectorAuthorityAdapter{handler: handler}, surface.Close, nil
}

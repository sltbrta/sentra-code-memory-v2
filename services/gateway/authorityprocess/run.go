// Package authorityprocess is the product-owned local authority process
// (ADR 0022). It serves peer-authenticated Unix-socket RPCs for grounded
// query, Git ingestion, conversation vault, factory/meeting/multimodal/
// connector/tracer surfaces. Company-doc residual ask and async gardener
// live on product-brain; this process is the authority half of the product
// superset — not a second competing product.
//
// Historical snapshot: archive/2026-07-stage-retirement/ (frozen copy).
// Comparison: docs/specs/product/STAGE-VS-PRODUCT.md
package authorityprocess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"time"

	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localbootstrap"
)

// RuntimeOpener opens the concrete durable authority selected by the command.
// Tests may supply a function with the same immutable signature; production
// always passes brain.OpenDarwin directly through Run.
type RuntimeOpener func(context.Context, brain.DarwinConfig) (*brain.Runtime, error)

// systemClock supplies live receipt timestamps after the single startup
// instant has been used for bootstrap expiry and grant registration.
type systemClock struct{}

func (systemClock) NowUnixMilli() int64 { return time.Now().UTC().UnixMilli() }

// Run starts the production command with the fixed Darwin Keychain-backed
// runtime opener. It does not consult ambient plugins, opener overrides, or fallbacks.
func Run(ctx context.Context, arguments []string) error {
	return RunWithOpener(ctx, arguments, brain.OpenDarwin)
}

// RunWithOpener loads one digest-pinned bootstrap, composes its trusted broker
// and durable brain, and serves the owner-only Unix socket until cancellation.
// Every startup failure collapses to errInvalidConfig without exposing an
// adapter, filesystem, Keychain, policy, or storage error.
func RunWithOpener(ctx context.Context, arguments []string, opener RuntimeOpener) error {
	if ctx == nil || opener == nil {
		return errInvalidConfig
	}
	parsed, err := parseConfig(arguments)
	if err != nil {
		return staticStartupError(err)
	}
	startupInstant := time.Now().UTC()
	config, err := localbootstrap.Load(localbootstrap.Options{
		ManifestPath: parsed.bootstrapPath, ExpectedSHA256: parsed.bootstrapSHA256,
		Now: func() time.Time { return startupInstant },
	})
	if err != nil {
		return staticStartupError(err)
	}
	uid, err := ownerUID()
	if err != nil {
		return staticStartupError(err)
	}
	policy, err := brokerFromBootstrap(config, uid, startupInstant)
	if err != nil {
		return staticStartupError(err)
	}
	runtime, err := opener(ctx, darwinConfig(config))
	if err != nil || runtime == nil {
		return staticStartupError(errInvalidConfig)
	}
	serveErr := serveWithDependencies(ctx, config, dependencies{runtime: runtime, broker: policy})
	closeErr := runtime.Close()
	if serveErr != nil || closeErr != nil {
		return staticStartupError(errInvalidConfig)
	}
	return nil
}

func ownerUID() (uint32, error) {
	uid := os.Getuid()
	if uid < 0 || uint64(uid) > math.MaxUint32 {
		return 0, errInvalidConfig
	}
	return uint32(uid), nil
}

func brokerFromBootstrap(config *localbootstrap.Config, uid uint32, now time.Time) (*broker.Broker, error) {
	if config == nil || now.IsZero() {
		return nil, errInvalidConfig
	}
	principal := broker.Identifier{Namespace: "principal", Value: config.Principal()}
	tenant := broker.Identifier{Namespace: "tenant", Value: config.Tenant()}
	policy, err := broker.New(broker.Config{
		UID: uid, Principal: principal, Tenant: tenant,
		Session: broker.Identifier{Namespace: "session", Value: config.Session()},
	})
	if err != nil {
		return nil, errInvalidConfig
	}
	for _, relationship := range config.Relationships() {
		if err := policy.AddRelationship(relationship.Object + "#" + relationship.Relation + "@" + relationship.User); err != nil {
			return nil, errInvalidConfig
		}
	}
	if err := policy.SetRevocationEpoch(config.Tenant(), config.RevocationEpoch()); err != nil {
		return nil, errInvalidConfig
	}
	policyDigest := broker.Digest{Algorithm: "sha256", Hex: config.PolicyDigest()}
	for _, issued := range config.IssuedGrants() {
		grant := broker.Grant{
			ID: issued.ID, IDNamespace: "grant", Principal: principal, Tenant: tenant,
			PolicyDigest: policyDigest, Actions: []string{issued.Action},
			Resources: []broker.Identifier{{Namespace: issued.Evidence.Namespace, Value: issued.Evidence.Value}},
			Limits:    issued.Limits, Fence: issued.Fence, RevocationEpoch: issued.RevocationEpoch,
			ExpiresAt: issued.ExpiresAt, Nonce: issued.Nonce,
		}
		if err := policy.RegisterGrant(grant, now); err != nil {
			return nil, errInvalidConfig
		}
	}
	return policy, nil
}

func darwinConfig(config *localbootstrap.Config) brain.DarwinConfig {
	return brain.DarwinConfig{
		Durable: brain.DurableConfig{
			DatabasePath: config.DatabasePath(), ObjectRoot: config.ObjectRoot(),
			Tenant: brain.Identifier{Namespace: "tenant", Value: config.Tenant()},
			CurrentKeyReference: brain.KeyReference{
				Root:  brain.Identifier{Namespace: "key-root", Value: config.Tenant()},
				KeyID: brain.Identifier{Namespace: "key", Value: config.KeyReference()},
				Epoch: config.KeyEpoch(),
			},
			Brain:               brain.Identifier{Namespace: "brain", Value: config.Brain()},
			ConfigurationDigest: brain.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()},
			Clock:               systemClock{},
			Storage:             brain.StorageOptions{FrameBytes: config.FrameBytes(), MaxReadBytes: config.MaxReadBytes()},
			Ingestion: &brain.IngestionConfig{
				ApprovedRoot: config.ApprovedSourceRoot(), GitExecutable: "/usr/bin/git", RepositoryID: bootstrapRepositoryID(config),
				CommandTimeout: 5 * time.Second, MaxFiles: 10_000, MaxPathBytes: 4 * 1024,
				MaxFileBytes: 1 << 20, MaxTotalBytes: 16 << 20, MaxIdempotencyRecords: 256,
			},
		},
		KeychainService: config.KeychainService(),
	}
}

func bootstrapRepositoryID(config *localbootstrap.Config) string {
	if config == nil {
		return ""
	}
	digest := sha256.Sum256([]byte("repository\x00" + config.ConfigurationDigest() + "\x00" + config.ApprovedSourceRoot()))
	return hex.EncodeToString(digest[:])
}

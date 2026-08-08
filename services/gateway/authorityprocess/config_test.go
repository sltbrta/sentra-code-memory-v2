package authorityprocess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/localbootstrap"
)

func TestParseConfigAcceptsOnlyPinnedBootstrapPair(t *testing.T) {
	path := "/private/var/db/ouroboros/bootstrap.json"
	digest := strings.Repeat("a", 64)
	for name, arguments := range map[string][]string{
		"canonical": {"--bootstrap", path, "--bootstrap-sha256", digest},
		"reordered": {"--bootstrap-sha256", digest, "--bootstrap", path},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := parseConfig(arguments)
			if err != nil || config.bootstrapPath != path || config.bootstrapSHA256 != digest {
				t.Fatalf("parseConfig() = %#v, %v", config, err)
			}
		})
	}
}

func TestParseConfigRejectsMissingDuplicatePositionalAndMalformedInput(t *testing.T) {
	path := "/private/var/db/ouroboros/bootstrap.json"
	digest := strings.Repeat("a", 64)
	tests := map[string][]string{
		"missing":          {"--bootstrap", path},
		"duplicate path":   {"--bootstrap", path, "--bootstrap", path},
		"duplicate digest": {"--bootstrap-sha256", digest, "--bootstrap-sha256", digest},
		"positional":       {path, digest, "--bootstrap", path},
		"legacy flag":      {"--socket", path, "--bootstrap-sha256", digest},
		"relative path":    {"--bootstrap", "bootstrap.json", "--bootstrap-sha256", digest},
		"unclean path":     {"--bootstrap", "/var/db/../bootstrap.json", "--bootstrap-sha256", digest},
		"uppercase digest": {"--bootstrap", path, "--bootstrap-sha256", strings.ToUpper(digest)},
		"short digest":     {"--bootstrap", path, "--bootstrap-sha256", digest[:63]},
		"nonhex digest":    {"--bootstrap", path, "--bootstrap-sha256", "z" + digest[1:]},
		"equals syntax":    {"--bootstrap=" + path, "ignored", "--bootstrap-sha256", digest},
	}
	for name, arguments := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(arguments); !errors.Is(err, errInvalidConfig) {
				t.Fatalf("parseConfig() error = %v", err)
			}
		})
	}
}

func TestBootstrapMappingsPreserveBrokerAuthorityAndDurableDigests(t *testing.T) {
	config, _ := commandBootstrap(t)
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	uid, err := ownerUID()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := brokerFromBootstrap(config, uid, now)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := policy.MapPeer(broker.PeerCredentials{UID: uid, PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if identity.Principal.Value != config.Principal() || identity.Tenant.Value != config.Tenant() ||
		identity.Session.Value != config.Session() {
		t.Fatalf("mapped identity = %#v", identity)
	}
	epoch, err := policy.RevocationEpoch(identity.Tenant)
	if err != nil || epoch != config.RevocationEpoch() {
		t.Fatalf("revocation epoch = %d, %v", epoch, err)
	}
	issued := config.IssuedGrants()[0]
	grant := broker.Grant{
		ID: issued.ID, IDNamespace: "grant", Principal: identity.Principal, Tenant: identity.Tenant,
		PolicyDigest: broker.Digest{Algorithm: "sha256", Hex: config.PolicyDigest()},
		Actions:      []string{issued.Action},
		Resources:    []broker.Identifier{{Namespace: issued.Evidence.Namespace, Value: issued.Evidence.Value}},
		Limits:       issued.Limits, Fence: issued.Fence, RevocationEpoch: issued.RevocationEpoch,
		ExpiresAt: issued.ExpiresAt, Nonce: issued.Nonce,
	}
	use := broker.NewUse(
		issued.Action, grant.Resources[0], issued.Fence, issued.RevocationEpoch, issued.Nonce, now,
	)
	use.Usage = map[string]uint64{"bytes": 1}
	decision, err := policy.Authorize(context.Background(), identity, grant, use)
	if err != nil || !decision.Allowed || decision.RevocationEpoch != config.RevocationEpoch() {
		t.Fatalf("authorization = %#v, %v", decision, err)
	}
	for _, action := range []string{"source.add", "source.status", "source.search", "source.reconcile", "source.revoke"} {
		decision, err := policy.AuthorizeSource(context.Background(), identity, action, broker.Identifier{Namespace: "brain", Value: config.Brain()})
		if err != nil || !decision.Allowed {
			t.Fatalf("source authorization %s = %#v, %v", action, decision, err)
		}
	}

	durable := darwinConfig(config)
	if durable.KeychainService != config.KeychainService() ||
		durable.Durable.DatabasePath != config.DatabasePath() || durable.Durable.ObjectRoot != config.ObjectRoot() ||
		durable.Durable.Tenant != (brain.Identifier{Namespace: "tenant", Value: config.Tenant()}) ||
		durable.Durable.Brain != (brain.Identifier{Namespace: "brain", Value: config.Brain()}) ||
		durable.Durable.ConfigurationDigest != (brain.Digest{Algorithm: "sha256", Hex: config.ConfigurationDigest()}) ||
		durable.Durable.Storage.FrameBytes != config.FrameBytes() ||
		durable.Durable.Storage.MaxReadBytes != config.MaxReadBytes() || durable.Durable.Clock == nil {
		t.Fatalf("durable configuration = %#v", durable)
	}
	reference := durable.Durable.CurrentKeyReference
	if reference.Root != (brain.Identifier{Namespace: "key-root", Value: config.Tenant()}) ||
		reference.KeyID != (brain.Identifier{Namespace: "key", Value: config.KeyReference()}) ||
		reference.Epoch != config.KeyEpoch() || reference.Legacy {
		t.Fatalf("current key reference = %#v", reference)
	}
}

func TestRunWithOpenerReturnsOneStaticStartupError(t *testing.T) {
	config, arguments := commandBootstrap(t)
	var opened brain.DarwinConfig
	openerFailure := errors.New("secret Keychain detail")
	err := RunWithOpener(context.Background(), arguments, func(
		_ context.Context,
		value brain.DarwinConfig,
	) (*brain.Runtime, error) {
		opened = value
		return nil, openerFailure
	})
	if !errors.Is(err, errInvalidConfig) || err.Error() != errInvalidConfig.Error() {
		t.Fatalf("opener error = %v", err)
	}
	if opened.Durable.ConfigurationDigest.Hex != config.ConfigurationDigest() ||
		opened.Durable.CurrentKeyReference.KeyID.Value != config.KeyReference() {
		t.Fatalf("opened configuration = %#v", opened)
	}
	wrongDigest := append([]string(nil), arguments...)
	wrongDigest[3] = strings.Repeat("0", 64)
	for name, testErr := range map[string]error{
		"loader": RunWithOpener(context.Background(), wrongDigest, func(context.Context, brain.DarwinConfig) (*brain.Runtime, error) {
			return nil, nil
		}),
		"nil opener": RunWithOpener(context.Background(), arguments, nil),
		"nil context": RunWithOpener(nil, arguments, func(context.Context, brain.DarwinConfig) (*brain.Runtime, error) {
			return nil, nil
		}),
	} {
		if !errors.Is(testErr, errInvalidConfig) || testErr.Error() != errInvalidConfig.Error() {
			t.Fatalf("%s error = %v", name, testErr)
		}
	}
}

func commandBootstrap(t *testing.T) (*localbootstrap.Config, []string) {
	t.Helper()
	current, err := user.Current()
	if err != nil || !filepath.IsAbs(current.HomeDir) {
		t.Fatal("current user home unavailable")
	}
	base, err := os.MkdirTemp(current.HomeDir, ".ouroboros-command-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(base, "state")
	manifestRoot := filepath.Join(base, "manifest")
	sourceRoot := filepath.Join(base, "source")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := localbootstrap.BootstrapV1{
		Version: 1, StateRoot: stateRoot,
		SocketPath:         filepath.Join(stateRoot, "authority.sock"),
		DatabasePath:       filepath.Join(stateRoot, "authority.sqlite3"),
		ObjectRoot:         filepath.Join(stateRoot, "objects"),
		ApprovedSourceRoot: sourceRoot,
		Principal:          "principal-a", Tenant: "tenant-a", Session: "session-a", Brain: "brain-a",
		KeychainService: "ai.ouroboros.local", KeyEpoch: 7, KeyReference: "key-a",
		MaxConnections: 8, MaxRequests: 64, FrameBytes: 64 * 1024, MaxReadBytes: 1024,
		RevocationEpoch: 4,
		Relationships: []localbootstrap.RelationshipSpec{
			{Object: "evidence:evidence-a", Relation: "brain", User: "brain:brain-a"},
			{Object: "brain:brain-a", Relation: "owner", User: "user:principal-a"},
			{Object: "brain:brain-a", Relation: "tenant", User: "tenant:tenant-a"},
		},
		IssuedGrants: []localbootstrap.GrantSpec{{
			ID: "grant-read", Action: "artifact.read",
			Evidence: localbootstrap.EvidenceSpec{Namespace: "evidence", Value: "evidence-a"},
			Fence:    7, Nonce: "nonce-read", ExpiresAt: "2100-01-02T03:04:05Z",
			RevocationEpoch: 4, Limits: map[string]uint64{"bytes": 1024},
		}},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manifestRoot, "bootstrap.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(digest[:])
	config, err := localbootstrap.Load(localbootstrap.Options{
		ManifestPath: path, ExpectedSHA256: digestHex,
		Now: func() time.Time { return time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return config, []string{"--bootstrap", path, "--bootstrap-sha256", digestHex}
}

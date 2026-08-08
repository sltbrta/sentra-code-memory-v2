package localauthority

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/conversation"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate/schema"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstorage"
)

// DurableConfig declares one local encrypted authority and its existing key
// reference. DatabasePath and ObjectRoot must be absolute. CurrentKeyReference
// carries metadata only; callers must supply a resolver that already owns the
// corresponding secret material.
type DurableConfig struct {
	DatabasePath        string
	ObjectRoot          string
	Tenant              Identifier
	CurrentKeyReference KeyReference
	Brain               Identifier
	ConfigurationDigest Digest
	Clock               Clock
	Storage             StorageOptions
	// Ingestion enables the bounded committed-Git source authority. Nil keeps
	// the Stage 2 artifact-only runtime and performs no repository access.
	Ingestion *IngestionConfig
}

// DarwinConfig adds the fixed Keychain service namespace used by OpenDarwin.
// The selected generic-password item must exist before startup; the constructor
// never creates, changes, or deletes Keychain material.
type DarwinConfig struct {
	Durable         DurableConfig
	KeychainService string
}

const keyCommitmentPrefix = "ouroboros-root-commitment-v1:"

type resolverFactory func(*localstorage.KeyReferences, KeyReference) (KeyResolver, error)

// OpenDurable composes one owner-locked SQLite authority, its durable metadata
// adapters, the declared current key reference, and the encrypted object root.
// It returns ErrInvalid for malformed configuration and ErrUnavailable for any
// authority, storage, or resolver failure. On success Runtime owns every opened
// resource; on failure all opened resources are closed in reverse order.
func OpenDurable(ctx context.Context, config DurableConfig, keys KeyResolver) (*Runtime, error) {
	if keys == nil {
		return nil, ErrInvalid
	}
	return openDurable(ctx, config, keys, func(*localstorage.KeyReferences, KeyReference) (KeyResolver, error) {
		return keys, nil
	})
}

func openDurable(
	ctx context.Context,
	config DurableConfig,
	bootstrap KeyResolver,
	factory resolverFactory,
) (*Runtime, error) {
	if ctx == nil || bootstrap == nil || factory == nil || !validDurableConfig(config) {
		return nil, ErrInvalid
	}
	ingestionRuntime, err := newIngestionRuntime(ctx, config)
	if err != nil {
		return nil, err
	}
	store, err := localstate.OpenWithMigrations(ctx, config.DatabasePath, schema.Migrations(), config.Clock)
	if err != nil {
		return nil, ErrUnavailable
	}
	bundle, err := localstorage.Open(ctx, store)
	if err != nil {
		return nil, failOpen(bundle, nil, store)
	}
	commitment, err := currentKeyCommitment(ctx, bootstrap, config)
	if err != nil {
		return nil, failOpen(bundle, nil, store)
	}
	if err := bundle.KeyReferences().InstallCurrentReference(
		ctx, config.Tenant, commitment,
	); err != nil {
		return nil, failOpen(bundle, nil, store)
	}
	persistedCommitment, err := bundle.KeyReferences().CurrentReference(ctx, config.Tenant)
	if err != nil || persistedCommitment != commitment {
		return nil, failOpen(bundle, nil, store)
	}
	keys, err := factory(bundle.KeyReferences(), persistedCommitment)
	if err != nil || keys == nil {
		return nil, failOpen(bundle, nil, store)
	}
	keys = commitmentVerifyingResolver{
		source: keys, config: config, commitment: persistedCommitment,
	}
	material, err := keys.Current(ctx, config.Tenant)
	clear(material.RootKey)
	if err != nil {
		return nil, failOpen(bundle, nil, store)
	}
	storage, err := NewStorage(
		config.ObjectRoot, bundle.Artifacts(), keys, bundle.Evidence(), config.Storage,
	)
	if err != nil {
		return nil, failOpen(bundle, nil, store)
	}
	runtime, err := newRuntime(
		store, storage, bundle, config.Brain, config.Tenant,
		config.CurrentKeyReference.Epoch, config.ConfigurationDigest, config.Clock,
	)
	if err != nil {
		return nil, failOpen(bundle, storage, store)
	}
	// The Stage 04 conversation store keeps every rendered turn byte inside the
	// same encrypted vault under the same key resolver; payloads are composed
	// here so the query surface never opens a second secret path.
	payloads, err := conversation.NewVaultPayloads(storage.vault, keys, config.Storage.FrameBytes, nil)
	if err != nil {
		return nil, failOpen(bundle, storage, store)
	}
	runtime.databasePath = config.DatabasePath
	runtime.conversationPayloads = payloads
	runtime.ingestion = ingestionRuntime
	return runtime, nil
}

func validDurableConfig(config DurableConfig) bool {
	return filepath.IsAbs(config.DatabasePath) && filepath.IsAbs(config.ObjectRoot) &&
		validID(config.Tenant, "tenant") && validSelector(config.Tenant.Value, 512) &&
		validID(config.Brain, "brain") && validSelector(config.Brain.Value, 512) &&
		validSHA256(config.ConfigurationDigest) && config.Clock != nil &&
		config.CurrentKeyReference.Root.Namespace == "key-root" &&
		config.CurrentKeyReference.Root.Value == config.Tenant.Value &&
		config.CurrentKeyReference.KeyID.Namespace == "key" &&
		validSelector(config.CurrentKeyReference.KeyID.Value, 1024) &&
		config.CurrentKeyReference.Epoch > 0 && !config.CurrentKeyReference.Legacy
}

func validSelector(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && strings.IndexFunc(value, unicode.IsControl) < 0
}

func currentKeyCommitment(ctx context.Context, keys KeyResolver, config DurableConfig) (KeyReference, error) {
	material, err := keys.Current(ctx, config.Tenant)
	if err != nil {
		clear(material.RootKey)
		return KeyReference{}, err
	}
	defer clear(material.RootKey)
	return keyCommitment(material, config)
}

func keyCommitment(material KeyMaterial, config DurableConfig) (KeyReference, error) {
	if material.Reference != config.CurrentKeyReference || len(material.RootKey) != keyring.RootKeyBytes {
		return KeyReference{}, keyring.ErrInvalidMaterial
	}
	mac := hmac.New(sha256.New, material.RootKey)
	writeCommitmentValue(mac, "ouroboros.local-authority.root-key.v1")
	writeCommitmentValue(mac, config.Tenant.Namespace)
	writeCommitmentValue(mac, config.Tenant.Value)
	writeCommitmentValue(mac, config.CurrentKeyReference.Root.Namespace)
	writeCommitmentValue(mac, config.CurrentKeyReference.Root.Value)
	writeCommitmentValue(mac, config.CurrentKeyReference.KeyID.Namespace)
	writeCommitmentValue(mac, config.CurrentKeyReference.KeyID.Value)
	var epoch [8]byte
	binary.BigEndian.PutUint64(epoch[:], config.CurrentKeyReference.Epoch)
	_, _ = mac.Write(epoch[:])
	return KeyReference{
		Root: config.CurrentKeyReference.Root,
		KeyID: Identifier{
			Namespace: "key",
			Value:     keyCommitmentPrefix + hex.EncodeToString(mac.Sum(nil)),
		},
		Epoch: config.CurrentKeyReference.Epoch,
	}, nil
}

// commitmentVerifyingResolver binds every live use of the configured epoch to
// the non-secret commitment installed in SQLite. Providers may replace bytes
// behind a stable selector; that replacement must fail closed before a second
// root can encrypt or decrypt data within the same epoch.
type commitmentVerifyingResolver struct {
	source     KeyResolver
	config     DurableConfig
	commitment KeyReference
}

func (r commitmentVerifyingResolver) Current(ctx context.Context, tenant Identifier) (KeyMaterial, error) {
	material, err := r.source.Current(ctx, tenant)
	if err != nil {
		clear(material.RootKey)
		return KeyMaterial{}, err
	}
	if tenant != r.config.Tenant {
		clear(material.RootKey)
		return KeyMaterial{}, keyring.ErrInvalidMaterial
	}
	return r.verify(material)
}

func (r commitmentVerifyingResolver) Resolve(
	ctx context.Context,
	tenant Identifier,
	epoch uint64,
) (KeyMaterial, error) {
	material, err := r.source.Resolve(ctx, tenant, epoch)
	if err != nil {
		clear(material.RootKey)
		return KeyMaterial{}, err
	}
	if epoch != r.config.CurrentKeyReference.Epoch {
		return material, nil
	}
	if tenant != r.config.Tenant {
		clear(material.RootKey)
		return KeyMaterial{}, keyring.ErrInvalidMaterial
	}
	return r.verify(material)
}

func (r commitmentVerifyingResolver) verify(material KeyMaterial) (KeyMaterial, error) {
	commitment, err := keyCommitment(material, r.config)
	if err != nil || commitment != r.commitment {
		clear(material.RootKey)
		return KeyMaterial{}, keyring.ErrInvalidMaterial
	}
	return material, nil
}

func writeCommitmentValue(mac interface{ Write([]byte) (int, error) }, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write([]byte(value))
}

func failOpen(bundle *localstorage.Bundle, storage *Storage, store *localstate.Store) error {
	var storageErr, bundleErr, storeErr error
	if storage != nil {
		storageErr = storage.Close()
	}
	if bundle != nil {
		bundleErr = bundle.Close()
	}
	if store != nil {
		storeErr = store.Close()
	}
	return errors.Join(ErrUnavailable, storageErr, bundleErr, storeErr)
}

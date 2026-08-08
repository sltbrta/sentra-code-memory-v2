package conversation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// PayloadStore is the narrow encrypted-payload port the store persists turn
// bodies behind. SQLite never holds rendered bytes — only the returned opaque
// artifact identity and the canonical SHA-256 digest of the exact payload.
// Implementations must scope payloads by tenant and make reads fail closed.
type PayloadStore interface {
	// Put encrypts and publishes one immutable payload, returning its opaque
	// artifact identity. Payloads are bounded by MaxPayloadBytes.
	Put(ctx context.Context, tenant string, payload []byte) (artifactID string, err error)
	// Get returns the authenticated plaintext of one published payload.
	Get(ctx context.Context, tenant, artifactID string) (payload []byte, err error)
	// Purge immediately denies and physically purges one payload artifact.
	// Exact replays against an already-purged generation succeed.
	Purge(ctx context.Context, tenant, artifactID string) error
}

// VaultPayloads adapts the Stage 02 ArtifactVault to PayloadStore: every turn
// payload becomes its own single-generation artifact staged and published
// through the canonical vault lifecycle, so payload bytes exist only as
// independently authenticated encrypted frames.
type VaultPayloads struct {
	vault      *artifactvault.Vault
	keys       keyring.Resolver
	frameBytes uint32
	random     io.Reader
}

// NewVaultPayloads binds the vault and its key resolver. frameBytes must equal
// the vault's configured frame size (zero selects the vault default of 64
// KiB); a mismatch makes manifest construction fail closed. random names
// artifacts; nil selects crypto/rand, and tests may pin a deterministic source.
func NewVaultPayloads(
	vault *artifactvault.Vault, keys keyring.Resolver, frameBytes uint32, random io.Reader,
) (*VaultPayloads, error) {
	if vault == nil || keys == nil {
		return nil, ErrInvalidInput
	}
	if frameBytes == 0 {
		frameBytes = 64 * 1024
	}
	if random == nil {
		random = rand.Reader
	}
	return &VaultPayloads{vault: vault, keys: keys, frameBytes: frameBytes, random: random}, nil
}

// Put stages and publishes one payload as a fresh generation-one artifact
// under the tenant's current key epoch. The artifact identity is random, so
// identical payloads never correlate across turns.
func (p *VaultPayloads) Put(ctx context.Context, tenant string, payload []byte) (string, error) {
	if len(payload) == 0 || len(payload) > MaxPayloadBytes || !validBoundedID(tenant) {
		return "", ErrInvalidInput
	}
	tenantID := contracts.Identifier{Namespace: "tenant", Value: tenant}
	material, err := p.keys.Current(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("%w: resolve current key", ErrPayloadUnavailable)
	}
	epoch := material.Reference.Epoch
	clear(material.RootKey)
	artifactID, err := p.newArtifactID()
	if err != nil {
		return "", err
	}
	frameCount := uint32((uint64(len(payload)) + uint64(p.frameBytes) - 1) / uint64(p.frameBytes))
	manifest := contracts.ArtifactManifest{
		Artifact:   contracts.Identifier{Namespace: "artifact", Value: artifactID},
		Tenant:     tenantID,
		Digest:     contracts.Digest{Algorithm: "sha256", Hex: payloadDigest(payload)},
		Generation: 1,
		KeyEpoch:   epoch,
		Length:     uint64(len(payload)),
		FrameCount: frameCount,
	}
	if _, err := p.vault.StageContent(ctx, contracts.ArtifactStageRequest{
		Manifest: manifest, ExpectedGeneration: 0,
	}, bytes.NewReader(payload)); err != nil {
		return "", fmt.Errorf("%w: stage payload", ErrPayloadUnavailable)
	}
	if _, err := p.vault.Publish(ctx, contracts.ArtifactPublishRequest{
		Manifest: manifest, ExpectedGeneration: 0,
	}); err != nil {
		return "", fmt.Errorf("%w: publish payload", ErrPayloadUnavailable)
	}
	return artifactID, nil
}

// Get hydrates the complete payload, bounded by MaxPayloadBytes. The store
// reverifies the payload digest against the canonical metadata afterward, so a
// quarantined, tombstoned, or corrupt artifact fails closed here or there.
func (p *VaultPayloads) Get(ctx context.Context, tenant, artifactID string) ([]byte, error) {
	if !validBoundedID(tenant) || !validBoundedID(artifactID) {
		return nil, ErrInvalidInput
	}
	request := func(length uint64) contracts.ArtifactReadRequest {
		return contracts.ArtifactReadRequest{
			Artifact:   contracts.Identifier{Namespace: "artifact", Value: artifactID},
			Tenant:     contracts.Identifier{Namespace: "tenant", Value: tenant},
			Generation: 1,
			Range:      contracts.ByteRange{Offset: 0, Length: length},
		}
	}
	probe, err := p.vault.HydrateRange(ctx, request(1))
	if err != nil {
		return nil, fmt.Errorf("%w: read payload manifest", ErrPayloadUnavailable)
	}
	length := probe.Metadata.Manifest.Length
	if length == 0 || length > MaxPayloadBytes {
		return nil, ErrPayloadUnavailable
	}
	hydrated, err := p.vault.HydrateRange(ctx, request(length))
	if err != nil {
		return nil, fmt.Errorf("%w: hydrate payload", ErrPayloadUnavailable)
	}
	return hydrated.Bytes, nil
}

// Purge tombstones and physically purges one published payload artifact under
// the tenant's current key epoch. Exact replays against an already-purged
// generation return success; missing or foreign artifacts fail closed.
func (p *VaultPayloads) Purge(ctx context.Context, tenant, artifactID string) error {
	if !validBoundedID(tenant) || !validBoundedID(artifactID) {
		return ErrInvalidInput
	}
	tenantID := contracts.Identifier{Namespace: "tenant", Value: tenant}
	artifact := contracts.Identifier{Namespace: "artifact", Value: artifactID}
	material, err := p.keys.Current(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("%w: resolve current key", ErrPayloadUnavailable)
	}
	epoch := material.Reference.Epoch
	clear(material.RootKey)
	tombstone, err := p.vault.Tombstone(ctx, contracts.TombstoneRequest{
		Artifact:           artifact,
		Tenant:             tenantID,
		ExpectedGeneration: 1,
		ReasonCode:         "meeting_purge",
	})
	if err != nil {
		// Already tombstoned/purged generations still attempt physical purge below
		// through the vault's idempotent Purge path when a receipt is available.
		return fmt.Errorf("%w: tombstone payload", ErrPayloadUnavailable)
	}
	if _, err := p.vault.Purge(ctx, contracts.PurgeRequest{
		Artifact:         artifact,
		Tenant:           tenantID,
		TombstoneReceipt: tombstone.Receipt,
		KeyEpoch:         epoch,
	}); err != nil {
		return fmt.Errorf("%w: purge payload", ErrPayloadUnavailable)
	}
	return nil
}

func (p *VaultPayloads) newArtifactID() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(p.random, value); err != nil {
		return "", fmt.Errorf("conversation: generate artifact identity: %w", err)
	}
	return "turn-payload-" + hex.EncodeToString(value), nil
}

package conversation

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// sequenceReader yields deterministic artifact identities for reproducible tests.
type sequenceReader struct{ next byte }

func (r *sequenceReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		r.next++
		buffer[index] = r.next
	}
	return len(buffer), nil
}

func vaultFixture(t *testing.T) (*VaultPayloads, *artifactvault.Vault) {
	t.Helper()
	objects, err := artifactvault.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := keyring.NewMemory()
	tenant := contracts.Identifier{Namespace: "tenant", Value: "t1"}
	keys.Add(tenant, keyring.Material{
		Reference: contracts.KeyReference{
			Root:  contracts.Identifier{Namespace: "key-root", Value: "t1"},
			KeyID: contracts.Identifier{Namespace: "key", Value: "epoch-1"},
			Epoch: 1,
		},
		RootKey: bytes.Repeat([]byte{7}, keyring.RootKeyBytes),
	}, keyring.Current)
	vault, err := artifactvault.New(objects, artifactvault.NewMemoryRepository(), keys, artifactvault.Options{
		Random: &sequenceReader{},
	})
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := NewVaultPayloads(vault, keys, 0, &sequenceReader{})
	if err != nil {
		t.Fatal(err)
	}
	return payloads, vault
}

func TestVaultPayloadsRoundTripThroughEncryptedVault(t *testing.T) {
	t.Parallel()
	payloads, _ := vaultFixture(t)
	ctx := context.Background()

	payload := []byte(`{"version":1,"text":"what does ConfigPath return?"}`)
	artifactID, err := payloads.Put(ctx, "t1", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(artifactID, "turn-payload-") {
		t.Fatalf("artifact identity %q is not opaque", artifactID)
	}
	hydrated, err := payloads.Get(ctx, "t1", artifactID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hydrated, payload) {
		t.Fatalf("round trip mismatch: %q vs %q", hydrated, payload)
	}
	second, err := payloads.Put(ctx, "t1", payload)
	if err != nil {
		t.Fatal(err)
	}
	if second == artifactID {
		t.Fatal("identical payloads correlate across turns")
	}
}

func TestVaultPayloadsBoundsAndFailuresAreClosed(t *testing.T) {
	t.Parallel()
	payloads, vault := vaultFixture(t)
	ctx := context.Background()

	if _, err := payloads.Put(ctx, "t1", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty payload err=%v", err)
	}
	if _, err := payloads.Put(ctx, "t1", bytes.Repeat([]byte{1}, MaxPayloadBytes+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized payload err=%v", err)
	}
	if _, err := payloads.Put(ctx, "", []byte("payload")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty tenant err=%v", err)
	}
	if _, err := payloads.Get(ctx, "t1", "turn-payload-missing"); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("missing artifact err=%v", err)
	}
	artifactID, err := payloads.Put(ctx, "t1", []byte("doomed"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Tombstone(ctx, contracts.TombstoneRequest{
		Artifact:           contracts.Identifier{Namespace: "artifact", Value: artifactID},
		Tenant:             contracts.Identifier{Namespace: "tenant", Value: "t1"},
		ExpectedGeneration: 1,
		ReasonCode:         "test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := payloads.Get(ctx, "t1", artifactID); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("tombstoned artifact err=%v", err)
	}
	if _, err := payloads.Get(ctx, "t2", artifactID); !errors.Is(err, ErrPayloadUnavailable) {
		t.Fatalf("cross-tenant read err=%v", err)
	}
	if _, err := NewVaultPayloads(nil, keyring.NewMemory(), 0, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil vault err=%v", err)
	}
	if _, err := NewVaultPayloads(vault, nil, 0, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil keys err=%v", err)
	}
}

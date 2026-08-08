package keyring_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestMemoryResolvesCurrentHistoricalLegacyAndUnreadableEpochs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ring := keyring.NewMemory()
	tenant := id("tenant", "tenant-a")
	ring.Add(tenant, keyring.Material{Reference: reference(tenant, "epoch-1", 1, false), RootKey: repeated(1)}, keyring.Historical)
	ring.Add(tenant, keyring.Material{Reference: reference(tenant, "epoch-2", 2, false), RootKey: repeated(2)}, keyring.Current)
	ring.Add(tenant, keyring.Material{Reference: reference(tenant, "legacy", 0, true), RootKey: repeated(3)}, keyring.Legacy)
	ring.Add(tenant, keyring.Material{Reference: reference(tenant, "lost", 3, false)}, keyring.Unreadable)

	current, err := ring.Current(ctx, tenant)
	if err != nil || current.Reference.Epoch != 2 {
		t.Fatalf("Current() = (%+v, %v), want epoch 2", current, err)
	}
	for _, epoch := range []uint64{0, 1, 2} {
		material, resolveErr := ring.Resolve(ctx, tenant, epoch)
		if resolveErr != nil || material.Reference.Epoch != epoch || len(material.RootKey) != keyring.RootKeyBytes {
			t.Fatalf("Resolve(%d) = (%+v, %v)", epoch, material, resolveErr)
		}
	}
	_, err = ring.Resolve(ctx, tenant, 3)
	if !errors.Is(err, keyring.ErrUnreadable) {
		t.Fatalf("Resolve(unreadable) error = %v, want ErrUnreadable", err)
	}
	_, err = ring.Resolve(ctx, id("tenant", "tenant-b"), 2)
	if !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("cross-tenant Resolve error = %v, want ErrNotFound", err)
	}
}

func TestMemoryCopiesKeyMaterialAtEveryBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ring := keyring.NewMemory()
	tenant := id("tenant", "tenant-a")
	root := repeated(9)
	ring.Add(tenant, keyring.Material{Reference: reference(tenant, "epoch-1", 1, false), RootKey: root}, keyring.Current)
	root[0] = 0

	first, err := ring.Resolve(ctx, tenant, 1)
	if err != nil {
		t.Fatal(err)
	}
	first.RootKey[0] = 0
	second, err := ring.Resolve(ctx, tenant, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.RootKey, second.RootKey) || second.RootKey[0] != 9 {
		t.Fatal("caller mutation escaped the keyring boundary")
	}
}

func id(namespace, value string) contracts.Identifier {
	return contracts.Identifier{Namespace: namespace, Value: value}
}

func reference(tenant contracts.Identifier, keyID string, epoch uint64, legacy bool) contracts.KeyReference {
	return contracts.KeyReference{
		Root:   id("key-root", tenant.Value),
		KeyID:  id("key", keyID),
		Epoch:  epoch,
		Legacy: legacy,
	}
}

func repeated(value byte) []byte {
	return bytes.Repeat([]byte{value}, keyring.RootKeyBytes)
}

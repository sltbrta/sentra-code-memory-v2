package localstorage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestInstallCurrentKeyReferenceIsExactDurableAndTenantScoped(t *testing.T) {
	ctx := context.Background()
	path := migratedPath(t)
	bundle := openTestBundle(t, path)
	tenant := identifier("tenant", "t1")
	reference := currentKeyReference("t1", "key-1", 1)
	if err := bundle.KeyReferences().InstallCurrentReference(ctx, tenant, reference); err != nil {
		t.Fatal(err)
	}
	if err := bundle.KeyReferences().InstallCurrentReference(ctx, tenant, reference); err != nil {
		t.Fatalf("exact retry = %v", err)
	}
	loaded, err := bundle.KeyReferences().CurrentReference(ctx, tenant)
	if err != nil || loaded != reference {
		t.Fatalf("current = %+v, %v", loaded, err)
	}
	if err := bundle.authority.Close(); err != nil {
		t.Fatal(err)
	}
	bundle = openTestBundle(t, path)
	loaded, err = bundle.KeyReferences().CurrentReference(ctx, tenant)
	if err != nil || loaded != reference {
		t.Fatalf("current after restart = %+v, %v", loaded, err)
	}
}

func TestInstallCurrentKeyReferenceRejectsConflictsAndMalformedScope(t *testing.T) {
	ctx := context.Background()
	bundle := openTestBundle(t, migratedPath(t))
	tenant := identifier("tenant", "t1")
	reference := currentKeyReference("t1", "key-1", 1)
	if err := bundle.KeyReferences().InstallCurrentReference(ctx, tenant, reference); err != nil {
		t.Fatal(err)
	}
	for _, conflict := range []contracts.KeyReference{
		currentKeyReference("t1", "key-2", 1),
		currentKeyReference("t1", "key-1", 2),
	} {
		if err := bundle.KeyReferences().InstallCurrentReference(ctx, tenant, conflict); !errors.Is(err, keyring.ErrKeyConflict) {
			t.Fatalf("conflict %+v = %v", conflict, err)
		}
	}
	for _, malformed := range []struct {
		tenant    contracts.Identifier
		reference contracts.KeyReference
	}{
		{identifier("tenant", "t2"), reference},
		{tenant, currentKeyReference("t2", "key-1", 1)},
		{tenant, currentKeyReference("t1", "bad\nkey", 1)},
		{tenant, currentKeyReference("t1", strings.Repeat("k", 1025), 1)},
		{tenant, currentKeyReference("t1", "key-1", 0)},
		{tenant, contracts.KeyReference{Root: identifier("wrong", "t1"), KeyID: identifier("key", "key-1"), Epoch: 1}},
		{tenant, contracts.KeyReference{Root: identifier("key-root", "t1"), KeyID: identifier("wrong", "key-1"), Epoch: 1}},
	} {
		if err := bundle.KeyReferences().InstallCurrentReference(ctx, malformed.tenant, malformed.reference); !errors.Is(err, keyring.ErrInvalidMaterial) {
			t.Fatalf("malformed %+v = %v", malformed, err)
		}
	}
}

func TestInstallCurrentKeyReferencePropagatesCancellationAndCorruption(t *testing.T) {
	bundle := openTestBundle(t, migratedPath(t))
	tenant := identifier("tenant", "t1")
	reference := currentKeyReference("t1", "key-1", 1)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bundle.KeyReferences().InstallCurrentReference(canceled, tenant, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled install = %v", err)
	}
	if _, err := bundle.KeyReferences().CurrentReference(context.Background(), tenant); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("canceled install wrote metadata: %v", err)
	}
	closed := openTestBundle(t, migratedPath(t))
	if err := closed.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.KeyReferences().InstallCurrentReference(context.Background(), tenant, reference); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed authority install = %v", err)
	}

	ctx := context.Background()
	if err := bundle.authority.WriteMetadata(ctx, func(writer localstate.MetadataWriter) error {
		if _, err := writer.ExecContext(ctx, "DROP INDEX one_current_key_epoch_per_tenant"); err != nil {
			return err
		}
		for epoch, keyID := range map[uint64]string{1: "key-1", 2: "key-2"} {
			if _, err := writer.ExecContext(ctx, `INSERT INTO key_epochs
				(tenant_id,key_epoch,key_reference,state) VALUES (?,?,?,'current')`, "t1", epoch, keyID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := bundle.KeyReferences().InstallCurrentReference(ctx, tenant, reference); !errors.Is(err, keyring.ErrKeyConflict) {
		t.Fatalf("multiple current install = %v", err)
	}
}

func currentKeyReference(tenant, keyID string, epoch uint64) contracts.KeyReference {
	return contracts.KeyReference{
		Root: identifier("key-root", tenant), KeyID: identifier("key", keyID), Epoch: epoch,
	}
}

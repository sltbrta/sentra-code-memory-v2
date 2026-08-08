package localstorage

import (
	"context"
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestOpenRequiresOwnedVersionTwoAuthority(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil authority error = %v", err)
	}
	path := migratedPath(t)
	v1, err := localstate.Open(ctx, path, migrationSource(t, "001_stage02_authority.sql"), fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, v1); !errors.Is(err, ErrSchemaUnavailable) {
		t.Fatalf("v1 authority error = %v", err)
	}
	if err := v1.Close(); err != nil {
		t.Fatal(err)
	}
	authority := openTestAuthority(t, path)
	bundle, err := Open(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := localstate.OpenWithMigrations(ctx, path, []localstate.Migration{
		{Version: 1, SQL: migrationSource(t, "001_stage02_authority.sql")},
		{Version: 2, SQL: migrationSource(t, "002_durable_storage_adapters.sql")},
	}, fixedClock{}); !errors.Is(err, localstate.ErrAuthorityOwned) {
		t.Fatalf("second owner error = %v", err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}
	var synchronous int
	if err := authority.ReadMetadata(ctx, func(reader localstate.MetadataReader) error {
		return reader.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous)
	}); err != nil || synchronous != 2 {
		t.Fatalf("authority after bundle close: synchronous=%d err=%v", synchronous, err)
	}
}

func TestMalformedRepositoryInputsFailClosed(t *testing.T) {
	ctx := context.Background()
	bundle := openTestBundle(t, migratedPath(t))
	if _, _, err := bundle.Artifacts().BeginStage(ctx, artifactStageWithBadTenant(), "locator"); !errors.Is(err, artifactvault.ErrInvalid) {
		t.Fatalf("artifact malformed: %v", err)
	}
	if _, err := bundle.Evidence().Put(ctx, evidenceledger.Record{}); !errors.Is(err, evidenceledger.ErrInvalid) {
		t.Fatalf("evidence malformed: %v", err)
	}
	if _, err := bundle.KeyReferences().Reference(ctx, identifier("principal", "p1"), 1); !errors.Is(err, keyring.ErrInvalidMaterial) {
		t.Fatalf("key scope malformed: %v", err)
	}
}

func artifactStageWithBadTenant() contracts.ArtifactStageRequest {
	request := contracts.ArtifactStageRequest{Manifest: manifest("t1", "a1", 1)}
	request.Manifest.Tenant.Namespace = "principal"
	return request
}

package localstorage

import (
	"context"
	"errors"
	"fmt"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
)

var (
	// ErrSchemaUnavailable reports an authority Store without migration version 2.
	ErrSchemaUnavailable = errors.New("localstorage: schema version 2 unavailable")
	// ErrUnavailable reports a closed authority or bounded SQLite operation failure.
	ErrUnavailable = errors.New("localstorage: unavailable")
)

// Bundle binds durable storage ports to the already-open authority Store. It
// owns no database, lock, transaction lifecycle, payload bytes, or key material.
type Bundle struct {
	authority *localstate.Store
	artifacts *ArtifactRepository
	evidence  *EvidenceRepository
	keys      *KeyReferences
}

// Open attaches durable metadata ports to an owner-locked authority Store.
// Migration version 2 must already be applied. Open creates no database handle,
// applies no migrations, and returns ErrSchemaUnavailable or ErrUnavailable on failure.
func Open(ctx context.Context, authority *localstate.Store) (*Bundle, error) {
	if authority == nil {
		return nil, ErrUnavailable
	}
	var applied int
	err := authority.ReadMetadata(ctx, func(reader localstate.MetadataReader) error {
		return reader.QueryRowContext(ctx,
			"SELECT count(*) FROM schema_migrations WHERE version=2").Scan(&applied)
	})
	if err != nil {
		return nil, operationError(ctx, "inspect schema")
	}
	if applied != 1 {
		return nil, ErrSchemaUnavailable
	}
	bundle := &Bundle{authority: authority}
	bundle.artifacts = &ArtifactRepository{authority: authority}
	bundle.evidence = &EvidenceRepository{authority: authority}
	bundle.keys = &KeyReferences{authority: authority}
	return bundle, nil
}

// Artifacts returns the durable ArtifactVault metadata port. Every operation
// runs through the authority Store's serialized metadata executor.
func (b *Bundle) Artifacts() artifactvault.Repository {
	if b == nil {
		return nil
	}
	return b.artifacts
}

// Evidence returns the durable tenant-and-brain-scoped evidence metadata port.
func (b *Bundle) Evidence() evidenceledger.Repository {
	if b == nil {
		return nil
	}
	return b.evidence
}

// KeyReferences returns tenant-scoped metadata for Darwin Keychain composition.
func (b *Bundle) KeyReferences() *KeyReferences {
	if b == nil {
		return nil
	}
	return b.keys
}

// Close releases no resources because Bundle never owns the authority Store.
// It remains for composition symmetry and is intentionally idempotent.
func (b *Bundle) Close() error {
	return nil
}

func operationError(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("localstorage: %s: %w", operation, ErrUnavailable)
}

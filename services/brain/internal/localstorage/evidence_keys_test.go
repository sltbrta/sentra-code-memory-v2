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

func TestEvidenceRepositoryPersistsScopeLineageAndTombstone(t *testing.T) {
	ctx := context.Background()
	path := migratedPath(t)
	bundle := openTestBundle(t, path)
	repository := bundle.Evidence()
	parent := evidenceRecord("t1", "b1", "e1", "a1")
	child := evidenceRecord("t1", "b1", "e2", "a2")
	publishEvidenceArtifact(t, bundle, parent)
	publishEvidenceArtifact(t, bundle, child)
	for _, record := range []evidenceledger.Record{parent, child} {
		created, err := repository.Put(ctx, record)
		if err != nil || !created {
			t.Fatalf("put: created=%v err=%v", created, err)
		}
	}
	created, err := repository.Put(ctx, parent)
	if err != nil || created {
		t.Fatalf("exact retry: created=%v err=%v", created, err)
	}
	changed := parent
	changed.Anchor = "different"
	if _, err := repository.Put(ctx, changed); !errors.Is(err, evidenceledger.ErrConflict) {
		t.Fatalf("changed duplicate: %v", err)
	}
	edge := evidenceledger.Lineage{
		Tenant: parent.Tenant, Brain: parent.Brain,
		Parent: parent.Evidence, Child: child.Evidence, Relation: "derived-from",
	}
	if created, err := repository.PutLineageIfEndpointsReadable(ctx, edge); err != nil || !created {
		t.Fatalf("link: created=%v err=%v", created, err)
	}
	if _, err := repository.Get(ctx, identifier("tenant", "t1"), identifier("brain", "other"), parent.Evidence); !errors.Is(err, evidenceledger.ErrNotFound) {
		t.Fatalf("cross-brain read: %v", err)
	}
	if err := repository.Tombstone(ctx, parent.Tenant, parent.Brain, parent.Evidence); err != nil {
		t.Fatal(err)
	}
	if err := bundle.authority.Close(); err != nil {
		t.Fatal(err)
	}
	bundle = openTestBundle(t, path)
	repository = bundle.Evidence()
	if _, err := repository.Get(ctx, parent.Tenant, parent.Brain, parent.Evidence); !errors.Is(err, evidenceledger.ErrNotFound) {
		t.Fatalf("tombstoned read after restart: %v", err)
	}
	edges, err := repository.Lineage(ctx, child.Tenant, child.Brain, child.Evidence)
	if err != nil || len(edges) != 0 {
		t.Fatalf("lineage after tombstone: edges=%v err=%v", edges, err)
	}
}

func TestEvidencePutPropagatesCanceledContextWithoutInsertion(t *testing.T) {
	bundle := openTestBundle(t, migratedPath(t))
	record := evidenceRecord("t1", "b1", "e1", "a1")
	publishEvidenceArtifact(t, bundle, record)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bundle.Evidence().Put(ctx, record); err == nil ||
		errors.Is(err, evidenceledger.ErrNotFound) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Put error = %v", err)
	}
	if _, err := bundle.Evidence().Get(context.Background(), record.Tenant, record.Brain, record.Evidence); !errors.Is(err, evidenceledger.ErrNotFound) {
		t.Fatalf("canceled Put inserted evidence: %v", err)
	}
}

func TestEvidenceTombstonePropagatesCanceledContextWithoutMutation(t *testing.T) {
	bundle := openTestBundle(t, migratedPath(t))
	record := evidenceRecord("t1", "b1", "e1", "a1")
	publishEvidenceArtifact(t, bundle, record)
	if created, err := bundle.Evidence().Put(context.Background(), record); err != nil || !created {
		t.Fatalf("Put: created=%v err=%v", created, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bundle.Evidence().Tombstone(ctx, record.Tenant, record.Brain, record.Evidence); err == nil ||
		errors.Is(err, evidenceledger.ErrNotFound) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Tombstone error = %v", err)
	}
	loaded, err := bundle.Evidence().Get(context.Background(), record.Tenant, record.Brain, record.Evidence)
	if err != nil || loaded != record {
		t.Fatalf("evidence changed after canceled Tombstone: loaded=%+v err=%v", loaded, err)
	}
}

func TestEvidencePutRequiresExactPublishedArtifactDigest(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *Bundle, evidenceledger.Record)
		want  error
	}{
		{name: "missing", want: evidenceledger.ErrNotFound},
		{name: "staged", want: evidenceledger.ErrNotFound, setup: func(t *testing.T, b *Bundle, record evidenceledger.Record) {
			stageEvidenceArtifact(t, b, record)
		}},
		{name: "quarantined", want: evidenceledger.ErrNotFound, setup: func(t *testing.T, b *Bundle, record evidenceledger.Record) {
			stageEvidenceArtifact(t, b, record)
			if err := b.Artifacts().Quarantine(context.Background(), record.Tenant, record.Artifact, record.Generation, "test"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "tombstoned", want: evidenceledger.ErrNotFound, setup: func(t *testing.T, b *Bundle, record evidenceledger.Record) {
			publishEvidenceArtifact(t, b, record)
			if _, err := b.Artifacts().Tombstone(context.Background(), contracts.TombstoneRequest{Tenant: record.Tenant, Artifact: record.Artifact, ExpectedGeneration: record.Generation, ReasonCode: "test"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "purged", want: evidenceledger.ErrNotFound, setup: func(t *testing.T, b *Bundle, record evidenceledger.Record) {
			publishEvidenceArtifact(t, b, record)
			tombstoned, err := b.Artifacts().Tombstone(context.Background(), contracts.TombstoneRequest{Tenant: record.Tenant, Artifact: record.Artifact, ExpectedGeneration: record.Generation, ReasonCode: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Artifacts().CompletePurge(context.Background(), tombstoned); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "digest mismatch", want: evidenceledger.ErrConflict, setup: func(t *testing.T, b *Bundle, record evidenceledger.Record) {
			record.Digest = digest(13)
			publishEvidenceArtifact(t, b, record)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := openTestBundle(t, migratedPath(t))
			record := evidenceRecord("t1", "b1", "e1", "a1")
			if test.setup != nil {
				test.setup(t, bundle, record)
			}
			if _, err := bundle.Evidence().Put(context.Background(), record); !errors.Is(err, test.want) {
				t.Fatalf("Put error=%v want=%v", err, test.want)
			}
		})
	}
	// A same artifact identity published in another tenant is indistinguishable from absent.
	bundle := openTestBundle(t, migratedPath(t))
	other := evidenceRecord("t2", "b1", "e1", "a1")
	published := evidenceRecord("t1", "b1", "source", "a1")
	publishEvidenceArtifact(t, bundle, published)
	if _, err := bundle.Evidence().Put(context.Background(), other); !errors.Is(err, evidenceledger.ErrNotFound) {
		t.Fatalf("cross-tenant Put = %v", err)
	}
}

func stageEvidenceArtifact(t *testing.T, bundle *Bundle, record evidenceledger.Record) artifactvault.GenerationRecord {
	t.Helper()
	m := manifest(record.Tenant.Value, record.Artifact.Value, record.Generation)
	m.Digest = record.Digest
	staged, _, err := bundle.Artifacts().BeginStage(context.Background(), contracts.ArtifactStageRequest{Manifest: m}, "locator-"+record.Artifact.Value)
	if err != nil {
		t.Fatal(err)
	}
	return staged
}

func TestKeyReferencesResolveCurrentHistoricalLegacyAndUnreadable(t *testing.T) {
	ctx := context.Background()
	path := migratedPath(t)
	bundle := openTestBundle(t, path)
	for _, row := range []struct {
		tenant, reference, state string
		epoch                    uint64
	}{
		{"t1", "current-key", "current", 3},
		{"t1", "historical-key", "historical", 2},
		{"t1", "legacy-key", "legacy", 1},
		{"t1", "lost-key", "unreadable", 4},
		{"t2", "other-key", "current", 3},
	} {
		if err := bundle.authority.WriteMetadata(ctx, func(writer localstate.MetadataWriter) error {
			_, err := writer.ExecContext(ctx, "INSERT INTO key_epochs(tenant_id,key_epoch,key_reference,state) VALUES (?,?,?,?)",
				row.tenant, row.epoch, row.reference, row.state)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	keys := bundle.KeyReferences()
	current, err := keys.CurrentReference(ctx, identifier("tenant", "t1"))
	if err != nil || current.Epoch != 3 || current.KeyID.Value != "current-key" || current.Legacy {
		t.Fatalf("current: ref=%+v err=%v", current, err)
	}
	historical, err := keys.Reference(ctx, identifier("tenant", "t1"), 2)
	if err != nil || historical.KeyID.Value != "historical-key" || historical.Legacy {
		t.Fatalf("historical: ref=%+v err=%v", historical, err)
	}
	legacy, err := keys.Reference(ctx, identifier("tenant", "t1"), 1)
	if err != nil || !legacy.Legacy {
		t.Fatalf("legacy: ref=%+v err=%v", legacy, err)
	}
	if _, err := keys.Reference(ctx, identifier("tenant", "t1"), 4); !errors.Is(err, keyring.ErrUnreadable) {
		t.Fatalf("unreadable: %v", err)
	}
	if _, err := keys.Reference(ctx, identifier("tenant", "t2"), 2); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("cross-tenant historical lookup: %v", err)
	}
	if err := bundle.authority.Close(); err != nil {
		t.Fatal(err)
	}
	bundle = openTestBundle(t, path)
	current, err = bundle.KeyReferences().CurrentReference(ctx, identifier("tenant", "t1"))
	if err != nil || current.KeyID.Value != "current-key" {
		t.Fatalf("current after restart: ref=%+v err=%v", current, err)
	}
}

func TestCurrentKeyReferenceRejectsCorruptDuplicateRows(t *testing.T) {
	ctx := context.Background()
	bundle := openTestBundle(t, migratedPath(t))
	if err := bundle.authority.WriteMetadata(ctx, func(writer localstate.MetadataWriter) error {
		if _, err := writer.ExecContext(ctx, "DROP INDEX one_current_key_epoch_per_tenant"); err != nil {
			return err
		}
		for epoch, reference := range map[uint64]string{1: "current-one", 2: "current-two"} {
			if _, err := writer.ExecContext(ctx,
				"INSERT INTO key_epochs(tenant_id,key_epoch,key_reference,state) VALUES (?,?,?,'current')",
				"t1", epoch, reference); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.KeyReferences().CurrentReference(ctx, identifier("tenant", "t1")); !errors.Is(err, keyring.ErrInvalidMaterial) {
		t.Fatalf("duplicate current references error = %v", err)
	}
}

package localstorage

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type fixedClock struct{}

func (fixedClock) NowUnixMilli() int64 { return 1 }

func migratedPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "authority.sqlite")
}

func migrationSource(t *testing.T, name string) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	paths := []string{
		filepath.Join(filepath.Dir(sourceFile), "..", "localstate", "schema", "migrations", name),
		filepath.Join("..", "localstate", "schema", "migrations", name),
	}
	if testRoot := os.Getenv("TEST_SRCDIR"); testRoot != "" {
		paths = append(paths, filepath.Join(testRoot, os.Getenv("TEST_WORKSPACE"), "services", "brain", "internal", "localstate", "schema", "migrations", name))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
	}
	t.Fatalf("Stage 02 migration not found at %v", paths)
	return ""
}

func openTestAuthority(t *testing.T, path string) *localstate.Store {
	t.Helper()
	authority, err := localstate.OpenWithMigrations(context.Background(), path, []localstate.Migration{
		{Version: 1, SQL: migrationSource(t, "001_stage02_authority.sql")},
		{Version: 2, SQL: migrationSource(t, "002_durable_storage_adapters.sql")},
	}, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	return authority
}

func openTestBundle(t *testing.T, path string) *Bundle {
	t.Helper()
	bundle, err := Open(context.Background(), openTestAuthority(t, path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	return bundle
}

func publishEvidenceArtifact(t *testing.T, bundle *Bundle, record evidenceledger.Record) {
	t.Helper()
	m := manifest(record.Tenant.Value, record.Artifact.Value, record.Generation)
	m.Digest = record.Digest
	staged, _, err := bundle.Artifacts().BeginStage(context.Background(), contracts.ArtifactStageRequest{Manifest: m}, "locator-"+record.Artifact.Value)
	if err != nil {
		t.Fatal(err)
	}
	staged.Frames = frames()
	if err := bundle.Artifacts().CompleteStage(context.Background(), staged); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Artifacts().Publish(context.Background(), contracts.ArtifactPublishRequest{Manifest: m}); err != nil {
		t.Fatal(err)
	}
}

func identifier(namespace, value string) contracts.Identifier {
	return contracts.Identifier{Namespace: namespace, Value: value}
}

func digest(value byte) contracts.Digest {
	const hex = "0123456789abcdef"
	encoded := make([]byte, 64)
	for index := range encoded {
		encoded[index] = hex[value%16]
	}
	return contracts.Digest{Algorithm: "sha256", Hex: string(encoded)}
}

func manifest(tenant, artifact string, generation uint64) contracts.ArtifactManifest {
	return contracts.ArtifactManifest{
		Tenant: identifier("tenant", tenant), Artifact: identifier("artifact", artifact),
		Digest: digest(byte(generation)), Generation: generation, KeyEpoch: 1,
		Length: 8, FrameCount: 2,
	}
}

func frames() []artifactvault.FrameRecord {
	return []artifactvault.FrameRecord{
		{Index: 0, Offset: 0, Length: 4, ObjectDigest: digest(10)},
		{Index: 1, Offset: 4, Length: 4, ObjectDigest: digest(11)},
	}
}

func evidenceRecord(tenant, brain, evidence, artifact string) evidenceledger.Record {
	return evidenceledger.Record{
		Tenant: identifier("tenant", tenant), Brain: identifier("brain", brain),
		Evidence: identifier("evidence", evidence), Artifact: identifier("artifact", artifact),
		Generation: 1, Anchor: "artifact:full", Digest: digest(12),
	}
}

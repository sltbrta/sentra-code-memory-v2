package localstate

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadIngestionSourceStateTracksLifecycleAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.db")
	store := openIngestionStore(t, path)
	openTestSession(t, store, "s1")
	first := testPublication("publish-1", "publish-key-1", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := testPublication("publish-2", "publish-key-2", "b", 2, "gen-1", "gen-2")
	if _, err := store.PublishGeneration(ctx, second); err != nil {
		t.Fatal(err)
	}

	state, err := store.LoadIngestionSourceState(ctx, second.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "ready" || state.CurrentGenerationID != "gen-2" || state.RepositoryID != "repo" {
		t.Fatalf("source state = %#v", state)
	}
	facts, err := store.LoadIngestionGenerationFacts(ctx, second.Scope, "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogFacts(t, facts, "gen-1", 1)
	if _, err := store.LoadIngestionGenerationFacts(ctx, second.Scope, "gen-unknown"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown generation facts error = %v", err)
	}
	if _, err := store.LoadIngestionSourceState(ctx, IngestionScope{
		Tenant: second.Scope.Tenant, Brain: second.Scope.Brain, SourceID: digest("9"),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown source state error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openIngestionStore(t, path)
	openTestSession(t, store, "s1")
	state, err = store.LoadIngestionSourceState(ctx, second.Scope)
	if err != nil || state.State != "ready" || state.CurrentGenerationID != "gen-2" {
		t.Fatalf("restarted source state = %#v, %v", state, err)
	}
	superseded, err := store.LoadIngestionGenerationFacts(ctx, second.Scope, "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogFacts(t, superseded, "gen-1", 1)
	current, err := store.LoadIngestionGenerationFacts(ctx, second.Scope, "gen-2")
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogFacts(t, current, "gen-2", 2)

	revocation := IngestionRevocation{
		Command: testIngestionCommand("revoke-1", "revoke-key", "b", IngestionRevokeCommand),
		Scope:   second.Scope, ExpectedCurrentGenerationID: "gen-2",
		RevocationEpoch: 2, ReasonCode: "source_removed",
	}
	revocation.Command.AuthenticatedDigest = IngestionRevocationDigest(revocation)
	if _, err := store.RevokeIngestionSource(ctx, revocation); err != nil {
		t.Fatal(err)
	}
	state, err = store.LoadIngestionSourceState(ctx, second.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "revoked" || state.CurrentGenerationID != "" {
		t.Fatalf("revoked source state = %#v", state)
	}
	// Generation facts stay immutable after revocation so an already admitted
	// query discloses freshness without leaking the revocation through a changed
	// outcome shape.
	revokedFacts, err := store.LoadIngestionGenerationFacts(ctx, second.Scope, "gen-2")
	if err != nil {
		t.Fatal(err)
	}
	assertCatalogFacts(t, revokedFacts, "gen-2", 2)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openIngestionStore(t, path)
	openTestSession(t, store, "s1")
	state, err = store.LoadIngestionSourceState(ctx, second.Scope)
	if err != nil || state.State != "revoked" || state.CurrentGenerationID != "" {
		t.Fatalf("restarted revoked source state = %#v, %v", state, err)
	}
	if _, err := store.LoadIngestionGenerationFacts(ctx, second.Scope, "gen-2"); err != nil {
		t.Fatalf("revoked generation facts must resolve after restart: %v", err)
	}
}

func TestLoadIngestionGenerationFactsRejectsMalformedAndBuildingRows(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	publication := testPublication("publish-1", "publish-key", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, publication); err != nil {
		t.Fatal(err)
	}
	for _, generationID := range []string{"", "  ", "gen-unknown"} {
		if _, err := store.LoadIngestionGenerationFacts(ctx, publication.Scope, generationID); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("generation %q error = %v", generationID, err)
		}
	}
	// A generation row stuck in `building` is not a complete publication and
	// must never resolve as catalog facts.
	if err := store.WriteMetadata(ctx, func(writer MetadataWriter) error {
		_, err := writer.ExecContext(ctx, `INSERT INTO ingestion_generations
			(tenant_id,brain_id,source_id,generation_id,generation_sequence,snapshot_id,state,source_watermark,created_at_ms)
			VALUES (?,?,?,?,?,?,'building',?,?)`,
			publication.Scope.Tenant.Value, publication.Scope.Brain.Value, publication.Scope.SourceID,
			"gen-building", 2, publication.Snapshot.SnapshotID, 2, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadIngestionGenerationFacts(ctx, publication.Scope, "gen-building"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("building generation facts error = %v", err)
	}
}

func assertCatalogFacts(t *testing.T, facts IngestionGenerationFacts, generationID string, sequence uint64) {
	t.Helper()
	if facts.GenerationID != generationID || facts.Sequence != sequence || facts.RepositoryID != "repo" ||
		facts.SnapshotID == "" || facts.CommitOID == "" || facts.TreeOID == "" || facts.PolicyDigest == "" ||
		facts.State != "ready" || facts.SourceWatermark != sequence || len(facts.Readiness) != len(p5Languages) {
		t.Fatalf("generation facts = %#v", facts)
	}
	for index, language := range p5Languages {
		if facts.Readiness[index].Language != language || facts.Readiness[index].Coverage != "syntax_aware" {
			t.Fatalf("readiness lane %d = %#v", index, facts.Readiness[index])
		}
	}
}

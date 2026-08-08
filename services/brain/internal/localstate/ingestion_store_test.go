package localstate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestPublishGenerationCommitsCompleteCheckpointAndReplaysAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.db")
	store := openIngestionStore(t, path)
	openTestSession(t, store, "s1")
	publication := testPublication("publish-1", "publish-key", "a", 1, "", "gen-1")

	first, err := store.PublishGeneration(ctx, publication)
	if err != nil || first.Replayed || first.Receipt.ReasonCode != "ingestion_generation_ready" {
		t.Fatalf("first publication = %#v, %v", first, err)
	}
	assertCheckpoint(t, first.Checkpoint, "gen-1", false)
	loaded, err := store.LoadIngestionCheckpoint(ctx, IngestionCheckpointQuery{
		Identity: testIdentity("s1"), Scope: publication.Scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpoint(t, loaded, "gen-1", false)
	swappedTarget := publication
	swappedTarget.Command.Command.Value = "publish-target-swap"
	swappedTarget.GenerationID = "gen-2"
	swappedTarget.Command.AuthenticatedDigest = GenerationPublicationDigest(swappedTarget)
	if _, err := store.PublishGeneration(ctx, swappedTarget); !errors.Is(err, ErrIngestionConflict) {
		t.Fatalf("target-swapped replay = %v", err)
	}
	crossScope := publication
	crossScope.Command.Command.Value = "publish-scope-swap"
	crossScope.Scope.Brain.Value = "other-brain"
	crossScope.Command.AuthenticatedDigest = GenerationPublicationDigest(crossScope)
	if _, err := store.PublishGeneration(ctx, crossScope); !errors.Is(err, ErrIngestionConflict) {
		t.Fatalf("cross-scope replay = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openIngestionStore(t, path)
	openTestSession(t, store, "s1")
	replay, err := store.PublishGeneration(ctx, publication)
	if err != nil || !replay.Replayed || replay.Receipt != first.Receipt {
		t.Fatalf("restart replay = %#v, %v", replay, err)
	}
	assertCheckpoint(t, replay.Checkpoint, "gen-1", false)
	assertIngestionCounts(t, store, 1, 1, 5, 1)
}

func TestPublishGenerationRejectsConflictStaleAndWrongDomainWithoutWrites(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	first := testPublication("publish-1", "same-key", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, first); err != nil {
		t.Fatal(err)
	}

	conflict := first
	conflict.Command.Command.Value = "publish-conflict"
	conflict.GenerationID = "conflicting-generation"
	conflict.Command.AuthenticatedDigest = GenerationPublicationDigest(conflict)
	if _, err := store.PublishGeneration(ctx, conflict); !errors.Is(err, ErrIngestionConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	stale := testPublication("publish-2", "next-key", "c", 2, "wrong", "gen-2")
	if _, err := store.PublishGeneration(ctx, stale); !errors.Is(err, ErrIngestionStale) {
		t.Fatalf("stale CAS = %v", err)
	}
	wrongBrain := testPublication("publish-3", "brain-key", "d", 2, "gen-1", "gen-3")
	wrongBrain.Scope.Brain.Value = "other"
	wrongBrain.Command.AuthenticatedDigest = GenerationPublicationDigest(wrongBrain)
	if _, err := store.PublishGeneration(ctx, wrongBrain); !errors.Is(err, ErrIngestionStale) {
		t.Fatalf("wrong brain = %v", err)
	}
	wrongTenant := testPublication("publish-4", "tenant-key", "e", 2, "gen-1", "gen-4")
	wrongTenant.Scope.Tenant.Value = "other"
	if _, err := store.PublishGeneration(ctx, wrongTenant); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong tenant = %v", err)
	}
	wrongIdentity := testIdentity("s1")
	wrongIdentity.Principal.Value = "other"
	if _, err := store.LoadIngestionCheckpoint(ctx, IngestionCheckpointQuery{
		Identity: wrongIdentity, Scope: first.Scope,
	}); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong principal = %v", err)
	}
	assertIngestionCounts(t, store, 1, 1, 5, 1)
}

func TestLoadIngestionCheckpointReturnsOnlySequenceTwoPredecessorAndRejectsMissingIt(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	first := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := testPublication("publish-2", "key-2", "b", 2, "gen-1", "gen-2")
	if _, err := store.PublishGeneration(ctx, second); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.LoadIngestionCheckpoint(ctx, IngestionCheckpointQuery{
		Identity: testIdentity("s1"), Scope: first.Scope,
	})
	if err != nil || checkpoint.PreviousGenerationID != first.GenerationID ||
		checkpoint.PreviousCommitOID != first.Snapshot.CommitOID {
		t.Fatalf("sequence-two checkpoint = %#v, %v", checkpoint, err)
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM ingestion_generations WHERE generation_id='gen-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadIngestionCheckpoint(ctx, IngestionCheckpointQuery{
		Identity: testIdentity("s1"), Scope: first.Scope,
	}); !errors.Is(err, ErrIngestionConflict) {
		t.Fatalf("missing predecessor checkpoint = %v", err)
	}
}

func TestPublishGenerationEnsuresCanonicalSnapshotAndMembership(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	first := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, first); err != nil {
		t.Fatal(err)
	}
	reused := first
	reused.Command = testIngestionCommand("publish-2", "key-2", "b", IngestionPublishCommand)
	reused.GenerationID = "gen-2"
	reused.Sequence = 2
	reused.ExpectedCurrentGenerationID = "gen-1"
	reused.SourceWatermark = 2
	reused.Command.AuthenticatedDigest = GenerationPublicationDigest(reused)
	if _, err := store.PublishGeneration(ctx, reused); err != nil {
		t.Fatalf("reuse canonical snapshot = %v", err)
	}
	assertIngestionCounts(t, store, 2, 1, 10, 1)

	conflicting := reused
	conflicting.Command = testIngestionCommand("publish-3", "key-3", "c", IngestionPublishCommand)
	conflicting.GenerationID = "gen-3"
	conflicting.Sequence = 3
	conflicting.ExpectedCurrentGenerationID = "gen-2"
	conflicting.SourceWatermark = 3
	conflicting.Snapshot.TreeOID = oid("f")
	conflicting.Command.AuthenticatedDigest = GenerationPublicationDigest(conflicting)
	if _, err := store.PublishGeneration(ctx, conflicting); !errors.Is(err, ErrIngestionConflict) {
		t.Fatalf("conflicting canonical snapshot = %v", err)
	}
	assertIngestionCounts(t, store, 2, 1, 10, 1)
}

func TestLoadCurrentCheckpointPropagatesDatabaseErrors(t *testing.T) {
	scope := testPublication("unused", "unused", "a", 1, "", "gen").Scope
	t.Run("closed database", func(t *testing.T) {
		store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
		if err := store.db.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := loadCurrentCheckpoint(context.Background(), store.db, scope)
		if err == nil || errors.Is(err, ErrInvalidInput) {
			t.Fatalf("closed database error = %v", err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := loadCurrentCheckpoint(ctx, store.db, scope)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled checkpoint load = %v", err)
		}
	})
}

func TestIngestionBoundaryRejectsMalformedOversizedAndDuplicateInput(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	base := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	tests := []struct {
		name   string
		mutate func(*GenerationPublication)
	}{
		{name: "missing brain", mutate: func(value *GenerationPublication) { value.Scope.Brain.Value = "" }},
		{name: "malformed digest", mutate: func(value *GenerationPublication) { value.Snapshot.SnapshotDigest = "no" }},
		{name: "oversized idempotency key", mutate: func(value *GenerationPublication) {
			value.Command.IdempotencyKey = strings.Repeat("x", 513)
		}},
		{name: "duplicate readiness", mutate: func(value *GenerationPublication) {
			value.Readiness = append(value.Readiness, value.Readiness[0])
		}},
		{name: "duplicate source object", mutate: func(value *GenerationPublication) {
			duplicate := value.Revisions[0]
			duplicate.RevisionID = "other-revision"
			duplicate.PathDigest = digest("e")
			value.Revisions = append(value.Revisions, duplicate)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publication := base
			publication.Readiness = append([]IngestionReadiness(nil), base.Readiness...)
			publication.Revisions = append([]IngestionRevisionMetadata(nil), base.Revisions...)
			test.mutate(&publication)
			if _, err := store.PublishGeneration(ctx, publication); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	assertIngestionCounts(t, store, 0, 0, 0, 0)
}

func TestPublishGenerationSerializesConcurrentCAS(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	first := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, first); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for index := 2; index <= 3; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			publication := testPublication(
				fmt.Sprintf("publish-%d", index), fmt.Sprintf("key-%d", index),
				fmt.Sprintf("%x", index), 2, "gen-1", fmt.Sprintf("gen-%d", index),
			)
			_, err := store.PublishGeneration(ctx, publication)
			results <- err
		}(index)
	}
	close(start)
	group.Wait()
	close(results)
	successes, stale := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrIngestionStale):
			stale++
		default:
			t.Fatalf("concurrent publication = %v", err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("successes=%d stale=%d", successes, stale)
	}
}

func TestPublishGenerationRollsBackEveryRowWhenPublicationTriggerFails(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_test_generation
		BEFORE UPDATE OF state ON ingestion_generations BEGIN SELECT raise(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	publication := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, publication); err == nil {
		t.Fatal("injected failure succeeded")
	}
	assertIngestionCounts(t, store, 0, 0, 0, 0)
	var commands int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM commands WHERE command_id='publish-1'`).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if commands != 0 {
		t.Fatalf("rolled-back commands = %d", commands)
	}
}

func TestPublishGenerationAcceptsPathFreeFilesSymlinksAndUnindexedText(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	publication := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	publication.Revisions = []IngestionRevisionMetadata{
		testRevision("ignore-revision", "ignore-object", "1", "file", "text/plain", "", ""),
		testRevision("symlink-revision", "symlink-object", "2", "symlink", "inode/symlink", "", ""),
		testRevision("go-revision", "go-object", "3", "file", "text/x-go", "go", ""),
	}
	publication.Command.AuthenticatedDigest = GenerationPublicationDigest(publication)
	if _, err := store.PublishGeneration(ctx, publication); err != nil {
		t.Fatal(err)
	}
	var nullLanguages, symlinks int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM ingestion_source_revisions
		WHERE language IS NULL`).Scan(&nullLanguages); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM ingestion_source_revisions
		WHERE entry_kind='symlink' AND media_type='inode/symlink'`).Scan(&symlinks); err != nil {
		t.Fatal(err)
	}
	if nullLanguages != 2 || symlinks != 1 {
		t.Fatalf("null languages=%d symlinks=%d", nullLanguages, symlinks)
	}
}

func TestLaterGenerationTombstonesRemovedAndReplacedButKeepsUnchanged(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	first := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	unchanged := testRevision("unchanged", "object-a", "1", "file", "text/plain", "", "")
	replaced := testRevision("replaced-old", "object-b", "2", "file", "text/x-go", "go", "")
	deleted := testRevision("deleted-old", "object-c", "3", "file", "text/plain", "", "")
	first.Revisions = []IngestionRevisionMetadata{unchanged, replaced, deleted}
	first.Command.AuthenticatedDigest = GenerationPublicationDigest(first)
	if _, err := store.PublishGeneration(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := testPublication("publish-2", "key-2", "b", 2, "gen-1", "gen-2")
	replacement := testRevision("replacement-new", "object-b", "4", "file", "text/x-go", "go", "replaced-old")
	second.Revisions = []IngestionRevisionMetadata{unchanged, replacement}
	second.Command.AuthenticatedDigest = GenerationPublicationDigest(second)
	if _, err := store.PublishGeneration(ctx, second); err != nil {
		t.Fatal(err)
	}
	assertRevisionState(t, store, "unchanged", "active")
	assertRevisionState(t, store, "replacement-new", "active")
	assertRevisionState(t, store, "replaced-old", "tombstoned")
	assertRevisionState(t, store, "deleted-old", "tombstoned")
	var tombstones int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM ingestion_tombstones
		WHERE reason_code='generation_superseded'`).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if tombstones != 2 {
		t.Fatalf("superseded tombstones = %d", tombstones)
	}

	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_third_generation
		BEFORE UPDATE OF state ON ingestion_generations WHEN new.generation_id='gen-3'
		BEGIN SELECT raise(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	third := testPublication("publish-3", "key-3", "c", 3, "gen-2", "gen-3")
	third.Revisions = []IngestionRevisionMetadata{unchanged}
	third.Command.AuthenticatedDigest = GenerationPublicationDigest(third)
	if _, err := store.PublishGeneration(ctx, third); err == nil {
		t.Fatal("injected third-generation failure succeeded")
	}
	assertRevisionState(t, store, "replacement-new", "active")
	var current string
	var countAfterRollback int
	if err := store.db.QueryRowContext(ctx, `SELECT generation_id FROM ingestion_current_generations`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM ingestion_tombstones`).Scan(&countAfterRollback); err != nil {
		t.Fatal(err)
	}
	if current != "gen-2" || countAfterRollback != 2 {
		t.Fatalf("rollback current=%q tombstones=%d", current, countAfterRollback)
	}
}

func TestRevokeSourceTombstonesRevisionsRemovesPointerAndReplays(t *testing.T) {
	ctx := context.Background()
	store := openIngestionStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	publication := testPublication("publish-1", "key-1", "a", 1, "", "gen-1")
	if _, err := store.PublishGeneration(ctx, publication); err != nil {
		t.Fatal(err)
	}
	revocation := IngestionRevocation{
		Command: testIngestionCommand("revoke-1", "revoke-key", "b", IngestionRevokeCommand),
		Scope:   publication.Scope, ExpectedCurrentGenerationID: "gen-1",
		RevocationEpoch: 2, ReasonCode: "source_removed",
	}
	revocation.Command.AuthenticatedDigest = IngestionRevocationDigest(revocation)
	first, err := store.RevokeIngestionSource(ctx, revocation)
	if err != nil || first.Replayed || !first.Checkpoint.Revoked || !first.Checkpoint.Tombstoned {
		t.Fatalf("revoke = %#v, %v", first, err)
	}
	replay, err := store.RevokeIngestionSource(ctx, revocation)
	if err != nil || !replay.Replayed || replay.Receipt != first.Receipt {
		t.Fatalf("revoke replay = %#v, %v", replay, err)
	}
	swappedTarget := revocation
	swappedTarget.Command.Command.Value = "revoke-target-swap"
	swappedTarget.ExpectedCurrentGenerationID = "gen-2"
	swappedTarget.Command.AuthenticatedDigest = IngestionRevocationDigest(swappedTarget)
	if _, err := store.RevokeIngestionSource(ctx, swappedTarget); !errors.Is(err, ErrIngestionConflict) {
		t.Fatalf("target-swapped revoke replay = %v", err)
	}
	crossScope := revocation
	crossScope.Command.Command.Value = "revoke-scope-swap"
	crossScope.Scope.Brain.Value = "other-brain"
	crossScope.Command.AuthenticatedDigest = IngestionRevocationDigest(crossScope)
	if _, err := store.RevokeIngestionSource(ctx, crossScope); !errors.Is(err, ErrIngestionConflict) {
		t.Fatalf("cross-scope revoke replay = %v", err)
	}
	loaded, err := store.LoadIngestionCheckpoint(ctx, IngestionCheckpointQuery{
		Identity: testIdentity("s1"), Scope: publication.Scope,
	})
	if err != nil || !loaded.Revoked || !loaded.Tombstoned || loaded.GenerationID != "gen-1" {
		t.Fatalf("revoked restart checkpoint = %#v, %v", loaded, err)
	}
	var active, pointers, tombstones int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM ingestion_source_revisions
		WHERE deletion_state='active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM ingestion_current_generations`).Scan(&pointers); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM ingestion_tombstones`).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if active != 0 || pointers != 0 || tombstones != 2 {
		t.Fatalf("active=%d pointers=%d tombstones=%d", active, pointers, tombstones)
	}
}

func openIngestionStore(t *testing.T, databasePath string) *Store {
	t.Helper()
	store, err := OpenWithMigrations(context.Background(), databasePath, []Migration{
		{Version: 1, SQL: migrationSource(t)},
		{Version: 2, SQL: migrationSourceNamed(t, "002_durable_storage_adapters.sql")},
		{Version: 3, SQL: migrationSourceNamed(t, "003_stage03_ingestion.sql")},
	}, fixedClock{value: 1_000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testPublication(commandID, key, digestSeed string, sequence uint64, expected, generationID string) GenerationPublication {
	state := "ready"
	publication := GenerationPublication{
		Command: testIngestionCommand(commandID, key, digestSeed, IngestionPublishCommand),
		Scope: IngestionScope{
			Tenant: contracts.Identifier{Namespace: "tenant", Value: "t"},
			Brain:  contracts.Identifier{Namespace: "brain", Value: "b"}, SourceID: digest("1"),
		},
		Source: IngestionSourceMetadata{
			RepositoryID: "repo", ConfigurationDigest: digest("2"), IgnorePolicyDigest: digest("3"),
			ApprovedRootID: digest("4"), ACLEpoch: 1,
		},
		Snapshot: IngestionSnapshotMetadata{
			SnapshotID: fmt.Sprintf("snapshot-%d", sequence), CommitOID: oid(fmt.Sprintf("%x", sequence)),
			TreeOID: oid(fmt.Sprintf("%x", sequence+5)), PolicyDigest: digest("3"), SnapshotDigest: digest(fmt.Sprintf("%x", sequence+8)),
		},
		GenerationID: generationID, Sequence: sequence, ExpectedCurrentGenerationID: expected,
		State: state, SourceWatermark: sequence,
		Revisions: []IngestionRevisionMetadata{{
			RevisionID: fmt.Sprintf("revision-%d", sequence), SourceObjectID: "object-1",
			PathDigest: digest(fmt.Sprintf("%x", sequence+10)), GitBlobOID: oid(fmt.Sprintf("%x", sequence+11)),
			ContentDigest: digest(fmt.Sprintf("%x", sequence+12)), ByteLength: 12,
			EntryKind: "file", MediaType: "text/x-go", Language: "go",
			PredecessorRevisionID: predecessor(sequence),
		}},
		Readiness: syntaxReadiness(),
	}
	publication.Command.AuthenticatedDigest = GenerationPublicationDigest(publication)
	return publication
}

func testIngestionCommand(commandID, key, digestSeed, commandType string) contracts.CommandRecord {
	return contracts.CommandRecord{
		Command:     contracts.Identifier{Namespace: "command", Value: commandID},
		Tenant:      contracts.Identifier{Namespace: "tenant", Value: "t"},
		Principal:   contracts.Identifier{Namespace: "principal", Value: "p"},
		Session:     contracts.Identifier{Namespace: "session", Value: "s1"},
		CommandType: commandType, IdempotencyKey: key,
		AuthenticatedDigest: contracts.Digest{Algorithm: "sha256", Hex: digest(digestSeed)}, Fence: 1,
	}
}

func testRevision(
	revisionID, objectID, seed, entryKind, mediaType, language, predecessor string,
) IngestionRevisionMetadata {
	return IngestionRevisionMetadata{
		RevisionID: revisionID, SourceObjectID: objectID, PathDigest: digest(seed), GitBlobOID: oid(seed),
		ContentDigest: digest(seed), ByteLength: 12, EntryKind: entryKind, MediaType: mediaType,
		Language: language, PredecessorRevisionID: predecessor,
	}
}

func syntaxReadiness() []IngestionReadiness {
	readiness := make([]IngestionReadiness, 0, len(p5Languages))
	for _, language := range p5Languages {
		readiness = append(readiness, IngestionReadiness{Language: language, Coverage: "syntax_aware"})
	}
	return readiness
}

func predecessor(sequence uint64) string {
	if sequence == 1 {
		return ""
	}
	return fmt.Sprintf("revision-%d", sequence-1)
}

func digest(seed string) string { return strings.Repeat(seed, 64/len(seed)) }

func oid(seed string) string { return strings.Repeat(seed, 40/len(seed)) }

func assertCheckpoint(t *testing.T, checkpoint IngestionCheckpoint, generationID string, revoked bool) {
	t.Helper()
	if checkpoint.GenerationID != generationID || checkpoint.Revoked != revoked ||
		checkpoint.CommitOID == "" || checkpoint.ApprovedRootID == "" {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
}

func assertIngestionCounts(t *testing.T, store *Store, generations, revisions, readiness, pointers int) {
	t.Helper()
	for table, expected := range map[string]int{
		"ingestion_generations": generations, "ingestion_source_revisions": revisions,
		"ingestion_generation_readiness": readiness, "ingestion_current_generations": pointers,
	} {
		var count int
		if err := store.db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != expected {
			t.Fatalf("%s count = %d, want %d", table, count, expected)
		}
	}
}

func assertRevisionState(t *testing.T, store *Store, revisionID, expected string) {
	t.Helper()
	var state string
	if err := store.db.QueryRowContext(context.Background(), `SELECT deletion_state
		FROM ingestion_source_revisions WHERE source_revision_id=?`, revisionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != expected {
		t.Fatalf("revision %s state = %q, want %q", revisionID, state, expected)
	}
}

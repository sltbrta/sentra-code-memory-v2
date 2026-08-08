package localstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/audit"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/deletion"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/eventkernel"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

type fixedClock struct{ value int64 }

func (clock fixedClock) NowUnixMilli() int64 { return clock.value }

func TestExecuteReplaysExactlyAcrossConcurrencyAndRestart(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "authority.db")
	store := openTestStore(t, databasePath)
	openTestSession(t, store, "s1")
	mutation := testMutation("c1", "s1", "same-key", "digest-a", 1)

	const workers = 8
	results := make(chan Execution, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := store.Execute(ctx, mutation)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	replays := 0
	for result := range results {
		if result.Replayed {
			replays++
		}
	}
	if replays != workers-1 {
		t.Fatalf("replays = %d, want %d", replays, workers-1)
	}
	registry := eventkernel.NewRegistry()
	if err := registry.Register("artifact", 1, func(event eventkernel.Event) ([]eventkernel.Command, error) {
		return []eventkernel.Command{{Type: "project", AggregateType: event.AggregateType, AggregateID: event.AggregateID, PayloadDigest: event.PayloadDigest}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := store.Replay(ctx, tenantID())
	if err != nil {
		t.Fatal(err)
	}
	beforeCommands, err := registry.React(beforeEvents, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, databasePath)
	openTestSession(t, store, "s2")
	restarted := testMutation("different-command", "s2", "same-key", "digest-a", 1)
	result, err := store.Execute(ctx, restarted)
	if err != nil || !result.Replayed || result.Receipt.ReasonCode != "ok" || result.Receipt.OperationID.Value != "c1" {
		t.Fatalf("restart replay = %#v, %v", result, err)
	}
	conflict := testMutation("c2", "s2", "same-key", "different", 1)
	if _, err := store.Execute(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	afterEvents, err := store.Replay(ctx, tenantID())
	if err != nil {
		t.Fatal(err)
	}
	afterCommands, err := registry.React(afterEvents, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterEvents, beforeEvents) || !reflect.DeepEqual(afterCommands, beforeCommands) {
		t.Fatalf("restart changed replay: events=%#v commands=%#v", afterEvents, afterCommands)
	}
}

func TestReserveFinalizeSurvivesRestartAndReplaysCanonicalState(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "authority.db")
	store := openTestStore(t, databasePath)
	openTestSession(t, store, "s1")
	mutation := testMutation("c1", "s1", "same-key", "digest-a", 1)

	reserved, err := store.Reserve(ctx, mutation.Command)
	if err != nil || reserved.Replayed || reserved.Status != "accepted" || reserved.Command != mutation.Command {
		t.Fatalf("reservation = %#v, %v", reserved, err)
	}
	globalConflict := mutation.Command
	globalConflict.IdempotencyKey = "different"
	if _, err := store.Reserve(ctx, globalConflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("global command conflict = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, databasePath)
	openTestSession(t, store, "s2")
	retry := testMutation("different-command", "s2", "same-key", "digest-a", 1)
	resumed, err := store.Reserve(ctx, retry.Command)
	if err != nil || !resumed.Replayed || resumed.Status != "accepted" || resumed.Command != mutation.Command {
		t.Fatalf("resumed reservation = %#v, %v", resumed, err)
	}
	conflict := retry.Command
	conflict.AuthenticatedDigest.Hex = "different"
	if _, err := store.Reserve(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("post-crash conflict = %v", err)
	}

	finalized, err := store.Finalize(ctx, mutation)
	if err != nil || finalized.Replayed || finalized.Receipt.OperationID != mutation.Command.Command {
		t.Fatalf("finalize = %#v, %v", finalized, err)
	}
	completed, err := store.Reserve(ctx, retry.Command)
	if err != nil || !completed.Replayed || completed.Status != "completed" ||
		completed.Command != mutation.Command || completed.Receipt != finalized.Receipt {
		t.Fatalf("completed reservation = %#v, %v", completed, err)
	}
	finalizeReplay, err := store.Finalize(ctx, mutation)
	if err != nil || !finalizeReplay.Replayed || finalizeReplay.Receipt != finalized.Receipt {
		t.Fatalf("finalize replay = %#v, %v", finalizeReplay, err)
	}
}

func TestFinalizeRejectsUnreservedMismatchedAndRejectedReservations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	openTestSession(t, store, "s2")
	canonical := testMutation("c1", "s1", "same-key", "digest-a", 1)
	if _, err := store.Finalize(ctx, canonical); !errors.Is(err, ErrReservationRequired) {
		t.Fatalf("unreserved finalize = %v", err)
	}
	if _, err := store.Reserve(ctx, canonical.Command); err != nil {
		t.Fatal(err)
	}
	mismatch := canonical
	mismatch.Command.Command.Value = "different"
	mismatch.Command.Session.Value = "s2"
	if _, err := store.Finalize(ctx, mismatch); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("mismatched finalize = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE commands SET status='rejected' WHERE command_id='c1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(ctx, canonical); !errors.Is(err, ErrReservationRequired) {
		t.Fatalf("rejected finalize = %v", err)
	}
}

func TestConcurrentReserveSelectsOneCanonicalWinner(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")

	const workers = 8
	results := make(chan Reservation, workers)
	errorsFound := make(chan error, workers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range workers {
		group.Add(1)
		go func(commandID string) {
			defer group.Done()
			<-start
			command := testMutation(commandID, "s1", "same-key", "digest-a", 1).Command
			result, err := store.Reserve(ctx, command)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- result
		}(fmt.Sprintf("c%d", index))
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	winners := 0
	var canonical contracts.CommandRecord
	commands := make([]contracts.CommandRecord, 0, workers)
	for result := range results {
		if !result.Replayed {
			winners++
			canonical = result.Command
		}
		commands = append(commands, result.Command)
		if result.Status != "accepted" {
			t.Fatalf("reservation status = %q", result.Status)
		}
	}
	if winners != 1 {
		t.Fatalf("canonical winners = %d, want 1", winners)
	}
	for _, command := range commands {
		if command != canonical {
			t.Fatalf("reservation returned non-canonical command: %#v, want %#v", command, canonical)
		}
	}
	retry := canonical
	retry.Command.Value = "retry"
	final, err := store.Reserve(ctx, retry)
	if err != nil || !final.Replayed || final.Command != canonical {
		t.Fatalf("canonical retry = %#v, %v", final, err)
	}
}

func TestOpenExclusivelyOwnsAbsoluteAuthorityPath(t *testing.T) {
	ctx := context.Background()
	migration := migrationSource(t)
	if _, err := Open(ctx, "relative.db", migration, fixedClock{value: 1000}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("relative open = %v", err)
	}
	if _, err := Open(ctx, t.TempDir(), migration, fixedClock{value: 1000}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("directory open = %v", err)
	}
	databasePath := filepath.Join(t.TempDir(), "authority.db")
	first, err := Open(ctx, databasePath, migration, fixedClock{value: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, databasePath, migration, fixedClock{value: 1000}); !errors.Is(err, ErrAuthorityOwned) {
		t.Fatalf("concurrent open = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, databasePath, migration, fixedClock{value: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	failurePath := filepath.Join(t.TempDir(), "failure.db")
	if _, err := Open(ctx, failurePath, "invalid migration", fixedClock{value: 1000}); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	afterFailure, err := Open(ctx, failurePath, migration, fixedClock{value: 1000})
	if err != nil {
		t.Fatalf("open after migration failure = %v", err)
	}
	if err := afterFailure.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCreatesOwnerOnlyDatabaseThatCanReopen(t *testing.T) {
	previousUmask := syscall.Umask(0o022)
	defer syscall.Umask(previousUmask)

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "authority.db")
	store, err := Open(ctx, databasePath, migrationSource(t), fixedClock{value: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %04o, want 0600", got)
	}
	reopened, err := Open(ctx, databasePath, migrationSource(t), fixedClock{value: 1000})
	if err != nil {
		t.Fatalf("reopen owner-only database: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteRollsBackWholeUnitAndDetectsAuditTamper(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	bad := testMutation("bad", "s1", "bad", "digest", 1)
	bad.Events = append(bad.Events, mutationEvent("e-bad-2", 3, "payload-2"))
	if _, err := store.Execute(ctx, bad); !errors.Is(err, ErrAggregateConflict) {
		t.Fatalf("aggregate error = %v", err)
	}
	assertRowCount(t, store.db, "commands", 0)
	good := testMutation("good", "s1", "good", "digest", 1)
	if _, err := store.Execute(ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyAudit(ctx, tenantID()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE checkpoints SET audit_digest='tampered' WHERE checkpoint_id='audit-head:t'`); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyAudit(ctx, tenantID()); !errors.Is(err, audit.ErrCorrupt) {
		t.Fatalf("checkpoint tamper error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE checkpoints SET audit_digest=(SELECT event_digest FROM audit_log WHERE tenant_id='t' ORDER BY sequence DESC LIMIT 1) WHERE checkpoint_id='audit-head:t'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE events SET aggregate_id='tampered' WHERE event_id='e-good'`); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyAudit(ctx, tenantID()); !errors.Is(err, audit.ErrCorrupt) {
		t.Fatalf("metadata tamper error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM audit_log WHERE tenant_id='t'`); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyAudit(ctx, tenantID()); !errors.Is(err, audit.ErrCorrupt) {
		t.Fatalf("deleted audit error = %v", err)
	}
}

func TestAggregateVersionUsesExactTenantScopedProjection(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	aggregate := contracts.Identifier{Namespace: "artifact", Value: "a"}
	if version, err := store.AggregateVersion(ctx, tenantID(), aggregate); err != nil || version != 0 {
		t.Fatalf("absent aggregate version = %d, %v", version, err)
	}
	if _, err := store.Execute(ctx, testMutation("v1", "s1", "v1", "digest-v1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO aggregate_versions VALUES ('other','artifact','a',99)`); err != nil {
		t.Fatal(err)
	}
	if version, err := store.AggregateVersion(ctx, tenantID(), aggregate); err != nil || version != 1 {
		t.Fatalf("tenant-scoped aggregate version = %d, %v", version, err)
	}
	if _, err := store.Execute(ctx, testMutation("v2", "s1", "v2", "digest-v2", 2)); err != nil {
		t.Fatal(err)
	}
	if version, err := store.AggregateVersion(ctx, tenantID(), aggregate); err != nil || version != 2 {
		t.Fatalf("incremented aggregate version = %d, %v", version, err)
	}
}

func TestExecuteRejectsUnpersistedEventTypingAndEmptyAuditCoverage(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	for name, mutate := range map[string]func(*Mutation){
		"empty":  func(mutation *Mutation) { mutation.Events = nil },
		"type":   func(mutation *Mutation) { mutation.Events[0].Type = "different" },
		"schema": func(mutation *Mutation) { mutation.Events[0].SchemaVersion = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			mutation := testMutation("bad-"+name, "s1", "key-"+name, "digest", 1)
			mutate(&mutation)
			if _, err := store.Execute(context.Background(), mutation); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	assertRowCount(t, store.db, "commands", 0)
}

func TestSessionMismatchAndDeletionStateRemainFailClosed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "authority.db"))
	openTestSession(t, store, "s1")
	mismatch := testIdentity("s1")
	mismatch.Principal.Value = "other"
	if err := store.OpenSession(ctx, mismatch); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("session mismatch error = %v", err)
	}
	seedPublishedArtifact(t, store.db)
	mutation := testMutation("delete", "s1", "delete", "delete-digest", 1)
	mutation.Command.CommandType = "artifact.delete"
	mutation.Receipt.ReasonCode = "purged"
	mutation.Events = []MutationEvent{mutationEvent("e-tombstone", 1, "tombstone"), mutationEvent("e-purge", 2, "purge")}
	mutation.Deletion = &deletion.Request{TenantID: "t", ArtifactID: "a", Generation: 1, TombstoneID: "ts1", PurgeID: "p1", KeyEpoch: 1, ReasonCode: "user_delete", OccurredAtMs: 1000}
	mutation.PurgeNow = true
	if _, err := store.Execute(ctx, mutation); err != nil {
		t.Fatal(err)
	}
	state, err := store.ArtifactState(ctx, tenantID(), contracts.Identifier{Namespace: "artifact", Value: "a"}, 1)
	if err != nil || state != "purged" {
		t.Fatalf("state = %q, %v", state, err)
	}
	assertRowCount(t, store.db, "tombstones", 1)
	assertRowCount(t, store.db, "purge_jobs", 1)
}

func openTestStore(t *testing.T, databasePath string) *Store {
	t.Helper()
	store, err := Open(context.Background(), databasePath, migrationSource(t), fixedClock{value: 1000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOpenWithMigrationsUpgradesWithoutRewritingVersionOneData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "authority.db")
	store := openTestStore(t, path)
	openTestSession(t, store, "s1")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenWithMigrations(ctx, path, []Migration{
		{Version: 1, SQL: migrationSource(t)},
		{Version: 2, SQL: migrationSourceNamed(t, "002_durable_storage_adapters.sql")},
	}, fixedClock{value: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var principal string
	if err := store.ReadMetadata(ctx, func(reader MetadataReader) error {
		return reader.QueryRowContext(ctx, "SELECT principal_id FROM sessions WHERE session_id='s1'").Scan(&principal)
	}); err != nil || principal != "p" {
		t.Fatalf("retained principal=%q err=%v", principal, err)
	}
	var versions int
	if err := store.ReadMetadata(ctx, func(reader MetadataReader) error {
		return reader.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version IN (1,2)").Scan(&versions)
	}); err != nil || versions != 2 {
		t.Fatalf("versions=%d err=%v", versions, err)
	}
}

func migrationSource(t *testing.T) string {
	return migrationSourceNamed(t, "001_stage02_authority.sql")
}

func migrationSourceNamed(t *testing.T, name string) string {
	t.Helper()
	paths := []string{filepath.Join("schema", "migrations", name)}
	if _, sourceFile, _, ok := runtime.Caller(0); ok {
		paths = append([]string{filepath.Join(filepath.Dir(sourceFile), "schema", "migrations", name)}, paths...)
		if marker := string(filepath.Separator) + "bazel-out" + string(filepath.Separator); strings.Contains(sourceFile, marker) {
			workspaceRoot := strings.SplitN(sourceFile, marker, 2)[0]
			paths = append([]string{filepath.Join(workspaceRoot, "services", "brain", "internal", "localstate", "schema", "migrations", name)}, paths...)
		}
	}
	if testRoot := os.Getenv("TEST_SRCDIR"); testRoot != "" {
		paths = append([]string{filepath.Join(testRoot, os.Getenv("TEST_WORKSPACE"), "services", "brain", "internal", "localstate", "schema", "migrations", name)}, paths...)
		marker := string(filepath.Separator) + "bazel-out" + string(filepath.Separator)
		if strings.Contains(testRoot, marker) {
			workspaceRoot := strings.SplitN(testRoot, marker, 2)[0]
			paths = append([]string{filepath.Join(workspaceRoot, "services", "brain", "internal", "localstate", "schema", "migrations", name)}, paths...)
		}
	}
	for _, candidate := range paths {
		data, err := os.ReadFile(candidate)
		if err == nil {
			return string(data)
		}
	}
	t.Fatalf("real Stage 02 migration not found at %v", paths)
	return ""
}

func openTestSession(t *testing.T, store *Store, session string) {
	t.Helper()
	if err := store.OpenSession(context.Background(), testIdentity(session)); err != nil {
		t.Fatal(err)
	}
}

func testIdentity(session string) contracts.MappedIdentityFact {
	return contracts.MappedIdentityFact{
		Principal: contracts.Identifier{Namespace: "principal", Value: "p"}, Tenant: tenantID(),
		Session: contracts.Identifier{Namespace: "session", Value: session}, Credentials: contracts.PeerCredentials{UID: 501, PID: 99},
	}
}

func tenantID() contracts.Identifier { return contracts.Identifier{Namespace: "tenant", Value: "t"} }

func testMutation(commandID, session, key, digest string, version uint64) Mutation {
	return Mutation{
		Command: contracts.CommandRecord{
			Command: contracts.Identifier{Namespace: "command", Value: commandID}, Tenant: tenantID(),
			Principal: contracts.Identifier{Namespace: "principal", Value: "p"}, Session: contracts.Identifier{Namespace: "session", Value: session},
			CommandType: "artifact.admit", IdempotencyKey: key, AuthenticatedDigest: contracts.Digest{Algorithm: "sha256", Hex: digest}, Fence: 7,
		},
		Events:  []MutationEvent{mutationEvent("e-"+commandID, version, "payload-"+commandID)},
		Receipt: contracts.Receipt{Status: "completed", ReasonCode: "ok"}, Projection: "authority",
	}
}

func mutationEvent(eventID string, version uint64, digest string) MutationEvent {
	return MutationEvent{Type: "artifact", SchemaVersion: 1, Record: contracts.EventRecord{
		Event: contracts.Identifier{Namespace: "event", Value: eventID}, Aggregate: contracts.Identifier{Namespace: "artifact", Value: "a"},
		Version: version, PayloadDigest: contracts.Digest{Algorithm: "sha256", Hex: digest},
	}}
}

func seedPublishedArtifact(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO artifact_manifests VALUES ('a','t',1,'content',1,1,1,'staged')`,
		`INSERT INTO artifact_frames VALUES ('t','a',1,0,0,1,'frame')`,
		`UPDATE artifact_manifests SET status='published' WHERE tenant_id='t' AND artifact_id='a' AND generation=1`,
		`INSERT INTO artifact_generations VALUES ('t','a',1)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRowCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	allowed := map[string]bool{"commands": true, "tombstones": true, "purge_jobs": true}
	if !allowed[table] {
		t.Fatalf("test table %q not allowed", table)
	}
	var got int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", table, got, want)
	}
}

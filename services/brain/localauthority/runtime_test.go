package localauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/evidenceledger"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	shared "github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	_ "modernc.org/sqlite"
)

type fixedClock struct{ millis int64 }

func (clock fixedClock) NowUnixMilli() int64 { return clock.millis }

type testDependencies struct {
	artifacts *countingArtifactRepository
	evidence  *evidenceledger.MemoryRepository
	keys      *keyring.Memory
	objects   string
}

type countingArtifactRepository struct {
	*artifactvault.MemoryRepository
	publishes  int
	tombstones int
	gets       int
	nextGetErr error
}

func (repository *countingArtifactRepository) Get(
	ctx context.Context,
	tenant Identifier,
	artifact Identifier,
	generation uint64,
) (artifactvault.GenerationRecord, error) {
	repository.gets++
	if repository.nextGetErr != nil {
		err := repository.nextGetErr
		repository.nextGetErr = nil
		return artifactvault.GenerationRecord{}, err
	}
	return repository.MemoryRepository.Get(ctx, tenant, artifact, generation)
}

func TestSessionAdmissionIsCanonicalAndStatusRequiresExactSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), newTestDependencies(filepath.Join(root, "objects")))
	identity := testIdentity()
	first, err := runtime.OpenSession(ctx, identity)
	if err != nil || first.Replayed || first.Receipt.Status != "completed" || first.Receipt.ReasonCode != "session_opened" {
		t.Fatalf("first session admission = %#v, %v", first, err)
	}
	replay, err := runtime.OpenSession(ctx, identity)
	if err != nil || !replay.Replayed || replay.Receipt != first.Receipt {
		t.Fatalf("session admission replay = %#v, %v", replay, err)
	}
	status, err := runtime.ReadStatus(ctx, identity)
	if err != nil || status.Watermark == 0 {
		t.Fatalf("session status = %#v, %v", status, err)
	}
	mismatch := identity
	mismatch.Session.Value = "missing"
	if _, err := runtime.ReadStatus(ctx, mismatch); !errors.Is(err, ErrDenied) {
		t.Fatalf("mismatched session status error = %v", err)
	}
}

func TestMaximumLengthSessionAdmissionUsesBoundedInternalIdentifiers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), newTestDependencies(filepath.Join(root, "objects")))
	identity := testIdentity()
	identity.Session.Value = strings.Repeat("s", 512)
	first, err := runtime.OpenSession(ctx, identity)
	if err != nil || first.Replayed || len(first.Receipt.OperationID.Value) > 128 {
		t.Fatalf("maximum session admission = %#v, %v", first, err)
	}
	replay, err := runtime.OpenSession(ctx, identity)
	if err != nil || !replay.Replayed || replay.Receipt != first.Receipt {
		t.Fatalf("maximum session replay = %#v, %v", replay, err)
	}
}

func (repository *countingArtifactRepository) Publish(
	ctx context.Context,
	request shared.ArtifactPublishRequest,
) (artifactvault.GenerationRecord, error) {
	repository.publishes++
	return repository.MemoryRepository.Publish(ctx, request)
}

func (repository *countingArtifactRepository) Tombstone(
	ctx context.Context,
	request shared.TombstoneRequest,
) (artifactvault.GenerationRecord, error) {
	repository.tombstones++
	return repository.MemoryRepository.Tombstone(ctx, request)
}

func TestRuntimeAdmitsReadsReplaysAndRejectsConflictsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := filepath.Join(root, "authority.db")
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, database, deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("encrypted authority bytes"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	admit := testRequest(identity, artifact, "artifact.admit", "admit", "payload-admit")
	first, err := runtime.Execute(ctx, admit)
	if err != nil || first.Replayed || deps.artifacts.publishes != 1 {
		t.Fatalf("first admit = %#v, %v", first, err)
	}
	replay, err := runtime.Execute(ctx, admit)
	if err != nil || !replay.Replayed || replay.Receipt.OperationID != first.Receipt.OperationID ||
		deps.artifacts.publishes != 1 {
		t.Fatalf("exact replay = %#v, %v", replay, err)
	}
	conflict := admit
	conflict.Command.ID.Value = "command-conflict"
	conflict.Command.PayloadDigest = digest([]byte("different authenticated request"))
	if _, err := runtime.Execute(ctx, conflict); !errors.Is(err, ErrDenied) {
		t.Fatalf("conflict error = %v", err)
	}
	read := testRequest(identity, artifact, "artifact.read", "read", "payload-read")
	read.Offset, read.Length = 0, uint64(len(content))
	result, err := runtime.Execute(ctx, read)
	if err != nil || !bytes.Equal(result.Bytes, content) || result.RangeDigest != digest(content) {
		t.Fatalf("read = %#v, %v", result, err)
	}
	clear(result.Bytes)
	assertObjectsEncrypted(t, deps.objects, content)

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = openTestRuntime(t, database, deps)
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	restarted, err := runtime.Execute(ctx, admit)
	if err != nil || !restarted.Replayed || restarted.Receipt.OperationID != first.Receipt.OperationID {
		t.Fatalf("restart replay = %#v, %v", restarted, err)
	}
}

func TestAcceptedCrashReservationBlocksConflictingEffect(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	first, _ := testArtifact([]byte("first"))
	reserved := testRequest(identity, first, "artifact.admit", "shared-key", "first-payload")
	reservation, err := runtime.store.Reserve(ctx, commandRecord(reserved))
	if err != nil || reservation.Status != "accepted" || reservation.Replayed {
		t.Fatalf("crash reservation = %#v, %v", reservation, err)
	}
	second, secondContent := testArtifact([]byte("second"))
	second.ID.Value = "b"
	if err := runtime.storage.stage(ctx, second, bytes.NewReader(secondContent)); err != nil {
		t.Fatal(err)
	}
	conflict := testRequest(identity, second, "artifact.admit", "shared-key", "different-payload")
	if _, err := runtime.Execute(ctx, conflict); !errors.Is(err, ErrDenied) {
		t.Fatalf("conflict error = %v", err)
	}
	if deps.artifacts.publishes != 0 {
		t.Fatalf("conflict reached publish %d times", deps.artifacts.publishes)
	}
	record, err := deps.artifacts.Get(ctx, second.Tenant, second.ID, second.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != artifactvault.StatusStaged {
		t.Fatalf("conflicting command changed artifact status to %q", record.Status)
	}
}

func TestAcceptedReservationResumesWithCanonicalCommandAndSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := filepath.Join(root, "authority.db")
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, database, deps)
	firstIdentity := testIdentity()
	if _, err := runtime.OpenSession(ctx, firstIdentity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("resume after crash"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	canonicalRequest := testRequest(firstIdentity, artifact, "artifact.admit", "resume-key", "resume-payload")
	canonical, err := runtime.store.Reserve(ctx, commandRecord(canonicalRequest))
	if err != nil || canonical.Status != "accepted" {
		t.Fatalf("canonical reservation = %#v, %v", canonical, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	runtime = openTestRuntime(t, database, deps)
	retryIdentity := testIdentity()
	retryIdentity.Session.Value = "s2"
	if _, err := runtime.OpenSession(ctx, retryIdentity); err != nil {
		t.Fatal(err)
	}
	retry := testRequest(retryIdentity, artifact, "artifact.admit", "resume-key", "resume-payload")
	retry.Command.ID.Value = "retry-command"
	result, err := runtime.Execute(ctx, retry)
	if err != nil || !result.Replayed || deps.artifacts.publishes != 1 {
		t.Fatalf("resumed result = %#v, %v", result, err)
	}
	completed, err := runtime.store.Reserve(ctx, commandRecord(retry))
	if err != nil || completed.Status != "completed" ||
		completed.Command.Command != canonical.Command.Command || completed.Command.Session != canonical.Command.Session {
		t.Fatalf("completed canonical reservation = %#v, %v", completed, err)
	}
}

func TestCurrentPolicyDenialRecordsCanonicalRejectedOutcomeWithoutEffect(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("revoked"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	request := testRequest(identity, artifact, "artifact.admit", "revoked-key", "revoked-payload")
	request.Authorize = func(context.Context, Identity, string, Identifier) (Authorization, error) {
		return Authorization{ReasonCode: "not_found_or_denied", RevocationEpoch: 1}, ErrDenied
	}
	result, err := runtime.Execute(ctx, request)
	if err != nil || result.Receipt.Status != "rejected" || result.Receipt.ReasonCode != "not_found_or_denied" {
		t.Fatalf("denied execute = %#v, %v", result, err)
	}
	if deps.artifacts.publishes != 0 {
		t.Fatalf("revoked request reached publish %d times", deps.artifacts.publishes)
	}
	replay, err := runtime.Execute(ctx, request)
	if err != nil || !replay.Replayed || replay.Receipt != result.Receipt {
		t.Fatalf("denial replay = %#v, %v", replay, err)
	}
	reservation, err := runtime.store.Reserve(ctx, commandRecord(request))
	if err != nil || !reservation.Replayed || reservation.Status != "completed" || reservation.Receipt.Status != "rejected" {
		t.Fatalf("canonical denied command = %#v, %v", reservation, err)
	}
	events, err := runtime.store.Replay(ctx, identity.Tenant)
	if err != nil || len(events) != 2 || events[1].AggregateType != "authorization" {
		t.Fatalf("denial audit events = %#v, %v", events, err)
	}
	conflict := request
	conflict.Command.ID.Value = "command-revoked-conflict"
	conflict.Command.PayloadDigest = digest([]byte("changed denied operation"))
	if _, err := runtime.Execute(ctx, conflict); !errors.Is(err, ErrDenied) {
		t.Fatalf("denial conflict error = %v", err)
	}
}

func TestRejectedReadRemainsTerminalAfterPolicyAllows(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("terminal denied read"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, testRequest(identity, artifact, "artifact.admit", "terminal-admit", "admit")); err != nil {
		t.Fatal(err)
	}
	deps.artifacts.gets = 0
	read := testRequest(identity, artifact, "artifact.read", "terminal-read", "read")
	read.Command.ID.Value = strings.Repeat("c", 512)
	read.Length = uint64(len(content))
	read.Authorize = func(context.Context, Identity, string, Identifier) (Authorization, error) {
		return Authorization{ReasonCode: "not_found_or_denied", RevocationEpoch: 1}, ErrDenied
	}
	denied, err := runtime.Execute(ctx, read)
	if err != nil || denied.Receipt.Status != "rejected" || deps.artifacts.gets != 0 {
		t.Fatalf("initial denied read = %#v, %v; metadata reads=%d", denied, err, deps.artifacts.gets)
	}
	events, err := runtime.store.Replay(ctx, identity.Tenant)
	if err != nil || len(events) == 0 || len(events[len(events)-1].ID) > 128 {
		t.Fatalf("bounded denial event = %#v, %v", events, err)
	}
	read.Authorize = func(context.Context, Identity, string, Identifier) (Authorization, error) {
		return Authorization{Allowed: true, ReasonCode: "allowed", RevocationEpoch: 2}, nil
	}
	replay, err := runtime.Execute(ctx, read)
	if err != nil || !replay.Replayed || replay.Receipt != denied.Receipt || replay.Authorization.Allowed ||
		len(replay.Bytes) != 0 || deps.artifacts.gets != 0 {
		t.Fatalf("terminal denial replay = %#v, %v; metadata reads=%d", replay, err, deps.artifacts.gets)
	}
	conflict := read
	conflict.Command.ID.Value = strings.Repeat("d", 512)
	conflict.Command.PayloadDigest = digest([]byte("conflicting retry"))
	if _, err := runtime.Execute(ctx, conflict); !errors.Is(err, ErrDenied) || deps.artifacts.gets != 0 {
		t.Fatalf("conflicting denial retry = %v; metadata reads=%d", err, deps.artifacts.gets)
	}
}

func TestAllowedAbsentReadFinalizesCanonicalRejectionAndReplays(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, _ := testArtifact([]byte("absent generation"))
	read := testRequest(identity, artifact, "artifact.read", "absent-read", "absent-read")
	read.Length = 1
	read.Authorize = func(context.Context, Identity, string, Identifier) (Authorization, error) {
		return Authorization{Allowed: true, ReasonCode: "allowed", RevocationEpoch: 9}, nil
	}
	first, err := runtime.Execute(ctx, read)
	if err != nil || first.Replayed || first.Receipt.Status != "rejected" ||
		first.Receipt.ReasonCode != "not_found_or_denied" || first.Authorization.Allowed ||
		first.Authorization.ReasonCode != "not_found_or_denied" || first.Authorization.RevocationEpoch != 9 ||
		len(first.Bytes) != 0 || deps.artifacts.gets != 1 {
		t.Fatalf("absent read = %#v, %v; metadata reads=%d", first, err, deps.artifacts.gets)
	}
	replay, err := runtime.Execute(ctx, read)
	if err != nil || !replay.Replayed || replay.Receipt != first.Receipt || replay.Authorization.Allowed ||
		replay.Authorization.ReasonCode != "not_found_or_denied" || replay.Authorization.RevocationEpoch != 9 ||
		len(replay.Bytes) != 0 || deps.artifacts.gets != 1 {
		t.Fatalf("absent replay = %#v, %v; metadata reads=%d", replay, err, deps.artifacts.gets)
	}
	reservation, err := runtime.store.Reserve(ctx, commandRecord(read))
	if err != nil || !reservation.Replayed || reservation.Status != "completed" ||
		reservation.Receipt.Status != "rejected" || reservation.Receipt != first.Receipt {
		t.Fatalf("absent reservation = %#v, %v", reservation, err)
	}
	events, err := runtime.store.Replay(ctx, identity.Tenant)
	if err != nil || len(events) != 2 || events[1].AggregateType != "authorization" {
		t.Fatalf("absent denial events = %#v, %v", events, err)
	}
}

func TestTransientCanonicalReadFailureLeavesReservationResumable(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "repository unavailable", err: errors.New("repository unavailable")},
		{name: "repository cancellation", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			deps := newTestDependencies(filepath.Join(root, "objects"))
			runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
			identity := testIdentity()
			if _, err := runtime.OpenSession(ctx, identity); err != nil {
				t.Fatal(err)
			}
			artifact, content := testArtifact([]byte("resumable read"))
			if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Execute(ctx, testRequest(identity, artifact, "artifact.admit", "transient-admit", "admit")); err != nil {
				t.Fatal(err)
			}

			read := testRequest(identity, artifact, "artifact.read", "transient-read", "read")
			read.Length = uint64(len(content))
			deps.artifacts.nextGetErr = test.err
			if _, err := runtime.Execute(ctx, read); !errors.Is(err, ErrDenied) {
				t.Fatalf("transient read error = %v", err)
			}
			reservation, err := runtime.store.Reserve(ctx, commandRecord(read))
			if err != nil || reservation.Status != "accepted" || !reservation.Replayed {
				t.Fatalf("resumable reservation = %#v, %v", reservation, err)
			}

			result, err := runtime.Execute(ctx, read)
			if err != nil || result.Receipt.Status != "completed" || !bytes.Equal(result.Bytes, content) {
				t.Fatalf("recovered read = %#v, %v", result, err)
			}
		})
	}
}

func TestCanonicalArtifactMismatchFinalizesRejectionAndReplays(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("canonical mismatch"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, testRequest(identity, artifact, "artifact.admit", "mismatch-admit", "admit")); err != nil {
		t.Fatal(err)
	}

	mismatched := artifact
	mismatched.Digest = digest([]byte("different immutable bytes"))
	read := testRequest(identity, mismatched, "artifact.read", "mismatch-read", "read")
	read.Length = uint64(len(content))
	first, err := runtime.Execute(ctx, read)
	if err != nil || first.Receipt.Status != "rejected" || first.Replayed {
		t.Fatalf("mismatch rejection = %#v, %v", first, err)
	}
	gets := deps.artifacts.gets
	replay, err := runtime.Execute(ctx, read)
	if err != nil || replay.Receipt != first.Receipt || !replay.Replayed || deps.artifacts.gets != gets {
		t.Fatalf("mismatch replay = %#v, %v; metadata reads=%d", replay, err, deps.artifacts.gets)
	}
}

func TestRevocationAfterReservationDeniesEffectAndLeavesResumableCommand(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("revoke after reserve"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	request := testRequest(identity, artifact, "artifact.admit", "revoke-after-reserve", "payload")
	checks := 0
	request.Authorize = func(context.Context, Identity, string, Identifier) (Authorization, error) {
		checks++
		if checks == 1 {
			return Authorization{Allowed: true, ReasonCode: "allowed"}, nil
		}
		return Authorization{ReasonCode: "not_found_or_denied", RevocationEpoch: 1}, ErrDenied
	}
	if _, err := runtime.Execute(ctx, request); !errors.Is(err, ErrDenied) {
		t.Fatalf("post-reserve revocation error = %v", err)
	}
	if deps.artifacts.publishes != 0 {
		t.Fatalf("revoked request reached publish %d times", deps.artifacts.publishes)
	}
	reservation, err := runtime.store.Reserve(ctx, commandRecord(request))
	if err != nil || reservation.Status != "accepted" || !reservation.Replayed {
		t.Fatalf("resumable reservation = %#v, %v", reservation, err)
	}
	request.Authorize = func(context.Context, Identity, string, Identifier) (Authorization, error) {
		return Authorization{Allowed: true, ReasonCode: "allowed", RevocationEpoch: 1}, nil
	}
	if _, err := runtime.Execute(ctx, request); err != nil || deps.artifacts.publishes != 1 {
		t.Fatalf("resumed execution = %v, publishes=%d", err, deps.artifacts.publishes)
	}
}

func TestReadAndPurgeUsePersistedGenerationKeyEpoch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("old epoch generation"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, testRequest(identity, artifact, "artifact.admit", "epoch-admit", "admit")); err != nil {
		t.Fatal(err)
	}
	advanced := artifact
	advanced.KeyEpoch = 2
	read := testRequest(identity, advanced, "artifact.read", "epoch-read", "read")
	read.Length = uint64(len(content))
	result, err := runtime.Execute(ctx, read)
	if err != nil || !bytes.Equal(result.Bytes, content) || result.Artifact.KeyEpoch != 1 {
		t.Fatalf("old-epoch read = %#v, %v", result, err)
	}
	clear(result.Bytes)
	deleted := testRequest(identity, advanced, "artifact.delete", "epoch-delete", "delete")
	deleted.Artifact.ExpectedGeneration = 1
	deleted.PurgeNow = true
	if _, err := runtime.Execute(ctx, deleted); err != nil {
		t.Fatalf("old-epoch purge = %v", err)
	}
}

func TestRuntimeAuditCorruptionFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := filepath.Join(root, "authority.db")
	runtime := openTestRuntime(t, database, newTestDependencies(filepath.Join(root, "objects")))
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("audit material"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, testRequest(identity, artifact, "artifact.admit", "admit", "payload")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE checkpoints SET audit_digest='tampered'"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ReadStatus(ctx, identity); !errors.Is(err, ErrDenied) {
		t.Fatalf("corrupt audit status error = %v", err)
	}
}

func TestRuntimeTombstoneRejectsNewReadAndDeleteReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, filepath.Join(root, "authority.db"), deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("tombstone only"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, testRequest(identity, artifact, "artifact.admit", "tombstone-admit", "admit")); err != nil {
		t.Fatal(err)
	}
	deleted := testRequest(identity, artifact, "artifact.delete", "tombstone-delete", "delete")
	deleted.Artifact.ExpectedGeneration = 1
	deleted.PurgeNow = false
	if _, err := runtime.Execute(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	record, err := deps.artifacts.Get(ctx, artifact.Tenant, artifact.ID, artifact.Generation)
	if err != nil || record.Status != artifactvault.StatusTombstoned {
		t.Fatalf("tombstoned generation = %#v, %v", record, err)
	}
	replay, err := runtime.Execute(ctx, deleted)
	if err != nil || !replay.Replayed || deps.artifacts.tombstones != 1 {
		t.Fatalf("delete replay = %#v, %v; tombstones=%d", replay, err, deps.artifacts.tombstones)
	}
	read := testRequest(identity, artifact, "artifact.read", "read-after-tombstone", "read")
	read.Length = uint64(len(content))
	rejected, err := runtime.Execute(ctx, read)
	if err != nil || rejected.Receipt.Status != "rejected" ||
		rejected.Receipt.ReasonCode != "not_found_or_denied" || rejected.Authorization.Allowed ||
		rejected.Authorization.ReasonCode != "not_found_or_denied" || len(rejected.Bytes) != 0 {
		t.Fatalf("tombstoned read rejection = %#v, %v", rejected, err)
	}
	readReplay, err := runtime.Execute(ctx, read)
	if err != nil || !readReplay.Replayed || readReplay.Receipt != rejected.Receipt || len(readReplay.Bytes) != 0 {
		t.Fatalf("tombstoned read replay = %#v, %v", readReplay, err)
	}
}

func TestRuntimeTombstonePurgeDoesNotResurrectAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database := filepath.Join(root, "authority.db")
	deps := newTestDependencies(filepath.Join(root, "objects"))
	runtime := openTestRuntime(t, database, deps)
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	artifact, content := testArtifact([]byte("delete me"))
	if err := runtime.storage.stage(ctx, artifact, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, testRequest(identity, artifact, "artifact.admit", "admit", "admit")); err != nil {
		t.Fatal(err)
	}
	published, err := deps.artifacts.Get(ctx, artifact.Tenant, artifact.ID, artifact.Generation)
	if err != nil || published.Status != artifactvault.StatusPublished {
		t.Fatalf("published generation = %#v, %v", published, err)
	}
	deleted := testRequest(identity, artifact, "artifact.delete", "delete", "delete")
	deleted.Artifact.ExpectedGeneration = 1
	deleted.PurgeNow = true
	if _, err := runtime.Execute(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if deps.artifacts.tombstones != 1 {
		t.Fatalf("delete tombstones = %d", deps.artifacts.tombstones)
	}
	purged, err := deps.artifacts.Get(ctx, artifact.Tenant, artifact.ID, artifact.Generation)
	if err != nil || purged.Status != artifactvault.StatusPurged {
		t.Fatalf("purged generation = %#v, %v", purged, err)
	}
	deleteReplay, err := runtime.Execute(ctx, deleted)
	if err != nil || !deleteReplay.Replayed || deps.artifacts.tombstones != 1 {
		t.Fatalf("delete replay = %#v, %v; tombstones=%d", deleteReplay, err, deps.artifacts.tombstones)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = openTestRuntime(t, database, deps)
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	read := testRequest(identity, artifact, "artifact.read", "read-after-delete", "read")
	read.Length = uint64(len(content))
	rejected, err := runtime.Execute(ctx, read)
	if err != nil || rejected.Receipt.Status != "rejected" ||
		rejected.Receipt.ReasonCode != "not_found_or_denied" || rejected.Authorization.Allowed ||
		rejected.Authorization.ReasonCode != "not_found_or_denied" || len(rejected.Bytes) != 0 {
		t.Fatalf("purged read rejection = %#v, %v", rejected, err)
	}
	replay, err := runtime.Execute(ctx, read)
	if err != nil || !replay.Replayed || replay.Receipt != rejected.Receipt || len(replay.Bytes) != 0 {
		t.Fatalf("purged read replay = %#v, %v", replay, err)
	}
}

func openTestRuntime(t *testing.T, database string, deps testDependencies) *Runtime {
	t.Helper()
	storage, err := NewStorage(deps.objects, deps.artifacts, deps.keys, deps.evidence, StorageOptions{FrameBytes: 4, MaxReadBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := New(context.Background(), Config{
		DatabasePath: database, MigrationSQL: migrationSource(t),
		Brain:               Identifier{Namespace: "brain", Value: "b"},
		ConfigurationDigest: digest([]byte("test-config")), Clock: fixedClock{millis: 1_000_000},
	}, storage)
	if err != nil {
		_ = storage.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

func newTestDependencies(objects string) testDependencies {
	tenant := Identifier{Namespace: "tenant", Value: "t"}
	keys := keyring.NewMemory()
	root := bytes.Repeat([]byte{7}, keyring.RootKeyBytes)
	keys.Add(tenant, keyring.Material{
		Reference: shared.KeyReference{
			Root:  Identifier{Namespace: "key-root", Value: "t"},
			KeyID: Identifier{Namespace: "key", Value: "test-key"}, Epoch: 1,
		}, RootKey: root,
	}, keyring.Current)
	clear(root)
	return testDependencies{
		artifacts: &countingArtifactRepository{MemoryRepository: artifactvault.NewMemoryRepository()},
		evidence:  evidenceledger.NewMemoryRepository(), keys: keys, objects: objects,
	}
}

func testIdentity() Identity {
	return Identity{
		Principal:   Identifier{Namespace: "principal", Value: "p"},
		Tenant:      Identifier{Namespace: "tenant", Value: "t"},
		Session:     Identifier{Namespace: "session", Value: "s"},
		Credentials: shared.PeerCredentials{UID: 501, PID: 42},
	}
}

func testArtifact(content []byte) (Artifact, []byte) {
	return Artifact{
		ID:     Identifier{Namespace: "artifact", Value: "a"},
		Tenant: Identifier{Namespace: "tenant", Value: "t"},
		Digest: digest(content), Generation: 1, ExpectedGeneration: 0,
		KeyEpoch: 1, Length: uint64(len(content)), FrameCount: uint32((len(content) + 3) / 4),
	}, content
}

func testRequest(identity Identity, artifact Artifact, commandType, key, payload string) ExecuteRequest {
	return ExecuteRequest{
		Identity: identity, Artifact: artifact,
		Command: Command{
			ID:   Identifier{Namespace: "command", Value: "command-" + key},
			Type: commandType, IdempotencyKey: key,
			PayloadDigest: digest([]byte(payload)), Fence: 7,
		},
		Authorize: func(_ context.Context, got Identity, action string, resource Identifier) (Authorization, error) {
			if got != identity || action != commandType || resource != evidenceID(artifact.ID) {
				return Authorization{}, ErrDenied
			}
			return Authorization{Allowed: true, ReasonCode: "allowed"}, nil
		},
	}
}

func digest(content []byte) Digest {
	sum := sha256.Sum256(content)
	return Digest{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

func assertObjectsEncrypted(t *testing.T, root string, plaintext []byte) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(contents, plaintext) {
			t.Fatalf("plaintext found in encrypted object %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func migrationSource(t *testing.T) string {
	t.Helper()
	paths := []string{filepath.Join("..", "internal", "localstate", "schema", "migrations", "001_stage02_authority.sql")}
	if _, source, _, ok := goruntime.Caller(0); ok {
		paths = append(paths, filepath.Join(filepath.Dir(source), "..", "internal", "localstate", "schema", "migrations", "001_stage02_authority.sql"))
		if marker := string(filepath.Separator) + "bazel-out" + string(filepath.Separator); strings.Contains(source, marker) {
			root := strings.SplitN(source, marker, 2)[0]
			paths = append(paths, filepath.Join(root, "services", "brain", "internal", "localstate", "schema", "migrations", "001_stage02_authority.sql"))
		}
	}
	if testRoot := os.Getenv("TEST_SRCDIR"); testRoot != "" {
		paths = append(paths, filepath.Join(
			testRoot, os.Getenv("TEST_WORKSPACE"), "services", "brain", "internal", "localstate", "schema", "migrations", "001_stage02_authority.sql",
		))
		marker := string(filepath.Separator) + "bazel-out" + string(filepath.Separator)
		if strings.Contains(testRoot, marker) {
			root := strings.SplitN(testRoot, marker, 2)[0]
			paths = append(paths, filepath.Join(root, "services", "brain", "internal", "localstate", "schema", "migrations", "001_stage02_authority.sql"))
		}
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err == nil {
			return string(contents)
		}
	}
	t.Fatalf("Stage 2 migration not found at %v", paths)
	return ""
}

package localstorage

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/artifactvault"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestArtifactRepositoryPersistsExactLifecycleAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := migratedPath(t)
	bundle := openTestBundle(t, path)
	repository := bundle.Artifacts()
	request := contracts.ArtifactStageRequest{Manifest: manifest("t1", "a1", 1)}
	record, duplicate, err := repository.BeginStage(ctx, request, "opaque-locator")
	if err != nil || duplicate || record.Fence == 0 {
		t.Fatalf("reserve: duplicate=%v record=%+v err=%v", duplicate, record, err)
	}
	record.Frames = frames()
	if err := repository.CompleteStage(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := bundle.authority.Close(); err != nil {
		t.Fatal(err)
	}

	bundle = openTestBundle(t, path)
	repository = bundle.Artifacts()
	retry, duplicate, err := repository.BeginStage(ctx, request, "different-retry-locator")
	if err != nil || !duplicate || retry.Locator != "opaque-locator" || retry.Fence != record.Fence {
		t.Fatalf("exact retry: duplicate=%v record=%+v err=%v", duplicate, retry, err)
	}
	published, err := repository.Publish(ctx, contracts.ArtifactPublishRequest{Manifest: request.Manifest})
	if err != nil || published.Status != artifactvault.StatusPublished {
		t.Fatalf("publish: record=%+v err=%v", published, err)
	}
	if _, err := repository.Publish(ctx, contracts.ArtifactPublishRequest{Manifest: request.Manifest}); err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	if _, err := repository.Tombstone(ctx, contracts.TombstoneRequest{
		Tenant: request.Manifest.Tenant, Artifact: request.Manifest.Artifact,
		ExpectedGeneration: 1, ReasonCode: "user_delete",
	}); err != nil {
		t.Fatal(err)
	}
	receipt := canonicalTombstoneReceipt(published)
	stale := published
	stale.Fence++
	if err := repository.CompletePurge(ctx, stale); !errors.Is(err, artifactvault.ErrConflict) {
		t.Fatalf("stale fence completed purge: %v", err)
	}
	tombstoned, err := repository.PreparePurge(ctx, contracts.PurgeRequest{
		Tenant: request.Manifest.Tenant, Artifact: request.Manifest.Artifact, KeyEpoch: 1, TombstoneReceipt: receipt,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CompletePurge(ctx, tombstoned); err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.Get(ctx, request.Manifest.Tenant, request.Manifest.Artifact, 1)
	if err != nil || loaded.Status != artifactvault.StatusPurged {
		t.Fatalf("load purged: record=%+v err=%v", loaded, err)
	}
}

func TestArtifactRepositoryRejectsConflictsAndIncompleteFrames(t *testing.T) {
	ctx := context.Background()
	repository := openTestBundle(t, migratedPath(t)).Artifacts()
	request := contracts.ArtifactStageRequest{Manifest: manifest("t1", "a1", 1)}
	record, _, err := repository.BeginStage(ctx, request, "locator")
	if err != nil {
		t.Fatal(err)
	}
	conflict := request
	conflict.Manifest.Digest = digest(15)
	if _, _, err := repository.BeginStage(ctx, conflict, "locator"); !errors.Is(err, artifactvault.ErrConflict) {
		t.Fatalf("changed duplicate: %v", err)
	}
	record.Frames = frames()[:1]
	if err := repository.CompleteStage(ctx, record); !errors.Is(err, artifactvault.ErrIncomplete) {
		t.Fatalf("incomplete frames: %v", err)
	}
	record.Frames = frames()
	record.Frames[1].Offset = 3
	if err := repository.CompleteStage(ctx, record); !errors.Is(err, artifactvault.ErrIncomplete) {
		t.Fatalf("overlapping frames: %v", err)
	}
	if _, err := repository.Get(ctx, identifier("tenant", "other"), request.Manifest.Artifact, 1); !errors.Is(err, artifactvault.ErrNotFound) {
		t.Fatalf("cross-tenant get: %v", err)
	}
}

func TestArtifactRepositorySerializesConcurrentGenerationReservation(t *testing.T) {
	ctx := context.Background()
	repository := openTestBundle(t, migratedPath(t)).Artifacts()
	request := contracts.ArtifactStageRequest{Manifest: manifest("t1", "a1", 1)}
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, _, err := repository.BeginStage(ctx, request, "locator")
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	var success, conflict int
	for err := range errorsSeen {
		switch {
		case err == nil:
			success++
		case errors.Is(err, artifactvault.ErrConflict):
			conflict++
		default:
			t.Fatalf("unexpected reservation result: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

func TestArtifactReservationFenceNeverReusesAfterAbort(t *testing.T) {
	ctx := context.Background()
	path := migratedPath(t)
	bundle := openTestBundle(t, path)
	repository := bundle.Artifacts()
	request := contracts.ArtifactStageRequest{Manifest: manifest("t1", "a1", 1)}
	first, _, err := repository.BeginStage(ctx, request, "first")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AbortStage(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := bundle.authority.Close(); err != nil {
		t.Fatal(err)
	}
	bundle = openTestBundle(t, path)
	second, _, err := bundle.Artifacts().BeginStage(ctx, request, "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.Fence <= first.Fence {
		t.Fatalf("reservation fence reused: first=%d second=%d", first.Fence, second.Fence)
	}
}

func TestTombstonePersistsExactReasonAndReceiptAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := migratedPath(t)
	bundle := openTestBundle(t, path)
	repository := bundle.Artifacts()
	request := contracts.ArtifactStageRequest{Manifest: manifest("t1", "a1", 1)}
	record, _, err := repository.BeginStage(ctx, request, "locator")
	if err != nil {
		t.Fatal(err)
	}
	record.Frames = frames()
	if err := repository.CompleteStage(ctx, record); err != nil {
		t.Fatal(err)
	}
	published, err := repository.Publish(ctx, contracts.ArtifactPublishRequest{Manifest: request.Manifest})
	if err != nil {
		t.Fatal(err)
	}
	tombstone := contracts.TombstoneRequest{Tenant: request.Manifest.Tenant, Artifact: request.Manifest.Artifact, ExpectedGeneration: 1, ReasonCode: "user_delete"}
	if _, err := repository.Tombstone(ctx, tombstone); err != nil {
		t.Fatal(err)
	}
	changed := tombstone
	changed.ReasonCode = "changed"
	if _, err := repository.Tombstone(ctx, changed); !errors.Is(err, artifactvault.ErrConflict) {
		t.Fatalf("changed tombstone retry = %v", err)
	}
	if err := bundle.authority.Close(); err != nil {
		t.Fatal(err)
	}
	bundle = openTestBundle(t, path)
	repository = bundle.Artifacts()
	if _, err := repository.Tombstone(ctx, tombstone); err != nil {
		t.Fatalf("exact restart retry = %v", err)
	}
	receipt := canonicalTombstoneReceipt(published)
	purge := contracts.PurgeRequest{Tenant: tombstone.Tenant, Artifact: tombstone.Artifact, KeyEpoch: 1, TombstoneReceipt: receipt}
	if _, err := repository.PreparePurge(ctx, purge, 1); err != nil {
		t.Fatalf("exact receipt = %v", err)
	}
	fabricated := purge
	fabricated.TombstoneReceipt.OperationID.Value = "fabricated"
	if _, err := repository.PreparePurge(ctx, fabricated, 1); !errors.Is(err, artifactvault.ErrTombstoned) {
		t.Fatalf("fabricated receipt = %v", err)
	}
	fabricated = purge
	fabricated.TombstoneReceipt.ReasonCode = "changed"
	if _, err := repository.PreparePurge(ctx, fabricated, 1); !errors.Is(err, artifactvault.ErrTombstoned) {
		t.Fatalf("changed receipt = %v", err)
	}
}

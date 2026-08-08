package artifactvault

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestEncryptedStagePublishAndBoundedRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	content := []byte("0123456789abcdefghij")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	stageReceipt, err := h.vault.StageContent(h.ctx, request, bytes.NewReader(content))
	if err != nil || stageReceipt.Status != "staged" {
		t.Fatalf("StageContent() = (%+v, %v)", stageReceipt, err)
	}
	if _, err := h.vault.Stage(h.ctx, request); err != nil {
		t.Fatalf("Stage() verification: %v", err)
	}
	if _, err := h.vault.Publish(h.ctx, contracts.ArtifactPublishRequest{Manifest: request.Manifest, ExpectedGeneration: 0}); err != nil {
		t.Fatalf("Publish(): %v", err)
	}

	hydrated, err := h.vault.HydrateRange(h.ctx, readRequest(request.Manifest, 6, 10))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(hydrated.Bytes), string(content[6:16]); got != want {
		t.Fatalf("HydrateRange bytes = %q, want %q", got, want)
	}
	if len(hydrated.Metadata.FrameDigests) != 2 || hydrated.Metadata.NextOffset != 16 {
		t.Fatalf("HydrateRange metadata = %+v", hydrated.Metadata)
	}
	assertNoPlaintext(t, h.root, content)
}

func TestStageRejectsTruncationAndCleansPartialFiles(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	content := []byte("0123456789")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	_, err := h.vault.StageContent(h.ctx, request, bytes.NewReader(content[:4]))
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("truncated stage error = %v", err)
	}
	if _, err := h.metadata.Get(h.ctx, request.Manifest.Tenant, request.Manifest.Artifact, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial metadata error = %v", err)
	}
	assertObjectCount(t, h.root, 0)
}

func TestPublishRejectsIncompleteAndStaleGenerations(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	content := []byte("complete")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	if _, _, err := h.metadata.BeginStage(h.ctx, request, "reserved"); err != nil {
		t.Fatal(err)
	}
	_, err := h.vault.Publish(h.ctx, contracts.ArtifactPublishRequest{Manifest: request.Manifest, ExpectedGeneration: 0})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("incomplete publish error = %v", err)
	}

	other := stageRequest("tenant-a", "artifact-b", 2, 1, 1, content, 8)
	_, err = h.vault.StageContent(h.ctx, other, bytes.NewReader(content))
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale stage error = %v", err)
	}
}

func TestPublishRetryIsExactAndIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	content := []byte("publish-once")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	stageAndPublish(t, h, request, content)
	publish := contracts.ArtifactPublishRequest{Manifest: request.Manifest, ExpectedGeneration: 0}
	if receipt, err := h.vault.Publish(h.ctx, publish); err != nil || receipt.Status != "published" {
		t.Fatalf("exact publish retry = (%+v, %v)", receipt, err)
	}
	conflict := publish
	conflict.Manifest.Digest = digestBytes([]byte("different"))
	if _, err := h.vault.Publish(h.ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting publish retry error = %v", err)
	}
}

func TestDuplicateAndConcurrentStageAreSafe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	content := []byte("duplicate-content")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	if _, err := h.vault.StageContent(h.ctx, request, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	duplicate, err := h.vault.StageContent(h.ctx, request, bytes.NewReader(content))
	if err != nil || duplicate.ReasonCode != "OURO-ARTIFACT-DUPLICATE" {
		t.Fatalf("duplicate = (%+v, %v)", duplicate, err)
	}

	concurrent := stageRequest("tenant-a", "artifact-b", 1, 0, 1, content, 8)
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, stageErr := h.vault.StageContent(h.ctx, concurrent, bytes.NewReader(content))
			results <- stageErr
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	if first != nil && second != nil {
		t.Fatalf("both concurrent stages failed: %v, %v", first, second)
	}
	if first != nil && !errors.Is(first, ErrConflict) || second != nil && !errors.Is(second, ErrConflict) {
		t.Fatalf("unexpected concurrent errors: %v, %v", first, second)
	}
	if _, err := h.vault.Publish(h.ctx, contracts.ArtifactPublishRequest{Manifest: concurrent.Manifest, ExpectedGeneration: 0}); err != nil {
		t.Fatalf("publish after concurrency: %v", err)
	}
}

func TestCorruptionWrongAADAndWrongTenantFailClosed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	content := []byte("authenticated")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	stageAndPublish(t, h, request, content)

	if _, err := h.vault.HydrateRange(h.ctx, readRequestWithTenant(request.Manifest, "tenant-b", 0, 4)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong tenant error = %v", err)
	}
	record, err := h.metadata.Get(h.ctx, request.Manifest.Tenant, request.Manifest.Artifact, 1)
	if err != nil {
		t.Fatal(err)
	}
	material, err := h.keys.Resolve(h.ctx, request.Manifest.Tenant, 1)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := h.objects.read(h.ctx, objectKey(record, 0))
	if err != nil {
		t.Fatal(err)
	}
	wrongAAD := record
	wrongAAD.Manifest.Tenant.Value = "tenant-b"
	if _, err := decryptFrame(material.RootKey, wrongAAD, record.Frames[0], envelope); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong AAD error = %v", err)
	}
	clear(material.RootKey)

	shard, name, err := objectName(objectKey(record, 0))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.root, shard, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.vault.HydrateRange(h.ctx, readRequest(request.Manifest, 0, 4)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corruption error = %v", err)
	}
	if _, err := h.vault.HydrateRange(h.ctx, readRequest(request.Manifest, 0, 4)); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("quarantined read error = %v", err)
	}
}

func TestVerifyDigestTreatsMalformedStoredMetadataAsCorrupt(t *testing.T) {
	t.Parallel()
	content := []byte("authenticated")
	digest := digestBytes(content)
	for _, malformed := range []contracts.Digest{
		{Algorithm: "sha512", Hex: digest.Hex},
		{Algorithm: digest.Algorithm, Hex: digest.Hex[:len(digest.Hex)-1]},
	} {
		if err := verifyDigest(malformed, content); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("verifyDigest(%+v) error = %v", malformed, err)
		}
	}
}

func TestHistoricalLegacyMigrationAndUnreadableQuarantine(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	content := []byte("historical")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	stageAndPublish(t, h, request, content)
	h.keys.Add(request.Manifest.Tenant, material(request.Manifest.Tenant, 1, false, 1), keyring.Historical)
	h.keys.Add(request.Manifest.Tenant, material(request.Manifest.Tenant, 2, false, 2), keyring.Current)
	if _, err := h.vault.HydrateRange(h.ctx, readRequest(request.Manifest, 0, uint64(len(content)))); err != nil {
		t.Fatalf("historical read: %v", err)
	}

	legacy := []byte("legacy-plaintext")
	migration := stageRequest("tenant-a", "artifact-legacy", 1, 0, 2, legacy, 8)
	if _, err := h.vault.MigrateLegacy(h.ctx, MigrationRequest{Stage: migration}, bytes.NewReader(legacy)); err != nil {
		t.Fatalf("legacy migration stage: %v", err)
	}
	if _, err := h.vault.Publish(h.ctx, contracts.ArtifactPublishRequest{Manifest: migration.Manifest, ExpectedGeneration: 0}); err != nil {
		t.Fatalf("legacy migration publish: %v", err)
	}

	h.keys.Add(request.Manifest.Tenant, keyring.Material{Reference: material(request.Manifest.Tenant, 1, false, 1).Reference}, keyring.Unreadable)
	if _, err := h.vault.HydrateRange(h.ctx, readRequest(request.Manifest, 0, 4)); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("unreadable key error = %v", err)
	}
}

func TestRangeBoundsTombstoneAndPurge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 8)
	content := []byte("purge-me")
	request := stageRequest("tenant-a", "artifact-a", 1, 0, 1, content, 8)
	stageAndPublish(t, h, request, content)
	for _, read := range []contracts.ArtifactReadRequest{
		readRequest(request.Manifest, 0, 0),
		readRequest(request.Manifest, 0, 9),
		readRequest(request.Manifest, 7, 2),
	} {
		if _, err := h.vault.HydrateRange(h.ctx, read); !errors.Is(err, ErrInvalid) {
			t.Fatalf("range %+v error = %v", read.Range, err)
		}
	}
	tombstone, err := h.vault.Tombstone(h.ctx, contracts.TombstoneRequest{
		Tenant: request.Manifest.Tenant, Artifact: request.Manifest.Artifact, ExpectedGeneration: 1, ReasonCode: "user-delete",
	})
	if err != nil || !tombstone.Tombstoned {
		t.Fatalf("Tombstone() = (%+v, %v)", tombstone, err)
	}
	if _, err := h.vault.HydrateRange(h.ctx, readRequest(request.Manifest, 0, 4)); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("post-tombstone read error = %v", err)
	}
	purged, err := h.vault.Purge(h.ctx, contracts.PurgeRequest{
		Tenant: request.Manifest.Tenant, Artifact: request.Manifest.Artifact,
		TombstoneReceipt: tombstone.Receipt, KeyEpoch: 1,
	})
	if err != nil || !purged.Purged {
		t.Fatalf("Purge() = (%+v, %v)", purged, err)
	}
	if repeated, err := h.vault.Purge(h.ctx, contracts.PurgeRequest{
		Tenant: request.Manifest.Tenant, Artifact: request.Manifest.Artifact,
		TombstoneReceipt: tombstone.Receipt, KeyEpoch: 1,
	}); err != nil || !repeated.Purged {
		t.Fatalf("repeated Purge() = (%+v, %v)", repeated, err)
	}
	assertObjectCount(t, h.root, 0)
}

func TestPurgeBindsReceiptGenerationWhenEpochIsReused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, 8, 16)
	firstContent := []byte("first")
	first := stageRequest("tenant-a", "artifact-a", 1, 0, 1, firstContent, 8)
	stageAndPublish(t, h, first, firstContent)
	firstTombstone, err := h.vault.Tombstone(h.ctx, contracts.TombstoneRequest{
		Tenant: first.Manifest.Tenant, Artifact: first.Manifest.Artifact, ExpectedGeneration: 1, ReasonCode: "replace",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondContent := []byte("second")
	second := stageRequest("tenant-a", "artifact-a", 2, 1, 1, secondContent, 8)
	stageAndPublish(t, h, second, secondContent)
	secondTombstone, err := h.vault.Tombstone(h.ctx, contracts.TombstoneRequest{
		Tenant: second.Manifest.Tenant, Artifact: second.Manifest.Artifact, ExpectedGeneration: 2, ReasonCode: "delete",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tombstone := range []contracts.TombstoneResult{firstTombstone, secondTombstone} {
		if _, err := h.vault.Purge(h.ctx, contracts.PurgeRequest{
			Tenant: first.Manifest.Tenant, Artifact: first.Manifest.Artifact,
			TombstoneReceipt: tombstone.Receipt, KeyEpoch: 1,
		}); err != nil {
			t.Fatalf("purge reused epoch: %v", err)
		}
	}
	assertObjectCount(t, h.root, 0)
}

func TestLocalStoreRejectsShardSymlinksAndSurvivesAncestorReplacement(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	rootPath := filepath.Join(base, "objects")
	store, err := NewLocalStore(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	key := digestBytes([]byte("object-key")).Hex
	shard, _, err := objectName(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, shard)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.putIfAbsent(context.Background(), key, []byte("blocked")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shard symlink stage error = %v", err)
	}
	if err := os.Remove(filepath.Join(rootPath, shard)); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "objects-moved")
	if err := os.Rename(rootPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, rootPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.putIfAbsent(context.Background(), key, []byte("descriptor-relative")); err != nil {
		t.Fatalf("stage after ancestor replacement: %v", err)
	}
	assertObjectCount(t, outside, 0)
	assertObjectCount(t, moved, 1)
}

func TestLocalStoreRejectsShardReplacementAndSurfacesDurabilityFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	firstKey := digestBytes([]byte("first-key")).Hex
	if _, err := store.putIfAbsent(context.Background(), firstKey, []byte("first")); err != nil {
		t.Fatal(err)
	}
	shard, _, _ := objectName(firstKey)
	originalShard := filepath.Join(root, shard)
	movedShard := filepath.Join(root, shard+"-moved")
	if err := os.Rename(originalShard, movedShard); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, originalShard); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(context.Background(), firstKey); !errors.Is(err, ErrInvalid) {
		t.Fatalf("replaced shard read error = %v", err)
	}
	if err := os.Remove(originalShard); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(movedShard, originalShard); err != nil {
		t.Fatal(err)
	}

	secondDigest := digestBytes([]byte("second-key")).Hex
	secondKey := firstKey[:2] + secondDigest[2:]
	originalSync := store.syncDirectory
	store.syncDirectory = func(*os.Root) error { return errors.New("injected directory sync failure") }
	if _, err := store.putIfAbsent(context.Background(), secondKey, []byte("second")); err == nil {
		t.Fatal("publication must surface directory durability failure")
	}
	if _, err := store.read(context.Background(), secondKey); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("failed publication left object readable: %v", err)
	}
	if err := store.delete(context.Background(), firstKey); err == nil {
		t.Fatal("purge must surface directory durability failure")
	}
	store.syncDirectory = originalSync
	if err := store.delete(context.Background(), firstKey); err != nil {
		t.Fatal(err)
	}
}

func TestLengthPrefixedArtifactKeysDoNotCollide(t *testing.T) {
	t.Parallel()
	left := artifactKey(
		contracts.Identifier{Namespace: "tenant", Value: "a\x00b"},
		contracts.Identifier{Namespace: "artifact", Value: "c"},
	)
	right := artifactKey(
		contracts.Identifier{Namespace: "tenant", Value: "a"},
		contracts.Identifier{Namespace: "artifact", Value: "b\x00c"},
	)
	if left == right {
		t.Fatal("opaque composite artifact scopes collided")
	}
}

func TestHardFrameAndReadAllocationBounds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := NewMemoryRepository()
	keys := keyring.NewMemory()
	if _, err := New(store, repository, keys, Options{FrameBytes: maxFrameBytes + 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized frame option error = %v", err)
	}
	if _, err := New(store, repository, keys, Options{MaxReadBytes: maxReadBytes + 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized read option error = %v", err)
	}
	manifest := stageRequest("tenant-a", "artifact-a", 1, 0, 1, []byte("x"), 1).Manifest
	manifest.Length = maxFrameCount + 1
	manifest.FrameCount = maxFrameCount + 1
	if err := validateManifest(manifest, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized frame count error = %v", err)
	}
	key := digestBytes([]byte("oversized-object")).Hex
	shard, name, _ := objectName(key)
	if err := os.Mkdir(filepath.Join(root, shard), 0o700); err != nil {
		t.Fatal(err)
	}
	oversized, err := os.OpenFile(filepath.Join(root, shard, name), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := oversized.Truncate(int64(maxFrameBytes + frameHeaderBytes + 17)); err != nil {
		_ = oversized.Close()
		t.Fatal(err)
	}
	if err := oversized.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.read(context.Background(), key); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized object read error = %v", err)
	}
}

type harness struct {
	ctx      context.Context
	root     string
	objects  *LocalStore
	metadata *MemoryRepository
	keys     *keyring.Memory
	vault    *Vault
}

func newHarness(t *testing.T, frameSize uint32, maxRead uint64) harness {
	t.Helper()
	root := t.TempDir()
	objects, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	metadata := NewMemoryRepository()
	keys := keyring.NewMemory()
	tenant := contracts.Identifier{Namespace: "tenant", Value: "tenant-a"}
	keys.Add(tenant, material(tenant, 1, false, 1), keyring.Current)
	vault, err := New(objects, metadata, keys, Options{FrameBytes: frameSize, MaxReadBytes: maxRead, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	return harness{ctx: context.Background(), root: root, objects: objects, metadata: metadata, keys: keys, vault: vault}
}

func stageAndPublish(t *testing.T, h harness, request contracts.ArtifactStageRequest, content []byte) {
	t.Helper()
	if _, err := h.vault.StageContent(h.ctx, request, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.vault.Publish(h.ctx, contracts.ArtifactPublishRequest{Manifest: request.Manifest, ExpectedGeneration: request.ExpectedGeneration}); err != nil {
		t.Fatal(err)
	}
}

func stageRequest(tenant, artifact string, generation, expected, epoch uint64, content []byte, frameSize uint32) contracts.ArtifactStageRequest {
	digest := digestBytes(content)
	return contracts.ArtifactStageRequest{Manifest: contracts.ArtifactManifest{
		Tenant: contracts.Identifier{Namespace: "tenant", Value: tenant}, Artifact: contracts.Identifier{Namespace: "artifact", Value: artifact},
		Digest: digest, Generation: generation, KeyEpoch: epoch, Length: uint64(len(content)),
		FrameCount: uint32((len(content) + int(frameSize) - 1) / int(frameSize)),
	}, ExpectedGeneration: expected}
}

func readRequest(manifest contracts.ArtifactManifest, offset, length uint64) contracts.ArtifactReadRequest {
	return readRequestWithTenant(manifest, manifest.Tenant.Value, offset, length)
}

func readRequestWithTenant(manifest contracts.ArtifactManifest, tenant string, offset, length uint64) contracts.ArtifactReadRequest {
	return contracts.ArtifactReadRequest{
		Tenant: contracts.Identifier{Namespace: "tenant", Value: tenant}, Artifact: manifest.Artifact,
		Generation: manifest.Generation, Range: contracts.ByteRange{Offset: offset, Length: length},
	}
}

func material(tenant contracts.Identifier, epoch uint64, legacy bool, value byte) keyring.Material {
	return keyring.Material{
		Reference: contracts.KeyReference{
			Root:  contracts.Identifier{Namespace: "key-root", Value: tenant.Value},
			KeyID: contracts.Identifier{Namespace: "key", Value: "epoch-" + uintString(epoch)}, Epoch: epoch, Legacy: legacy,
		},
		RootKey: bytes.Repeat([]byte{value}, keyring.RootKeyBytes),
	}
}

func assertNoPlaintext(t *testing.T, root string, plaintext []byte) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, plaintext) {
			t.Fatalf("plaintext persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertObjectCount(t *testing.T, root string, want int) {
	t.Helper()
	count := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && !info.IsDir() {
			count++
		}
		return walkErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("object file count = %d, want %d", count, want)
	}
}

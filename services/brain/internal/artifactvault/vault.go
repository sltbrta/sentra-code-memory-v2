package artifactvault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

// Vault coordinates encrypted immutable objects with metadata publication.
// Repository owns metadata CAS; LocalStore owns ciphertext; Resolver owns key history.
type Vault struct {
	objects   objectStore
	metadata  Repository
	keys      keyring.Resolver
	frameSize uint32
	maxRead   uint64
	random    io.Reader
}

// New constructs a vault with bounded frames and reads. It returns ErrInvalid for
// missing authorities or options outside the Stage 2 storage limits.
func New(objects *LocalStore, metadata Repository, keys keyring.Resolver, options Options) (*Vault, error) {
	if objects == nil || metadata == nil || keys == nil {
		return nil, ErrInvalid
	}
	if options.FrameBytes == 0 {
		options.FrameBytes = defaultFrameBytes
	}
	if options.MaxReadBytes == 0 {
		options.MaxReadBytes = defaultMaxRead
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.FrameBytes > maxFrameBytes || options.MaxReadBytes > maxReadBytes || options.MaxReadBytes > uint64(maxInt()) {
		return nil, ErrInvalid
	}
	return &Vault{
		objects: objects, metadata: metadata, keys: keys,
		frameSize: options.FrameBytes, maxRead: options.MaxReadBytes, random: options.Random,
	}, nil
}

// StageContent encrypts an exact-length stream into independently authenticated frames.
// Generation CAS is enforced by Repository. The upstream authority must already have
// validated the caller's capability fence because the frozen ArtifactStageRequest has no fence.
func (v *Vault) StageContent(ctx context.Context, request contracts.ArtifactStageRequest, reader io.Reader) (contracts.Receipt, error) {
	if reader == nil || validateStage(request, v.frameSize) != nil {
		return contracts.Receipt{}, ErrInvalid
	}
	material, err := v.keys.Current(ctx, request.Manifest.Tenant)
	if err != nil || material.Reference.Legacy || material.Reference.Epoch != request.Manifest.KeyEpoch {
		clear(material.RootKey)
		return contracts.Receipt{}, ErrQuarantined
	}
	defer clear(material.RootKey)
	locator, err := v.newLocator()
	if err != nil {
		return contracts.Receipt{}, err
	}
	record, duplicate, err := v.metadata.BeginStage(ctx, request, locator)
	if err != nil {
		return contracts.Receipt{}, err
	}
	if duplicate {
		return receipt(record, "staged", "OURO-ARTIFACT-DUPLICATE"), nil
	}
	created := make([]string, 0, request.Manifest.FrameCount)
	abort := func(stageErr error) (contracts.Receipt, error) {
		cleanupErrors := []error{stageErr}
		for _, key := range created {
			if cleanupErr := v.objects.delete(context.Background(), key); cleanupErr != nil {
				cleanupErrors = append(cleanupErrors, cleanupErr)
			}
		}
		if cleanupErr := v.metadata.AbortStage(context.Background(), record); cleanupErr != nil {
			cleanupErrors = append(cleanupErrors, cleanupErr)
		}
		return contracts.Receipt{}, errors.Join(cleanupErrors...)
	}
	hasher := sha256.New()
	remaining := request.Manifest.Length
	for index := uint32(0); index < request.Manifest.FrameCount; index++ {
		length := minUint64(uint64(v.frameSize), remaining)
		plaintext := make([]byte, int(length))
		if _, err := io.ReadFull(reader, plaintext); err != nil {
			clear(plaintext)
			return abort(ErrIncomplete)
		}
		_, _ = hasher.Write(plaintext)
		envelope, err := encryptFrame(v.random, material.RootKey, record, index, plaintext)
		clear(plaintext)
		if err != nil {
			return abort(err)
		}
		key := objectKey(record, index)
		wasCreated, err := v.objects.putIfAbsent(ctx, key, envelope)
		if err != nil {
			clear(envelope)
			return abort(err)
		}
		if wasCreated {
			created = append(created, key)
		}
		record.Frames = append(record.Frames, FrameRecord{
			Index: index, Offset: request.Manifest.Length - remaining, Length: uint32(length), ObjectDigest: digestBytes(envelope),
		})
		clear(envelope)
		remaining -= length
	}
	extra := make([]byte, 1)
	read, readErr := io.ReadFull(reader, extra)
	if read > 0 || !errors.Is(readErr, io.EOF) || remaining != 0 {
		return abort(ErrIncomplete)
	}
	actualDigest := hex.EncodeToString(hasher.Sum(nil))
	if request.Manifest.Digest.Algorithm != "sha256" || actualDigest != request.Manifest.Digest.Hex {
		return abort(ErrCorrupt)
	}
	if err := v.metadata.CompleteStage(ctx, record); err != nil {
		return abort(err)
	}
	return receipt(record, "staged", "OURO-ARTIFACT-STAGED"), nil
}

// Stage verifies that encrypted frames already staged through StageContent are complete.
// This method satisfies the frozen contracts.ArtifactVault port without accepting bytes.
func (v *Vault) Stage(ctx context.Context, request contracts.ArtifactStageRequest) (contracts.Receipt, error) {
	if validateStage(request, v.frameSize) != nil {
		return contracts.Receipt{}, ErrInvalid
	}
	record, err := v.metadata.Get(ctx, request.Manifest.Tenant, request.Manifest.Artifact, request.Manifest.Generation)
	if err != nil {
		return contracts.Receipt{}, err
	}
	if !sameManifest(record.Manifest, request.Manifest) || len(record.Frames) != int(record.Manifest.FrameCount) || record.Status != StatusStaged {
		return contracts.Receipt{}, ErrIncomplete
	}
	return receipt(record, "staged", "OURO-ARTIFACT-STAGED"), nil
}

// Publish verifies every object and content digest before conditionally advancing current generation.
func (v *Vault) Publish(ctx context.Context, request contracts.ArtifactPublishRequest) (contracts.Receipt, error) {
	if validateManifest(request.Manifest, v.frameSize) != nil || request.Manifest.Generation != request.ExpectedGeneration+1 {
		return contracts.Receipt{}, ErrInvalid
	}
	record, err := v.metadata.Get(ctx, request.Manifest.Tenant, request.Manifest.Artifact, request.Manifest.Generation)
	if err != nil {
		return contracts.Receipt{}, err
	}
	if !sameManifest(record.Manifest, request.Manifest) {
		return contracts.Receipt{}, ErrConflict
	}
	if record.Status != StatusStaged && record.Status != StatusPublished {
		return contracts.Receipt{}, ErrIncomplete
	}
	if err := v.verifyGeneration(ctx, record); err != nil {
		return contracts.Receipt{}, err
	}
	published, err := v.metadata.Publish(ctx, request)
	if err != nil {
		return contracts.Receipt{}, err
	}
	return receipt(published, "published", "OURO-ARTIFACT-PUBLISHED"), nil
}

// HydrateRange returns only the requested authenticated plaintext bytes and frame metadata.
// Reads are bounded before object lookup and fail closed for non-published lifecycle states.
func (v *Vault) HydrateRange(ctx context.Context, request contracts.ArtifactReadRequest) (HydratedRange, error) {
	if err := validateRead(request, v.maxRead); err != nil {
		return HydratedRange{}, err
	}
	record, err := v.metadata.Get(ctx, request.Tenant, request.Artifact, request.Generation)
	if err != nil {
		return HydratedRange{}, err
	}
	if err := readable(record.Status); err != nil {
		return HydratedRange{}, err
	}
	if request.Range.Offset > record.Manifest.Length || request.Range.Length > record.Manifest.Length-request.Range.Offset {
		return HydratedRange{}, ErrInvalid
	}
	material, err := v.resolveForRead(ctx, record)
	if err != nil {
		return HydratedRange{}, err
	}
	defer clear(material.RootKey)
	end := request.Range.Offset + request.Range.Length
	result := HydratedRange{Bytes: make([]byte, 0, int(request.Range.Length))}
	result.Metadata.Manifest = record.Manifest
	for _, frame := range record.Frames {
		frameEnd := frame.Offset + uint64(frame.Length)
		if frameEnd <= request.Range.Offset || frame.Offset >= end {
			continue
		}
		plaintext, frameErr := v.readFrame(ctx, material.RootKey, record, frame)
		if frameErr != nil {
			clear(result.Bytes)
			_ = v.metadata.Quarantine(context.Background(), record.Manifest.Tenant, record.Manifest.Artifact, record.Manifest.Generation, "integrity")
			return HydratedRange{}, frameErr
		}
		startInFrame := maxUint64(request.Range.Offset, frame.Offset) - frame.Offset
		endInFrame := minUint64(end, frameEnd) - frame.Offset
		result.Bytes = append(result.Bytes, plaintext[startInFrame:endInFrame]...)
		clear(plaintext)
		result.Metadata.FrameDigests = append(result.Metadata.FrameDigests, frame.ObjectDigest)
	}
	if uint64(len(result.Bytes)) != request.Range.Length {
		clear(result.Bytes)
		return HydratedRange{}, ErrIncomplete
	}
	result.Metadata.NextOffset = end
	return result, nil
}

// ReadRange verifies and returns frozen-port metadata without retaining hydrated bytes.
func (v *Vault) ReadRange(ctx context.Context, request contracts.ArtifactReadRequest) (contracts.ArtifactReadResult, error) {
	hydrated, err := v.HydrateRange(ctx, request)
	if err != nil {
		return contracts.ArtifactReadResult{}, err
	}
	clear(hydrated.Bytes)
	return hydrated.Metadata, nil
}

// Reconcile verifies one observed generation's immutable manifest and object set.
func (v *Vault) Reconcile(ctx context.Context, request contracts.ArtifactReconcileRequest) (contracts.Receipt, error) {
	if validateID(request.Tenant, "tenant") != nil || validateID(request.Artifact, "artifact") != nil || request.ObservedGeneration == 0 {
		return contracts.Receipt{}, ErrInvalid
	}
	record, err := v.metadata.Get(ctx, request.Tenant, request.Artifact, request.ObservedGeneration)
	if err != nil {
		return contracts.Receipt{}, err
	}
	if err := readable(record.Status); err != nil && !errors.Is(err, ErrIncomplete) {
		return contracts.Receipt{}, err
	}
	if err := v.verifyGeneration(ctx, record); err != nil {
		return contracts.Receipt{}, err
	}
	return receipt(record, "verified", "OURO-ARTIFACT-RECONCILED"), nil
}

// Tombstone applies immediate metadata denial before any physical object deletion.
func (v *Vault) Tombstone(ctx context.Context, request contracts.TombstoneRequest) (contracts.TombstoneResult, error) {
	if validateID(request.Tenant, "tenant") != nil || validateID(request.Artifact, "artifact") != nil || request.ExpectedGeneration == 0 || request.ReasonCode == "" {
		return contracts.TombstoneResult{}, ErrInvalid
	}
	record, err := v.metadata.Tombstone(ctx, request)
	if err != nil {
		return contracts.TombstoneResult{}, err
	}
	result := contracts.TombstoneResult{Tombstoned: true, Receipt: receipt(record, "tombstoned", "OURO-ARTIFACT-TOMBSTONED")}
	return result, nil
}

// Purge idempotently deletes every scoped primary object after a tombstone receipt,
// then marks metadata purged. It makes no claim about backups or external replicas.
func (v *Vault) Purge(ctx context.Context, request contracts.PurgeRequest) (contracts.PurgeResult, error) {
	if validateID(request.Tenant, "tenant") != nil || validateID(request.Artifact, "artifact") != nil || request.KeyEpoch == 0 || request.TombstoneReceipt.Status != "tombstoned" {
		return contracts.PurgeResult{}, ErrInvalid
	}
	if request.TombstoneReceipt.OperationID.Namespace != "artifact-operation" {
		return contracts.PurgeResult{}, ErrInvalid
	}
	operationParts, err := decodeComposite(request.TombstoneReceipt.OperationID.Value, 2)
	if err != nil || operationParts[0] != request.Artifact.Value {
		return contracts.PurgeResult{}, ErrInvalid
	}
	generation, err := strconv.ParseUint(operationParts[1], 10, 64)
	if err != nil || generation == 0 {
		return contracts.PurgeResult{}, ErrInvalid
	}
	record, err := v.metadata.PreparePurge(ctx, request, generation)
	if err != nil {
		return contracts.PurgeResult{}, err
	}
	if record.Status == StatusPurged {
		return contracts.PurgeResult{Purged: true, Receipt: receipt(record, "purged", "OURO-ARTIFACT-PURGED")}, nil
	}
	for _, frame := range record.Frames {
		if err := v.objects.delete(ctx, objectKey(record, frame.Index)); err != nil {
			return contracts.PurgeResult{}, err
		}
	}
	if err := v.metadata.CompletePurge(ctx, record); err != nil {
		return contracts.PurgeResult{}, err
	}
	return contracts.PurgeResult{Purged: true, Receipt: receipt(record, "purged", "OURO-ARTIFACT-PURGED")}, nil
}

// MigrateLegacy imports authorized legacy plaintext into a fresh encrypted generation.
// The caller remains responsible for tombstoning and purging the legacy source afterward.
func (v *Vault) MigrateLegacy(ctx context.Context, request MigrationRequest, plaintext io.Reader) (contracts.Receipt, error) {
	return v.StageContent(ctx, request.Stage, plaintext)
}

func (v *Vault) verifyGeneration(ctx context.Context, record GenerationRecord) error {
	if len(record.Frames) != int(record.Manifest.FrameCount) {
		return ErrIncomplete
	}
	material, err := v.resolveForRead(ctx, record)
	if err != nil {
		return err
	}
	defer clear(material.RootKey)
	hasher := sha256.New()
	var expectedOffset uint64
	for index, frame := range record.Frames {
		if frame.Index != uint32(index) || frame.Offset != expectedOffset || frame.Length == 0 {
			return ErrIncomplete
		}
		plaintext, frameErr := v.readFrame(ctx, material.RootKey, record, frame)
		if frameErr != nil {
			_ = v.metadata.Quarantine(context.Background(), record.Manifest.Tenant, record.Manifest.Artifact, record.Manifest.Generation, "integrity")
			return frameErr
		}
		_, _ = hasher.Write(plaintext)
		clear(plaintext)
		expectedOffset += uint64(frame.Length)
	}
	if expectedOffset != record.Manifest.Length || hex.EncodeToString(hasher.Sum(nil)) != record.Manifest.Digest.Hex {
		_ = v.metadata.Quarantine(context.Background(), record.Manifest.Tenant, record.Manifest.Artifact, record.Manifest.Generation, "digest")
		return ErrCorrupt
	}
	return nil
}

func (v *Vault) readFrame(ctx context.Context, root []byte, record GenerationRecord, frame FrameRecord) ([]byte, error) {
	envelope, err := v.objects.read(ctx, objectKey(record, frame.Index))
	if err != nil {
		return nil, err
	}
	defer clear(envelope)
	if verifyDigest(frame.ObjectDigest, envelope) != nil {
		return nil, ErrCorrupt
	}
	return decryptFrame(root, record, frame, envelope)
}

func (v *Vault) resolveForRead(ctx context.Context, record GenerationRecord) (keyring.Material, error) {
	material, err := v.keys.Resolve(ctx, record.Manifest.Tenant, record.Manifest.KeyEpoch)
	if err != nil {
		_ = v.metadata.Quarantine(context.Background(), record.Manifest.Tenant, record.Manifest.Artifact, record.Manifest.Generation, "key-unreadable")
		return keyring.Material{}, ErrQuarantined
	}
	return material, nil
}

func (v *Vault) newLocator() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(v.random, value); err != nil {
		return "", fmt.Errorf("artifactvault: generate locator: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func validateStage(request contracts.ArtifactStageRequest, frameSize uint32) error {
	if request.ExpectedGeneration == math.MaxUint64 || request.Manifest.Generation != request.ExpectedGeneration+1 {
		return ErrInvalid
	}
	return validateManifest(request.Manifest, frameSize)
}

func validateManifest(manifest contracts.ArtifactManifest, frameSize uint32) error {
	if validateID(manifest.Tenant, "tenant") != nil || validateID(manifest.Artifact, "artifact") != nil || manifest.Generation == 0 || manifest.KeyEpoch == 0 || manifest.Length == 0 || manifest.Length > maxArtifactBytes {
		return ErrInvalid
	}
	if manifest.Digest.Algorithm != "sha256" || len(manifest.Digest.Hex) != sha256.Size*2 {
		return ErrInvalid
	}
	expectedFrames := (manifest.Length + uint64(frameSize) - 1) / uint64(frameSize)
	if expectedFrames == 0 || expectedFrames > maxFrameCount || manifest.FrameCount != uint32(expectedFrames) {
		return ErrInvalid
	}
	return nil
}

func validateRead(request contracts.ArtifactReadRequest, maxRead uint64) error {
	if validateID(request.Tenant, "tenant") != nil || validateID(request.Artifact, "artifact") != nil || request.Generation == 0 || request.Range.Length == 0 || request.Range.Length > maxRead || request.Range.Offset > math.MaxUint64-request.Range.Length {
		return ErrInvalid
	}
	return nil
}

func validateID(identifier contracts.Identifier, namespace string) error {
	if identifier.Namespace != namespace || identifier.Value == "" || len(identifier.Value) > 512 {
		return ErrInvalid
	}
	return nil
}

func readable(status Status) error {
	switch status {
	case StatusPublished:
		return nil
	case StatusStaged:
		return ErrIncomplete
	case StatusTombstoned, StatusPurged:
		return ErrTombstoned
	case StatusQuarantined:
		return ErrQuarantined
	default:
		return ErrQuarantined
	}
}

func receipt(record GenerationRecord, status, reason string) contracts.Receipt {
	return contracts.Receipt{
		OperationID: contracts.Identifier{Namespace: "artifact-operation", Value: encodeComposite(record.Manifest.Artifact.Value, uintString(record.Manifest.Generation))},
		Status:      status, ReasonCode: reason, Watermark: record.Manifest.Generation,
	}
}

func minUint64(left, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}

func maxUint64(left, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

var _ contracts.ArtifactVault = (*Vault)(nil)

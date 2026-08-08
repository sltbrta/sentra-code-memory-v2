package companymode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"
)

var (
	// ErrDrillFailed reports that a disposable recovery target was not verified.
	ErrDrillFailed = errors.New("companymode: recovery drill failed")
	// ErrInjectedDrillFailure distinguishes an intentional drill interruption.
	ErrInjectedDrillFailure = errors.New("companymode: injected recovery drill failure")
	errDrillLimit           = errors.New("companymode: recovery drill limit exceeded")
	errTargetNotEmpty       = errors.New("companymode: recovery target not empty")
	errTargetProbeFailed    = errors.New("companymode: recovery target probe failed")
)

// DrillFailurePoint names deterministic interruptions between substrate steps.
type DrillFailurePoint string

const (
	FailureNone               DrillFailurePoint = ""
	FailureAfterSnapshot      DrillFailurePoint = "after_snapshot"
	FailureAfterEventRestore  DrillFailurePoint = "after_event_restore"
	FailureAfterObjectRestore DrillFailurePoint = "after_object_restore"
)

const (
	defaultMaxDrillEvents    = 10_000
	defaultMaxDrillBlobs     = 10_000
	defaultMaxDrillBlobBytes = int64(64 << 20)
)

// DrillLimits bound material copied into a disposable recovery target.
type DrillLimits struct {
	MaxEvents    int
	MaxBlobs     int
	MaxBlobBytes int64
}

// ErasureMarker is the current tombstone overlay applied to an older backup.
// ObjectDigest fences key reuse; the receipt is opaque and contains no payload.
type ErasureMarker struct {
	TenantID           string `json:"tenant_id"`
	Key                string `json:"key"`
	ObjectDigest       string `json:"object_digest"`
	TombstoneReceiptID string `json:"tombstone_receipt_id"`
	TombstonedAt       string `json:"tombstoned_at"`
	BackupEraseHorizon string `json:"backup_erase_horizon"`
}

// DrillRequest identifies one deterministic, disposable recovery exercise.
type DrillRequest struct {
	DrillID   string
	Profile   ProfileID
	TenantID  string
	StartedAt time.Time
	Limits    DrillLimits
	Erasures  []ErasureMarker
	FailAt    DrillFailurePoint
}

// DrillCheck is one non-payload-bearing step receipt.
type DrillCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

// DrillErasureReceipt proves only that a tombstoned payload was not made
// readable in the disposable restore target.
type DrillErasureReceipt struct {
	TombstoneReceiptID  string `json:"tombstone_receipt_id"`
	ObjectDigest        string `json:"object_digest"`
	ExcludedFromRestore bool   `json:"excluded_from_restore"`
	AbsenceVerified     bool   `json:"absence_verified"`
	HorizonElapsed      bool   `json:"horizon_elapsed"`
}

// DrillReceipt records verification without containing event or object bytes.
type DrillReceipt struct {
	DrillID                string                `json:"drill_id"`
	Profile                ProfileID             `json:"profile"`
	TenantID               string                `json:"tenant_id"`
	StartedAt              string                `json:"started_at"`
	Status                 string                `json:"status"`
	FailurePoint           DrillFailurePoint     `json:"failure_point,omitempty"`
	FailureCode            string                `json:"failure_code,omitempty"`
	BackupDigest           string                `json:"backup_digest,omitempty"`
	EventCount             int                   `json:"event_count"`
	BackupBlobCount        int                   `json:"backup_blob_count"`
	RestoredBlobCount      int                   `json:"restored_blob_count"`
	Verified               bool                  `json:"verified"`
	ProductionErasureClaim bool                  `json:"production_erasure_claim"`
	Checks                 []DrillCheck          `json:"checks"`
	Erasures               []DrillErasureReceipt `json:"erasures,omitempty"`
}

// RunRecoveryDrill snapshots both canonical substrates, restores into empty
// disposable targets, applies current tombstones, and verifies exact contents.
// Callers must not publish or promote a target unless the receipt is verified.
func RunRecoveryDrill(
	ctx context.Context,
	request DrillRequest,
	sourceEvents EventStore,
	sourceObjects ObjectStore,
	targetEvents EventStore,
	targetObjects ObjectStore,
) (DrillReceipt, error) {
	receipt := DrillReceipt{
		DrillID: request.DrillID, Profile: request.Profile, TenantID: request.TenantID,
		StartedAt: request.StartedAt.UTC().Format(time.RFC3339Nano), Status: "failed",
		ProductionErasureClaim: false,
	}
	if err := validateDrillRequest(request, sourceEvents, sourceObjects, targetEvents, targetObjects); err != nil {
		return failDrill(receipt, "request", "invalid_request", err)
	}

	limits := normalizedLimits(request.Limits)
	backup, err := createBackupAt(ctx, request.Profile, request.TenantID, sourceEvents, sourceObjects, request.StartedAt, &limits)
	if err != nil {
		if errors.Is(err, errDrillLimit) {
			return failDrill(receipt, "bounds", "limit_exceeded", err)
		}
		return failDrill(receipt, "source_snapshot", "snapshot_failed", err)
	}
	receipt.BackupDigest = backup.Manifest.ContentDigest
	receipt.EventCount = len(backup.Events)
	receipt.BackupBlobCount = len(backup.Blobs)
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "source_snapshot", Status: "passed"})
	if request.FailAt == FailureAfterSnapshot {
		return injectedDrillFailure(receipt, request.FailAt)
	}
	if err := checkDrillBounds(backup, limits); err != nil {
		return failDrill(receipt, "bounds", "limit_exceeded", err)
	}
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "bounds", Status: "passed"})
	if err := VerifyBackup(backup); err != nil {
		return failDrill(receipt, "manifest_integrity", "manifest_rejected", err)
	}
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "manifest_integrity", Status: "passed"})

	restoredBlobs, erasures, err := applyErasureOverlay(backup.Blobs, request.Erasures, request.StartedAt)
	if err != nil {
		return failDrill(receipt, "erasure_overlay", "overlay_rejected", err)
	}
	receipt.Erasures = erasures
	receipt.RestoredBlobCount = len(restoredBlobs)
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "erasure_overlay", Status: "passed"})
	if err := requireEmptyTargets(ctx, targetEvents, targetObjects); err != nil {
		code := "target_probe_failed"
		if errors.Is(err, errTargetNotEmpty) {
			code = "target_not_empty"
		}
		return failDrill(receipt, "target_isolation", code, err)
	}
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "target_isolation", Status: "passed"})

	if err := targetEvents.Restore(ctx, backup.Events); err != nil {
		return failDrill(receipt, "event_restore", "event_restore_failed", err)
	}
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "event_restore", Status: "passed"})
	if request.FailAt == FailureAfterEventRestore {
		return injectedDrillFailure(receipt, request.FailAt)
	}
	if err := targetObjects.Restore(ctx, restoredBlobs); err != nil {
		return failDrill(receipt, "object_restore", "object_restore_failed", err)
	}
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "object_restore", Status: "passed"})
	if request.FailAt == FailureAfterObjectRestore {
		return injectedDrillFailure(receipt, request.FailAt)
	}

	if err := verifyDrillTarget(ctx, targetEvents, targetObjects, backup.Events, restoredBlobs, request.Erasures); err != nil {
		return failDrill(receipt, "final_verification", "verification_failed", err)
	}
	for index := range receipt.Erasures {
		receipt.Erasures[index].AbsenceVerified = true
	}
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: "final_verification", Status: "passed"})
	receipt.Verified = true
	receipt.Status = "passed"
	return receipt, nil
}

func validateDrillRequest(request DrillRequest, stores ...any) error {
	if request.DrillID == "" || request.TenantID == "" || request.StartedAt.IsZero() ||
		(request.Profile != ProfileLocal && request.Profile != ProfileCompany) {
		return ErrRejected
	}
	for _, store := range stores {
		if isNilStore(store) {
			return ErrRejected
		}
	}
	switch request.FailAt {
	case FailureNone, FailureAfterSnapshot, FailureAfterEventRestore, FailureAfterObjectRestore:
		return nil
	default:
		return ErrRejected
	}
}

func isNilStore(store any) bool {
	if store == nil {
		return true
	}
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func normalizedLimits(limits DrillLimits) DrillLimits {
	if limits.MaxEvents == 0 {
		limits.MaxEvents = defaultMaxDrillEvents
	}
	if limits.MaxBlobs == 0 {
		limits.MaxBlobs = defaultMaxDrillBlobs
	}
	if limits.MaxBlobBytes == 0 {
		limits.MaxBlobBytes = defaultMaxDrillBlobBytes
	}
	return limits
}

func checkDrillBounds(backup Backup, limits DrillLimits) error {
	if limits.MaxEvents < 0 || limits.MaxBlobs < 0 || limits.MaxBlobBytes < 0 ||
		len(backup.Events) > limits.MaxEvents || len(backup.Blobs) > limits.MaxBlobs {
		return ErrRejected
	}
	var total int64
	for _, blob := range backup.Blobs {
		if blob.Ref.Length > limits.MaxBlobBytes-total {
			return ErrRejected
		}
		total += blob.Ref.Length
	}
	return nil
}

func applyErasureOverlay(blobs []BlobObject, markers []ErasureMarker, now time.Time) ([]BlobObject, []DrillErasureReceipt, error) {
	byObject := make(map[string]ErasureMarker, len(markers))
	receipts := make([]DrillErasureReceipt, 0, len(markers))
	for _, marker := range markers {
		tombstonedAt, tombstoneErr := time.Parse(time.RFC3339Nano, marker.TombstonedAt)
		horizon, horizonErr := time.Parse(time.RFC3339Nano, marker.BackupEraseHorizon)
		identity := objectKey(marker.TenantID, marker.Key)
		if marker.TenantID == "" || marker.Key == "" || marker.ObjectDigest == "" ||
			marker.TombstoneReceiptID == "" || tombstoneErr != nil || horizonErr != nil ||
			tombstonedAt.After(now) || horizon.Before(tombstonedAt) {
			return nil, nil, ErrRejected
		}
		if _, duplicate := byObject[identity]; duplicate {
			return nil, nil, ErrRejected
		}
		byObject[identity] = marker
		receipts = append(receipts, DrillErasureReceipt{
			TombstoneReceiptID:  marker.TombstoneReceiptID,
			ObjectDigest:        marker.ObjectDigest,
			ExcludedFromRestore: true,
			HorizonElapsed:      !now.Before(horizon),
		})
	}
	found := make(map[string]bool, len(markers))
	restored := make([]BlobObject, 0, len(blobs))
	for _, blob := range blobs {
		identity := objectKey(blob.Ref.TenantID, blob.Ref.Key)
		marker, erased := byObject[identity]
		if !erased {
			restored = append(restored, blob)
			continue
		}
		if marker.ObjectDigest != blob.Ref.Digest {
			return nil, nil, ErrRejected
		}
		found[identity] = true
	}
	for identity := range byObject {
		if !found[identity] {
			return nil, nil, ErrRejected
		}
	}
	return restored, receipts, nil
}

func requireEmptyTargets(ctx context.Context, events EventStore, objects ObjectStore) error {
	existingEvents, err := events.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("%w: event store: %v", errTargetProbeFailed, err)
	}
	if len(existingEvents) != 0 {
		return errTargetNotEmpty
	}
	existingObjects, err := objects.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("%w: object store: %v", errTargetProbeFailed, err)
	}
	if len(existingObjects) != 0 {
		return errTargetNotEmpty
	}
	return nil
}

func verifyDrillTarget(ctx context.Context, events EventStore, objects ObjectStore, wantEvents []Event, wantBlobs []BlobObject, erasures []ErasureMarker) error {
	gotEvents, err := events.Snapshot(ctx)
	if err != nil {
		return err
	}
	sortEvents(gotEvents)
	wantEventsCopy := append([]Event(nil), wantEvents...)
	sortEvents(wantEventsCopy)
	gotEventBytes, err := backupContent(gotEvents, nil)
	if err != nil {
		return err
	}
	wantEventBytes, err := backupContent(wantEventsCopy, nil)
	if err != nil || !bytes.Equal(gotEventBytes, wantEventBytes) {
		return ErrRejected
	}
	inventory, err := objects.Inventory(ctx)
	if err != nil || len(inventory) != len(wantBlobs) {
		return ErrRejected
	}
	want := make(map[string]BlobObject, len(wantBlobs))
	for _, blob := range wantBlobs {
		want[objectKey(blob.Ref.TenantID, blob.Ref.Key)] = blob
	}
	seen := make(map[string]struct{}, len(inventory))
	for _, ref := range inventory {
		identity := objectKey(ref.TenantID, ref.Key)
		blob, ok := want[identity]
		if _, duplicate := seen[identity]; !ok || duplicate || ref != blob.Ref {
			return ErrRejected
		}
		seen[identity] = struct{}{}
		body, err := objects.Get(ctx, ref.TenantID, ref.Key)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if int64(len(body)) != ref.Length || hex.EncodeToString(sum[:]) != ref.Digest {
			return ErrRejected
		}
	}
	for _, marker := range erasures {
		if _, err := objects.Get(ctx, marker.TenantID, marker.Key); !errors.Is(err, ErrDenied) {
			return ErrRejected
		}
	}
	return nil
}

func injectedDrillFailure(receipt DrillReceipt, point DrillFailurePoint) (DrillReceipt, error) {
	receipt.FailurePoint = point
	receipt.FailureCode = "injected_failure"
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: string(point), Status: "failed", Code: "injected_failure"})
	return receipt, fmt.Errorf("%w: %w at %s", ErrDrillFailed, ErrInjectedDrillFailure, point)
}

func failDrill(receipt DrillReceipt, check, code string, cause error) (DrillReceipt, error) {
	receipt.FailureCode = code
	receipt.Checks = append(receipt.Checks, DrillCheck{Name: check, Status: "failed", Code: code})
	return receipt, fmt.Errorf("%w: %s: %v", ErrDrillFailed, check, cause)
}

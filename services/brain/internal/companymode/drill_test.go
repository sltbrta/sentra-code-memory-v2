package companymode_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/companymode"
)

func TestRecoveryDrillIsDeterministicAndDoesNotResurrectErasedBlob(t *testing.T) {
	ctx := context.Background()
	events := companymode.NewFakePostgres()
	objects := companymode.NewFakeS3()
	appendEvent(t, events, "tenant-b", "event-b", "digest-b")
	appendEvent(t, events, "tenant-a", "event-a", "digest-a")
	erasedRef := putBlob(t, objects, "tenant-a", "erased", "erased payload")
	putBlob(t, objects, "tenant-a", "retained", "retained payload")
	startedAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	request := companymode.DrillRequest{
		DrillID: "drill-001", Profile: companymode.ProfileCompany, TenantID: "tenant-a", StartedAt: startedAt,
		Erasures: []companymode.ErasureMarker{{
			TenantID: "tenant-a", Key: "erased", ObjectDigest: erasedRef.Digest,
			TombstoneReceiptID: "tombstone-receipt-001",
			TombstonedAt:       startedAt.Add(-time.Hour).Format(time.RFC3339Nano),
			BackupEraseHorizon: startedAt.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
		}},
	}

	firstEvents, firstObjects := companymode.NewFakePostgres(), companymode.NewFakeS3()
	first, err := companymode.RunRecoveryDrill(ctx, request, events, objects, firstEvents, firstObjects)
	if err != nil {
		t.Fatal(err)
	}
	second, err := companymode.RunRecoveryDrill(ctx, request, events, objects, companymode.NewFakePostgres(), companymode.NewFakeS3())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("receipts differ:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if first.Status != "passed" || !first.Verified || first.ProductionErasureClaim {
		t.Fatalf("receipt = %+v", first)
	}
	if first.EventCount != 1 || first.BackupBlobCount != 2 || first.RestoredBlobCount != 1 {
		t.Fatalf("counts = %+v", first)
	}
	if len(first.Erasures) != 1 || !first.Erasures[0].ExcludedFromRestore ||
		!first.Erasures[0].AbsenceVerified || first.Erasures[0].HorizonElapsed {
		t.Fatalf("erasure receipt = %+v", first.Erasures)
	}
	if _, err := firstObjects.Get(ctx, "tenant-a", "erased"); !errors.Is(err, companymode.ErrDenied) {
		t.Fatalf("erased payload readable: %v", err)
	}
	body, err := firstObjects.Get(ctx, "tenant-a", "retained")
	if err != nil || string(body) != "retained payload" {
		t.Fatalf("retained payload = %q, %v", body, err)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "erased payload") || strings.Contains(string(encoded), "retained payload") {
		t.Fatalf("receipt disclosed payload: %s", encoded)
	}
}

func TestRestoreBackupRejectsTamperingBeforeTargetMutation(t *testing.T) {
	ctx := context.Background()
	events := companymode.NewFakePostgres()
	objects := companymode.NewFakeS3()
	appendEvent(t, events, "tenant-a", "event-a", "digest-a")
	putBlob(t, objects, "tenant-a", "object-a", "original")
	backup, err := companymode.CreateBackup(ctx, companymode.ProfileCompany, "tenant-a", events, objects)
	if err != nil {
		t.Fatal(err)
	}
	backup.Blobs[0].Body[0] ^= 0xff
	targetEvents, targetObjects := companymode.NewFakePostgres(), companymode.NewFakeS3()
	if err := companymode.RestoreBackup(ctx, targetEvents, targetObjects, backup); !errors.Is(err, companymode.ErrRejected) {
		t.Fatalf("tampered restore = %v", err)
	}
	if snapshot, err := targetEvents.Snapshot(ctx); err != nil || len(snapshot) != 0 {
		t.Fatalf("target events mutated: %+v, %v", snapshot, err)
	}
	if inventory, err := targetObjects.Inventory(ctx); err != nil || len(inventory) != 0 {
		t.Fatalf("target objects mutated: %+v, %v", inventory, err)
	}
}

func TestRecoveryDrillInjectedFailureNeverVerifiesTarget(t *testing.T) {
	ctx := context.Background()
	events := companymode.NewFakePostgres()
	objects := companymode.NewFakeS3()
	appendEvent(t, events, "tenant-a", "event-a", "digest-a")
	putBlob(t, objects, "tenant-a", "object-a", "payload")
	points := []companymode.DrillFailurePoint{
		companymode.FailureAfterSnapshot,
		companymode.FailureAfterEventRestore,
		companymode.FailureAfterObjectRestore,
	}
	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			request := companymode.DrillRequest{
				DrillID: "drill-injected", Profile: companymode.ProfileCompany, TenantID: "tenant-a",
				StartedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), FailAt: point,
			}
			receipt, err := companymode.RunRecoveryDrill(
				ctx, request, events, objects, companymode.NewFakePostgres(), companymode.NewFakeS3(),
			)
			if !errors.Is(err, companymode.ErrDrillFailed) || !errors.Is(err, companymode.ErrInjectedDrillFailure) {
				t.Fatalf("injected failure = %v", err)
			}
			if receipt.Status != "failed" || receipt.Verified || receipt.FailurePoint != point ||
				receipt.FailureCode != "injected_failure" {
				t.Fatalf("receipt = %+v", receipt)
			}
		})
	}
}

func TestRecoveryDrillDistinguishesTargetProbeFailureFromOccupiedTarget(t *testing.T) {
	ctx := context.Background()
	sourceEvents := companymode.NewFakePostgres()
	sourceObjects := companymode.NewFakeS3()
	appendEvent(t, sourceEvents, "tenant-a", "source-event", "source")
	putBlob(t, sourceObjects, "tenant-a", "source-object", "source")
	request := companymode.DrillRequest{
		DrillID: "drill-target-isolation", Profile: companymode.ProfileCompany, TenantID: "tenant-a",
		StartedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	}

	tests := []struct {
		name     string
		mode     string
		wantCode string
	}{
		{name: "event probe failure", mode: "event_probe_failure", wantCode: "target_probe_failed"},
		{name: "object probe failure", mode: "object_probe_failure", wantCode: "target_probe_failed"},
		{name: "existing event", mode: "existing_event", wantCode: "target_not_empty"},
		{name: "existing object", mode: "existing_object", wantCode: "target_not_empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targetEventState := companymode.NewFakePostgres()
			targetObjectState := companymode.NewFakeS3()
			var targetEvents companymode.EventStore = targetEventState
			var targetObjects companymode.ObjectStore = targetObjectState
			switch test.mode {
			case "event_probe_failure":
				targetEvents = &snapshotFailingEventStore{FakePostgres: targetEventState}
			case "object_probe_failure":
				targetObjects = &inventoryFailingObjectStore{FakeS3: targetObjectState}
			case "existing_event":
				appendEvent(t, targetEventState, "tenant-a", "existing-event", "existing")
			case "existing_object":
				putBlob(t, targetObjectState, "tenant-a", "existing-object", "existing")
			default:
				t.Fatalf("unknown test mode %q", test.mode)
			}
			beforeEvents, err := targetEventState.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			beforeObjects, err := targetObjectState.Inventory(ctx)
			if err != nil {
				t.Fatal(err)
			}

			receipt, err := companymode.RunRecoveryDrill(
				ctx, request, sourceEvents, sourceObjects, targetEvents, targetObjects,
			)
			if !errors.Is(err, companymode.ErrDrillFailed) || receipt.FailureCode != test.wantCode || receipt.Verified {
				t.Fatalf("receipt = %+v, err=%v", receipt, err)
			}
			lastCheck := receipt.Checks[len(receipt.Checks)-1]
			if lastCheck.Name != "target_isolation" || lastCheck.Code != test.wantCode {
				t.Fatalf("target check = %+v", lastCheck)
			}
			afterEvents, snapshotErr := targetEventState.Snapshot(ctx)
			afterObjects, inventoryErr := targetObjectState.Inventory(ctx)
			if snapshotErr != nil || inventoryErr != nil || !reflect.DeepEqual(afterEvents, beforeEvents) ||
				!reflect.DeepEqual(afterObjects, beforeObjects) {
				t.Fatalf("target mutated: events=%+v objects=%+v snapshot_err=%v inventory_err=%v",
					afterEvents, afterObjects, snapshotErr, inventoryErr)
			}
		})
	}
}

func TestRecoveryDrillRejectsUnknownFailurePoint(t *testing.T) {
	request := companymode.DrillRequest{
		DrillID: "drill-unknown-failure-point", Profile: companymode.ProfileCompany, TenantID: "tenant-a",
		StartedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
		FailAt:    companymode.DrillFailurePoint("before_verification"),
	}
	receipt, err := companymode.RunRecoveryDrill(
		context.Background(), request,
		companymode.NewFakePostgres(), companymode.NewFakeS3(),
		companymode.NewFakePostgres(), companymode.NewFakeS3(),
	)
	if !errors.Is(err, companymode.ErrDrillFailed) || errors.Is(err, companymode.ErrInjectedDrillFailure) ||
		receipt.FailureCode != "invalid_request" || receipt.Verified {
		t.Fatalf("receipt = %+v, err=%v", receipt, err)
	}
}

func TestRecoveryDrillBoundsAndCorruptTargetFailClosed(t *testing.T) {
	ctx := context.Background()
	events := companymode.NewFakePostgres()
	objects := companymode.NewFakeS3()
	appendEvent(t, events, "tenant-a", "event-a", "digest-a")
	putBlob(t, objects, "tenant-a", "object-a", "payload")
	base := companymode.DrillRequest{
		DrillID: "drill-bounds", Profile: companymode.ProfileCompany, TenantID: "tenant-a",
		StartedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	}
	targetEvents, targetObjects := companymode.NewFakePostgres(), companymode.NewFakeS3()
	bounded := base
	bounded.Limits = companymode.DrillLimits{MaxEvents: 1, MaxBlobs: 1, MaxBlobBytes: 3}
	receipt, err := companymode.RunRecoveryDrill(ctx, bounded, events, objects, targetEvents, targetObjects)
	if !errors.Is(err, companymode.ErrDrillFailed) || receipt.FailureCode != "limit_exceeded" || receipt.Verified {
		t.Fatalf("bounded receipt = %+v, err=%v", receipt, err)
	}
	if inventory, inventoryErr := targetObjects.Inventory(ctx); inventoryErr != nil || len(inventory) != 0 {
		t.Fatalf("bounded drill mutated target: %+v, %v", inventory, inventoryErr)
	}

	corrupt := &corruptingObjectStore{FakeS3: companymode.NewFakeS3()}
	receipt, err = companymode.RunRecoveryDrill(ctx, base, events, objects, companymode.NewFakePostgres(), corrupt)
	if !errors.Is(err, companymode.ErrDrillFailed) || receipt.FailureCode != "verification_failed" || receipt.Verified {
		t.Fatalf("corrupt receipt = %+v, err=%v", receipt, err)
	}
}

func TestRecoveryDrillRequiresConclusiveErasureDenial(t *testing.T) {
	ctx := context.Background()
	events := companymode.NewFakePostgres()
	objects := companymode.NewFakeS3()
	appendEvent(t, events, "tenant-a", "event-a", "digest-a")
	erasedRef := putBlob(t, objects, "tenant-a", "erased", "payload")
	startedAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	request := companymode.DrillRequest{
		DrillID: "drill-erasure-denial", Profile: companymode.ProfileCompany, TenantID: "tenant-a", StartedAt: startedAt,
		Erasures: []companymode.ErasureMarker{{
			TenantID: "tenant-a", Key: "erased", ObjectDigest: erasedRef.Digest,
			TombstoneReceiptID: "tombstone-receipt-001",
			TombstonedAt:       startedAt.Add(-time.Hour).Format(time.RFC3339Nano),
			BackupEraseHorizon: startedAt.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
		}},
	}
	target := &outageOnMissingObjectStore{FakeS3: companymode.NewFakeS3(), tenantID: "tenant-a", key: "erased"}
	receipt, err := companymode.RunRecoveryDrill(ctx, request, events, objects, companymode.NewFakePostgres(), target)
	if !errors.Is(err, companymode.ErrDrillFailed) || receipt.FailureCode != "verification_failed" || receipt.Verified {
		t.Fatalf("inconclusive denial receipt = %+v, err=%v", receipt, err)
	}
	if len(receipt.Erasures) != 1 || receipt.Erasures[0].AbsenceVerified {
		t.Fatalf("erasure incorrectly verified: %+v", receipt.Erasures)
	}
}

type corruptingObjectStore struct {
	*companymode.FakeS3
}

type outageOnMissingObjectStore struct {
	*companymode.FakeS3
	tenantID string
	key      string
}

type snapshotFailingEventStore struct {
	*companymode.FakePostgres
}

type inventoryFailingObjectStore struct {
	*companymode.FakeS3
}

func (s *snapshotFailingEventStore) Snapshot(context.Context) ([]companymode.Event, error) {
	return nil, errors.New("event substrate unavailable")
}

func (s *inventoryFailingObjectStore) Inventory(context.Context) ([]companymode.BlobRef, error) {
	return nil, errors.New("object substrate unavailable")
}

func (s *outageOnMissingObjectStore) Get(ctx context.Context, tenantID, key string) ([]byte, error) {
	body, err := s.FakeS3.Get(ctx, tenantID, key)
	if errors.Is(err, companymode.ErrDenied) && tenantID == s.tenantID && key == s.key {
		return nil, errors.New("substrate unavailable")
	}
	return body, err
}

func (s *corruptingObjectStore) Restore(ctx context.Context, blobs []companymode.BlobObject) error {
	corrupt := append([]companymode.BlobObject(nil), blobs...)
	if len(corrupt) > 0 {
		corrupt[0].Body = append([]byte(nil), corrupt[0].Body...)
		corrupt[0].Body[0] ^= 0xff
	}
	return s.FakeS3.Restore(ctx, corrupt)
}

func appendEvent(t *testing.T, store companymode.EventStore, tenantID, eventID, payload string) {
	t.Helper()
	if err := store.Append(context.Background(), companymode.Event{
		TenantID: tenantID, EventID: eventID, Kind: "ingest", Payload: []byte(payload),
	}); err != nil {
		t.Fatal(err)
	}
}

func putBlob(t *testing.T, store companymode.ObjectStore, tenantID, key, body string) companymode.BlobRef {
	t.Helper()
	ref, err := store.Put(context.Background(), tenantID, key, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

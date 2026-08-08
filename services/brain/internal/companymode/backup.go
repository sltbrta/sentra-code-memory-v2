package companymode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// BackupManifest pins one encrypted backup point for restore smoke.
type BackupManifest struct {
	Profile       ProfileID `json:"profile"`
	TenantID      string    `json:"tenant_id"`
	CreatedAt     string    `json:"created_at"`
	EventCount    int       `json:"event_count"`
	BlobCount     int       `json:"blob_count"`
	ContentDigest string    `json:"content_digest"`
}

// Backup is the full restoreable dump.
type Backup struct {
	Manifest BackupManifest `json:"manifest"`
	Events   []Event        `json:"events"`
	Blobs    []BlobObject   `json:"blobs"`
}

// CreateBackup snapshots events and blobs for one tenant (or all when empty).
func CreateBackup(ctx context.Context, profile ProfileID, tenantID string, events EventStore, objects ObjectStore) (Backup, error) {
	return createBackupAt(ctx, profile, tenantID, events, objects, time.Now().UTC(), nil)
}

func createBackupAt(ctx context.Context, profile ProfileID, tenantID string, events EventStore, objects ObjectStore, createdAt time.Time, limits *DrillLimits) (Backup, error) {
	allEvents, err := events.Snapshot(ctx)
	if err != nil {
		return Backup{}, err
	}
	filtered := make([]Event, 0, len(allEvents))
	for _, event := range allEvents {
		if tenantID == "" || event.TenantID == tenantID {
			event.Payload = append([]byte(nil), event.Payload...)
			filtered = append(filtered, event)
		}
	}
	if limits != nil && len(filtered) > limits.MaxEvents {
		return Backup{}, errDrillLimit
	}
	sortEvents(filtered)
	inventory, err := objects.Inventory(ctx)
	if err != nil {
		return Backup{}, err
	}
	sort.Slice(inventory, func(i, j int) bool {
		if inventory[i].TenantID != inventory[j].TenantID {
			return inventory[i].TenantID < inventory[j].TenantID
		}
		return inventory[i].Key < inventory[j].Key
	})
	selected := make([]BlobRef, 0, len(inventory))
	var selectedBytes int64
	for _, ref := range inventory {
		if tenantID != "" && ref.TenantID != tenantID {
			continue
		}
		if ref.Length < 0 {
			return Backup{}, ErrRejected
		}
		if limits != nil && (len(selected) >= limits.MaxBlobs || ref.Length > limits.MaxBlobBytes-selectedBytes) {
			return Backup{}, errDrillLimit
		}
		selectedBytes += ref.Length
		selected = append(selected, ref)
	}
	blobs := make([]BlobObject, 0, len(selected))
	for _, ref := range selected {
		body, err := objects.Get(ctx, ref.TenantID, ref.Key)
		if err != nil {
			return Backup{}, err
		}
		blobs = append(blobs, BlobObject{Ref: ref, Body: append([]byte(nil), body...)})
	}
	raw, err := backupContent(filtered, blobs)
	if err != nil {
		return Backup{}, err
	}
	sum := sha256.Sum256(raw)
	return Backup{
		Manifest: BackupManifest{
			Profile:       profile,
			TenantID:      tenantID,
			CreatedAt:     createdAt.UTC().Format(time.RFC3339Nano),
			EventCount:    len(filtered),
			BlobCount:     len(blobs),
			ContentDigest: hex.EncodeToString(sum[:]),
		},
		Events: filtered,
		Blobs:  blobs,
	}, nil
}

// RestoreBackup loads a backup into disposable target stores.
func RestoreBackup(ctx context.Context, targetEvents EventStore, targetObjects ObjectStore, backup Backup) error {
	if err := VerifyBackup(backup); err != nil {
		return err
	}
	if err := targetEvents.Restore(ctx, backup.Events); err != nil {
		return err
	}
	return targetObjects.Restore(ctx, backup.Blobs)
}

// VerifyBackup validates the manifest content digest and every object before a
// restore target is mutated. It makes no statement about provider-side
// encryption or the expiry of production backup copies.
func VerifyBackup(backup Backup) error {
	manifest := backup.Manifest
	if (manifest.Profile != ProfileLocal && manifest.Profile != ProfileCompany) ||
		manifest.ContentDigest == "" || manifest.EventCount != len(backup.Events) ||
		manifest.BlobCount != len(backup.Blobs) {
		return ErrRejected
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return ErrRejected
	}
	eventIDs := make(map[string]struct{}, len(backup.Events))
	for _, event := range backup.Events {
		if event.TenantID == "" || event.EventID == "" || event.Kind == "" || event.Sequence == 0 ||
			(manifest.TenantID != "" && event.TenantID != manifest.TenantID) {
			return ErrRejected
		}
		identity := event.TenantID + "\x00" + event.EventID
		if _, duplicate := eventIDs[identity]; duplicate {
			return ErrRejected
		}
		eventIDs[identity] = struct{}{}
	}
	objectIDs := make(map[string]struct{}, len(backup.Blobs))
	for _, blob := range backup.Blobs {
		if blob.Ref.TenantID == "" || blob.Ref.Key == "" || blob.Ref.Digest == "" ||
			blob.Ref.Length < 0 || int64(len(blob.Body)) != blob.Ref.Length ||
			(manifest.TenantID != "" && blob.Ref.TenantID != manifest.TenantID) {
			return ErrRejected
		}
		sum := sha256.Sum256(blob.Body)
		if hex.EncodeToString(sum[:]) != blob.Ref.Digest {
			return ErrRejected
		}
		identity := objectKey(blob.Ref.TenantID, blob.Ref.Key)
		if _, duplicate := objectIDs[identity]; duplicate {
			return ErrRejected
		}
		objectIDs[identity] = struct{}{}
	}
	raw, err := backupContent(backup.Events, backup.Blobs)
	if err != nil {
		return ErrRejected
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != manifest.ContentDigest {
		return ErrRejected
	}
	return nil
}

func backupContent(events []Event, blobs []BlobObject) ([]byte, error) {
	return json.Marshal(struct {
		Events []Event
		Blobs  []BlobObject
	}{Events: events, Blobs: blobs})
}

func sortEvents(events []Event) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].TenantID != events[j].TenantID {
			return events[i].TenantID < events[j].TenantID
		}
		if events[i].Sequence != events[j].Sequence {
			return events[i].Sequence < events[j].Sequence
		}
		return events[i].EventID < events[j].EventID
	})
}

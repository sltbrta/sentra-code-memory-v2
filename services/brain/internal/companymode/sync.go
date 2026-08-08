package companymode

import (
	"bytes"
	"context"
	"strings"
)

// SyncBundle is the only legal local→company transfer unit: events + blobs.
type SyncBundle struct {
	Events []Event
	Blobs  []BlobObject
}

// DetectSQLiteTransfer rejects SQLite database and WAL page payloads.
func DetectSQLiteTransfer(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	// SQLite magic header: "SQLite format 3\000"
	if bytes.HasPrefix(payload, []byte("SQLite format 3")) {
		return ErrSQLiteTransfer
	}
	// WAL header starts with magic 0x377f0682 or 0x377f0683 (big-endian).
	if len(payload) >= 4 {
		magic := uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
		if magic == 0x377f0682 || magic == 0x377f0683 {
			return ErrSQLiteTransfer
		}
	}
	lower := strings.ToLower(string(payload))
	if strings.Contains(lower, "sqlite_master") || strings.Contains(lower, "wal-index") {
		return ErrSQLiteTransfer
	}
	return nil
}

// ApplySync applies a validated event/blob bundle into the company ports.
func ApplySync(ctx context.Context, events EventStore, objects ObjectStore, bundle SyncBundle) error {
	for _, event := range bundle.Events {
		if err := DetectSQLiteTransfer(event.Payload); err != nil {
			return err
		}
		if err := events.Append(ctx, event); err != nil {
			return err
		}
	}
	for _, blob := range bundle.Blobs {
		if err := DetectSQLiteTransfer(blob.Body); err != nil {
			return err
		}
		if _, err := objects.Put(ctx, blob.Ref.TenantID, blob.Ref.Key, blob.Body); err != nil {
			return err
		}
	}
	return nil
}

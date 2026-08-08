package ingestion

import (
	"errors"
	"testing"
)

func TestNegativePersistedRevisionSizeIsIntegrityError(t *testing.T) {
	err := validateHydrationBounds(
		[]FileRevision{{SizeBytes: -1}},
		HydrationRequest{MaxFiles: 1, MaxTotalBytes: 1},
	)
	if !errors.Is(err, ErrGit) {
		t.Fatalf("negative persisted size got %v", err)
	}
}

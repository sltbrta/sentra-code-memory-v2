package ingestion

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type cancelAfterChecks struct {
	checks int
}

func (c *cancelAfterChecks) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterChecks) Done() <-chan struct{}       { return nil }
func (c *cancelAfterChecks) Value(any) any               { return nil }
func (c *cancelAfterChecks) Err() error {
	c.checks++
	if c.checks >= 3 {
		return context.Canceled
	}
	return nil
}

func TestDeriveDeltaCancelsDuringLargeRenameWork(t *testing.T) {
	const fileCount = 10_000
	previous := make([]FileRevision, fileCount)
	next := make([]FileRevision, fileCount)
	for index := range fileCount {
		blob := fmt.Sprintf("%040x", index)
		previous[index] = FileRevision{Path: fmt.Sprintf("old/%05d.go", index), BlobOID: blob, Mode: "100644"}
		next[index] = FileRevision{Path: fmt.Sprintf("new/%05d.go", index), BlobOID: blob, Mode: "100644"}
	}
	ctx := &cancelAfterChecks{}
	if _, err := deriveDelta(ctx, previous, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("large delta ignored cancellation: %v", err)
	}
}

func TestDeriveDeltaPairsDuplicateRenamesBySortedPath(t *testing.T) {
	previous := []FileRevision{
		{Path: "old/a.go", BlobOID: "same", Mode: "100644", RevisionID: "old-a"},
		{Path: "old/b.go", BlobOID: "same", Mode: "100644", RevisionID: "old-b"},
	}
	next := []FileRevision{
		{Path: "new/a.go", BlobOID: "same", Mode: "100644", RevisionID: "new-a"},
		{Path: "new/b.go", BlobOID: "same", Mode: "100644", RevisionID: "new-b"},
	}
	delta, err := deriveDelta(context.Background(), previous, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != 2 || delta[0].OldPath != "old/a.go" || delta[0].NewPath != "new/a.go" ||
		delta[1].OldPath != "old/b.go" || delta[1].NewPath != "new/b.go" {
		t.Fatalf("duplicate rename pairing drifted: %#v", delta)
	}
}

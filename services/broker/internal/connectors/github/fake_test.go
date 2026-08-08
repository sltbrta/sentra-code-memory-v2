package github

import (
	"context"
	"testing"
)

func TestFakeSnapshotAndDelta(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := NewFakeSourceAPI()

	page, err := fake.Snapshot(ctx, "ouroboros-dogfood", "sample-repo")
	if err != nil || !page.Complete || page.Cursor != "cursor-v1" {
		t.Fatalf("snapshot = %+v err=%v", page, err)
	}
	if len(page.Objects) < 4 {
		t.Fatalf("objects = %d", len(page.Objects))
	}

	// Rate limit does not advance cursor and never implies deletion.
	fake.RateLimitSnapshot = true
	limited, err := fake.Snapshot(ctx, "ouroboros-dogfood", "sample-repo")
	if err != nil || limited.Complete || limited.ErrorCode != "rate_limited" {
		t.Fatalf("rate limit = %+v err=%v", limited, err)
	}

	delta, err := fake.Delta(ctx, "ouroboros-dogfood", "sample-repo", "cursor-v1")
	if err != nil || !delta.Complete || delta.Cursor != "cursor-v2" {
		t.Fatalf("delta = %+v err=%v", delta, err)
	}
	if len(delta.Objects) == 0 {
		t.Fatal("expected delta objects")
	}

	fake.MalformedDelta = true
	malformed, err := fake.Delta(ctx, "ouroboros-dogfood", "sample-repo", "cursor-v1")
	if err != nil || malformed.Complete || malformed.ErrorCode != "malformed_page" {
		t.Fatalf("malformed = %+v err=%v", malformed, err)
	}
}

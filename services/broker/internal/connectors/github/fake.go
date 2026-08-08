package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// FakeSourceAPI is a deterministic in-process GitHub source stand-in. It never
// opens sockets. Concurrent callers share one sealed fixture set with optional
// delta and deletion seeds for conformance.
type FakeSourceAPI struct {
	mu sync.Mutex

	// repos maps "owner/repo" to baseline objects.
	repos map[string][]Object
	// deltas maps "owner/repo#cursor" to additional objects returned by Delta.
	deltas map[string][]Object
	// FailSnapshotOnce, when >0, fails the next Snapshot that many times with ErrorCode.
	FailSnapshotOnce int32
	// FailDeltaOnce, when >0, fails the next Delta that many times.
	FailDeltaOnce int32
	// RateLimitSnapshot forces the next Snapshot to return rate_limited incomplete.
	RateLimitSnapshot bool
	// MalformedDelta forces the next Delta to return malformed_page incomplete.
	MalformedDelta bool
	// SnapshotCalls counts Snapshot invocations.
	SnapshotCalls int32
	// DeltaCalls counts Delta invocations.
	DeltaCalls int32
}

// NewFakeSourceAPI returns a fake preloaded with the Stage 08 dogfood fixture.
func NewFakeSourceAPI() *FakeSourceAPI {
	fake := &FakeSourceAPI{
		repos:  make(map[string][]Object),
		deltas: make(map[string][]Object),
	}
	fake.SeedRepo("ouroboros-dogfood", "sample-repo", []Object{
		{
			ID: "repo:meta", Kind: ObjectKindRepository, Title: "sample-repo",
			Body:    "Sample dogfood repository for Stage 08 connector conformance.",
			Version: "v1",
		},
		{
			ID: "file:README.md", Kind: ObjectKindFile, Title: "README.md",
			Body: "Billing service ships next sprint. See issue tracker for rollout.",
			Path: "README.md", StartLine: 1, EndLine: 3, Version: "sha-readme-1",
		},
		{
			ID: "file:docs/runbook.md", Kind: ObjectKindFile, Title: "runbook.md",
			Body: "Rate-limit backoff uses Retry-After. Never infer deletion from silence.",
			Path: "docs/runbook.md", StartLine: 1, EndLine: 4, Version: "sha-runbook-1",
		},
		{
			ID: "issue:1", Kind: ObjectKindIssue, Title: "Track billing rollout",
			Body:        "Coordinate the billing service rollout with finance.",
			IssueNumber: 1, Version: "issue-1-v1",
		},
		{
			ID: "issue:2", Kind: ObjectKindIssue, Title: "Document rate limits",
			Body:        "Connector must honor rate_limited and preserve last complete generation.",
			IssueNumber: 2, Version: "issue-2-v1",
		},
	})
	fake.SeedDelta("ouroboros-dogfood", "sample-repo", "cursor-v1", []Object{
		{
			ID: "issue:3", Kind: ObjectKindIssue, Title: "Delta issue after reconcile",
			Body:        "Incremental issue admitted only after complete delta page.",
			IssueNumber: 3, Version: "issue-3-v1",
		},
		{
			ID: "issue:2", Kind: ObjectKindIssue, Title: "Document rate limits",
			Body:        "Updated body after complete reconcile.",
			IssueNumber: 2, Version: "issue-2-v2",
		},
	})
	return fake
}

// SeedRepo installs the baseline snapshot for owner/repo.
func (f *FakeSourceAPI) SeedRepo(owner, repo string, objects []Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := repoKey(owner, repo)
	copied := make([]Object, len(objects))
	copy(copied, objects)
	f.repos[key] = copied
}

// SeedDelta installs objects returned by Delta for a prior cursor.
func (f *FakeSourceAPI) SeedDelta(owner, repo, priorCursor string, objects []Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := deltaKey(owner, repo, priorCursor)
	copied := make([]Object, len(objects))
	copy(copied, objects)
	f.deltas[key] = copied
}

// SeedDeletion marks one object deleted in a subsequent complete delta.
func (f *FakeSourceAPI) SeedDeletion(owner, repo, priorCursor, objectID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := deltaKey(owner, repo, priorCursor)
	f.deltas[key] = append(f.deltas[key], Object{
		ID: objectID, Kind: ObjectKindIssue, Deleted: true, Version: "deleted",
	})
}

// Snapshot implements SourceAPI.
func (f *FakeSourceAPI) Snapshot(ctx context.Context, owner, repo string) (SnapshotPage, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotPage{}, err
	}
	atomic.AddInt32(&f.SnapshotCalls, 1)
	if atomic.AddInt32(&f.FailSnapshotOnce, -1) >= 0 {
		return SnapshotPage{Complete: false, ErrorCode: "provider_unavailable"}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RateLimitSnapshot {
		f.RateLimitSnapshot = false
		return SnapshotPage{Complete: false, ErrorCode: "rate_limited"}, nil
	}
	objects, ok := f.repos[repoKey(owner, repo)]
	if !ok {
		return SnapshotPage{Complete: false, ErrorCode: "provider_unavailable"}, nil
	}
	return SnapshotPage{
		Cursor:   "cursor-v1",
		Objects:  cloneObjects(objects),
		Complete: true,
	}, nil
}

// Delta implements SourceAPI.
func (f *FakeSourceAPI) Delta(ctx context.Context, owner, repo, priorCursor string) (SnapshotPage, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotPage{}, err
	}
	atomic.AddInt32(&f.DeltaCalls, 1)
	if atomic.AddInt32(&f.FailDeltaOnce, -1) >= 0 {
		return SnapshotPage{Complete: false, ErrorCode: "provider_unavailable", Cursor: priorCursor}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MalformedDelta {
		f.MalformedDelta = false
		return SnapshotPage{Complete: false, ErrorCode: "malformed_page", Cursor: priorCursor}, nil
	}
	if strings.TrimSpace(priorCursor) == "" {
		return SnapshotPage{Complete: false, ErrorCode: "malformed_page"}, nil
	}
	objects, ok := f.deltas[deltaKey(owner, repo, priorCursor)]
	if !ok {
		// Empty complete delta preserves cursor.
		return SnapshotPage{Cursor: priorCursor, Objects: nil, Complete: true}, nil
	}
	next := nextCursor(priorCursor)
	return SnapshotPage{
		Cursor:   next,
		Objects:  cloneObjects(objects),
		Complete: true,
	}, nil
}

func repoKey(owner, repo string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo)
}

func deltaKey(owner, repo, cursor string) string {
	return repoKey(owner, repo) + "#" + cursor
}

func nextCursor(prior string) string {
	if strings.HasPrefix(prior, "cursor-v") {
		n, err := strconv.Atoi(strings.TrimPrefix(prior, "cursor-v"))
		if err == nil {
			return fmt.Sprintf("cursor-v%d", n+1)
		}
	}
	return prior + ".next"
}

func cloneObjects(in []Object) []Object {
	out := make([]Object, len(in))
	copy(out, in)
	return out
}

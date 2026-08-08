package ingestion_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ingestion"
)

func TestAdmitCommittedSnapshot(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	if generation.Sequence != 1 || len(generation.Manifest.Files) != 1 {
		t.Fatalf("unexpected generation: %#v", generation)
	}
	if generation.Manifest.Files[0].Path != "main.go" {
		t.Fatalf("unexpected path: %q", generation.Manifest.Files[0].Path)
	}
}

func TestAdmitScanDoesNotBlockStatusOrPreAdmissionRevoke(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	wrapper, block, started, release := blockingGit(t, git)
	authority, err := ingestion.New(context.Background(), testConfig(root, wrapper))
	if err != nil {
		t.Fatal(err)
	}
	commit := gitOutput(t, git, root, "rev-parse", "HEAD")
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	type admissionResult struct {
		generation ingestion.Generation
		err        error
	}
	result := make(chan admissionResult, 1)
	go func() {
		generation, admitErr := authority.Admit(context.Background(), ingestion.Admission{
			ExpectedCommitOID: commit,
			IdempotencyKey:    "blocked-admit",
		})
		result <- admissionResult{generation: generation, err: admitErr}
	}()
	waitForFile(t, started)
	statusResult := make(chan ingestion.Status, 1)
	go func() { statusResult <- authority.Status() }()
	select {
	case status := <-statusResult:
		if status.Sequence != 0 || status.CurrentGenerationID != "" {
			t.Fatalf("scan exposed an unpublished generation: %#v", status)
		}
	case <-time.After(time.Second):
		_ = os.WriteFile(release, nil, 0o600)
		t.Fatal("status waited for admission Git scan")
	}
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- authority.Revoke(context.Background(), ingestion.RevokeRequest{
			ExpectedGenerationID: strings.Repeat("0", 64),
			IdempotencyKey:       "pre-admission-revoke",
		})
	}()
	select {
	case err := <-revokeResult:
		if !errors.Is(err, ingestion.ErrStaleGeneration) {
			t.Fatalf("pre-admission revoke got %v", err)
		}
	case <-time.After(time.Second):
		_ = os.WriteFile(release, nil, 0o600)
		t.Fatal("revoke waited for admission Git scan")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	admitted := <-result
	if admitted.err != nil || admitted.generation.Sequence != 1 {
		t.Fatalf("admission after concurrent reads: %#v, %v", admitted.generation, admitted.err)
	}
}

func TestAdmissionAndReconcileAreIdempotent(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	commit := gitOutput(t, git, root, "rev-parse", "HEAD")
	request := ingestion.Admission{ExpectedCommitOID: commit, IdempotencyKey: "admit-key"}
	first, err := authority.Admit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := authority.Admit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Sequence != second.Sequence {
		t.Fatalf("duplicate admit changed generation: %#v %#v", first, second)
	}
	_, err = authority.Admit(context.Background(), ingestion.Admission{
		ExpectedCommitOID: strings.Repeat("0", 40),
		IdempotencyKey:    request.IdempotencyKey,
	})
	if !errors.Is(err, ingestion.ErrConflict) {
		t.Fatalf("changed idempotency input got %v", err)
	}

	reconcile := ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID,
		ExpectedCommitOID:    commit,
		TargetCommitOID:      commit,
		IdempotencyKey:       "reconcile-key",
	}
	third, err := authority.Reconcile(context.Background(), reconcile)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := authority.Reconcile(context.Background(), reconcile)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != first.ID || fourth.ID != first.ID || third.Sequence != 1 {
		t.Fatalf("no-op reconcile changed generation: %#v %#v", third, fourth)
	}
}

func TestReconcileDetectsExactRename(t *testing.T) {
	root, git := newRepository(t, map[string]string{"old/name.go": "package renamed\n"})
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	if err := os.MkdirAll(root+"/new", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root+"/old/name.go", root+"/new/name.go"); err != nil {
		t.Fatal(err)
	}
	target := commitFiles(t, git, root, map[string]string{})
	second, err := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID,
		ExpectedCommitOID:    first.CommitOID,
		TargetCommitOID:      target,
		IdempotencyKey:       "rename",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Delta) != 1 || second.Delta[0].Kind != ingestion.ChangeRename ||
		second.Delta[0].OldPath != "old/name.go" || second.Delta[0].NewPath != "new/name.go" {
		t.Fatalf("unexpected rename delta: %#v", second.Delta)
	}
}

func TestWatcherHintsNeverDeleteAndMissedEventsAreReconciled(t *testing.T) {
	root, git := newRepository(t, map[string]string{"kept.go": "package kept\n", "removed.go": "package removed\n"})
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	if err := authority.ObserveHints([]ingestion.WatchHint{{Kind: ingestion.HintRemove, Path: "kept.go"}}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID,
		ExpectedCommitOID:    first.CommitOID,
		TargetCommitOID:      first.CommitOID,
		IdempotencyKey:       "hint-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Manifest.Files) != 2 || len(unchanged.Delta) != 0 {
		t.Fatalf("watch hint changed authority: %#v", unchanged)
	}
	target := commitFiles(t, git, root, map[string]string{
		"removed.go": "",
		"added.go":   "package added\n",
	})
	reconciled, err := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: unchanged.ID,
		ExpectedCommitOID:    unchanged.CommitOID,
		TargetCommitOID:      target,
		IdempotencyKey:       "missed-events",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[ingestion.ChangeKind]int{}
	for _, change := range reconciled.Delta {
		kinds[change.Kind]++
	}
	if kinds[ingestion.ChangeAdd] != 1 || kinds[ingestion.ChangeDelete] != 1 {
		t.Fatalf("missed watcher changes were not repaired: %#v", reconciled.Delta)
	}
}

func TestOverflowHintSaturatesCoverageAtHintCapacity(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	config := testConfig(root, git)
	config.MaxFiles = 1
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.ObserveHints([]ingestion.WatchHint{{Kind: ingestion.HintModify, Path: "main.go"}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := authority.ObserveHints([]ingestion.WatchHint{{Kind: ingestion.HintOverflow}}); err != nil {
			t.Fatalf("overflow at capacity: %v", err)
		}
	}
	status := authority.Status()
	if status.PendingWatcherHints != 1 || !status.WatcherCoverageLost {
		t.Fatalf("overflow accounting drifted: %#v", status)
	}
	if err := authority.ObserveHints([]ingestion.WatchHint{{Kind: ingestion.HintModify, Path: "main.go"}}); !errors.Is(err, ingestion.ErrLimit) {
		t.Fatalf("normal hint exceeded capacity: %v", err)
	}
}

func TestRejectsStaleGeneration(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	target := commitFiles(t, git, root, map[string]string{"main.go": "package two\n"})
	second, err := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID,
		ExpectedCommitOID:    first.CommitOID,
		TargetCommitOID:      target,
		IdempotencyKey:       "advance",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID,
		ExpectedCommitOID:    first.CommitOID,
		TargetCommitOID:      second.CommitOID,
		IdempotencyKey:       "stale",
	})
	if !errors.Is(err, ingestion.ErrStaleGeneration) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentReconcileAllowsOnePublisher(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	target := commitFiles(t, git, root, map[string]string{"main.go": "package two\n"})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, key := range []string{"publisher-a", "publisher-b"} {
		wait.Add(1)
		go func(idempotencyKey string) {
			defer wait.Done()
			_, reconcileErr := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
				ExpectedGenerationID: first.ID,
				ExpectedCommitOID:    first.CommitOID,
				TargetCommitOID:      target,
				IdempotencyKey:       idempotencyKey,
			})
			results <- reconcileErr
		}(key)
	}
	wait.Wait()
	close(results)
	var successes, stale int
	for result := range results {
		if result == nil {
			successes++
		} else if errors.Is(result, ingestion.ErrStaleGeneration) {
			stale++
		} else {
			t.Fatalf("unexpected concurrent result: %v", result)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("successes=%d stale=%d", successes, stale)
	}
}

func TestRevokeCommitsWhileReconcileScans(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
	wrapper, block, started, release := blockingGit(t, git)
	config := testConfig(root, wrapper)
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	target := commitFiles(t, git, root, map[string]string{"main.go": "package two\n"})
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reconcileResult := make(chan error, 1)
	go func() {
		_, reconcileErr := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
			ExpectedGenerationID: first.ID,
			ExpectedCommitOID:    first.CommitOID,
			TargetCommitOID:      target,
			IdempotencyKey:       "blocked-reconcile",
		})
		reconcileResult <- reconcileErr
	}()
	waitForFile(t, started)
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- authority.Revoke(context.Background(), ingestion.RevokeRequest{
			ExpectedGenerationID: first.ID,
			IdempotencyKey:       "revoke-during-scan",
		})
	}()
	select {
	case err := <-revokeResult:
		if err != nil {
			t.Fatalf("revoke during scan: %v", err)
		}
	case <-time.After(time.Second):
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Fatal("revoke waited for Git scan")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-reconcileResult; !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("reconcile published after revoke: %v", err)
	}
}

func TestReconcileHonorsCancellation(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
	authority, err := ingestion.New(context.Background(), testConfig(root, git))
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	target := commitFiles(t, git, root, map[string]string{"main.go": "package two\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = authority.Reconcile(ctx, ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID,
		ExpectedCommitOID:    first.CommitOID,
		TargetCommitOID:      target,
		IdempotencyKey:       "cancelled",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestReconcilePropagatesCancellationDuringGitOutputRead(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
	wrapper, block, started, release := blockingGit(t, git)
	authority, err := ingestion.New(context.Background(), testConfig(root, wrapper))
	if err != nil {
		t.Fatal(err)
	}
	first := admitHead(t, authority, git, root)
	target := commitFiles(t, git, root, map[string]string{"main.go": "package two\n"})
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reconcileResult := make(chan error, 1)
	go func() {
		_, reconcileErr := authority.Reconcile(ctx, ingestion.ReconcileRequest{
			ExpectedGenerationID: first.ID,
			ExpectedCommitOID:    first.CommitOID,
			TargetCommitOID:      target,
			IdempotencyKey:       "cancel-during-output-read",
		})
		reconcileResult <- reconcileErr
	}()
	waitForFile(t, started)
	cancel()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reconcileResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation during stdout read got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled Git output read did not return")
	}
}

func TestRevokeAndTombstone(t *testing.T) {
	root, git := newRepository(t, map[string]string{"secret-name.go": "package secret\n"})
	config := testConfig(root, git)
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	revoke := ingestion.RevokeRequest{ExpectedGenerationID: generation.ID, IdempotencyKey: "revoke"}
	if err := authority.Revoke(context.Background(), revoke); err != nil {
		t.Fatal(err)
	}
	if err := authority.Revoke(context.Background(), revoke); err != nil {
		t.Fatalf("duplicate revoke: %v", err)
	}
	afterRevoke, err := authority.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	alternateRevoke := ingestion.RevokeRequest{ExpectedGenerationID: generation.ID, IdempotencyKey: "alternate-revoke"}
	if err := authority.Revoke(context.Background(), alternateRevoke); !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("alternate revoke got %v", err)
	}
	if err := authority.Revoke(context.Background(), revoke); err != nil {
		t.Fatalf("exact revoke retry after rejection: %v", err)
	}
	afterRejectedRevoke, err := authority.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRejectedRevoke, afterRevoke) {
		t.Fatal("alternate revoke grew persisted operation state")
	}
	if _, err := authority.Current(); !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("revoked read got %v", err)
	}
	tombstone := ingestion.TombstoneRequest{ExpectedGenerationID: generation.ID, IdempotencyKey: "tombstone"}
	if err := authority.Tombstone(context.Background(), tombstone); err != nil {
		t.Fatal(err)
	}
	if err := authority.Tombstone(context.Background(), tombstone); err != nil {
		t.Fatalf("duplicate tombstone: %v", err)
	}
	afterTombstone, err := authority.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	alternateTombstone := ingestion.TombstoneRequest{ExpectedGenerationID: generation.ID, IdempotencyKey: "alternate-tombstone"}
	if err := authority.Tombstone(context.Background(), alternateTombstone); !errors.Is(err, ingestion.ErrTombstoned) {
		t.Fatalf("alternate tombstone got %v", err)
	}
	if err := authority.Tombstone(context.Background(), tombstone); err != nil {
		t.Fatalf("exact tombstone retry after rejection: %v", err)
	}
	afterRejectedTombstone, err := authority.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRejectedTombstone, afterTombstone) {
		t.Fatal("alternate tombstone grew persisted operation state")
	}
	if _, err := ingestion.Restore(context.Background(), config, afterRejectedTombstone); err != nil {
		t.Fatalf("restore stable tombstone state: %v", err)
	}
	if status := authority.Status(); !status.Revoked || !status.Tombstoned {
		t.Fatalf("unexpected status: %#v", status)
	}
	if strings.Contains(string(afterRejectedTombstone), "secret-name.go") {
		t.Fatalf("serialized state exposed repository path: %s", afterRejectedTombstone)
	}
}

func TestLifecycleOperationsBypassNormalReceiptExhaustion(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	config := testConfig(root, git)
	config.MaxIdempotencyRecords = 1
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	revoke := ingestion.RevokeRequest{ExpectedGenerationID: generation.ID, IdempotencyKey: "reserved-revoke"}
	if err := authority.Revoke(context.Background(), revoke); err != nil {
		t.Fatalf("revoke at normal record limit: %v", err)
	}
	tombstone := ingestion.TombstoneRequest{ExpectedGenerationID: generation.ID, IdempotencyKey: "reserved-tombstone"}
	if err := authority.Tombstone(context.Background(), tombstone); err != nil {
		t.Fatalf("tombstone at normal record limit: %v", err)
	}
	if err := authority.Revoke(context.Background(), revoke); err != nil {
		t.Fatalf("exact revoke retry: %v", err)
	}
	if err := authority.Tombstone(context.Background(), tombstone); err != nil {
		t.Fatalf("exact tombstone retry: %v", err)
	}
}

func TestExactGenerationRetriesSurviveTransitionsAndRestart(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
	config := testConfig(root, git)
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := ingestion.Admission{
		ExpectedCommitOID: gitOutput(t, git, root, "rev-parse", "HEAD"),
		IdempotencyKey:    "original-admit",
	}
	first, err := authority.Admit(context.Background(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondCommit := commitFiles(t, git, root, map[string]string{"main.go": "package two\n", "two.go": "package two\n"})
	secondRequest := ingestion.ReconcileRequest{
		ExpectedGenerationID: first.ID, ExpectedCommitOID: first.CommitOID,
		TargetCommitOID: secondCommit, IdempotencyKey: "original-reconcile",
	}
	second, err := authority.Reconcile(context.Background(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	thirdCommit := commitFiles(t, git, root, map[string]string{"main.go": "package three\n"})
	third, err := authority.Reconcile(context.Background(), ingestion.ReconcileRequest{
		ExpectedGenerationID: second.ID, ExpectedCommitOID: second.CommitOID,
		TargetCommitOID: thirdCommit, IdempotencyKey: "later-reconcile",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExactRetry := func(t *testing.T, candidate *ingestion.Authority) {
		t.Helper()
		replayedFirst, err := candidate.Admit(context.Background(), firstRequest)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(replayedFirst, first) {
			t.Fatalf("admit retry returned a later generation: %#v", replayedFirst)
		}
		replayedSecond, err := candidate.Reconcile(context.Background(), secondRequest)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(replayedSecond, second) {
			t.Fatalf("reconcile retry changed result: %#v", replayedSecond)
		}
	}
	assertExactRetry(t, authority)
	encoded, err := authority.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ingestion.Restore(context.Background(), config, encoded)
	if err != nil {
		t.Fatal(err)
	}
	restoredCurrent, err := restored.Current()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restoredCurrent, third) {
		t.Fatalf("restart changed current generation or delta: %#v", restoredCurrent)
	}
	assertExactRetry(t, restored)
	if err := restored.Revoke(context.Background(), ingestion.RevokeRequest{
		ExpectedGenerationID: third.ID, IdempotencyKey: "deny-replays",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Admit(context.Background(), firstRequest); !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("admit replay bypassed revoke: %v", err)
	}
	if _, err := restored.Reconcile(context.Background(), secondRequest); !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("reconcile replay bypassed revoke: %v", err)
	}
}

func TestUnlockedScanPanicRestoresAuthorityLock(t *testing.T) {
	t.Run("reconcile", func(t *testing.T) {
		root, git := newRepository(t, map[string]string{"main.go": "package one\n"})
		authority, err := ingestion.New(context.Background(), testConfig(root, git))
		if err != nil {
			t.Fatal(err)
		}
		first := admitHead(t, authority, git, root)
		target := commitFiles(t, git, root, map[string]string{"main.go": "package two\n"})
		assertScanPanicRestoresLock(t, authority, func(ctx context.Context) {
			_, _ = authority.Reconcile(ctx, ingestion.ReconcileRequest{
				ExpectedGenerationID: first.ID, ExpectedCommitOID: first.CommitOID,
				TargetCommitOID: target, IdempotencyKey: "panic-reconcile",
			})
		})
	})
	t.Run("replay", func(t *testing.T) {
		root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
		config := testConfig(root, git)
		authority, err := ingestion.New(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		request := ingestion.Admission{
			ExpectedCommitOID: gitOutput(t, git, root, "rev-parse", "HEAD"),
			IdempotencyKey:    "panic-replay",
		}
		if _, err := authority.Admit(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		encoded, err := authority.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		restored, err := ingestion.Restore(context.Background(), config, encoded)
		if err != nil {
			t.Fatal(err)
		}
		assertScanPanicRestoresLock(t, restored, func(ctx context.Context) {
			_, _ = restored.Admit(ctx, request)
		})
	})
}

func TestRebuildScanAllowsImmediateRevoke(t *testing.T) {
	root, git := newRepository(t, map[string]string{"main.go": "package main\n"})
	wrapper, block, started, release := blockingGit(t, git)
	authority, err := ingestion.New(context.Background(), testConfig(root, wrapper))
	if err != nil {
		t.Fatal(err)
	}
	generation := admitHead(t, authority, git, root)
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rebuildResult := make(chan error, 1)
	go func() {
		_, rebuildErr := authority.Rebuild(context.Background())
		rebuildResult <- rebuildErr
	}()
	waitForFile(t, started)
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- authority.Revoke(context.Background(), ingestion.RevokeRequest{
			ExpectedGenerationID: generation.ID,
			IdempotencyKey:       "revoke-during-rebuild",
		})
	}()
	select {
	case err := <-revokeResult:
		if err != nil {
			t.Fatalf("revoke during rebuild: %v", err)
		}
	case <-time.After(time.Second):
		_ = os.WriteFile(release, nil, 0o600)
		t.Fatal("revoke waited for rebuild Git scan")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-rebuildResult; !errors.Is(err, ingestion.ErrRevoked) {
		t.Fatalf("rebuild returned after revoke: %v", err)
	}
}

func TestRestartAndCleanRebuildEquivalence(t *testing.T) {
	root, git := newRepository(t, map[string]string{
		"main.go":       "package main\n",
		"src/worker.ts": "export const worker = true;\n",
	})
	config := testConfig(root, git)
	authority, err := ingestion.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	original := admitHead(t, authority, git, root)
	rebuilt, err := authority.Rebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ID != original.ID || rebuilt.Manifest.Digest != original.Manifest.Digest ||
		len(rebuilt.Manifest.Files) != len(original.Manifest.Files) {
		t.Fatalf("clean rebuild drift: %#v %#v", original, rebuilt)
	}
	encoded, err := authority.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ingestion.Restore(context.Background(), config, encoded)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := restored.Current()
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.ID != original.ID || afterRestart.Manifest.Digest != original.Manifest.Digest ||
		len(afterRestart.Manifest.Files) != len(original.Manifest.Files) {
		t.Fatalf("restart drift: %#v", afterRestart)
	}
	invalid := append(encoded[:len(encoded)-1], []byte(",\"unknown\":true}")...)
	if _, err := ingestion.Restore(context.Background(), config, invalid); !errors.Is(err, ingestion.ErrInvalidInput) {
		t.Fatalf("unknown state field got %v", err)
	}
}

type panicDeadlineContext struct{ context.Context }

func (panicDeadlineContext) Deadline() (time.Time, bool) {
	panic("scan deadline panic")
}

func assertScanPanicRestoresLock(t *testing.T, authority *ingestion.Authority, operation func(context.Context)) {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		operation(panicDeadlineContext{Context: context.Background()})
	}()
	if recovered != "scan deadline panic" {
		t.Fatalf("unexpected recovered panic: %v", recovered)
	}
	statusResult := make(chan ingestion.Status, 1)
	go func() { statusResult <- authority.Status() }()
	select {
	case <-statusResult:
	case <-time.After(time.Second):
		t.Fatal("authority lock was not restored after scan panic")
	}
}

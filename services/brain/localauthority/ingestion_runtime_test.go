package localauthority

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
)

func TestIngestionRuntimeAddSearchRestartReconcileAndRevoke(t *testing.T) {
	ctx := context.Background()
	repository := t.TempDir()
	writeRepositoryFiles(t, repository, map[string]string{
		"main.go":   "package sample\n\nfunc Anchor() int { return helper() }\n\nfunc helper() int { return 1 }\n",
		"broken.ts": "const broken =\n",
		"notes.txt": "not indexed Anchor",
	})
	if err := os.Symlink("notes.txt", filepath.Join(repository, "notes-link")); err != nil {
		t.Fatal(err)
	}
	firstCommit := commitRepository(t, repository, "initial")

	root := t.TempDir()
	config, keys := durableTestConfig(root)
	config.Ingestion = testIngestionConfig(repository)
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	requestContext := testIngestionContext(config, identity)
	added, err := runtime.AddSource(ctx, AddSourceRequest{
		IngestionContext: requestContext, ExpectedCommitOID: firstCommit, IdempotencyKey: "add-source",
	})
	if err != nil || added.Status.Sequence != 1 || added.Status.State != "degraded" || len(added.Status.Readiness) != 5 {
		t.Fatalf("add source = %#v, %v", added, err)
	}
	replay, err := runtime.AddSource(ctx, AddSourceRequest{
		IngestionContext: requestContext, ExpectedCommitOID: firstCommit, IdempotencyKey: "add-source",
	})
	if err != nil || !replay.Replayed || replay.Receipt != added.Receipt {
		t.Fatalf("add replay = %#v, %v", replay, err)
	}
	assertSearch(t, runtime, SearchCodeRequest{
		IngestionContext: requestContext, GenerationID: added.Status.GenerationID,
		Query: "Anchor", Kind: SearchSymbol, Limit: 10,
	}, "main.go", "Anchor")
	assertSearch(t, runtime, SearchCodeRequest{
		IngestionContext: requestContext, GenerationID: added.Status.GenerationID,
		Query: "helper", Kind: SearchReference, Limit: 10,
	}, "main.go", "helper")

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	status, err := runtime.GetSourceStatus(ctx, SourceStatusRequest{IngestionContext: requestContext})
	if err != nil || status.GenerationID != added.Status.GenerationID || status.CommitOID != firstCommit {
		t.Fatalf("restart status = %#v, %v", status, err)
	}
	assertSearch(t, runtime, SearchCodeRequest{
		IngestionContext: requestContext, GenerationID: status.GenerationID,
		Query: "Anchor", Kind: SearchExact, Limit: 1,
	}, "main.go", "Anchor")

	writeRepositoryFiles(t, repository, map[string]string{
		"main.go":  "package sample\n\nfunc NextAnchor() int { return helper() }\n\nfunc helper() int { return 2 }\n",
		"extra.py": "\"\"\"Bounded Python fixture.\"\"\"\n\n\ndef python_anchor():\n    \"\"\"Return the fixture anchor.\"\"\"\n    return 1\n",
	})
	secondCommit := commitRepository(t, repository, "update")
	reconciled, err := runtime.ReconcileSource(ctx, ReconcileSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: status.GenerationID,
		ExpectedCommitOID: firstCommit, TargetCommitOID: secondCommit, IdempotencyKey: "reconcile-source",
	})
	if err != nil || reconciled.Status.Sequence != 2 || reconciled.Status.GenerationID == status.GenerationID {
		t.Fatalf("reconcile = %#v, %v", reconciled, err)
	}
	reconcileReplay, err := runtime.ReconcileSource(ctx, ReconcileSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: status.GenerationID,
		ExpectedCommitOID: firstCommit, TargetCommitOID: secondCommit, IdempotencyKey: "reconcile-source",
	})
	if err != nil || !reconcileReplay.Replayed || reconcileReplay.Receipt != reconciled.Receipt ||
		reconcileReplay.Status.GenerationID != reconciled.Status.GenerationID {
		t.Fatalf("reconcile replay = %#v, %v", reconcileReplay, err)
	}
	assertSearch(t, runtime, SearchCodeRequest{
		IngestionContext: requestContext, GenerationID: reconciled.Status.GenerationID,
		Query: "NextAnchor", Kind: SearchSymbol, Limit: 10,
	}, "main.go", "NextAnchor")
	beforeRestart, err := runtime.SearchCode(ctx, SearchCodeRequest{
		IngestionContext: requestContext, GenerationID: reconciled.Status.GenerationID,
		Query: "NextAnchor", Kind: SearchSymbol, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	restarted, err := runtime.GetSourceStatus(ctx, SourceStatusRequest{IngestionContext: requestContext})
	if err != nil || !reflect.DeepEqual(restarted, reconciled.Status) {
		t.Fatalf("reconciled restart status = %#v, %v", restarted, err)
	}
	afterRestart, err := runtime.SearchCode(ctx, SearchCodeRequest{
		IngestionContext: requestContext, GenerationID: restarted.GenerationID,
		Query: "NextAnchor", Kind: SearchSymbol, Limit: 10,
	})
	if err != nil || !reflect.DeepEqual(afterRestart, beforeRestart) {
		t.Fatalf("reconciled restart search = %#v, %#v, %v", afterRestart, beforeRestart, err)
	}
	restartReplay, err := runtime.ReconcileSource(ctx, ReconcileSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: status.GenerationID,
		ExpectedCommitOID: firstCommit, TargetCommitOID: secondCommit, IdempotencyKey: "reconcile-source",
	})
	if err != nil || !restartReplay.Replayed || restartReplay.Receipt != reconciled.Receipt ||
		restartReplay.Status.GenerationID != reconciled.Status.GenerationID {
		t.Fatalf("reconcile replay after restart = %#v, %v", restartReplay, err)
	}
	checkpoint, err := runtime.store.LoadIngestionCheckpoint(ctx, localstate.IngestionCheckpointQuery{
		Identity: identity, Scope: runtime.ingestion.scope,
	})
	if err != nil || checkpoint.GenerationID != reconciled.Status.GenerationID ||
		checkpoint.SnapshotID == "" || checkpoint.TreeOID == "" || checkpoint.SnapshotDigest == "" {
		t.Fatalf("reconciled restart checkpoint = %#v, %v", checkpoint, err)
	}
	writeRepositoryFiles(t, repository, map[string]string{
		"main.go": "package sample\n\nfunc ThirdAnchor() int { return helper() }\n\nfunc helper() int { return 3 }\n",
	})
	thirdCommit := commitRepository(t, repository, "unsupported update")
	if _, err := runtime.ReconcileSource(ctx, ReconcileSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: restarted.GenerationID,
		ExpectedCommitOID: restarted.CommitOID, TargetCommitOID: thirdCommit, IdempotencyKey: "third-reconcile",
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("third reconcile = %v", err)
	}
	if status, err := runtime.GetSourceStatus(ctx, SourceStatusRequest{IngestionContext: requestContext}); err != nil || !reflect.DeepEqual(status, reconciled.Status) {
		t.Fatalf("status after third reconcile = %#v, %v", status, err)
	}
	if _, err := runtime.ReconcileSource(ctx, ReconcileSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: status.GenerationID,
		ExpectedCommitOID: firstCommit, TargetCommitOID: secondCommit, IdempotencyKey: "stale-reconcile",
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale reconcile = %v", err)
	}

	revoked, err := runtime.RevokeSource(ctx, RevokeSourceRequest{
		IngestionContext: requestContext, ExpectedGenerationID: reconciled.Status.GenerationID,
		RevocationEpoch: 2, IdempotencyKey: "revoke-source",
	})
	if err != nil || !revoked.Status.Revoked {
		t.Fatalf("revoke = %#v, %v", revoked, err)
	}
	if _, err := runtime.SearchCode(ctx, SearchCodeRequest{
		IngestionContext: requestContext, GenerationID: reconciled.Status.GenerationID,
		Query: "NextAnchor", Kind: SearchExact, Limit: 10,
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("search after revoke = %v", err)
	}
}

func TestIngestionRuntimeDeniesBeforeRepositoryWorkAndValidatesConfiguration(t *testing.T) {
	ctx := context.Background()
	repository := t.TempDir()
	writeRepositoryFiles(t, repository, map[string]string{"main.go": "package sample\n"})
	commit := commitRepository(t, repository, "initial")
	config, keys := durableTestConfig(t.TempDir())
	config.Ingestion = testIngestionConfig(repository)
	runtime, err := OpenDurable(ctx, config, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	identity := testIdentity()
	if _, err := runtime.OpenSession(ctx, identity); err != nil {
		t.Fatal(err)
	}
	requestContext := testIngestionContext(config, identity)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := runtime.AddSource(canceled, AddSourceRequest{
		IngestionContext: requestContext, ExpectedCommitOID: commit, IdempotencyKey: "canceled-add",
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled add = %v", err)
	}
	requestContext.Authorize = func(context.Context, Identity, string, Identifier) (Authorization, error) {
		return Authorization{Allowed: false}, nil
	}
	if err := os.Rename(filepath.Join(repository, ".git"), filepath.Join(repository, ".git-hidden")); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.AddSource(ctx, AddSourceRequest{
		IngestionContext: requestContext, ExpectedCommitOID: commit, IdempotencyKey: "denied-add",
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied add = %v", err)
	}

	invalid, invalidKeys := durableTestConfig(t.TempDir())
	invalid.Ingestion = testIngestionConfig(repository)
	invalid.Ingestion.ApprovedRoot = "relative"
	if _, err := OpenDurable(ctx, invalid, invalidKeys); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relative root = %v", err)
	}
}

func assertSearch(t *testing.T, runtime *Runtime, request SearchCodeRequest, path, content string) {
	t.Helper()
	result, err := runtime.SearchCode(context.Background(), request)
	if err != nil || len(result.Matches) == 0 || result.Matches[0].Path != path ||
		result.Matches[0].Content != content || result.Matches[0].BlobOID == "" {
		t.Fatalf("search = %#v, %v", result, err)
	}
}

func testIngestionConfig(root string) *IngestionConfig {
	return &IngestionConfig{
		ApprovedRoot: root, GitExecutable: "/usr/bin/git", RepositoryID: "test-repository",
		CommandTimeout: 10 * time.Second, MaxFiles: 256, MaxPathBytes: 4096,
		MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxIdempotencyRecords: 128,
	}
}

func testIngestionContext(config DurableConfig, identity Identity) IngestionContext {
	return IngestionContext{
		Identity: identity, ConfigurationDigest: config.ConfigurationDigest,
		Policy: IngestionPolicyBothIgnoreNoFollow, Fence: 1,
		Authorize: func(_ context.Context, got Identity, action string, resource Identifier) (Authorization, error) {
			if got != identity || !stringsHasPrefix(action, "source.") || resource != config.Brain {
				return Authorization{}, ErrDenied
			}
			return Authorization{Allowed: true, ReasonCode: "allowed", RevocationEpoch: 1}, nil
		},
	}
}

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

func writeRepositoryFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func commitRepository(t *testing.T, root, message string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); errors.Is(err, os.ErrNotExist) {
		runRepositoryGit(t, root, "init", "--quiet")
	}
	runRepositoryGit(t, root, "add", "-A")
	runRepositoryGit(t, root, "-c", "user.name=Ouroboros Test", "-c", "user.email=test@example.invalid",
		"commit", "--quiet", "-m", message)
	command := exec.Command("/usr/bin/git", "rev-parse", "HEAD")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output[:len(output)-1])
}

func runRepositoryGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("/usr/bin/git", args...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

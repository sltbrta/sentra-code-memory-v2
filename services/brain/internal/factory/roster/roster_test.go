package roster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate/schema"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	_ "modernc.org/sqlite"
)

type testClock struct{ now int64 }

func (c *testClock) NowUnixMilli() int64 { return c.now }

// openFixtureDB builds a real migrated authority database with one run, one
// leaf plan node, and the admitting session seeded through the canonical
// ledgers, then returns a direct handle for roster-level tests.
func openFixtureDB(t *testing.T) (*sql.DB, *testClock, *Store) {
	t.Helper()
	ctx := context.Background()
	path := t.TempDir() + "/authority.db"
	authority, err := localstate.OpenWithMigrations(ctx, path, schema.Migrations(), localstate.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.OpenSession(ctx, contracts.MappedIdentityFact{
		Principal:   contracts.Identifier{Namespace: "principal", Value: "p1"},
		Tenant:      contracts.Identifier{Namespace: "tenant", Value: "t1"},
		Session:     contracts.Identifier{Namespace: "session", Value: "s1"},
		Credentials: contracts.PeerCredentials{UID: 501, PID: 4242},
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"+
		"&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(16)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO factory_runs
		(tenant_id,principal_id,run_id,session_id,intent_id,intent_digest,intent_artifact_id,
		 repository_git_oid,plan_id,admitted_at_ms)
		VALUES ('t1','p1','run-1','s1','intent-1',printf('%064d',9),'artifact-intent',
		 printf('%040d',7),'plan-1',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO factory_plan_nodes
		(tenant_id,principal_id,run_id,node_id,kind,goal_artifact_id,goal_digest,owned_paths,
		 route_profile_digest,route_model_identity,route_rationale_code,grant_actions,grant_allowed_paths,
		 grant_nonce,grant_expires_at_ms,grant_revocation_epoch,grant_command_fence,grant_policy_digest)
		VALUES ('t1','p1','run-1','leaf-a','leaf','artifact-goal',printf('%064d',8),'["src/go/a.go"]',
		 printf('%064d',2),'model-1','static','["factory.leaf.execute"]','["src/go/a.go"]',
		 'nonce-1',100,1,1,printf('%064d',3))`); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: 10}
	store, err := New(clock)
	if err != nil {
		t.Fatal(err)
	}
	return db, clock, store
}

func leaseFixture(holder string, expiresAt int64) Lease {
	return Lease{
		Tenant: "t1", Principal: "p1", RunID: "run-1", NodeID: "leaf-a",
		HolderPrincipalID: holder, ExpiresAtMs: expiresAt,
	}
}

func TestIssueAssignsDenseFencesAndOneCurrentWinner(t *testing.T) {
	db, clock, store := openFixtureDB(t)
	ctx := context.Background()
	first, err := store.Issue(ctx, db, leaseFixture("worker-1", 100))
	if err != nil {
		t.Fatal(err)
	}
	if first.Fence != 1 || first.LeaseID == "" {
		t.Fatalf("first lease = %#v", first)
	}
	second, err := store.Issue(ctx, db, leaseFixture("worker-2", 200))
	if err != nil {
		t.Fatal(err)
	}
	if second.Fence != 2 {
		t.Fatalf("second fence = %d, want 2", second.Fence)
	}
	current, found, err := store.Current(ctx, db, "t1", "p1", "run-1", "leaf-a")
	if err != nil || !found {
		t.Fatalf("current = %v %v", current, err)
	}
	if current.Fence != 2 || current.HolderPrincipalID != "worker-2" {
		t.Fatalf("current winner = %#v, want fence 2 worker-2", current)
	}
	if _, err := store.Authorize(ctx, db, "t1", "p1", "run-1", "leaf-a", 1); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("superseded fence error = %v, want ErrStaleFence", err)
	}
	if _, err := store.Authorize(ctx, db, "t1", "p1", "run-1", "leaf-a", 2); err != nil {
		t.Fatalf("current fence must authorize: %v", err)
	}
	clock.now = 250
	if _, err := store.Authorize(ctx, db, "t1", "p1", "run-1", "leaf-a", 2); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("expired fence error = %v, want ErrStaleFence", err)
	}
}

func TestDenseFenceTriggerRejectsDirectDuplicates(t *testing.T) {
	db, _, store := openFixtureDB(t)
	ctx := context.Background()
	if _, err := store.Issue(ctx, db, leaseFixture("worker-1", 100)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO factory_leases VALUES ('t1','p1','run-1','leaf-a',1,'lease-x','worker-9',1,100)`,
		`INSERT INTO factory_leases VALUES ('t1','p1','run-1','leaf-a',3,'lease-y','worker-9',1,100)`,
		`UPDATE factory_leases SET holder_principal_id='worker-9' WHERE lease_id != ''`,
		`DELETE FROM factory_leases`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("statement unexpectedly succeeded: %s", statement)
		}
	}
}

func TestCommitResultAuthorizesFenceAndCollapsesReplay(t *testing.T) {
	db, clock, store := openFixtureDB(t)
	ctx := context.Background()
	if _, err := store.Issue(ctx, db, leaseFixture("worker-1", 100)); err != nil {
		t.Fatal(err)
	}
	result := Result{
		Lease:      leaseFixture("worker-1", 100),
		ArtifactID: "artifact-result",
		Digest:     "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	result.Lease.Fence = 1
	committed, err := store.CommitResult(ctx, db, result)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Replayed {
		t.Fatal("first commit replayed")
	}
	replayed, err := store.CommitResult(ctx, db, result)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed {
		t.Fatal("exact commit replay was not collapsed")
	}
	conflicting := result
	conflicting.Digest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if _, err := store.CommitResult(ctx, db, conflicting); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("conflicting result error = %v, want ErrResultConflict", err)
	}
	clock.now = 150
	stale := Result{
		Lease:      Lease{Tenant: "t1", Principal: "p1", RunID: "run-1", NodeID: "leaf-a", Fence: 1},
		ArtifactID: "artifact-other",
		Digest:     "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}
	// Fence authorization precedes replay resolution: an expired-fence commit
	// denies as stale and never reaches the canonical result.
	if _, err := store.CommitResult(ctx, db, stale); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("expired second commit error = %v, want ErrStaleFence", err)
	}
}

func TestConcurrentIssuersProduceDistinctDenseFences(t *testing.T) {
	db, _, store := openFixtureDB(t)
	ctx := context.Background()
	const issuers = 8
	var wait sync.WaitGroup
	fences := make([]uint64, issuers)
	errs := make([]error, issuers)
	for index := 0; index < issuers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				errs[index] = err
				return
			}
			defer func() { _ = tx.Rollback() }()
			lease, err := store.Issue(ctx, tx, leaseFixture(fmt.Sprintf("worker-%d", index), 1_000))
			if err != nil {
				errs[index] = err
				return
			}
			if err := tx.Commit(); err != nil {
				errs[index] = err
				return
			}
			fences[index] = lease.Fence
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("issuer %d: %v", index, err)
		}
	}
	seen := map[uint64]bool{}
	for _, fence := range fences {
		if fence < 1 || fence > issuers || seen[fence] {
			t.Fatalf("fences are not dense and distinct: %v", fences)
		}
		seen[fence] = true
	}
	current, found, err := store.Current(ctx, db, "t1", "p1", "run-1", "leaf-a")
	if err != nil || !found {
		t.Fatal(err)
	}
	if current.Fence != issuers {
		t.Fatalf("current fence = %d, want %d after %d issuances", current.Fence, issuers, issuers)
	}
}

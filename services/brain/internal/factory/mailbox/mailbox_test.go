package mailbox

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/localstate/schema"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
	_ "modernc.org/sqlite"
)

type testClock struct{ now int64 }

func (c *testClock) NowUnixMilli() int64 { return c.now }

const testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO factory_runs
		(tenant_id,principal_id,run_id,session_id,intent_id,intent_digest,intent_artifact_id,
		 repository_git_oid,plan_id,admitted_at_ms)
		VALUES ('t1','p1','run-1','s1','intent-1',printf('%064d',9),'artifact-intent',
		 printf('%040d',7),'plan-1',1)`); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"leaf-a", "leaf-b"} {
		if _, err := db.Exec(`INSERT INTO factory_plan_nodes
			(tenant_id,principal_id,run_id,node_id,kind,goal_artifact_id,goal_digest,owned_paths,
			 route_profile_digest,route_model_identity,route_rationale_code,grant_actions,grant_allowed_paths,
			 grant_nonce,grant_expires_at_ms,grant_revocation_epoch,grant_command_fence,grant_policy_digest)
			VALUES ('t1','p1','run-1',?,'leaf','artifact-goal',printf('%064d',8),'["src/go"]',
			 printf('%064d',2),'model-1','static','["factory.leaf.execute"]','["src/go"]',
			 'nonce-1',100,1,1,printf('%064d',3))`, node); err != nil {
			t.Fatal(err)
		}
	}
	clock := &testClock{now: 10}
	store, err := New(clock)
	if err != nil {
		t.Fatal(err)
	}
	return db, clock, store
}

func messageFixture(id, task string) Message {
	return Message{
		Tenant: "t1", Principal: "p1", RunID: "run-1", TaskID: task, MessageID: id,
		Kind: KindQuestion, SenderPrincipalID: "p1", PayloadArtifactID: "artifact-" + id,
		PayloadDigest: testDigest,
	}
}

func TestDuplicateDeliveryCollapsesAndConflictingReuseFails(t *testing.T) {
	db, _, store := openFixtureDB(t)
	ctx := context.Background()
	first, err := store.Send(ctx, db, messageFixture("msg-1", "leaf-a"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || first.Replayed {
		t.Fatalf("first send = %#v", first)
	}
	replayed, err := store.Send(ctx, db, messageFixture("msg-1", "leaf-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Sequence != 1 {
		t.Fatalf("duplicate delivery did not collapse: %#v", replayed)
	}
	conflicting := messageFixture("msg-1", "leaf-a")
	conflicting.PayloadDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := store.Send(ctx, db, conflicting); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("conflicting reuse error = %v, want ErrMessageConflict", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM factory_mailbox_messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("messages = %d, want exactly 1 after duplicates", count)
	}
}

func TestSequencesAreDensePerTask(t *testing.T) {
	db, _, store := openFixtureDB(t)
	ctx := context.Background()
	for index, id := range []string{"msg-1", "msg-2", "msg-3"} {
		result, err := store.Send(ctx, db, messageFixture(id, "leaf-a"))
		if err != nil {
			t.Fatal(err)
		}
		if result.Sequence != uint64(index+1) {
			t.Fatalf("sequence = %d, want %d", result.Sequence, index+1)
		}
	}
	other, err := store.Send(ctx, db, messageFixture("msg-4", "leaf-b"))
	if err != nil {
		t.Fatal(err)
	}
	if other.Sequence != 1 {
		t.Fatalf("second task sequence = %d, want 1", other.Sequence)
	}
	if _, err := db.Exec(`INSERT INTO factory_mailbox_messages
		(tenant_id,principal_id,run_id,message_id,task_id,kind,sequence,correlation_id,causation_id,
		 sender_principal_id,payload_artifact_id,payload_digest,sent_at_ms)
		VALUES ('t1','p1','run-1','msg-gap','leaf-a','QUESTION',9,'','','p1','artifact-x',printf('%064d',1),1)`); err == nil {
		t.Fatal("gapped sequence insert unexpectedly succeeded")
	}
}

func TestPendingFiltersExpiredAndJoinsAcknowledgements(t *testing.T) {
	db, clock, store := openFixtureDB(t)
	ctx := context.Background()
	fresh := messageFixture("msg-fresh", "leaf-a")
	fresh.Kind = KindBlocked
	if _, err := store.Send(ctx, db, fresh); err != nil {
		t.Fatal(err)
	}
	expiring := messageFixture("msg-expiring", "leaf-a")
	expiring.ExpiresAtMs = 50
	if _, err := store.Send(ctx, db, expiring); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Acknowledge(ctx, db, "t1", "p1", "run-1", "msg-fresh")
	if err != nil || replayed {
		t.Fatalf("first ack = %v %v", replayed, err)
	}
	replayed, err = store.Acknowledge(ctx, db, "t1", "p1", "run-1", "msg-fresh")
	if err != nil || !replayed {
		t.Fatalf("repeat ack = %v %v, want replayed", replayed, err)
	}
	if _, err := store.Acknowledge(ctx, db, "t1", "p1", "run-1", "msg-absent"); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("unknown ack error = %v, want ErrUnknownMessage", err)
	}
	clock.now = 60
	pending, err := store.Pending(ctx, db, "t1", "p1", "run-1", "leaf-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Message.MessageID != "msg-fresh" {
		t.Fatalf("pending = %#v, want only the unexpired message", pending)
	}
	if pending[0].AcknowledgedAtMs == 0 {
		t.Fatal("ack state was not joined")
	}
}

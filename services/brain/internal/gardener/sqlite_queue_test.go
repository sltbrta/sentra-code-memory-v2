package gardener

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteQueueEnqueueClaimComplete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, err := OpenSQLiteQueue(filepath.Join(dir, "gardener.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	ctx := context.Background()
	jobs := PlanEnrichmentJobs("gen-1", map[string]string{
		"doc-a": "RPO is 15 minutes for MedThink.",
	}, []JobKind{JobDoc2Query})
	if err := q.Enqueue(ctx, jobs...); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-enqueue.
	if err := q.Enqueue(ctx, jobs...); err != nil {
		t.Fatal(err)
	}
	n, err := q.PendingCount(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pending=%d err=%v want 1", n, err)
	}

	claimed, err := q.Claim(ctx, "w1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d", len(claimed))
	}
	if claimed[0].Payload["text"] == "" {
		t.Fatal("missing payload text")
	}
	// Second claim while leased should be empty.
	again, err := q.Claim(ctx, "w2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("expected empty claim, got %d", len(again))
	}

	rec, err := DeterministicDoc2QueryWorker{}.Run(ctx, claimed[0], DefaultBudget())
	if err != nil || !rec.OK {
		t.Fatalf("worker: %+v err=%v", rec, err)
	}
	if err := q.Complete(ctx, rec); err != nil {
		t.Fatal(err)
	}
	n, err = q.PendingCount(ctx)
	if err != nil || n != 0 {
		t.Fatalf("after complete pending=%d err=%v", n, err)
	}
}

func TestSQLiteQueueLeaseExpiry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, err := OpenSQLiteQueue(filepath.Join(dir, "gardener.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.LeaseTTL = 50 * time.Millisecond

	ctx := context.Background()
	_ = q.Enqueue(ctx, Job{
		ID: "gen:doc2query:d1", Kind: JobDoc2Query, GenerationID: "gen",
		DocumentID: "d1", Payload: map[string]string{"text": "hello world RPO"},
		CreatedAt: time.Now(),
	})
	first, err := q.Claim(ctx, "w1", 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %v n=%d", err, len(first))
	}
	time.Sleep(80 * time.Millisecond)
	// Expired lease → reclaimable.
	second, err := q.Claim(ctx, "w2", 1)
	if err != nil || len(second) != 1 {
		t.Fatalf("reclaim: err=%v n=%d", err, len(second))
	}
}

func TestDaemonRunOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, err := OpenSQLiteQueue(filepath.Join(dir, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	ctx := context.Background()
	docs := map[string]string{"p": "Policy RPO is four hours."}
	enr := &GenerationEnricher{Queue: q, Budget: DefaultBudget()}
	enr.Budget.MaxJobs = 12
	n, err := enr.OnPublished(ctx, "gen-d", docs)
	if err != nil || n < 1 {
		t.Fatalf("enqueue n=%d err=%v", n, err)
	}
	var waves int
	d := &Daemon{
		Queue: q, Workers: DefaultWorkers(), Budget: DefaultBudget(),
		OnWave: func([]Receipt) { waves++ },
	}
	recs, err := d.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 1 {
		t.Fatal("expected receipts")
	}
	if waves != 1 {
		t.Fatalf("OnWave calls=%d", waves)
	}
	pending, _ := q.PendingCount(ctx)
	if pending != 0 {
		t.Fatalf("pending left %d", pending)
	}
}

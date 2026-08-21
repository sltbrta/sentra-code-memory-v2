package gardener

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// C-009. The scheduler discarded Queue.Complete's error. Complete is what
// releases a claimed job and records its receipt, so losing that write strands
// the job as permanently claimed: it never redelivers, and the wave reports a
// successful receipt for work whose completion was never persisted. The caller
// has no other channel through which to learn this.
//
// The fix records the failure on the receipt. Nothing made Complete fail, so
// reverting it left the suite green.

var errCompleteFailed = errors.New("gardener: durable receipt write failed")

// failingCompleteQueue claims exactly one job and then fails every Complete,
// which is what a full disk or a revoked lease looks like from here.
type failingCompleteQueue struct {
	job       Job
	claimed   bool
	completes int
}

func (q *failingCompleteQueue) Enqueue(context.Context, ...Job) error { return nil }

func (q *failingCompleteQueue) Claim(context.Context, string, int) ([]Job, error) {
	if q.claimed {
		return nil, nil
	}
	q.claimed = true
	return []Job{q.job}, nil
}

func (q *failingCompleteQueue) Complete(context.Context, Receipt) error {
	q.completes++
	return errCompleteFailed
}

func schedulerOverFailingComplete(queue Queue) *Scheduler {
	return &Scheduler{
		Queue:   queue,
		Workers: map[JobKind]Worker{JobDoc2Query: DeterministicDoc2QueryWorker{}},
		Budget:  Budget{MaxConcurrent: 1, MaxJobs: 4, MaxJobDuration: 2 * time.Second},
	}
}

func TestAFailedCompleteIsRecordedOnTheReceipt(t *testing.T) {
	queue := &failingCompleteQueue{job: Job{
		ID: "job-1", Kind: JobDoc2Query, GenerationID: "gen-1",
		DocumentID: "doc-1", Payload: map[string]string{"text": "some indexed prose"},
	}}
	receipts, err := schedulerOverFailingComplete(queue).RunWave(context.Background(), "w1")
	if err != nil {
		t.Fatalf("RunWave: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(receipts))
	}
	if queue.completes != 1 {
		t.Fatalf("Complete was called %d times, want 1", queue.completes)
	}
	receipt := receipts[0]
	if receipt.OK {
		t.Fatal("the wave reported a successful job whose completion was never " +
			"persisted: the job is stranded as permanently claimed and nothing says so")
	}
	if !strings.Contains(receipt.Error, "complete_failed") {
		t.Fatalf("the receipt does not name the failure: %+v", receipt)
	}
	if receipt.Output == "" {
		t.Fatal("the worker's output was overwritten by the completion failure")
	}
}

// TestAFailedCompleteDoesNotOverwriteAWorkerFailure keeps the recorded cause
// of a job that had already failed for its own reasons.
func TestAFailedCompleteDoesNotOverwriteAWorkerFailure(t *testing.T) {
	queue := &failingCompleteQueue{job: Job{
		// No worker is registered for this kind, so the receipt already
		// carries "no_worker" before Complete is ever attempted.
		ID: "job-2", Kind: JobNREMConsolidate, GenerationID: "gen-1",
		DocumentID: "doc-2",
	}}
	receipts, err := schedulerOverFailingComplete(queue).RunWave(context.Background(), "w1")
	if err != nil {
		t.Fatalf("RunWave: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(receipts))
	}
	if receipts[0].OK {
		t.Fatal("a job with no worker reported success")
	}
	if receipts[0].Error != "no_worker" {
		t.Fatalf("the original failure cause was lost: %+v", receipts[0])
	}
}

// TestASucceedingCompleteLeavesTheReceiptAlone keeps the guard from passing
// because every receipt is failed.
func TestASucceedingCompleteLeavesTheReceiptAlone(t *testing.T) {
	queue := &MemoryQueue{}
	if err := queue.Enqueue(context.Background(), Job{
		ID: "job-3", Kind: JobDoc2Query, GenerationID: "gen-1",
		DocumentID: "doc-3", Payload: map[string]string{"text": "some indexed prose"},
	}); err != nil {
		t.Fatal(err)
	}
	receipts, err := schedulerOverFailingComplete(queue).RunWave(context.Background(), "w1")
	if err != nil {
		t.Fatalf("RunWave: %v", err)
	}
	if len(receipts) != 1 || !receipts[0].OK {
		t.Fatalf("a job whose completion persisted must report success: %+v", receipts)
	}
}

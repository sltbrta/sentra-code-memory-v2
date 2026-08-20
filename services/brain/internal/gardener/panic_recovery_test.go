package gardener

import (
	"context"
	"testing"
	"time"
)

// panicWorker stands in for any worker defect. Before the recovery boundary, a
// panic here had no caller to catch it: the scheduler runs its workers in bare
// goroutines inside long-lived services, so one malformed payload took the
// whole process down.
type panicWorker struct{}

func (panicWorker) Kind() JobKind { return JobNREMConsolidate }
func (panicWorker) Run(context.Context, Job, Budget) (Receipt, error) {
	panic("worker defect")
}

func TestSchedulerConvertsAWorkerPanicIntoAFailedReceipt(t *testing.T) {
	queue := &MemoryQueue{}
	job := Job{
		ID: "job-1", Kind: JobNREMConsolidate, GenerationID: "gen-1",
		DocumentID: "doc-1", Payload: map[string]string{"text": "hello"},
	}
	if err := queue.Enqueue(context.Background(), job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	scheduler := &Scheduler{
		Queue:   queue,
		Workers: map[JobKind]Worker{JobNREMConsolidate: panicWorker{}},
		Budget:  Budget{MaxJobs: 4, MaxJobDuration: 2 * time.Second},
	}

	receipts, err := scheduler.RunWave(context.Background(), "test-worker")
	if err != nil {
		t.Fatalf("RunWave returned an error rather than a failed receipt: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("want 1 receipt, got %d", len(receipts))
	}
	if receipts[0].OK {
		t.Fatalf("a panicking worker must produce a failed receipt: %+v", receipts[0])
	}
	if receipts[0].JobID != "job-1" {
		t.Fatalf("receipt lost its job identity: %+v", receipts[0])
	}
}

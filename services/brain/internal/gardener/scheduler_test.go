package gardener

import (
	"context"
	"testing"
	"time"
)

func TestSchedulerRunsDeterministicDoc2Query(t *testing.T) {
	t.Parallel()
	q := &MemoryQueue{}
	_ = q.Enqueue(context.Background(), Job{
		ID: "j1", Kind: JobDoc2Query, GenerationID: "g1", DocumentID: "d1",
		Payload:   map[string]string{"text": "MedThink RPO is 15 minutes."},
		CreatedAt: time.Now(),
	})
	s := &Scheduler{
		Queue: q,
		Workers: map[JobKind]Worker{
			JobDoc2Query: DeterministicDoc2QueryWorker{},
		},
		Budget: Budget{MaxConcurrent: 4, MaxJobs: 10, MaxJobDuration: time.Second},
	}
	recs, err := s.RunWave(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || !recs[0].OK {
		t.Fatalf("receipts = %+v", recs)
	}
	if recs[0].Artifacts < 1 {
		t.Fatalf("expected artifacts, got %d", recs[0].Artifacts)
	}
}

func TestDefaultBudgetHitsSemanticGraphCap(t *testing.T) {
	t.Parallel()
	b := DefaultBudget()
	if b.MaxWallClock > 20*time.Minute {
		t.Fatalf("wall clock %v exceeds 20m semantic/graph target", b.MaxWallClock)
	}
	if b.MaxConcurrent < 1 {
		t.Fatal("need concurrency")
	}
}

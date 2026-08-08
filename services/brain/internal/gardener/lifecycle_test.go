package gardener_test

import (
	"context"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
)

func TestC1SkipsHeavyWhenHealthy(t *testing.T) {
	t.Parallel()
	pol := gardener.LifecyclePolicy{PredictionError: 0.05, Threshold: 0.15}
	if !pol.ShouldSkipHeavy() {
		t.Fatal("expected skip")
	}
	jobs := gardener.PlanLifecycleJobs("g1", map[string]string{"d1": "hello world body"}, pol)
	if len(jobs) != 1 || jobs[0].Kind != gardener.JobPredictCalibrate {
		t.Fatalf("jobs=%+v", jobs)
	}
}

func TestLifecycleWaveDoesNotRequireDocumentsForGate(t *testing.T) {
	t.Parallel()
	q := &gardener.MemoryQueue{}
	ctx := context.Background()
	docs := map[string]string{"a": "alpha text", "b": "beta text"}
	pol := gardener.LifecyclePolicy{
		PredictionError: 0.5, // force heavy
		Utility:         map[string]float64{"a": 1, "b": 0.5},
		Edges:           map[string]float64{"a->b": 0.05, "b->a": 0.9},
	}
	recs, err := gardener.RunLifecycle(ctx, q, "gen-1", docs, pol, gardener.Budget{MaxConcurrent: 4, MaxJobs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("no receipts")
	}
	kinds := map[gardener.JobKind]int{}
	for _, r := range recs {
		if !r.OK {
			t.Fatalf("failed receipt: %+v", r)
		}
		kinds[r.Kind]++
	}
	for _, need := range []gardener.JobKind{
		gardener.JobPredictCalibrate, gardener.JobNREMConsolidate, gardener.JobUtilityDecay,
		gardener.JobHypothesisTest, gardener.JobWeakEdgePrune, gardener.JobGCQuarantine,
	} {
		if kinds[need] == 0 {
			t.Fatalf("missing kind %s in %v", need, kinds)
		}
	}
}

func TestC1WaveOnlyGate(t *testing.T) {
	t.Parallel()
	q := &gardener.MemoryQueue{}
	ctx := context.Background()
	pol := gardener.LifecyclePolicy{PredictionError: 0.01}
	recs, err := gardener.RunLifecycle(ctx, q, "g", map[string]string{"d": "x"}, pol, gardener.DefaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Output != "skip_heavy" {
		t.Fatalf("%+v", recs)
	}
}

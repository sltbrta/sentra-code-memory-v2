package gardener

import (
	"strings"
	"testing"
)

func TestPlanEnrichmentJobsOnePerKindPerDoc(t *testing.T) {
	t.Parallel()
	docs := map[string]string{
		"doc-b": "second",
		"doc-a": "first",
	}
	kinds := []JobKind{JobDoc2Query, JobEdgePropose, JobContextHeader}
	jobs := PlanEnrichmentJobs("gen-1", docs, kinds)
	if len(jobs) != 6 {
		t.Fatalf("want 6 jobs, got %d", len(jobs))
	}
	// Stable order: sorted doc IDs, then kinds in given order.
	wantIDs := []string{
		"gen-1:doc2query:doc-a",
		"gen-1:edge_propose:doc-a",
		"gen-1:context_header:doc-a",
		"gen-1:doc2query:doc-b",
		"gen-1:edge_propose:doc-b",
		"gen-1:context_header:doc-b",
	}
	for i, j := range jobs {
		if j.ID != wantIDs[i] {
			t.Fatalf("job[%d].ID = %q, want %q", i, j.ID, wantIDs[i])
		}
		if j.GenerationID != "gen-1" {
			t.Fatalf("generation = %q", j.GenerationID)
		}
		if j.Payload["text"] == "" {
			t.Fatalf("empty payload on %s", j.ID)
		}
	}
}

func TestPlanEnrichmentJobsTruncatesPayload(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", payloadTextCap+500)
	jobs := PlanEnrichmentJobs("g", map[string]string{"d1": long}, []JobKind{JobDoc2Query})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	if got := len([]rune(jobs[0].Payload["text"])); got != payloadTextCap {
		t.Fatalf("payload runes = %d, want %d", got, payloadTextCap)
	}
}

func TestPlanEnrichmentJobsDefaultKinds(t *testing.T) {
	t.Parallel()
	jobs := PlanEnrichmentJobs("g", map[string]string{"d": "t"}, nil)
	if len(jobs) != 3 {
		t.Fatalf("default kinds: want 3 jobs, got %d", len(jobs))
	}
	seen := map[JobKind]bool{}
	for _, j := range jobs {
		seen[j.Kind] = true
	}
	for _, k := range []JobKind{JobDoc2Query, JobEdgePropose, JobContextHeader} {
		if !seen[k] {
			t.Fatalf("missing default kind %s", k)
		}
	}
}

func TestPlanEnrichmentJobsBudgetedCaps(t *testing.T) {
	t.Parallel()
	docs := map[string]string{"a": "1", "b": "2", "c": "3"}
	jobs := PlanEnrichmentJobsBudgeted("g", docs, []JobKind{JobDoc2Query}, Budget{MaxJobs: 2})
	if len(jobs) != 2 {
		t.Fatalf("want 2, got %d", len(jobs))
	}
	// Uncapped when MaxJobs is 0.
	all := PlanEnrichmentJobsBudgeted("g", docs, []JobKind{JobDoc2Query}, Budget{})
	if len(all) != 3 {
		t.Fatalf("uncapped want 3, got %d", len(all))
	}
}

func TestStableJobIDIdempotent(t *testing.T) {
	t.Parallel()
	a := PlanEnrichmentJobs("gen", map[string]string{"x": "body"}, []JobKind{JobEdgePropose})
	b := PlanEnrichmentJobs("gen", map[string]string{"x": "body"}, []JobKind{JobEdgePropose})
	if a[0].ID != b[0].ID {
		t.Fatalf("ids differ: %q vs %q", a[0].ID, b[0].ID)
	}
	if a[0].ID != "gen:edge_propose:x" {
		t.Fatalf("id = %q", a[0].ID)
	}
}

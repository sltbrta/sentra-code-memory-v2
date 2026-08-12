package sessionlog_test

import (
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
)

func TestContinuationBudgetRejectsOnlyNonFittingPointers(t *testing.T) {
	t.Parallel()
	events := []sessionlog.Event{
		{Kind: sessionlog.KindRead, Freshness: sessionlog.FreshnessPointer,
			Provenance: sessionlog.Provenance{Path: "long-pointer.go", Range: sessionlog.Range{Start: 1, End: 5000}}},
		{Kind: sessionlog.KindRead, Freshness: sessionlog.FreshnessPointer,
			Provenance: sessionlog.Provenance{Path: "a.go", Range: sessionlog.Range{Start: 1, End: 5000}}},
	}
	opts := sessionlog.DefaultContinuationOptions()
	opts.Budgets = sessionlog.Budgets{L0: len("a.go") + 32}
	cont, err := sessionlog.BuildContinuation(events,
		sessionlog.Provenance{Repository: "local", Tree: "t"}, opts,
		time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(cont.ReadRanges) != 1 || cont.ReadRanges[0].Path != "a.go" {
		t.Fatalf("small pointer should still fit after rejection: %+v", cont.ReadRanges)
	}
	if cont.Budgets.Used.L0 != len("a.go")+32 {
		t.Fatalf("used budget includes rejected pointer: %+v", cont.Budgets)
	}
}

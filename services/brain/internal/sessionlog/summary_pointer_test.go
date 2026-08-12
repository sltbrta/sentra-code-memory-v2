package sessionlog_test

import (
	"fmt"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
)

func TestSummaryReadRangesRetainPathIdentity(t *testing.T) {
	t.Parallel()
	events := []sessionlog.Event{
		{Schema: sessionlog.Schema, Seq: 1, Time: "2026-08-12T09:00:00Z", Kind: sessionlog.KindRead,
			Provenance: sessionlog.Provenance{Path: "a.go", Range: sessionlog.Range{Start: 1, End: 10}}},
		{Schema: sessionlog.Schema, Seq: 2, Time: "2026-08-12T09:00:01Z", Kind: sessionlog.KindRead,
			Provenance: sessionlog.Provenance{Path: "b.go", Range: sessionlog.Range{Start: 1, End: 10}}},
	}
	summary, err := sessionlog.Rebuild(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.ReadRanges) != 2 {
		t.Fatalf("identical ranges from different paths collapsed: %+v", summary.ReadRanges)
	}
	if summary.ReadRanges[0].Path != "a.go" || summary.ReadRanges[1].Path != "b.go" {
		t.Fatalf("summary paths wrong: %+v", summary.ReadRanges)
	}
}

func TestSummaryNotesStayBounded(t *testing.T) {
	t.Parallel()
	events := make([]sessionlog.Event, 0, 100)
	for i := 0; i < 100; i++ {
		events = append(events, sessionlog.Event{
			Schema: sessionlog.Schema, Seq: uint64(i + 1),
			Time: fmt.Sprintf("2026-08-12T09:00:%02dZ", i%60), Kind: sessionlog.KindFailure,
			FreeText: fmt.Sprintf("failure-%d", i), CoverageWarnings: []string{fmt.Sprintf("warning-%d", i)},
		})
	}
	summary, err := sessionlog.Rebuild(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Failures) > 64 || len(summary.CoverageWarnings) > 64 {
		t.Fatalf("summary notes exceeded bounds: failures=%d warnings=%d", len(summary.Failures), len(summary.CoverageWarnings))
	}
}

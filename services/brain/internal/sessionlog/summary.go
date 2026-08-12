package sessionlog

import (
	"encoding/json"
	"sort"
)

// Summary is the deterministic session projection rebuilt from an event stream
// (Phase 4, issue #31). The same events always rebuild the same Summary, so
// incremental and replayed state agree.
type Summary struct {
	// Session is the session id of the last event (empty until the first).
	Session string `json:"session,omitempty"`
	// EventCounts tallies events per kind, lexical key order.
	EventCounts map[string]int64 `json:"event_counts,omitempty"`
	// Totals aggregates caller-supplied counts across all events.
	Totals map[string]int64 `json:"totals,omitempty"`
	// ReadRanges are the unique path/range pointers seen (L0 retention).
	ReadRanges []Range `json:"read_ranges,omitempty"`
	// ChangedFiles are the unique edited/refreshed paths (sorted, unique).
	ChangedFiles []string `json:"changed_files,omitempty"`
	// Failures are the bounded failure notes (issue #27 continuation field).
	Failures []string `json:"failures,omitempty"`
	// CoverageWarnings aggregate coverage gaps.
	CoverageWarnings []string `json:"coverage_warnings,omitempty"`
	// LastFreshness is the freshness of the most recent non-compaction event.
	LastFreshness Freshness `json:"last_freshness,omitempty"`
	// Started/Completed track lifecycle flags.
	Started   bool `json:"started,omitempty"`
	Completed bool `json:"completed,omitempty"`
	// FirstTime/LastTime bound the stream.
	FirstTime string `json:"first_time,omitempty"`
	LastTime  string `json:"last_time,omitempty"`
}

// summaryBuilder accumulates a Summary from events; it is the live projection
// used by both Append-time state and Replay/Rebuild to prove determinism.
type summaryBuilder struct {
	s            Summary
	readRangeSet map[Range]struct{}
	changedSet   map[string]struct{}
}

func newSummaryBuilder() *summaryBuilder {
	return &summaryBuilder{
		readRangeSet: map[Range]struct{}{},
		changedSet:   map[string]struct{}{},
		s: Summary{
			EventCounts: map[string]int64{},
			Totals:      map[string]int64{},
		},
	}
}

// Apply folds one event into the in-progress summary.
func (b *summaryBuilder) Apply(ev Event) error {
	if b.s.Session == "" {
		b.s.Session = ev.Session
	}
	if b.s.FirstTime == "" || (ev.Time != "" && ev.Time < b.s.FirstTime) {
		b.s.FirstTime = ev.Time
	}
	if ev.Time > b.s.LastTime {
		b.s.LastTime = ev.Time
	}
	b.s.EventCounts[string(ev.Kind)]++
	for k, v := range ev.Counts {
		b.s.Totals[k] += v
	}
	if ev.Provenance.Path != "" && ev.Provenance.Range != (Range{}) {
		if _, ok := b.readRangeSet[ev.Provenance.Range]; !ok && ev.Provenance.Path != "" {
			b.readRangeSet[ev.Provenance.Range] = struct{}{}
			b.s.ReadRanges = append(b.s.ReadRanges, ev.Provenance.Range)
		}
	}
	switch ev.Kind {
	case KindEdit, KindRefresh:
		if ev.Provenance.Path != "" {
			if _, ok := b.changedSet[ev.Provenance.Path]; !ok {
				b.changedSet[ev.Provenance.Path] = struct{}{}
				b.s.ChangedFiles = append(b.s.ChangedFiles, ev.Provenance.Path)
			}
		}
	case KindFailure:
		if ev.FreeText != "" {
			b.s.Failures = append(b.s.Failures, ev.FreeText)
		}
	case KindTaskStart:
		b.s.Started = true
	case KindCompletion:
		b.s.Completed = true
	}
	for _, w := range ev.CoverageWarnings {
		b.s.CoverageWarnings = append(b.s.CoverageWarnings, w)
	}
	if ev.Kind != KindCompaction && ev.Freshness != "" {
		b.s.LastFreshness = ev.Freshness
	}
	return nil
}

// finalize sorts slice fields so the Summary is order-independent and stable.
func (b *summaryBuilder) finalize() Summary {
	s := b.s
	s.EventCounts = canonicalCounts(s.EventCounts)
	s.Totals = canonicalCounts(s.Totals)
	sort.Slice(s.ReadRanges, func(i, j int) bool {
		if s.ReadRanges[i].Start != s.ReadRanges[j].Start {
			return s.ReadRanges[i].Start < s.ReadRanges[j].Start
		}
		return s.ReadRanges[i].End < s.ReadRanges[j].End
	})
	sort.Strings(s.ChangedFiles)
	sort.Strings(s.CoverageWarnings)
	return s
}

// Rebuild reconstructs the deterministic Summary from a persisted event
// stream. It is the canonical replay path (issue #31): a Summary built live
// (one Apply per appended event) must equal Rebuild(Writer.Events()).
func Rebuild(events []Event) (Summary, error) {
	b := newSummaryBuilder()
	if err := Replay(events, b); err != nil {
		return Summary{}, err
	}
	return b.finalize(), nil
}

// MarshalJSON emits canonical JSON with lexical-count key order.
func (s Summary) MarshalJSON() ([]byte, error) {
	type alias Summary
	cpy := alias(s)
	if len(cpy.EventCounts) > 0 {
		cpy.EventCounts = canonicalCounts(cpy.EventCounts)
	}
	if len(cpy.Totals) > 0 {
		cpy.Totals = canonicalCounts(cpy.Totals)
	}
	return json.Marshal(cpy)
}

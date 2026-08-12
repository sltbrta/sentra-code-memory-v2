package sessionlog_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
)

// fixedClock returns successive UTC times for deterministic write ordering.
func fixedClock() sessionlog.Clock {
	t := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

// sampleEvents returns a representative ordered run across all kinds.
func sampleEvents(session string) []sessionlog.Event {
	return []sessionlog.Event{
		{Kind: sessionlog.KindTaskStart, Session: session, FreeText: "implement phase 4-5 slice"},
		{Kind: sessionlog.KindContextServed, Session: session, Verb: "code_find_relevant",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "abc123",
				Path: "a/b.go", Range: sessionlog.Range{Start: 1, End: 9}, Symbol: "Alpha"},
			Freshness: sessionlog.FreshnessAsOf, Counts: map[string]int64{"served_bytes": 240}},
		{Kind: sessionlog.KindRead, Session: session, Verb: "code_read",
			Provenance: sessionlog.Provenance{Path: "a/b.go", Range: sessionlog.Range{Start: 10, End: 20}},
			Freshness:  sessionlog.FreshnessPointer},
		{Kind: sessionlog.KindEdit, Session: session, Verb: "code_edit",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "abc123",
				Path: "a/b.go", Range: sessionlog.Range{Start: 12, End: 14}, Symbol: "Alpha", Confidence: 0.9},
			PredictedDigest: "pred123", ObservedDigest: "obs456"},
		{Kind: sessionlog.KindTest, Session: session, Counts: map[string]int64{"tests": 3, "failed": 0}},
		{Kind: sessionlog.KindFailure, Session: session, FreeText: "compile error in b.go"},
		{Kind: sessionlog.KindCompletion, Session: session, FreeText: "all green"},
	}
}

func TestAppendAssignsSealedFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range sampleEvents("s1") {
		if _, err := w.Append(ev); err != nil {
			t.Fatalf("append %s: %v", ev.Kind, err)
		}
	}
	events := w.Events()
	if len(events) != 7 {
		t.Fatalf("want 7 events, got %d", len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("seq %d want %d", ev.Seq, i+1)
		}
		if ev.Schema != sessionlog.Schema {
			t.Fatalf("schema not sealed: %q", ev.Schema)
		}
		if ev.ID == "" {
			t.Fatalf("id empty at %d", i)
		}
		if ev.Time == "" {
			t.Fatalf("time empty at %d", i)
		}
		if err := ev.Validate(); err != nil {
			t.Fatalf("invalid persisted event %d: %v", i, err)
		}
	}
}

func TestUnknownKindRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(sessionlog.Event{Kind: "bogus"}); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestProvenanceFirstAdmission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Durable kinds require provenance; a bare edit is rejected (#29).
	if _, err := w.Append(sessionlog.Event{Kind: sessionlog.KindEdit, Session: "s"}); err == nil {
		t.Fatal("edit without provenance must be rejected")
	}
	// With provenance it is admitted.
	_, err = w.Append(sessionlog.Event{
		Kind: sessionlog.KindEdit, Session: "s",
		Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
			Path: "x.go", Symbol: "X"},
	})
	if err != nil {
		t.Fatalf("edit with provenance should pass: %v", err)
	}
}

func TestPrivacyPathEscapeRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "/abs/path", "a\\b.go", "a/../../c"} {
		ev := sessionlog.Event{
			Kind: sessionlog.KindEdit, Session: "s",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "t", Path: bad},
		}
		if _, err := w.Append(ev); err == nil {
			t.Fatalf("path escape %q must be rejected", bad)
		}
	}
}

func TestFreeTextBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", sessionlog.MaxFreeTextBytes*4)
	ev, err := w.Append(sessionlog.Event{Kind: sessionlog.KindFailure, Session: "s", FreeText: huge})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.FreeText) > sessionlog.MaxFreeTextBytes {
		t.Fatalf("free_text not bounded: %d", len(ev.FreeText))
	}
}

func TestAppendOnlyAndAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	one := sampleEvents("s1")[:3]
	for _, ev := range one {
		if _, err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	// Reopening must read back exactly the same stream (append-only + durable).
	w2, err := sessionlog.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := w2.Events()
	if len(got) != 3 {
		t.Fatalf("reopen want 3 events, got %d", len(got))
	}
	for i := range got {
		if got[i].ID != w.Events()[i].ID {
			t.Fatalf("event %d id drift on reopen", i)
		}
	}
}

func TestMalformedFileRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sessionlog.Filename), []byte("{not json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Open(dir); err == nil {
		t.Fatal("malformed log must not be silently repaired")
	}
}

func TestCompactionBoundsStream(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Very small cap so a compaction is forced quickly.
	w, err := sessionlog.Open(dir, sessionlog.WithMaxEvents(4), sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	// Append more than the cap: oldest must be folded into a compaction event.
	for i := 0; i < 8; i++ {
		if _, err := w.Append(sessionlog.Event{
			Kind: sessionlog.KindRead, Session: "s",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
				Path: "f.go", Range: sessionlog.Range{Start: i, End: i + 1}},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	events := w.Events()
	if len(events) > 4 {
		t.Fatalf("compaction must bound stream: got %d", len(events))
	}
	var folds int
	for _, ev := range events {
		if ev.Kind == sessionlog.KindCompaction {
			folds += ev.Folded
		}
	}
	if folds == 0 {
		t.Fatal("compaction event must record folded count")
	}
	// Compaction events themselves must validate.
	for _, ev := range events {
		if err := ev.Validate(); err != nil {
			t.Fatalf("compacted event invalid: %v", err)
		}
	}
	// The stream stays monotonic and contiguous after compaction.
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("seq non-monotonic after compaction: %d -> %d", i, ev.Seq)
		}
	}
}

func TestReplayDeterministicRebuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	// Build the live summary incrementally as events are appended.
	live, err := sessionlog.Rebuild(nil)
	if err != nil {
		t.Fatal(err)
	}
	var appended []sessionlog.Event
	for _, ev := range sampleEvents("s1") {
		sealed, err := w.Append(ev)
		if err != nil {
			t.Fatal(err)
		}
		appended = append(appended, sealed)
	}
	live, err = sessionlog.Rebuild(appended)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild from the persisted stream must equal the live projection (#31).
	replayed, err := sessionlog.Rebuild(w.Events())
	if err != nil {
		t.Fatal(err)
	}
	liveJSON, _ := json.Marshal(live)
	replayedJSON, _ := json.Marshal(replayed)
	if !bytes.Equal(liveJSON, replayedJSON) {
		t.Fatalf("replay diverged from live:\nlive=%s\nreplay=%s", liveJSON, replayedJSON)
	}
	if replayed.EventCounts["edit"] != 1 || replayed.EventCounts["failure"] != 1 {
		t.Fatalf("event counts wrong: %+v", replayed.EventCounts)
	}
	if len(replayed.ChangedFiles) != 1 || replayed.ChangedFiles[0] != "a/b.go" {
		t.Fatalf("changed files wrong: %v", replayed.ChangedFiles)
	}
}

func TestReplayStrictRejectsBadLine(t *testing.T) {
	t.Parallel()
	// Replay an in-memory stream containing a structurally invalid event.
	bad := []sessionlog.Event{{Kind: "bogus", Seq: 1, Time: "2026-08-12T09:00:00Z"}}
	if err := sessionlog.Replay(bad, sessionlog.ApplierFunc(func(sessionlog.Event) error { return nil })); err == nil {
		t.Fatal("strict replay must reject invalid kind")
	}
}

func TestReadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range sampleEvents("s1") {
		if _, err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	// The on-disk bytes, read through Read, must equal the Writer view.
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	events, err := sessionlog.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(sampleEvents("s1")) {
		t.Fatalf("read round-trip lost events: %d", len(events))
	}
	for i := range events {
		if events[i].ID != w.Events()[i].ID {
			t.Fatalf("read round-trip id drift at %d", i)
		}
	}
}

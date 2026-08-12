package sessionlog_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/sessionlog"
)

func TestContinuationExcludesSupersededByDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	events := []sessionlog.Event{
		{Kind: sessionlog.KindRead, Session: "s", Freshness: sessionlog.FreshnessTimeless,
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
				Path: "timeless.go", Range: sessionlog.Range{Start: 1, End: 2}, Symbol: "Stable"}},
		{Kind: sessionlog.KindRead, Session: "s", Freshness: sessionlog.FreshnessAsOf,
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
				Path: "asof.go", Range: sessionlog.Range{Start: 1, End: 2}, Symbol: "AsOf"}},
		{Kind: sessionlog.KindRead, Session: "s", Freshness: sessionlog.FreshnessSuperseded,
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
				Path: "old.go", Range: sessionlog.Range{Start: 1, End: 2}, Symbol: "Old"}},
	}
	for _, ev := range events {
		if _, err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	cont, err := sessionlog.BuildContinuation(w.Events(),
		sessionlog.Provenance{Repository: "local", Tree: "t"},
		sessionlog.DefaultContinuationOptions(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Superseded path must be excluded by default.
	for _, rp := range cont.ReadRanges {
		if rp.Path == "old.go" {
			t.Fatalf("superseded content leaked into continuation: %+v", cont.ReadRanges)
		}
	}
	if !cont.Stale {
		// as_of content should mark the continuation stale.
		t.Fatal("as_of content must mark continuation stale")
	}
}

func TestContinuationIncludesSupersededOnOptIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Append(sessionlog.Event{Kind: sessionlog.KindRead, Session: "s",
		Freshness: sessionlog.FreshnessSuperseded,
		Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
			Path: "old.go", Range: sessionlog.Range{Start: 1, End: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	opts := sessionlog.DefaultContinuationOptions()
	opts.IncludeSuperseded = true
	cont, err := sessionlog.BuildContinuation(w.Events(),
		sessionlog.Provenance{Repository: "local", Tree: "t"}, opts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !cont.Superseded {
		t.Fatal("opted-in superseded content must be flagged")
	}
	found := false
	for _, rp := range cont.ReadRanges {
		if rp.Path == "old.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("superseded content not included on opt-in: %+v", cont.ReadRanges)
	}
}

func TestContinuationDeterministicJSON(t *testing.T) {
	t.Parallel()
	mk := func() (sessionlog.Continuation, error) {
		dir := t.TempDir()
		w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
		if err != nil {
			return sessionlog.Continuation{}, err
		}
		for _, ev := range sampleEvents("s1") {
			if _, err := w.Append(ev); err != nil {
				return sessionlog.Continuation{}, err
			}
		}
		return sessionlog.BuildContinuation(w.Events(),
			sessionlog.Provenance{Repository: "local", Tree: "t"},
			sessionlog.DefaultContinuationOptions(),
			time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC))
	}
	a, err := mk()
	if err != nil {
		t.Fatal(err)
	}
	b, err := mk()
	if err != nil {
		t.Fatal(err)
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatalf("continuation JSON not deterministic\na=%s\nb=%s", aj, bj)
	}
}

func TestFreshnessClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mark     sessionlog.Freshness
		base     string
		observed string
		want     sessionlog.Freshness
	}{
		{sessionlog.FreshnessSuperseded, "x", "x", sessionlog.FreshnessSuperseded},
		{sessionlog.FreshnessTimeless, "", "", sessionlog.FreshnessTimeless},
		{sessionlog.FreshnessPointer, "b", "b", sessionlog.FreshnessPointer},
		{sessionlog.FreshnessPointer, "b", "c", sessionlog.FreshnessStale},
		{sessionlog.FreshnessPointer, "b", "", sessionlog.FreshnessAsOf},
		{"", "b", "c", sessionlog.FreshnessStale},
		{"", "", "", sessionlog.FreshnessAsOf},
	}
	for _, c := range cases {
		got := sessionlog.Fresh(c.mark, c.base, c.observed)
		if got != c.want {
			t.Fatalf("Fresh(%q,%q,%q)=%q want %q", c.mark, c.base, c.observed, got, c.want)
		}
	}
}

func TestRecallAbstainsBelowThreshold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	events := []sessionlog.Event{
		{Kind: sessionlog.KindContextServed, Session: "s", Verb: "code_find_relevant",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
				Path: "a.go", Symbol: "Alpha", Confidence: 0.9}},
		{Kind: sessionlog.KindContextServed, Session: "s",
			Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
				Path: "z.go", Symbol: "Zeta", Confidence: 0.9}},
	}
	for _, ev := range events {
		if _, err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	// Matching memory is admitted.
	res := sessionlog.Recall(w.Events(), "Alpha", sessionlog.DefaultRecallOptions())
	if res.Abstained || len(res.Memories) != 1 {
		t.Fatalf("expected 1 admission, got %+v", res)
	}
	if res.Memories[0].Provenance.Symbol != "Alpha" {
		t.Fatalf("wrong memory admitted: %+v", res.Memories[0])
	}
	// Unrelated query abstains.
	res = sessionlog.Recall(w.Events(), "Omega", sessionlog.DefaultRecallOptions())
	if !res.Abstained || len(res.Memories) != 0 {
		t.Fatalf("unrelated query must abstain: %+v", res)
	}
	// Low-confidence memory is rejected even when relevant.
	low, err := sessionlog.Open(t.TempDir(), sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := low.Append(sessionlog.Event{Kind: sessionlog.KindContextServed, Session: "s",
		Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
			Path: "a.go", Symbol: "Alpha", Confidence: 0.1}}); err != nil {
		t.Fatal(err)
	}
	res = sessionlog.Recall(low.Events(), "Alpha", sessionlog.DefaultRecallOptions())
	if !res.Abstained {
		t.Fatalf("low-confidence memory must be rejected: %+v", res)
	}
}

func TestRecallExcludesSupersededByDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := sessionlog.Open(dir, sessionlog.WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(sessionlog.Event{Kind: sessionlog.KindContextServed, Session: "s",
		Freshness: sessionlog.FreshnessSuperseded,
		Provenance: sessionlog.Provenance{Repository: "local", Tree: "t",
			Path: "old.go", Symbol: "Alpha", Confidence: 0.9}}); err != nil {
		t.Fatal(err)
	}
	res := sessionlog.Recall(w.Events(), "Alpha", sessionlog.DefaultRecallOptions())
	if !res.Abstained {
		t.Fatalf("superseded memory must be excluded by default: %+v", res)
	}
}

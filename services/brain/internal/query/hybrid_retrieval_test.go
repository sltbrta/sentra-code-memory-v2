package query

import (
	"reflect"
	"testing"
)

func TestRRFFuseClassic(t *testing.T) {
	// Classic: doc in both lists ranks above single-list docs at same ranks.
	lists := [][]string{
		{"a", "b", "c"},
		{"b", "d", "a"},
	}
	got := rrfFuse(lists, 60, 4)
	// b appears rank1 list0 + rank0 list1 → highest; a next; then c, d.
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %v", len(got), got)
	}
	if got[0] != "b" {
		t.Fatalf("top = %q, want b (present in both lists high): %v", got[0], got)
	}
	if got[1] != "a" {
		t.Fatalf("second = %q, want a: %v", got[1], got)
	}
}

func TestRRFFuseTopNAndEmpty(t *testing.T) {
	if got := rrfFuse(nil, 60, 10); got != nil {
		t.Fatalf("empty lists = %v, want nil", got)
	}
	if got := rrfFuse([][]string{{}, {}}, 60, 10); got != nil {
		t.Fatalf("empty inner = %v, want nil", got)
	}
	got := rrfFuse([][]string{{"x", "y", "z"}}, 60, 2)
	if !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Fatalf("topN=2 = %v, want [x y]", got)
	}
	// k <= 0 falls back to defaultRRFK; still ranks first first.
	got = rrfFuse([][]string{{"p", "q"}}, 0, 1)
	if !reflect.DeepEqual(got, []string{"p"}) {
		t.Fatalf("k=0 top1 = %v, want [p]", got)
	}
}

func TestRRFFuseDeterministicTies(t *testing.T) {
	// Disjoint equal ranks: first-seen order across lists breaks ties.
	lists := [][]string{
		{"a"},
		{"b"},
	}
	got := rrfFuse(lists, 60, 2)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tie-break = %v, want [a b] first-seen", got)
	}
}

func TestMultiQueryVariantsOriginalFirst(t *testing.T) {
	q := "What does the billing service return for an overdue invoice?"
	got := multiQueryVariants(q)
	if len(got) == 0 || got[0] != q {
		t.Fatalf("variants = %v, want original first", got)
	}
	if len(got) > multiQueryMaxVariants {
		t.Fatalf("too many variants: %d", len(got))
	}
	// Content short form should drop stopwords and keep content words.
	foundShort := false
	for _, v := range got[1:] {
		if containsAllWords(v, "billing", "service", "overdue", "invoice") {
			foundShort = true
		}
	}
	if !foundShort {
		t.Fatalf("expected content-word short form among %v", got)
	}
}

func TestMultiQueryVariantsIdentifiers(t *testing.T) {
	q := "What is the status of TICKET-123 for api.latency.p99 under SLO_P95?"
	got := multiQueryVariants(q)
	if len(got) == 0 || got[0] != q {
		t.Fatalf("variants = %v, want original first", got)
	}
	// Identifier variant should surface ticket + dotted metric + ALLCAPS.
	joined := ""
	for _, v := range got {
		joined += " " + v
	}
	for _, want := range []string{"TICKET-123", "api.latency.p99", "SLO_P95"} {
		if !containsToken(joined, want) {
			t.Fatalf("variants %v missing identifier %q", got, want)
		}
	}
	if len(got) > multiQueryMaxVariants {
		t.Fatalf("cap exceeded: %v", got)
	}
}

func TestMultiQueryVariantsDedup(t *testing.T) {
	// Short form identical to original (all content words) must not duplicate.
	q := "anchor function"
	got := multiQueryVariants(q)
	seen := map[string]bool{}
	for _, v := range got {
		key := toLower(v)
		if seen[key] {
			t.Fatalf("duplicate variant %q in %v", v, got)
		}
		seen[key] = true
	}
}

func TestExpandWithGraphAppendsNeighbors(t *testing.T) {
	base := []candidate{
		{path: "a.go", definitions: []string{"A"}, degraded: false},
		{path: "b.go", definitions: []string{"B"}, degraded: true},
	}
	neighbors := []string{"b.go", "c.go", "d.go", "e.go"}
	got := expandWithGraph(base, neighbors, 2)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (2 base + 2 extra): %#v", len(got), got)
	}
	if got[0].path != "a.go" || got[1].path != "b.go" {
		t.Fatalf("base order disturbed: %#v", got)
	}
	if got[2].path != "c.go" || got[3].path != "d.go" {
		t.Fatalf("extras = %#v, want c.go then d.go (b.go already present)", got)
	}
	// Neighbors are non-degraded path-only candidates.
	if got[2].degraded || len(got[2].definitions) != 0 {
		t.Fatalf("graph neighbor must be non-degraded path-only: %#v", got[2])
	}
}

func TestExpandWithGraphNoop(t *testing.T) {
	base := []candidate{{path: "a.go"}}
	if got := expandWithGraph(base, []string{"b.go"}, 0); !reflect.DeepEqual(got, base) {
		t.Fatalf("maxExtra=0 changed candidates: %#v", got)
	}
	if got := expandWithGraph(base, nil, 5); !reflect.DeepEqual(got, base) {
		t.Fatalf("nil graph changed candidates: %#v", got)
	}
	if got := expandWithGraph(base, []string{"a.go", ""}, 5); !reflect.DeepEqual(got, base) {
		t.Fatalf("only-already-present changed candidates: %#v", got)
	}
}

func containsAllWords(s string, words ...string) bool {
	lower := toLower(s)
	for _, w := range words {
		if !containsToken(lower, toLower(w)) {
			return false
		}
	}
	return true
}

func containsToken(s, tok string) bool {
	// Simple substring check is enough for unit assertions on joined variants.
	return len(tok) > 0 && (s == tok || len(s) >= len(tok) &&
		(indexOf(s, tok) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

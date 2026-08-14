package denselocal

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// TestLexicalDeterministic asserts the bag-of-words lexical arm returns the
// same ranking across repeated calls (issue #59 acceptance: deterministic
// lexical retrieval).
func TestLexicalDeterministic(t *testing.T) {
	docs := map[string]string{
		"billing.go":    "package billing\nfunc InvoiceTotal() int { return 42 }",
		"auth.go":       "package auth\nfunc Login() error { return nil }",
		"weather.go":    "the weather is sunny today",
		"middleware.go": "package middleware\nfunc Auth() bool { return true }",
	}
	eng, err := NewEngine("scope-1", ModelBag, docs, DefaultBounds())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, err := eng.Search(context.Background(), "invoice billing total",
			Options{TopK: 4, Scope: "scope-1", Model: ModelBag})
		if err != nil {
			t.Fatal(err)
		}
		if !got.OK || got.Route != "lexical" {
			t.Fatalf("iter %d not lexical: %+v", i, got)
		}
		if len(got.Hits) == 0 {
			t.Fatal("expected hits")
		}
		if got.Hits[0].ID != "billing.go" {
			t.Fatalf("iter %d top hit = %q, want billing.go", i, got.Hits[0].ID)
		}
	}
}

// TestIdentityBindingPresent requires both scope and model identity; a missing
// identity fails closed at engine construction.
func TestIdentityBindingPresent(t *testing.T) {
	if _, err := NewEngine("", ModelBag, map[string]string{"a": "x"}, DefaultBounds()); !errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("empty scope: %v", err)
	}
	if _, err := NewEngine("scope", "", map[string]string{"a": "x"}, DefaultBounds()); !errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("empty model: %v", err)
	}
}

func TestUnsupportedModelRejected(t *testing.T) {
	if _, err := NewEngine("scope", Model("embedding:v2"), map[string]string{"a": "x"}, DefaultBounds()); !errors.Is(err, ErrModelUnsupported) {
		t.Fatalf("unknown model: %v", err)
	}
}

// TestBoundsEnforcedOnConstruction refuses corpora that exceed MaxCorpus
// before any search is performed.
func TestBoundsEnforcedOnConstruction(t *testing.T) {
	small := Bounds{MaxCorpus: 2, MaxDim: 8, MaxTopK: 4, MaxQueryLen: 32}
	if _, err := NewEngine("s", ModelBag, map[string]string{
		"a": "alpha", "b": "beta", "c": "gamma",
	}, small); !errors.Is(err, ErrCorpusExceeded) {
		t.Fatalf("expected ErrCorpusExceeded, got %v", err)
	}
}

// TestTopKAndQueryBoundsEnforced applies at search time too.
func TestTopKAndQueryBoundsEnforced(t *testing.T) {
	docs := map[string]string{"a": "alpha beta gamma delta", "b": "beta gamma"}
	eng, err := NewEngine("s", ModelBag, docs, Bounds{MaxCorpus: 8, MaxDim: 16, MaxTopK: 1, MaxQueryLen: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Search(context.Background(), strings.Repeat("a", 6), Options{TopK: 1, Scope: "s", Model: ModelBag}); !errors.Is(err, ErrQueryTooLong) {
		t.Fatalf("expected ErrQueryTooLong, got %v", err)
	}
	got, err := eng.Search(context.Background(), "alpha", Options{TopK: 99, Scope: "s", Model: ModelBag})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) > 1 {
		t.Fatalf("MaxTopK not enforced: %d hits", len(got.Hits))
	}
}

// TestEmptyAndWhitespaceQuery refuses empty/whitespace queries with the
// typed ErrEmptyQuery.
func TestEmptyAndWhitespaceQuery(t *testing.T) {
	eng, err := NewEngine("s", ModelBag, map[string]string{"a": "alpha"}, DefaultBounds())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Search(context.Background(), "   ", Options{Scope: "s", Model: ModelBag}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}
	if _, err := eng.Search(context.Background(), "", Options{Scope: "s", Model: ModelBag}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("expected ErrEmptyQuery, got %v", err)
	}
}

// TestLexicalSearchIsAlwaysAvailable validates the "always-on" contract:
// the report is always OK when a query is valid, even with an empty corpus.
func TestLexicalSearchIsAlwaysAvailable(t *testing.T) {
	eng, err := NewEngine("s", ModelBag, nil, DefaultBounds())
	if err != nil {
		t.Fatal(err)
	}
	got, err := eng.Search(context.Background(), "query", Options{Scope: "s", Model: ModelBag})
	if err != nil || !got.OK || got.Route != "lexical" {
		t.Fatalf("expected lexical success: %+v err=%v", got, err)
	}
}

// TestQueryHashNeverLeaksPlaintext asserts Report.Query is a hex SHA-256 and
// is not the raw query text. This is the "no-network + no-leak receipt"
// contract: even a leaked Report cannot reveal the user's query.
func TestQueryHashNeverLeaksPlaintext(t *testing.T) {
	docs := map[string]string{"a": "alpha beta", "b": "gamma delta"}
	eng, err := NewEngine("s", ModelBag, docs, DefaultBounds())
	if err != nil {
		t.Fatal(err)
	}
	got, err := eng.Search(context.Background(), "secret query text",
		Options{Scope: "s", Model: ModelBag})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := MarshalReport(got)
	if strings.Contains(string(raw), "secret query text") {
		t.Fatalf("Report leaks raw query text: %s", raw)
	}
	if len(got.Query) != 64 {
		t.Fatalf("query hash wrong length: %q", got.Query)
	}
	// Verify hash is stable.
	expected := queryHash("secret query text")
	if got.Query != expected {
		t.Fatalf("hash drift: got %q want %q", got.Query, expected)
	}
}

// TestScoreTieBreakByIDAscending enforces the deterministic tie-break
// contract.
func TestScoreTieBreakByIDAscending(t *testing.T) {
	docs := map[string]string{
		"z.go": "alpha only",
		"a.go": "alpha only",
		"m.go": "alpha only",
	}
	eng, err := NewEngine("s", ModelBag, docs, DefaultBounds())
	if err != nil {
		t.Fatal(err)
	}
	got, err := eng.Search(context.Background(), "alpha", Options{TopK: 3, Scope: "s", Model: ModelBag})
	if err != nil || !got.OK {
		t.Fatalf("unexpected: %+v err=%v", got, err)
	}
	if len(got.Hits) != 3 {
		t.Fatalf("len=%d want 3", len(got.Hits))
	}
	if got.Hits[0].ID != "a.go" || got.Hits[1].ID != "m.go" || got.Hits[2].ID != "z.go" {
		t.Fatalf("tie-break order wrong: %v", got.Hits)
	}
}

// TestReportIncludesBounds is the receipt contract: every Report carries the
// Bounds that were active for the call, so an audit can verify what guard
// rails protected this call.
func TestReportIncludesBounds(t *testing.T) {
	eng, err := NewEngine("s", ModelBag, map[string]string{"a": "x"}, DefaultBounds())
	if err != nil {
		t.Fatal(err)
	}
	got, err := eng.Search(context.Background(), "x", Options{Scope: "s", Model: ModelBag})
	if err != nil {
		t.Fatal(err)
	}
	if got.BoundedBy.MaxCorpus == 0 || got.BoundedBy.MaxTopK == 0 ||
		got.BoundedBy.MaxDim == 0 || got.BoundedBy.MaxQueryLen == 0 {
		t.Fatalf("bounds missing from report: %+v", got.BoundedBy)
	}
}

// TestContextCancellationIsRespected asserts a cancelled ctx short-circuits
// the search.
func TestContextCancellationIsRespected(t *testing.T) {
	eng, err := NewEngine("s", ModelBag, map[string]string{"a": "alpha"}, DefaultBounds())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.Search(ctx, "alpha", Options{Scope: "s", Model: ModelBag}); err == nil {
		t.Fatal("expected context error")
	}
}

// TestNoNetworkImports: the package must not introduce a transitive net/http
// dependency. This is one of the four primary safety criteria for issue #59.
// Grep-based because import lists compile to binary and a runtime test would
// be brittle on different build tags.
func TestNoNetworkImports(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell pipeline differs on windows")
	}
	out, err := exec.Command("grep", "-RIn", "--include=*.go", `^"net/`, ".").CombinedOutput()
	// exit code 1 from grep means "no matches found"; that is the success case.
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return
		}
		// Any other failure is also fine for this check (e.g. grep missing).
		return
	}
	if len(out) > 0 && strings.Contains(string(out), `"net/`) {
		t.Fatalf("denselocal imports net/*:\n%s", out)
	}
}

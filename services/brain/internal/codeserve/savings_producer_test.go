package codeserve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
)

// The savings ledger had no producer in the product. Only the benchmark wrote
// to it, so savings_summary answered steps: 0 after any amount of real use --
// the one surface reporting what retrieval saved had nothing to report.

func savingsRepo(t *testing.T) (root, cache string) {
	t.Helper()
	root = t.TempDir()
	cache = t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Bodies long enough that reading the files is meaningfully more than
	// reading a result set naming them.
	filler := ""
	for i := 0; i < 200; i++ {
		filler += "// authentication token validation and session refresh\n"
	}
	write("auth/token.go", "package auth\n\n"+filler+"\nfunc ValidateToken() {}\n")
	write("auth/session.go", "package auth\n\n"+filler+"\nfunc RefreshSession() {}\n")
	return root, cache
}

func TestAServedSearchRecordsASavingsStep(t *testing.T) {
	root, cache := savingsRepo(t)
	ctx := context.Background()

	if resp := Handle(ctx, Request{
		"verb": "code_index", "root": root, "index_cache": cache,
	}); resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	if resp := Handle(ctx, Request{
		"verb": "code_search", "root": root, "index_cache": cache,
		"q": "ValidateToken", "top_k": 5,
	}); resp["ok"] != true {
		t.Fatalf("search: %v", resp)
	}

	ledger, err := openSavingsLedgerForRead(savingsLedgerDir(Request{"index_cache": cache}))
	if err != nil {
		t.Fatal(err)
	}
	summary := ledger.Summary()
	if summary.Steps == 0 {
		t.Fatal("a served search recorded nothing: savings_summary still answers " +
			"steps: 0 after real use, which is the whole finding")
	}
	if summary.Totals.BaselineTokensEst <= summary.Totals.ServedTokensEst {
		t.Fatalf("the recorded step claims no saving: %+v", summary.Totals)
	}
	for _, step := range ledger.Steps() {
		if step.Estimator != savings.EstimatorBytesDiv4 {
			t.Errorf("step %q has estimator %q: a figure whose estimator is "+
				"unrecorded cannot be compared with anything", step.Name, step.Estimator)
		}
		if step.BaselineModel != savings.BaselineGoldFiles {
			t.Errorf("step %q has baseline model %q, want the files the answer "+
				"cites rather than the whole tree", step.Name, step.BaselineModel)
		}
	}
}

// TestSavingsSummaryReflectsRealUse is the finding stated end to end, through
// the verb a caller actually invokes.
func TestSavingsSummaryReflectsRealUse(t *testing.T) {
	root, cache := savingsRepo(t)
	ctx := context.Background()

	if resp := Handle(ctx, Request{
		"verb": "code_index", "root": root, "index_cache": cache,
	}); resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	for i := 0; i < 3; i++ {
		if resp := Handle(ctx, Request{
			"verb": "code_search", "root": root, "index_cache": cache,
			"q": "session refresh", "top_k": 5,
		}); resp["ok"] != true {
			t.Fatalf("search: %v", resp)
		}
	}

	resp := Handle(ctx, Request{"verb": "savings_summary", "dir": cache})
	if resp["ok"] != true {
		t.Fatalf("savings_summary: %v", resp)
	}
	summary, ok := resp["summary"].(savings.Summary)
	if !ok {
		t.Fatalf("summary is %T", resp["summary"])
	}
	if summary.Steps != 3 {
		t.Fatalf("savings_summary reports %d steps after three searches: %+v",
			summary.Steps, summary)
	}
}

// TestRecordingNeverFailsARequest keeps a metrics counter from being able to
// break retrieval: an unwritable cache directory must not turn a good answer
// into an error.
func TestRecordingNeverFailsARequest(t *testing.T) {
	root, cache := savingsRepo(t)
	ctx := context.Background()

	if resp := Handle(ctx, Request{
		"verb": "code_index", "root": root, "index_cache": cache,
	}); resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	if err := os.Chmod(cache, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cache, 0o700) })

	resp := Handle(ctx, Request{
		"verb": "code_search", "root": root, "index_cache": cache,
		"q": "ValidateToken", "top_k": 5,
	})
	if resp["ok"] != true {
		t.Fatalf("a search failed because its savings step could not be written: %v", resp)
	}
}

// TestNoLedgerWithoutACacheDirectory keeps the producer from inventing a
// location: a request that names no index cache records nothing.
func TestNoLedgerWithoutACacheDirectory(t *testing.T) {
	if dir := savingsLedgerDir(Request{}); dir != "" {
		t.Fatalf("a request naming no cache resolved to %q", dir)
	}
}

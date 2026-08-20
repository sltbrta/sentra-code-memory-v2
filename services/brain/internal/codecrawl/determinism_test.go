package codecrawl_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/testsupport"
)

// The package advertises deterministic personalised PageRank throughout, and
// the product's whole pitch is receipt-backed, reproducible retrieval. But
// resolveEdgeTarget returned whatever key a map iteration reached first, so the
// adjacency feeding PageRank differed between processes and identical queries
// returned different hit orders.
//
// Go randomises map iteration deliberately to stop exactly this from going
// unnoticed. It went unnoticed because nothing compared two runs.

// determinismCorpus builds a tree with several files whose stems all contain a
// common substring, so more than one key matches the import-stem lookup and the
// choice between them is observable.
func determinismCorpus(t *testing.T) string {
	t.Helper()
	files := map[string]string{
		"main.go": "package main\n\nimport \"example/auth\"\n\nfunc main() { auth.Login() }\n",
	}
	for i := 0; i < 12; i++ {
		files[fmt.Sprintf("auth%02d/auth.go", i)] = fmt.Sprintf(
			"package auth\n\n// Login authenticates a caller.\nfunc Login%02d() {}\n\nfunc Validate%02d() {}\n", i, i)
	}
	for i := 0; i < 12; i++ {
		files[fmt.Sprintf("svc/authsvc%02d.go", i)] = fmt.Sprintf(
			"package svc\n\nimport \"example/auth\"\n\nfunc Serve%02d() { auth.Login() }\n", i)
	}
	return testsupport.WorkTree(t, files)
}

func rankedPaths(t *testing.T, root string, query string) []string {
	t.Helper()
	idx, _, err := codecrawl.CrawlDir(root, 4)
	if err != nil {
		t.Fatalf("CrawlDir: %v", err)
	}
	payload := idx.FindRelevantRanked(root, query, 10, false, codecrawl.DefaultRankerConfig())
	out := make([]string, 0, len(payload.Hits))
	for _, h := range payload.Hits {
		out = append(out, h.Path)
	}
	return out
}

// TestRankedRetrievalIsStableAcrossRuns is the shape the repository already
// uses for its benchmark digest: run it twice and compare.
func TestRankedRetrievalIsStableAcrossRuns(t *testing.T) {
	root := determinismCorpus(t)
	first := rankedPaths(t, root, "auth login")
	if len(first) == 0 {
		t.Fatal("no hits; the fixture does not exercise ranking")
	}
	for run := 2; run <= 12; run++ {
		got := rankedPaths(t, root, "auth login")
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d hits, run 1 returned %d", run, len(got), len(first))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("run %d differs from run 1 at position %d: %q vs %q\nrun 1: %v\nrun %d: %v",
					run, i, got[i], first[i], first, run, got)
			}
		}
	}
}

// TestRepoMapIsStableAcrossRuns covers the other consumer of the same graph.
func TestRepoMapIsStableAcrossRuns(t *testing.T) {
	root := determinismCorpus(t)
	cache := filepath.Join(t.TempDir(), "code-index.gob")
	idx, _, _, _, err := codecrawl.OpenOrRefresh(root, cache, 4, false)
	if err != nil {
		t.Fatalf("OpenOrRefresh: %v", err)
	}
	opts := codecrawl.RepoMapOptions{MaxFiles: 16, MaxSymbols: 8, Iterations: 12}
	first := idx.RepoMap("auth login", opts)
	for run := 2; run <= 8; run++ {
		got := idx.RepoMap("auth login", opts)
		if len(got) != len(first) {
			t.Fatalf("run %d produced %d entries, run 1 produced %d", run, len(got), len(first))
		}
		for i := range got {
			if got[i].Path != first[i].Path {
				t.Fatalf("repo map run %d differs at %d: %q vs %q", run, i, got[i].Path, first[i].Path)
			}
		}
	}
}

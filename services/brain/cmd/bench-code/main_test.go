package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fixturePath resolves the checked-in qafixture corpus relative to this cmd.
func fixturePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "internal", "scmbench", "testdata", "qafixture")
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		t.Fatalf("fixture missing at %s: %v", abs, err)
	}
	return abs
}

func TestSmokeMatrixAllSurfacesAgree(t *testing.T) {
	t.Parallel()
	root := fixturePath(t)
	cache := t.TempDir()
	matrix, err := runSmokeMatrix(context.Background(), root, cache)
	if err != nil {
		t.Fatalf("runSmokeMatrix: %v", err)
	}
	if !matrix.Pass {
		for _, r := range matrix.Results {
			if !r.Match {
				t.Errorf("surface mismatch for %s: cli=%v http=%v mcp=%v", r.Verb, r.CLI, r.HTTP, r.MCP)
			}
		}
		t.Fatal("smoke matrix did not pass")
	}
	if len(matrix.Results) == 0 {
		t.Fatal("no smoke probes ran")
	}
	// The deferred verb must agree as a failure across all three surfaces.
	for _, r := range matrix.Results {
		if r.Verb == "session_product" && r.OK {
			t.Fatalf("deferred verb should not succeed on any surface: %+v", r)
		}
	}
}

func TestRunEndToEndOnFixture(t *testing.T) {
	root := fixturePath(t)
	out := filepath.Join(t.TempDir(), "report.json")
	code := run([]string{"--fixture", root, "--out", out, "--quiet"})
	if code != 0 {
		t.Fatalf("bench-code exit=%d want 0 on the checked-in fixture", code)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatalf("artifact not written: %v", err)
	}
}

func TestRunRejectsMissingFixture(t *testing.T) {
	code := run([]string{"--fixture", filepath.Join(t.TempDir(), "does-not-exist"), "--quiet"})
	if code == 0 {
		t.Fatal("missing fixture must not pass")
	}
}

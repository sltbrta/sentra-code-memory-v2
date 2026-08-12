package codeserve_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// TestImpactResponseSurfacesPhase2Fields verifies the codeserve impact
// verb surfaces the new Phase 2 fields (truncated, severity, affected_tests,
// changed_symbols, schema) when the typed-edge projection is available.
// Existing JSON fields stay byte-compatible with the Phase 1 contract.
func TestImpactResponseSurfacesPhase2Fields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// Three Go files: a.go defines Alpha, b.go calls Alpha, c.go defines
	// and references a separate symbol to ensure deterministic ordering.
	if err := os.WriteFile(filepath.Join(src, "a.go"),
		[]byte("package p\nfunc Alpha() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b.go"),
		[]byte("package p\nfunc Beta() int { return Alpha() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "c.go"),
		[]byte("package p\nfunc Gamma() int { return Alpha() + 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	ctx := context.Background()
	// Build the index first so the impact verb can load it.
	if resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": src, "index_cache": cache,
	}); resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_impact", "root": src, "index_cache": cache,
		"seed": "Alpha", "no_refresh": true,
	})
	if resp["ok"] != true {
		t.Fatalf("impact: %v", resp)
	}
	if resp["receipt"] == nil {
		t.Fatal("nil receipt")
	}
	raw, err := json.Marshal(resp["receipt"])
	if err != nil {
		t.Fatal(err)
	}
	// Confirm the new Phase 2 fields are present on the wire (omitempty
	// fields that are zero are omitted, so we look for the schema tag at
	// minimum).
	if !contains(raw, `"schema":"v2"`) {
		t.Fatalf("expected schema=v2 in %s", raw)
	}
	if !contains(raw, `"authority":"heuristic"`) {
		t.Fatalf("expected authority=heuristic in %s", raw)
	}
	// severity must be one of low/medium/high.
	if !containsAny(raw, `"severity":"low"`, `"severity":"medium"`, `"severity":"high"`) {
		t.Fatalf("expected severity in %s", raw)
	}
}

// TestImpactResponseUnknownSeedFailsClosed verifies a seed that does not
// resolve to any def/ref produces a structured response with the
// no_defs_or_refs_found unknown note so callers can fail closed rather
// than crash.
func TestImpactResponseUnknownSeedFailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "x.go"),
		[]byte("package p\nfunc X() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	ctx := context.Background()
	if resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": src, "index_cache": cache,
	}); resp["ok"] != true {
		t.Fatalf("index: %v", resp)
	}
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_impact", "root": src, "index_cache": cache,
		"seed": "DoesNotExist12345", "no_refresh": true,
	})
	if resp["ok"] != true {
		t.Fatalf("impact: %v", resp)
	}
	raw, err := json.Marshal(resp["receipt"])
	if err != nil {
		t.Fatal(err)
	}
	if !contains(raw, `"unknowns"`) {
		t.Fatalf("expected unknowns field in %s", raw)
	}
	if !contains(raw, `"no_defs_or_refs_found"`) {
		t.Fatalf("expected no_defs_or_refs_found unknown note in %s", raw)
	}
}

func contains(haystack []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

func containsAny(haystack []byte, needles ...string) bool {
	for _, n := range needles {
		if contains(haystack, n) {
			return true
		}
	}
	return false
}

package codeserve_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// TestConformanceVerbMatrix exercises every code verb on a real index (SCM-001/007).
func TestConformanceVerbMatrix(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	code := "package demo\n\nfunc Alpha() {}\nfunc Beta() { Alpha() }\n"
	if err := os.WriteFile(filepath.Join(src, "demo.go"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	ctx := context.Background()

	mustOK := func(name string, req codeserve.Request) codeserve.Response {
		t.Helper()
		req["verb"] = name
		if _, ok := req["root"]; !ok {
			req["root"] = src
		}
		if _, ok := req["index_cache"]; !ok {
			req["index_cache"] = cache
		}
		resp := codeserve.Handle(ctx, req)
		if resp["ok"] != true {
			t.Fatalf("%s: %+v", name, resp)
		}
		return resp
	}

	mustOK("code_index", codeserve.Request{})
	// Warm second index: stamp path (no force) must succeed.
	warm := mustOK("code_index", codeserve.Request{})
	if warm["ok"] != true {
		t.Fatal(warm)
	}
	// OpenOrRefresh warm unit: zero bytes when stamps match.
	gob := filepath.Join(cache, "code-index.gob")
	idx, st, wrote, _, err := codecrawl.OpenOrRefresh(src, gob, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if idx == nil {
		t.Fatal("nil index")
	}
	if wrote && st.SkippedByStamp == 0 && st.Changed > 0 {
		// First warm after index may write; third call should be stamp-only.
		_, st2, wrote2, _, err := codecrawl.OpenOrRefresh(src, gob, 2, false)
		if err != nil {
			t.Fatal(err)
		}
		if wrote2 {
			t.Fatalf("warm reindex wrote gob: st=%+v", st2)
		}
		if st2.SkippedByStamp == 0 && st2.BytesRead != 0 {
			t.Fatalf("expected stamp warm zero-work, got %+v", st2)
		}
	}

	mustOK("code_search", codeserve.Request{"q": "Alpha", "no_refresh": true})
	mustOK("code_find_relevant", codeserve.Request{"q": "Alpha", "no_refresh": true})
	mustOK("code_expand", codeserve.Request{"seed": "Alpha", "no_refresh": true})
	mustOK("code_impact", codeserve.Request{"seed": "Alpha", "no_refresh": true})
	mustOK("code_find_route", codeserve.Request{"from": "Alpha", "to": "Beta"})
	mustOK("code_freshness", codeserve.Request{})
	mustOK("code_ingest_paths", codeserve.Request{"paths": "demo.go"})
	mustOK("code_defs", codeserve.Request{"q": "Alpha", "no_refresh": true})
	mustOK("code_refs", codeserve.Request{"q": "Alpha", "no_refresh": true})
	mustOK("catalog", codeserve.Request{})
	mustOK("ping", codeserve.Request{})
}

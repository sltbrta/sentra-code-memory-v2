package codeserve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

func TestCatalogAndPing(t *testing.T) {
	t.Parallel()
	cat := codeserve.Catalog()
	if len(cat) < 10 {
		t.Fatalf("catalog too small: %v", cat)
	}
	resp := codeserve.Handle(context.Background(), codeserve.Request{"verb": "ping"})
	if resp["ok"] != true {
		t.Fatalf("%v", resp)
	}
	resp = codeserve.Handle(context.Background(), codeserve.Request{"verb": "catalog"})
	verbs, _ := resp["verbs"].([]string)
	if len(verbs) < 10 {
		t.Fatalf("verbs=%v", resp)
	}
}

func TestCodeIndexSearchImpactRoute(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "package pkg\n\n// Foo is a seed.\nfunc Foo() {}\n\nfunc Bar() { Foo() }\n"
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "idx")
	ctx := context.Background()

	idxResp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": src, "index_cache": cache, "workers": 2,
	})
	if idxResp["ok"] != true {
		t.Fatalf("index: %v", idxResp)
	}

	search := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_search", "root": src, "index_cache": cache, "q": "Foo", "no_refresh": true,
	})
	if search["ok"] != true {
		t.Fatalf("search: %v", search)
	}

	impact := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_impact", "root": src, "index_cache": cache, "seed": "Foo", "no_refresh": true,
	})
	if impact["ok"] != true {
		t.Fatalf("impact: %v", impact)
	}
	rec, _ := impact["receipt"].(map[string]any)
	// receipt is struct encoded as json-ish via map from Response - actually it's ImpactReceipt struct
	// Response stores typed value; type assert loosely
	if impact["receipt"] == nil {
		t.Fatal("missing receipt")
	}
	_ = rec

	route := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_find_route", "root": src, "index_cache": cache,
		"from": "Foo", "to": "Bar",
	})
	if route["ok"] != true {
		t.Fatalf("route: %v", route)
	}

	fresh := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_freshness", "root": src, "index_cache": cache,
	})
	if fresh["ok"] != true {
		t.Fatalf("fresh: %v", fresh)
	}

	unknown := codeserve.Handle(ctx, codeserve.Request{"verb": "not_a_verb"})
	if unknown["ok"] != false {
		t.Fatalf("unknown should fail: %v", unknown)
	}
}

func TestImpactAuthorityHeuristic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\nfunc Seed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "c")
	ctx := context.Background()
	_ = codeserve.Handle(ctx, codeserve.Request{"verb": "code_index", "root": dir, "index_cache": cache})
	impact := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_impact", "root": dir, "index_cache": cache, "seed": "Seed", "no_refresh": true,
	})
	if impact["ok"] != true {
		t.Fatalf("%v", impact)
	}
	if impact["receipt"] == nil {
		t.Fatal("nil receipt")
	}
	// Authority must be honest heuristic label (SCM-006).
	raw, err := json.Marshal(impact["receipt"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"authority":"heuristic"`)) {
		t.Fatalf("expected authority=heuristic in %s", raw)
	}
}

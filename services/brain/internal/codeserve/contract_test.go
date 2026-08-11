package codeserve_test

// Contract and conformance tests for the canonical codeserve contract types
// (Phase 0). They run the live map-based handlers against the static fixture
// in testdata/scmfixture and prove the typed contracts decode the exact wire
// responses — no behavior change, no network providers.

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

func fixtureRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "scmfixture"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// indexFixture builds the durable index once per test and returns the cache dir.
func indexFixture(t *testing.T, ctx context.Context) (root, cache string) {
	t.Helper()
	root = fixtureRoot(t)
	cache = t.TempDir()
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": root, "index_cache": cache, "workers": 2,
	})
	if resp["ok"] != true {
		t.Fatalf("code_index: %+v", resp)
	}
	return root, cache
}

func decode[T any](t *testing.T, resp codeserve.Response) T {
	t.Helper()
	var out T
	if err := codeserve.DecodeResponse(resp, &out); err != nil {
		t.Fatalf("decode %T: %v (resp=%v)", out, err, resp)
	}
	return out
}

// TestCatalogMetadataContract: metadata covers every live verb, marks planned
// verbs, and keeps aliases compatible with existing entry points.
func TestCatalogMetadataContract(t *testing.T) {
	t.Parallel()
	specs := codeserve.CatalogMetadata()
	byName := map[string]codeserve.VerbSpec{}
	for _, s := range specs {
		if s.Name == "" || s.Summary == "" || s.Status == "" || s.Surface == "" {
			t.Fatalf("incomplete spec: %+v", s)
		}
		if _, dup := byName[s.Name]; dup {
			t.Fatalf("duplicate spec %q", s.Name)
		}
		byName[s.Name] = s
	}
	// Every live catalog verb has a stable spec.
	for _, v := range codeserve.Catalog() {
		s, ok := byName[v]
		if !ok {
			t.Fatalf("live verb %q missing catalog metadata", v)
		}
		if s.Status != codeserve.StatusStable {
			t.Fatalf("live verb %q must be stable, got %q", v, s.Status)
		}
	}
	// Planned verbs are typed but not live yet.
	for _, planned := range []string{"code_read", "code_imports", "code_watch"} {
		s, ok := byName[planned]
		if !ok {
			t.Fatalf("planned verb %q missing metadata", planned)
		}
		if s.Status != codeserve.StatusPlanned {
			t.Fatalf("%q must be planned, got %q", planned, s.Status)
		}
		if slices.Contains(codeserve.Catalog(), planned) {
			t.Fatalf("planned verb %q must not be live yet", planned)
		}
	}
	// Compatibility aliases stay wired to their canonical verbs.
	route := byName["code_find_route"]
	if !slices.Contains(route.Aliases, "find_route") {
		t.Fatalf("find_route back-compat alias missing: %+v", route)
	}
	if !slices.Contains(byName["code_index"].Aliases, "index") {
		t.Fatal("CLI index alias missing from code_index spec")
	}
}

// TestCatalogVerbExposesSpecs: the live catalog verb carries the typed
// metadata alongside the legacy verbs list (additive, back-compat).
func TestCatalogVerbExposesSpecs(t *testing.T) {
	t.Parallel()
	resp := codeserve.Handle(context.Background(), codeserve.Request{"verb": "catalog"})
	if resp["ok"] != true {
		t.Fatalf("%v", resp)
	}
	if resp["contract"] != codeserve.ContractID {
		t.Fatalf("contract id mismatch: %v", resp["contract"])
	}
	if _, ok := resp["verbs"].([]string); !ok {
		t.Fatalf("legacy verbs list missing: %v", resp)
	}
	specs, ok := resp["specs"].([]codeserve.VerbSpec)
	if !ok || len(specs) == 0 {
		t.Fatalf("specs metadata missing: %T", resp["specs"])
	}
}

// TestErrorContractConformance: failures decode into ErrorResponse with
// stable machine-readable codes and preserved message keys.
func TestErrorContractConformance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	unknown := decode[codeserve.ErrorResponse](t,
		codeserve.Handle(ctx, codeserve.Request{"verb": "not_a_verb"}))
	if unknown.OK || unknown.ErrorCode != codeserve.ErrUnknownVerb {
		t.Fatalf("unknown verb: %+v", unknown)
	}
	if len(unknown.Supported) == 0 || unknown.Error == "" {
		t.Fatalf("unknown verb envelope incomplete: %+v", unknown)
	}

	missingQ := decode[codeserve.ErrorResponse](t,
		codeserve.Handle(ctx, codeserve.Request{"verb": "code_search"}))
	if missingQ.OK || missingQ.ErrorCode != codeserve.ErrInvalidRequest {
		t.Fatalf("missing q: %+v", missingQ)
	}

	noIndex := decode[codeserve.ErrorResponse](t, codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_search", "q": "Alpha", "no_refresh": true,
		"index_cache": filepath.Join(t.TempDir(), "absent"),
	}))
	if noIndex.OK || noIndex.ErrorCode != codeserve.ErrIndexUnavailable {
		t.Fatalf("absent index: %+v", noIndex)
	}
}

// TestResponseContractConformance: every live code verb on the fixture
// decodes into its canonical typed response with expected current behavior.
func TestResponseContractConformance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, cache := indexFixture(t, ctx)
	sel := func() codeserve.Request {
		return codeserve.Request{"root": root, "index_cache": cache, "no_refresh": true}
	}

	t.Run("index", func(t *testing.T) {
		resp := codeserve.Handle(ctx, codeserve.Request{
			"verb": "code_index", "root": root, "index_cache": cache, "workers": 2,
		})
		out := decode[codeserve.IndexResponse](t, resp)
		if !out.OK || out.Verb != "code_index" || out.Root == "" || out.GobPath == "" {
			t.Fatalf("%+v", out)
		}
		if out.SearchBackend != "codecrawl" {
			t.Fatalf("backend: %+v", out)
		}
	})

	t.Run("find_relevant", func(t *testing.T) {
		req := sel()
		req["verb"] = "code_find_relevant"
		req["q"] = "Alpha"
		out := decode[codeserve.FindRelevantResponse](t, codeserve.Handle(ctx, req))
		if !out.OK || out.Payload.Query != "Alpha" || len(out.Payload.Hits) == 0 {
			t.Fatalf("%+v", out)
		}
	})

	t.Run("expand", func(t *testing.T) {
		req := sel()
		req["verb"] = "code_expand"
		req["seed"] = "Alpha"
		out := decode[codeserve.ExpandResponse](t, codeserve.Handle(ctx, req))
		if !out.OK || out.Seed != "Alpha" || out.Authority != "heuristic" {
			t.Fatalf("%+v", out)
		}
		if len(out.Defs) == 0 {
			t.Fatalf("expected defs for Alpha: %+v", out)
		}
	})

	t.Run("impact", func(t *testing.T) {
		req := sel()
		req["verb"] = "code_impact"
		req["seed"] = "Alpha"
		out := decode[codeserve.ImpactResponse](t, codeserve.Handle(ctx, req))
		if !out.OK || out.Receipt.Authority != "heuristic" || out.Receipt.Seed != "Alpha" {
			t.Fatalf("%+v", out)
		}
	})

	t.Run("find_route", func(t *testing.T) {
		req := sel()
		req["verb"] = "code_find_route"
		req["from"] = "Alpha"
		req["to"] = "Beta"
		out := decode[codeserve.FindRouteResponse](t, codeserve.Handle(ctx, req))
		if !out.OK || out.Receipt.From != "Alpha" || out.Receipt.To != "Beta" {
			t.Fatalf("%+v", out)
		}
	})

	t.Run("freshness", func(t *testing.T) {
		req := sel()
		req["verb"] = "code_freshness"
		delete(req, "no_refresh")
		out := decode[codeserve.FreshnessResponse](t, codeserve.Handle(ctx, req))
		if !out.OK || !out.Report.Fresh || out.Report.IndexedFiles == 0 {
			t.Fatalf("%+v", out)
		}
	})

	t.Run("ingest_paths", func(t *testing.T) {
		req := codeserve.Request{
			"verb": "code_ingest_paths", "root": root, "index_cache": cache,
			"paths": "alpha.go,beta.go",
		}
		out := decode[codeserve.IngestPathsResponse](t, codeserve.Handle(ctx, req))
		if !out.OK || len(out.Paths) != 2 || out.Root == "" {
			t.Fatalf("%+v", out)
		}
	})

	t.Run("exact_defs_refs_imports", func(t *testing.T) {
		for verb, kind := range map[string]codeserve.ExactKind{
			"code_exact": codeserve.ExactKindAny,
			"code_defs":  codeserve.ExactKindDefinition,
			"code_refs":  codeserve.ExactKindReference,
		} {
			req := codeserve.Request{
				"verb": verb, "root": root, "q": "Alpha", "kind": string(kind),
			}
			out := decode[codeserve.ExactResponse](t, codeserve.Handle(ctx, req))
			if !out.OK || out.Verb != verb || out.SearchBackend != "codeindex" {
				t.Fatalf("%s: %+v", verb, out)
			}
		}
		// Imports lane today: code_exact with kind=import.
		req := codeserve.Request{
			"verb": "code_exact", "root": root, "q": "Alpha",
			"kind": string(codeserve.ExactKindImport),
		}
		out := decode[codeserve.ExactResponse](t, codeserve.Handle(ctx, req))
		if !out.OK {
			t.Fatalf("imports lane: %+v", out)
		}
	})
}

// TestCompatibilityAliases: existing aliases keep working unchanged.
func TestCompatibilityAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, cache := indexFixture(t, ctx)

	// Back-compat serve alias "find_route" normalizes to code_find_route.
	resp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "find_route", "root": root, "index_cache": cache,
		"from": "Alpha", "to": "Beta",
	})
	out := decode[codeserve.FindRouteResponse](t, resp)
	if !out.OK || out.Verb != "code_find_route" {
		t.Fatalf("find_route alias: %+v", out)
	}

	// Empty verb defaults to catalog (existing behavior).
	resp = codeserve.Handle(ctx, codeserve.Request{})
	if resp["ok"] != true || resp["verb"] != "catalog" {
		t.Fatalf("empty verb default: %v", resp)
	}
}

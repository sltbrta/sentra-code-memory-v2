package hosted

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Red-proof tests for the governed metadata-filter contract (issue #328):
// normalization, fail-closed denial, cross-arm consistency, cache isolation,
// and official/blind gold-derived rejection.

func TestMetadataFilterNormalizationCanonical(t *testing.T) {
	a, err := NormalizeMetadataFilter(map[string]any{
		"tenant":       "  Brain-A ",
		"scopes":       []any{"Team", " team ", "company"},
		"source_types": []any{"Slack", "GITHUB", "slack"},
		"tags":         []any{"Fin", "ops"},
		"valid_from":   "2025-06-01T12:00:00+02:00",
		"valid_until":  "2025-12-31",
		"principals":   []any{"Alice", "bob"},
	}, FilterAuthority{Tenant: "brain-a"})
	if err != nil {
		t.Fatalf("valid filter rejected: %v", err)
	}
	if a.Tenant != "brain-a" {
		t.Fatalf("tenant not normalized: %q", a.Tenant)
	}
	if strings.Join(a.Scopes, ",") != "company,team" {
		t.Fatalf("scopes not sorted/deduped/lowercased: %v", a.Scopes)
	}
	if strings.Join(a.SourceTypes, ",") != "github,slack" {
		t.Fatalf("source_types not sorted/deduped: %v", a.SourceTypes)
	}

	// Same filter, different key order / list order / time offset spelling:
	// identity must be identical (canonical form).
	b, err := NormalizeMetadataFilter(map[string]any{
		"principals":   []any{"bob", "alice"},
		"tags":         []any{"ops", "fin"},
		"source_types": []any{"github", "slack"},
		"scopes":       []any{"company", "team"},
		"tenant":       "brain-a",
		"valid_from":   "2025-06-01T10:00:00Z",
		"valid_until":  "2025-12-31T00:00:00Z",
	}, FilterAuthority{Tenant: "brain-a"})
	if err != nil {
		t.Fatalf("valid filter rejected: %v", err)
	}
	if a.Identity() != b.Identity() {
		t.Fatalf("canonical identity drifted: %q vs %q", a.Identity(), b.Identity())
	}
	if a.Identity() == "" {
		t.Fatal("non-zero filter must have a non-empty identity")
	}

	// Mutating a single predicate value changes the identity.
	c, err := NormalizeMetadataFilter(map[string]any{
		"tenant": "brain-a",
		"tags":   []any{"fin"},
	}, FilterAuthority{Tenant: "brain-a"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Identity() == a.Identity() {
		t.Fatal("different filters must not share an identity")
	}

	// Empty raw map is opt-out, not an error.
	zero, err := NormalizeMetadataFilter(nil, FilterAuthority{Tenant: "brain-a"})
	if err != nil || zero != nil {
		t.Fatalf("nil raw filter must normalize to (nil, nil), got %v, %v", zero, err)
	}
	if zero.Identity() != "" {
		t.Fatal("zero filter identity must be empty")
	}
}

func TestMetadataFilterMalformedFailsClosed(t *testing.T) {
	cases := map[string]map[string]any{
		"unknown predicate":          {"doc_ids": []any{"d1"}},
		"document_ids never allowed": {"document_ids": []any{"d1"}},
		"gold_doc_ids never allowed": {"gold_doc_ids": []any{"d1"}},
		"scopes not a list":          {"scopes": "team"},
		"scopes non-string entry":    {"scopes": []any{1}},
		"scopes empty entry":         {"scopes": []any{"  "}},
		"scopes empty list":          {"scopes": []any{}},
		"tenant not a string":        {"tenant": 7},
		"tenant empty":               {"tenant": "  "},
		"valid_from bad timestamp":   {"valid_from": "yesterday"},
		"valid_until bad type":       {"valid_until": 20250101},
		"inverted validity window":   {"valid_from": "2025-06-01", "valid_until": "2025-01-01"},
	}
	for name, raw := range cases {
		f, err := NormalizeMetadataFilter(raw, FilterAuthority{})
		if err == nil {
			t.Errorf("%s: malformed filter must fail closed, got filter %+v", name, f)
		}
		if f != nil {
			t.Errorf("%s: rejected filter must return nil filter, got %+v", name, f)
		}
	}
}

func TestMetadataFilterAuthorizationFailsClosed(t *testing.T) {
	// Tenant binding: a filter naming another tenant is rejected.
	if _, err := NormalizeMetadataFilter(
		map[string]any{"tenant": "brain-b"},
		FilterAuthority{Tenant: "brain-a"},
	); err == nil {
		t.Fatal("cross-tenant filter must be rejected")
	}
	// Matching tenant passes.
	if _, err := NormalizeMetadataFilter(
		map[string]any{"tenant": " brain-A "},
		FilterAuthority{Tenant: "brain-a"},
	); err != nil {
		t.Fatalf("authorized tenant filter rejected: %v", err)
	}
	// Per-caller predicate allowlist: unauthorized predicate fails closed.
	if _, err := NormalizeMetadataFilter(
		map[string]any{"tags": []any{"fin"}},
		FilterAuthority{AllowedPredicates: []string{"tenant", "scopes"}},
	); err == nil {
		t.Fatal("predicate outside caller allowlist must be rejected")
	}
	if _, err := NormalizeMetadataFilter(
		map[string]any{"scopes": []any{"team"}},
		FilterAuthority{AllowedPredicates: []string{"tenant", "scopes"}},
	); err != nil {
		t.Fatalf("authorized predicate rejected: %v", err)
	}
}

func TestMetadataFilterBlindRejectsGoldDerived(t *testing.T) {
	blind := FilterAuthority{Blind: true}
	for _, key := range []string{
		"source_types", "question_type", "document_ids",
		"expected_doc_ids", "gold_doc_ids", "gold_answer", "answer_facts",
	} {
		if _, err := NormalizeMetadataFilter(
			map[string]any{key: []any{"x"}},
			blind,
		); err == nil {
			t.Errorf("blind mode must reject gold-derived predicate %q", key)
		}
	}
	// Governance predicates (tenant/scope/tags/validity/ACL) are not
	// gold-derived and stay legal in blind mode.
	if _, err := NormalizeMetadataFilter(
		map[string]any{"tenant": "brain-a", "tags": []any{"fin"}},
		FilterAuthority{Tenant: "brain-a", Blind: true},
	); err != nil {
		t.Fatalf("non-gold governance filter must survive blind mode: %v", err)
	}
	// Non-blind posture still rejects document-id pins (not allowlisted).
	if _, err := NormalizeMetadataFilter(
		map[string]any{"expected_doc_ids": []any{"d1"}},
		FilterAuthority{},
	); err == nil {
		t.Fatal("document-id predicates must be rejected even outside blind mode")
	}
}

func TestFilterAllowsFailClosedOnMissingMetadata(t *testing.T) {
	f, err := NormalizeMetadataFilter(map[string]any{
		"tenant":          "brain-a",
		"scopes":          []any{"team"},
		"tags":            []any{"fin"},
		"valid_from":      "2025-01-01",
		"valid_until":     "2025-12-31",
		"principals":      []any{"alice"},
		"deny_principals": []any{"mallory"},
	}, FilterAuthority{Tenant: "brain-a"})
	if err != nil {
		t.Fatal(err)
	}
	// Zero metadata: every active predicate must deny, never pass on absence.
	if ok, reason := f.Allows(DocMeta{}); ok {
		t.Fatal("filter must fail closed on missing metadata")
	} else if reason == "" {
		t.Fatal("denial must carry a receipt reason")
	}
	// Fully matching metadata passes.
	ok, _ := f.Allows(DocMeta{
		Tenant:     "brain-a",
		Scope:      "team",
		SourceType: "slack",
		Tags:       []string{"fin", "ops"},
		ValidFrom:  mustTime(t, "2024-01-01"),
		ValidUntil: mustTime(t, "2026-01-01"),
		Principals: []string{"alice"},
	})
	if !ok {
		t.Fatal("fully matching metadata must be allowed")
	}
	// Denied principal present → deny even when allow also matches.
	if ok, reason := f.Allows(DocMeta{
		Tenant: "brain-a", Scope: "team", Tags: []string{"fin"},
		ValidFrom: mustTime(t, "2024-01-01"), ValidUntil: mustTime(t, "2026-01-01"),
		Principals: []string{"alice", "mallory"},
	}); ok || reason != "principal_denied" {
		t.Fatalf("deny_principals must win, got ok=%v reason=%q", ok, reason)
	}
	// Document outside the validity window → deny.
	if ok, _ := f.Allows(DocMeta{
		Tenant: "brain-a", Scope: "team", Tags: []string{"fin"},
		ValidFrom: mustTime(t, "2026-06-01"), ValidUntil: mustTime(t, "2026-07-01"),
		Principals: []string{"alice"},
	}); ok {
		t.Fatal("document valid only after the filter window must be denied")
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestFilterPassagesCrossArmConsistency(t *testing.T) {
	f, err := NormalizeMetadataFilter(
		map[string]any{"source_types": []any{"slack"}},
		FilterAuthority{},
	)
	if err != nil {
		t.Fatal(err)
	}
	// One mixed pool as the merged arms would produce it: the same two
	// documents surfaced by lexical, dense, graph/structure, and hydrate.
	pool := []Passage{
		{DocumentID: "d-slack", Text: "a", Channel: "lexical"},
		{DocumentID: "d-github", Text: "b", Channel: "dense"},
		{DocumentID: "d-slack", Text: "c", Channel: "structure"},
		{DocumentID: "d-github", Text: "d", Channel: "hydrate"},
	}
	meta := map[string]DocMeta{
		"d-slack":  {SourceType: "slack"},
		"d-github": {SourceType: "github"},
	}
	out, dropped := FilterPassages(pool, f, func(p Passage) DocMeta { return meta[p.DocumentID] })
	if dropped != 2 || len(out) != 2 {
		t.Fatalf("expected 2 kept / 2 dropped, got %d kept / %d dropped", len(out), dropped)
	}
	for _, p := range out {
		if p.DocumentID != "d-slack" {
			t.Fatalf("unauthorized doc %q survived via channel %q", p.DocumentID, p.Channel)
		}
	}
	// The filter applies identically no matter which arm surfaced the doc:
	// no channel ordering or arm name changes the outcome.
	if out[0].Channel != "lexical" || out[1].Channel != "structure" {
		t.Fatalf("order must be preserved across arms, got %q, %q", out[0].Channel, out[1].Channel)
	}

	// Passage-local fallback (no provider): SourceURI scheme drives source
	// type and yields the same decision.
	poolURI := []Passage{
		{DocumentID: "d-slack", SourceURI: "slack://c/1", Channel: "lexical"},
		{DocumentID: "d-github", SourceURI: "github://r/2", Channel: "hydrate"},
	}
	outURI, droppedURI := FilterPassages(poolURI, f, nil)
	if droppedURI != 1 || len(outURI) != 1 || outURI[0].DocumentID != "d-slack" {
		t.Fatalf("passage-local fallback mismatch: kept=%v dropped=%d", outURI, droppedURI)
	}
}

func TestRetrieveCacheKeyFilterIsolation(t *testing.T) {
	c := testCacheClient()
	plan := QueryPlan{EffectiveType: "basic", Mode: "light"}
	prod := ProdProfile{Enabled: true}
	base := RetrieveOptions{TopK: 8, QuestionType: "basic", Mode: "light"}

	unfiltered := cacheKeyOf(c, "what is the rpo?", 8, base, plan, prod)

	finOpts := base
	finOpts.Filter = map[string]any{"tags": []any{"fin"}}
	finKey := cacheKeyOf(c, "what is the rpo?", 8, finOpts, plan, prod)
	if finKey == unfiltered {
		t.Fatal("filtered and unfiltered asks must not share a cache entry")
	}

	// Same filter, different raw ordering → same key (normalization).
	reordered := base
	reordered.Filter = map[string]any{"tags": []any{" fin ", "fin"}}
	if k := cacheKeyOf(c, "what is the rpo?", 8, reordered, plan, prod); k != finKey {
		t.Fatal("canonically identical filters must share a cache key")
	}

	// Different filter value → different key (no cross-filter reuse).
	ops := base
	ops.Filter = map[string]any{"tags": []any{"ops"}}
	if k := cacheKeyOf(c, "what is the rpo?", 8, ops, plan, prod); k == finKey || k == unfiltered {
		t.Fatal("different filters must be cache-isolated")
	}
}

func TestRetrieveOptsFilterEndToEnd(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	c := OpenMemory("cache-filter")
	defer c.Close()
	seedCacheTestBrain(t, c, "cache-filter")
	c.SetFilterMetadataProvider(func(documentID string) (DocMeta, bool) {
		m := map[string]DocMeta{
			"doc_rpo":   {Tenant: "cache-filter", Tags: []string{"fin"}},
			"doc_rto":   {Tenant: "cache-filter", Tags: []string{"fin"}},
			"doc_noise": {Tenant: "cache-filter", Tags: []string{"ops"}},
		}
		v, ok := m[documentID]
		return v, ok
	})
	ctx := context.Background()

	// Malformed / unauthorized filters fail closed: error, no retrieval.
	bad := []map[string]any{
		{"document_ids": []any{"doc_rpo"}},         // gold-adjacent, never allowed
		{"tenant": "other-brain"},                  // cross-tenant
		{"scopes": "team"},                         // malformed type
		{"definitely_not_a_predicate": []any{"x"}}, // unknown key
	}
	for _, raw := range bad {
		ps, diag, err := c.RetrieveOpts(ctx, "MedThink cache recovery failover",
			RetrieveOptions{TopK: 4, QuestionType: "basic", Filter: raw})
		if err == nil {
			t.Errorf("filter %v must fail closed", raw)
		}
		if ps != nil {
			t.Errorf("rejected filter %v must not retrieve, got %d passages", raw, len(ps))
		}
		if diag["filter_rejected"] == nil {
			t.Errorf("rejected filter %v must stamp filter_rejected receipt", raw)
		}
	}

	// Authorized filter: identity + predicates land in receipts; cold ask
	// caches under the filter identity so the next ask is a hit, while the
	// unfiltered ask shape stays a separate entry.
	opts := RetrieveOptions{
		TopK:         4,
		QuestionType: "basic",
		Filter:       map[string]any{"tags": []any{"fin"}},
	}
	_, diagCold, err := c.RetrieveOpts(ctx, "MedThink cache recovery failover", opts)
	if err != nil {
		t.Fatal(err)
	}
	if diagCold["filter_identity"] == nil || diagCold["filter_identity"] == "" {
		t.Fatalf("filtered retrieve must stamp filter_identity, diag=%v", diagCold)
	}
	preds, _ := diagCold["filter_predicates"].([]string)
	if len(preds) != 1 || preds[0] != "tags" {
		t.Fatalf("receipt predicates mismatch: %v", diagCold["filter_predicates"])
	}
	_, diagWarm, err := c.RetrieveOpts(ctx, "MedThink cache recovery failover", opts)
	if err != nil {
		t.Fatal(err)
	}
	if diagWarm["cache_event"] != cacheEventHit {
		t.Fatalf("same filter must hit the same cache entry, got %v", diagWarm["cache_event"])
	}
	// Unfiltered ask with the same question must NOT hit the filtered entry.
	_, diagUnfiltered, err := c.RetrieveOpts(ctx, "MedThink cache recovery failover",
		RetrieveOptions{TopK: 4, QuestionType: "basic"})
	if err != nil {
		t.Fatal(err)
	}
	if diagUnfiltered["cache_event"] == cacheEventHit {
		t.Fatal("unfiltered ask must not reuse the filtered cache entry")
	}
}

func TestRetrieveOptsBlindEnvRejectsGoldDerivedFilter(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_BLIND_PLAN", "1")
	c := OpenMemory("blind-filter-hosted")
	defer c.Close()
	// source_types is a legal predicate generally, but gold-derived under a
	// blind posture — the hosted layer fails closed from env alone.
	ps, diag, err := c.RetrieveOpts(context.Background(), "anything",
		RetrieveOptions{TopK: 4, Filter: map[string]any{"source_types": []any{"slack"}}})
	if err == nil || ps != nil {
		t.Fatalf("blind posture must reject gold-derived filter, err=%v ps=%v", err, ps)
	}
	if diag["filter_rejected"] == nil {
		t.Fatal("blind rejection must stamp filter_rejected")
	}
}

package hosted

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func completeScoresByText(callCount *int, seen *[]Passage) rerankScoreCall {
	return func(_ context.Context, _ string, passages []Passage, topN int) ([]remoteRerankResult, error) {
		(*callCount)++
		if seen != nil {
			*seen = append((*seen)[:0], passages...)
		}
		if topN != len(passages) {
			return nil, fmt.Errorf("topN=%d want complete %d", topN, len(passages))
		}
		results := make([]remoteRerankResult, len(passages))
		for i, passage := range passages {
			results[i] = remoteRerankResult{Index: i, RelevanceScore: float64(strings.Count(passage.Text, "target"))}
		}
		return results, nil
	}
}

func testRerankScope() rerankScoreScope {
	return rerankScoreScope{ScopeID: "brain-a\x00generation-1", Dimension: 1536, ACLIdentity: "acl-a"}
}

func TestRerankPrefilterIsBoundedBlindAndPreservesCitationCandidates(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_RERANK_PREFILTER_N", "3")
	pool := []Passage{
		{DocumentID: "gold-looking-id", ChunkID: "c0", Text: "unrelated", SourceURI: "slack://0", Channel: "rrf"},
		{DocumentID: "doc-1", ChunkID: "c1", Text: "target", SourceURI: "drive://1", Channel: "dense"},
		{DocumentID: "doc-2", ChunkID: "c2", Text: "target target", SourceURI: "drive://2", Channel: "hotlex", Locator: Locator{Present: true, PageNumber: 7, Section: "Policy"}},
		{DocumentID: "doc-3", ChunkID: "c3", Text: "noise", SourceURI: "notion://3", Score: 0.9, Channel: "rrf"},
		{DocumentID: "doc-4", ChunkID: "c4", Text: "target target target", SourceURI: "notion://4", Channel: "dense"},
		{DocumentID: "doc-5", ChunkID: "c5", Text: "other", SourceURI: "slack://5", Channel: "hotlex"},
	}
	original := append([]Passage(nil), pool...)
	providerCalls := 0
	var providerInput []Passage
	out, run, err := rerankRemoteBounded(
		context.Background(), "target", pool, len(pool), "cohere", "rerank-v3.5",
		testRerankScope(), newRerankScoreCache(time.Minute, 32), completeScoresByText(&providerCalls, &providerInput),
	)
	if err != nil {
		t.Fatal(err)
	}
	if run.input != 6 || run.selected != 3 || providerCalls != 1 || len(providerInput) != 3 {
		t.Fatalf("unbounded CE workload: run=%+v calls=%d input=%d", run, providerCalls, len(providerInput))
	}
	if got := docIDs(providerInput); !reflect.DeepEqual(got, []string{"doc-4", "doc-2", "doc-1"}) {
		t.Fatalf("prefilter used non-content/gold identity or lost deterministic order: %v", got)
	}
	if len(out) != len(pool) || !samePassageCandidates(pool, out) {
		t.Fatalf("candidate/citation floor changed: %v", docIDs(out))
	}
	for _, before := range original {
		found := false
		for _, after := range out {
			if passageCandidateKey(before) == passageCandidateKey(after) {
				found = true
				if before.Locator.Present != after.Locator.Present || before.Locator.PageNumber != after.Locator.PageNumber || before.Locator.Section != after.Locator.Section {
					t.Fatalf("citation locator changed for %s: before=%+v after=%+v", before.DocumentID, before.Locator, after.Locator)
				}
			}
		}
		if !found {
			t.Fatalf("authorized source candidate disappeared: %s", before.DocumentID)
		}
	}
	if got := out[0].DocumentID; got != "doc-4" {
		t.Fatalf("highest CE score did not lead: %s", got)
	}
	diag := map[string]any{}
	stampRerankScoreRun(diag, run)
	rendered := fmt.Sprint(diag)
	for _, forbidden := range []string{"target", "doc-", "drive://", "gold", "Policy", "acl-a"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("rerank diagnostics leaked %q: %s", forbidden, rendered)
		}
	}
}

func TestRerankACLIdentityIsCompleteAndDeterministic(t *testing.T) {
	a := &Client{}
	a.Security.Profile, a.Security.Principal, a.Security.Owner, a.Security.BrainID = "multi_principal", "reader", "owner", "brain"
	a.Security.Grants = map[string]bool{"reader": true, "former": false}
	b := &Client{}
	b.Security.Profile, b.Security.Principal, b.Security.Owner, b.Security.BrainID = "multi_principal", "reader", "owner", "brain"
	b.Security.Grants = map[string]bool{"former": false, "reader": true}
	base := rerankACLIdentity(a)
	if base == "" || base != rerankACLIdentity(b) {
		t.Fatal("equivalent ACL maps must have one deterministic identity")
	}
	mutations := []func(*Client){
		func(c *Client) { c.Security.Profile = "single_user" },
		func(c *Client) { c.Security.Principal = "other" },
		func(c *Client) { c.Security.Owner = "other" },
		func(c *Client) { c.Security.BrainID = "other" },
		func(c *Client) { c.Security.Grants["reader"] = false },
	}
	for i, mutate := range mutations {
		candidate := &Client{}
		candidate.Security.Profile, candidate.Security.Principal, candidate.Security.Owner, candidate.Security.BrainID = "multi_principal", "reader", "owner", "brain"
		candidate.Security.Grants = map[string]bool{"reader": true, "former": false}
		mutate(candidate)
		if got := rerankACLIdentity(candidate); got == base {
			t.Fatalf("ACL mutation %d did not change identity", i)
		}
	}
}

func TestRerankScoreCacheKeysEveryServingIdentity(t *testing.T) {
	basePassage := Passage{DocumentID: "doc", ChunkID: "chunk", SourceURI: "drive://source", Text: "target evidence"}
	mutations := []struct {
		name    string
		backend string
		model   string
		query   string
		scope   rerankScoreScope
		passage Passage
	}{
		{name: "backend", backend: "zeroentropy"},
		{name: "model", model: "rerank-v4"},
		{name: "scope", scope: rerankScoreScope{ScopeID: "brain-b\x00generation-1"}},
		{name: "dimension", scope: rerankScoreScope{Dimension: 1024}},
		{name: "acl", scope: rerankScoreScope{ACLIdentity: "acl-b"}},
		{name: "query", query: "different target"},
		{name: "query_case", query: "Target"},
		{name: "document", passage: Passage{DocumentID: "doc-b"}},
		{name: "chunk", passage: Passage{ChunkID: "chunk-b"}},
		{name: "source_uri", passage: Passage{SourceURI: "drive://other"}},
		{name: "source_text", passage: Passage{Text: "updated target evidence"}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			cache := newRerankScoreCache(time.Minute, 16)
			calls := 0
			scorer := completeScoresByText(&calls, nil)
			baseScope := testRerankScope()
			if _, _, err := rerankRemoteBounded(context.Background(), "target", []Passage{basePassage}, 1, "cohere", "rerank-v3.5", baseScope, cache, scorer); err != nil {
				t.Fatal(err)
			}
			if _, run, err := rerankRemoteBounded(context.Background(), "target", []Passage{basePassage}, 1, "cohere", "rerank-v3.5", baseScope, cache, scorer); err != nil || run.cacheHits != 1 || calls != 1 {
				t.Fatalf("control did not hit: run=%+v calls=%d err=%v", run, calls, err)
			}

			backend, model, query := "cohere", "rerank-v3.5", "target"
			scope, passage := baseScope, basePassage
			if mutation.backend != "" {
				backend = mutation.backend
			}
			if mutation.model != "" {
				model = mutation.model
			}
			if mutation.query != "" {
				query = mutation.query
			}
			if mutation.scope.ScopeID != "" {
				scope.ScopeID = mutation.scope.ScopeID
			}
			if mutation.scope.Dimension != 0 {
				scope.Dimension = mutation.scope.Dimension
			}
			if mutation.scope.ACLIdentity != "" {
				scope.ACLIdentity = mutation.scope.ACLIdentity
			}
			if mutation.passage.DocumentID != "" {
				passage.DocumentID = mutation.passage.DocumentID
			}
			if mutation.passage.ChunkID != "" {
				passage.ChunkID = mutation.passage.ChunkID
			}
			if mutation.passage.SourceURI != "" {
				passage.SourceURI = mutation.passage.SourceURI
			}
			if mutation.passage.Text != "" {
				passage.Text = mutation.passage.Text
			}
			if _, run, err := rerankRemoteBounded(context.Background(), query, []Passage{passage}, 1, backend, model, scope, cache, scorer); err != nil || run.cacheHits != 0 || calls != 2 {
				t.Fatalf("identity mutation reused score: run=%+v calls=%d err=%v", run, calls, err)
			}
		})
	}
}

func TestRerankScoreCacheStaleNegativeAndFailureSafety(t *testing.T) {
	cache := newRerankScoreCache(10*time.Millisecond, 8)
	now := time.Unix(0, 0)
	cache.now = func() time.Time { return now }
	pool := []Passage{{DocumentID: "doc", ChunkID: "chunk", SourceURI: "drive://source", Text: "target"}}
	calls := 0
	negative := func(_ context.Context, _ string, passages []Passage, _ int) ([]remoteRerankResult, error) {
		calls++
		return []remoteRerankResult{{Index: 0, RelevanceScore: -0.75}}, nil
	}
	first, run, err := rerankRemoteBounded(context.Background(), "target", pool, 1, "cohere", "model", testRerankScope(), cache, negative)
	if err != nil || first[0].Score != -0.75 || run.misses != 1 {
		t.Fatalf("negative score was not cached as a value: out=%+v run=%+v err=%v", first, run, err)
	}
	second, run, err := rerankRemoteBounded(context.Background(), "target", pool, 1, "cohere", "model", testRerankScope(), cache, negative)
	if err != nil || second[0].Score != -0.75 || run.cacheHits != 1 || calls != 1 {
		t.Fatalf("negative cache hit confused with miss: out=%+v run=%+v calls=%d err=%v", second, run, calls, err)
	}
	now = now.Add(time.Second)
	if _, run, err = rerankRemoteBounded(context.Background(), "target", pool, 1, "cohere", "model", testRerankScope(), cache, negative); err != nil || run.stales != 1 || calls != 2 {
		t.Fatalf("stale entry was served: run=%+v calls=%d err=%v", run, calls, err)
	}

	failedCache := newRerankScoreCache(time.Minute, 8)
	failureCalls := 0
	failing := func(_ context.Context, _ string, _ []Passage, _ int) ([]remoteRerankResult, error) {
		failureCalls++
		return nil, errors.New("provider unavailable")
	}
	for i := 0; i < 2; i++ {
		if _, _, err := rerankRemoteBounded(context.Background(), "target", pool, 1, "cohere", "model", testRerankScope(), failedCache, failing); err == nil {
			t.Fatal("provider failure must fail over, not become a cached negative result")
		}
	}
	if failureCalls != 2 || len(failedCache.items) != 0 {
		t.Fatalf("failure was negatively cached: calls=%d size=%d", failureCalls, len(failedCache.items))
	}
	partialCalls := 0
	partial := func(_ context.Context, _ string, _ []Passage, _ int) ([]remoteRerankResult, error) {
		partialCalls++
		return nil, nil
	}
	if _, _, err := rerankRemoteBounded(context.Background(), "target", pool, 1, "cohere", "model", testRerankScope(), failedCache, partial); err == nil || len(failedCache.items) != 0 {
		t.Fatalf("incomplete response entered cache: calls=%d size=%d err=%v", partialCalls, len(failedCache.items), err)
	}
	nonFinite := func(_ context.Context, _ string, _ []Passage, _ int) ([]remoteRerankResult, error) {
		return []remoteRerankResult{{Index: 0, RelevanceScore: math.NaN()}}, nil
	}
	if _, _, err := rerankRemoteBounded(context.Background(), "target", pool, 1, "cohere", "model", testRerankScope(), failedCache, nonFinite); err == nil || len(failedCache.items) != 0 {
		t.Fatalf("non-finite response entered cache: size=%d err=%v", len(failedCache.items), err)
	}
	twoPool := []Passage{{DocumentID: "a", Text: "target a"}, {DocumentID: "b", Text: "target b"}}
	invalid := []struct {
		name    string
		results []remoteRerankResult
	}{
		{name: "duplicate", results: []remoteRerankResult{{Index: 0, RelevanceScore: 1}, {Index: 0, RelevanceScore: 0}}},
		{name: "out_of_range", results: []remoteRerankResult{{Index: 0, RelevanceScore: 1}, {Index: 2, RelevanceScore: 0}}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			invalidCache := newRerankScoreCache(time.Minute, 8)
			scorer := func(_ context.Context, _ string, _ []Passage, _ int) ([]remoteRerankResult, error) {
				return test.results, nil
			}
			if _, _, err := rerankRemoteBounded(context.Background(), "target", twoPool, 2, "cohere", "model", testRerankScope(), invalidCache, scorer); err == nil || len(invalidCache.items) != 0 {
				t.Fatalf("invalid response entered cache: size=%d err=%v", len(invalidCache.items), err)
			}
		})
	}
}

func TestRerankScoreCacheBoundAndIncompleteIdentity(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_RERANK_PREFILTER_N", "10000")
	if got := rerankPrefilterMax(); got != hardRerankPrefilterMax {
		t.Fatalf("prefilter cap=%d want hard maximum %d", got, hardRerankPrefilterMax)
	}
	cache := newRerankScoreCache(time.Minute, 2)
	calls := 0
	scorer := completeScoresByText(&calls, nil)
	for _, query := range []string{"target one", "target two", "target three"} {
		if _, _, err := rerankRemoteBounded(context.Background(), query, []Passage{{DocumentID: "doc", Text: "target"}}, 1, "cohere", "model", testRerankScope(), cache, scorer); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.items) != 2 {
		t.Fatalf("LRU size=%d want hard configured bound 2", len(cache.items))
	}
	if _, run, err := rerankRemoteBounded(context.Background(), "target one", []Passage{{DocumentID: "doc", Text: "target"}}, 1, "cohere", "model", testRerankScope(), cache, scorer); err != nil || run.cacheHits != 0 || calls != 4 {
		t.Fatalf("oldest entry was not evicted: run=%+v calls=%d err=%v", run, calls, err)
	}

	incomplete := rerankScoreScope{ScopeID: "brain", Dimension: 1536}
	for i := 0; i < 2; i++ {
		if _, run, err := rerankRemoteBounded(context.Background(), "target", []Passage{{DocumentID: "doc", Text: "target"}}, 1, "cohere", "model", incomplete, cache, scorer); err != nil || run.cacheable || run.cacheHits != 0 {
			t.Fatalf("incomplete identity reused a score: run=%+v err=%v", run, err)
		}
	}
	if calls != 6 {
		t.Fatalf("incomplete identity must call provider each time: calls=%d", calls)
	}
	clientScope := (&Client{cfg: Config{CohereDim: 1536}}).rerankScope()
	if clientScope.cacheable() {
		t.Fatalf("empty brain scope must disable reuse: %+v", clientScope)
	}
}

func TestRerankCacheDiagnosticsDistinguishDisabledAndIncompleteIdentity(t *testing.T) {
	pool := []Passage{{DocumentID: "doc", Text: "target"}}
	calls := 0
	scorer := completeScoresByText(&calls, nil)
	tests := []struct {
		name  string
		scope rerankScoreScope
		cache *rerankScoreCache
		want  string
	}{
		{name: "cache_disabled", scope: testRerankScope(), cache: nil, want: "disabled"},
		{name: "identity_incomplete", scope: rerankScoreScope{ScopeID: "brain", Dimension: 1536}, cache: newRerankScoreCache(time.Minute, 8), want: "identity_incomplete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, run, err := rerankRemoteBounded(context.Background(), "target", pool, 1, "cohere", "model", test.scope, test.cache, scorer)
			if err != nil {
				t.Fatal(err)
			}
			diag := map[string]any{}
			stampRerankScoreRun(diag, run)
			if got := diag["rerank_cache"]; got != test.want {
				t.Fatalf("rerank_cache=%v want %q; run=%+v", got, test.want, run)
			}
		})
	}
	if calls != len(tests) {
		t.Fatalf("uncached runs must each call provider: calls=%d want=%d", calls, len(tests))
	}
}

func TestRerankProviderTimeoutUsesCapOrRemainingDeadline(t *testing.T) {
	tests := []struct {
		backend string
		cap     time.Duration
	}{
		{backend: "cohere", cap: cohereRerankTimeoutCap},
		{backend: "zeroentropy", cap: zeRerankTimeoutCap},
		{backend: "mlx", cap: mlxRerankTimeoutCap},
	}
	pool := []Passage{{DocumentID: "doc", Text: "target evidence"}}
	for _, test := range tests {
		t.Run(test.backend+"_provider_cap", func(t *testing.T) {
			var observed time.Duration
			scorer := func(ctx context.Context, _ string, passages []Passage, _ int) ([]remoteRerankResult, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("provider scorer received no deadline")
				}
				observed = time.Until(deadline)
				return []remoteRerankResult{{Index: 0, RelevanceScore: 1}}, nil
			}
			_, run, err := rerankRemoteBounded(context.Background(), "target", pool, 1, test.backend, "model", rerankScoreScope{}, nil, scorer)
			if err != nil {
				t.Fatal(err)
			}
			if observed <= test.cap-time.Second || observed > test.cap {
				t.Fatalf("provider deadline=%v want cap near %v", observed, test.cap)
			}
			if run.providerTimeout <= test.cap-time.Second || run.providerTimeout > test.cap {
				t.Fatalf("recorded timeout=%v want cap near %v", run.providerTimeout, test.cap)
			}
		})

		t.Run(test.backend+"_remaining_request_deadline", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			var observed time.Duration
			scorer := func(callCtx context.Context, _ string, _ []Passage, _ int) ([]remoteRerankResult, error) {
				deadline, ok := callCtx.Deadline()
				if !ok {
					t.Fatal("provider scorer received no request deadline")
				}
				observed = time.Until(deadline)
				return []remoteRerankResult{{Index: 0, RelevanceScore: 1}}, nil
			}
			_, run, err := rerankRemoteBounded(ctx, "target", pool, 1, test.backend, "model", rerankScoreScope{}, nil, scorer)
			if err != nil {
				t.Fatal(err)
			}
			if observed <= 0 || observed > 250*time.Millisecond || run.providerTimeout <= 0 || run.providerTimeout > 250*time.Millisecond {
				t.Fatalf("provider deadline escaped remaining request wall: observed=%v recorded=%v", observed, run.providerTimeout)
			}
		})
	}
}

func TestRerankProviderDeadlineExhaustionFailsClosed(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	called := false
	scorer := func(context.Context, string, []Passage, int) ([]remoteRerankResult, error) {
		called = true
		return []remoteRerankResult{{Index: 0, RelevanceScore: 1}}, nil
	}
	out, run, err := rerankRemoteBounded(ctx, "target", []Passage{{DocumentID: "doc", Text: "target"}}, 1, "cohere", "model", rerankScoreScope{}, nil, scorer)
	if err == nil || out != nil || called {
		t.Fatalf("exhausted deadline did not fail closed: out=%v called=%v err=%v", out, called, err)
	}
	if run.failureReason != "deadline_exhausted" || run.providerSubmitted || run.providerScored != 0 {
		t.Fatalf("exhausted deadline diagnostics=%+v", run)
	}
}

func TestRerankProviderTimeoutCancelsScorer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	scorer := func(callCtx context.Context, _ string, _ []Passage, _ int) ([]remoteRerankResult, error) {
		<-callCtx.Done()
		return nil, callCtx.Err()
	}
	_, run, err := rerankRemoteBounded(ctx, "target", []Passage{{DocumentID: "doc", Text: "target"}}, 1, "mlx", "model", rerankScoreScope{}, nil, scorer)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provider call err=%v want deadline exceeded", err)
	}
	if run.failureReason != "timeout" || !run.providerSubmitted || run.providerScored != 0 || run.providerLatency <= 0 {
		t.Fatalf("timeout run=%+v", run)
	}
}

func TestRerankDiagnosticsCountCharactersCostAndWarmProviderWork(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_RERANK_PREFILTER_N", "2")
	pool := []Passage{
		{DocumentID: "a", Text: strings.Repeat("a", 3000) + " target", Score: 2},
		{DocumentID: "b", Text: "target evidence", Score: 1},
		{DocumentID: "tail", Text: "unselected"},
	}
	cache := newRerankScoreCache(time.Minute, 16)
	calls := 0
	scorer := completeScoresByText(&calls, nil)
	_, cold, err := rerankRemoteBounded(context.Background(), "target", pool, 3, "cohere", "model", testRerankScope(), cache, scorer)
	if err != nil {
		t.Fatal(err)
	}
	if cold.providerScored != 2 || cold.ceCharacters != len("target")+2000+len("target evidence") || cold.ceCharactersCapped {
		t.Fatalf("cold provider accounting=%+v", cold)
	}
	diag := map[string]any{}
	stampRerankScoreRun(diag, cold)
	if diag["rerank_provider_scored"] != 2 || diag["rerank_ce_characters"] != len("target")+2000+len("target evidence") {
		t.Fatalf("cold diagnostics=%v", diag)
	}
	cost, ok := diag["rerank_ce_cost"].(map[string]any)
	if !ok || cost["status"] != "unknown" || cost["reason"] != "provider_pricing_unavailable" {
		t.Fatalf("provider cost must be explicitly unknown: %#v", diag["rerank_ce_cost"])
	}
	if _, has := cost["cost_usd"]; has {
		t.Fatalf("unknown provider pricing fabricated a USD cost: %#v", cost)
	}

	_, warm, err := rerankRemoteBounded(context.Background(), "target", pool, 3, "cohere", "model", testRerankScope(), cache, scorer)
	if err != nil {
		t.Fatal(err)
	}
	if warm.providerScored != 0 || warm.ceCharacters != 0 || warm.providerSubmitted || calls != 1 {
		t.Fatalf("warm cache claimed provider work: run=%+v calls=%d", warm, calls)
	}
	warmDiag := map[string]any{}
	stampRerankScoreRun(warmDiag, warm)
	warmCost := warmDiag["rerank_ce_cost"].(map[string]any)
	if warmCost["status"] != "not_incurred" || warmCost["cost_usd"] != 0.0 {
		t.Fatalf("warm provider cost=%#v", warmCost)
	}

	longQuestion := strings.Repeat("界", maxRerankCECharactersDiag+1)
	if got, capped := rerankCECharacters(longQuestion, pool, "cohere"); got != maxRerankCECharactersDiag || !capped {
		t.Fatalf("character diagnostic was not bounded: got=%d capped=%v", got, capped)
	}
}

func TestRerankTieOrderDeterministicAcrossInputOrder(t *testing.T) {
	base := []Passage{
		{DocumentID: "z", ChunkID: "3", SourceURI: "drive://z", Text: "target z"},
		{DocumentID: "a", ChunkID: "1", SourceURI: "drive://a", Text: "target a", Locator: Locator{Present: true, PageNumber: 2}},
		{DocumentID: "m", ChunkID: "2", SourceURI: "drive://m", Text: "target m"},
	}
	equalScores := func(_ context.Context, _ string, passages []Passage, _ int) ([]remoteRerankResult, error) {
		results := make([]remoteRerankResult, len(passages))
		for i := range passages {
			results[len(passages)-1-i] = remoteRerankResult{Index: i, RelevanceScore: 0.5}
		}
		return results, nil
	}
	outA, _, err := rerankRemoteBounded(context.Background(), "target", base, 3, "cohere", "model", rerankScoreScope{}, nil, equalScores)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []Passage{base[2], base[0], base[1]}
	outB, _, err := rerankRemoteBounded(context.Background(), "target", reversed, 3, "cohere", "model", rerankScoreScope{}, nil, equalScores)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(docIDs(outA), docIDs(outB)) {
		t.Fatalf("tie order depends on provider/input order: a=%v b=%v", docIDs(outA), docIDs(outB))
	}
	for _, passage := range outB {
		if passage.DocumentID == "a" && (!passage.Locator.Present || passage.Locator.PageNumber != 2) {
			t.Fatalf("citation locator lost across deterministic ranking: %+v", passage)
		}
	}
}

func fixedRerankWorkload() []Passage {
	passages := make([]Passage, 150)
	for i := range passages {
		passages[i] = Passage{
			DocumentID: fmt.Sprintf("doc-%03d", i),
			ChunkID:    fmt.Sprintf("chunk-%03d", i),
			SourceURI:  fmt.Sprintf("drive://source/%03d", i),
			Text: fmt.Sprintf("recovery objective escalation owner candidate %03d %s", i,
				strings.Repeat("bounded evidence payload ", 16)),
			Score: float64(150-i) / 150,
		}
	}
	return passages
}

func fixedRerankScorer(_ context.Context, question string, passages []Passage, _ int) ([]remoteRerankResult, error) {
	results := make([]remoteRerankResult, len(passages))
	for i, passage := range passages {
		// Allocate and scan a clipped payload to model the deterministic client
		// work that grows with the number of CE query/document pairs.
		payload := strings.ToLower(question + "\x00" + passage.Text)
		results[i] = remoteRerankResult{Index: i, RelevanceScore: float64(strings.Count(payload, "recovery"))}
	}
	return results, nil
}

func TestRerankFixedWorkloadBoundAndAllocations(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_RERANK_PREFILTER_N", "32")
	pool := fixedRerankWorkload()
	cache := newRerankScoreCache(time.Minute, 512)
	if _, run, err := rerankRemoteBounded(context.Background(), "recovery objective", pool, len(pool), "cohere", "model", testRerankScope(), cache, fixedRerankScorer); err != nil || run.selected != 32 {
		t.Fatalf("fixed workload did not enforce CE bound: run=%+v err=%v", run, err)
	}
	allocs := testing.AllocsPerRun(25, func() {
		out, run, err := rerankRemoteBounded(context.Background(), "recovery objective", pool, len(pool), "cohere", "model", testRerankScope(), cache, fixedRerankScorer)
		if err != nil || len(out) != len(pool) || run.cacheHits != 32 || run.misses != 0 {
			panic("invalid warm fixed-workload result")
		}
	})
	if allocs > 800 {
		t.Fatalf("warm fixed-workload allocations=%.1f, want <=800", allocs)
	}
	t.Logf("fixed workload candidates=%d CE_pairs=32 warm_allocs/op=%.1f", len(pool), allocs)
}

func BenchmarkRerankPrefilterCacheFixedWorkload(b *testing.B) {
	b.Setenv("OUROBOROS_ERB_RERANK_PREFILTER_N", "32")
	pool := fixedRerankWorkload()
	question := "recovery objective escalation owner"
	b.Run("unbounded_control_150_pairs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			results, err := fixedRerankScorer(context.Background(), question, pool, len(pool))
			if err != nil || len(results) != len(pool) {
				b.Fatal(err)
			}
			sort.Slice(results, func(i, j int) bool {
				if results[i].RelevanceScore != results[j].RelevanceScore {
					return results[i].RelevanceScore > results[j].RelevanceScore
				}
				return results[i].Index < results[j].Index
			})
			out, err := assembleRemoteRerank(pool, results, "cohere")
			if err != nil || len(out) != len(pool) {
				b.Fatal(err)
			}
		}
	})
	b.Run("bounded_cold_32_pairs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out, run, err := rerankRemoteBounded(context.Background(), question, pool, len(pool), "cohere", "model", rerankScoreScope{}, nil, fixedRerankScorer)
			if err != nil || len(out) != len(pool) || run.selected != 32 {
				b.Fatal(err)
			}
		}
	})
	b.Run("bounded_warm_cache_32_hits", func(b *testing.B) {
		cache := newRerankScoreCache(time.Minute, 512)
		if _, _, err := rerankRemoteBounded(context.Background(), question, pool, len(pool), "cohere", "model", testRerankScope(), cache, fixedRerankScorer); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			out, run, err := rerankRemoteBounded(context.Background(), question, pool, len(pool), "cohere", "model", testRerankScope(), cache, fixedRerankScorer)
			if err != nil || len(out) != len(pool) || run.cacheHits != 32 || run.misses != 0 {
				b.Fatal(err)
			}
		}
	})
}

type rerankBlindMatrixCase struct {
	Name     string
	Question string
	Pool     []Passage
	Gold     map[string]bool
}

func rerankBlindMatrixCases() []rerankBlindMatrixCase {
	return []rerankBlindMatrixCase{
		{
			Name: "semantic_single_source", Question: "recovery escalation owner",
			Gold: map[string]bool{"sem-gold": true},
			Pool: []Passage{
				{DocumentID: "sem-noise-0", Text: "general handbook", Channel: "hotlex"},
				{DocumentID: "sem-gold", Text: "recovery escalation owner procedure", Channel: "dense", Locator: Locator{Present: true, PageNumber: 7}},
				{DocumentID: "sem-near-2", Text: "recovery escalation", Channel: "hotlex"},
				{DocumentID: "sem-near-1", Text: "recovery", Channel: "dense"},
				{DocumentID: "sem-noise-1", Text: "expense policy", Channel: "hotlex"},
				{DocumentID: "sem-noise-2", Text: "travel calendar", Channel: "dense"},
				{DocumentID: "sem-noise-3", Text: "benefits enrollment", Channel: "hotlex"},
				{DocumentID: "sem-noise-4", Text: "office map", Channel: "dense"},
			},
		},
		{
			Name: "multi_source", Question: "retention deletion audit",
			Gold: map[string]bool{"multi-gold-a": true, "multi-gold-b": true},
			Pool: []Passage{
				{DocumentID: "multi-noise-0", Text: "general handbook", Channel: "hotlex"},
				{DocumentID: "multi-gold-a", Text: "retention deletion audit schedule", Channel: "dense", Locator: Locator{Present: true, Section: "Retention"}},
				{DocumentID: "multi-near", Text: "retention audit", Channel: "hotlex"},
				{DocumentID: "multi-gold-b", Text: "audit deletion retention exception", Channel: "dense", Locator: Locator{Present: true, PageNumber: 3}},
				{DocumentID: "multi-noise-1", Text: "expense policy", Channel: "hotlex"},
				{DocumentID: "multi-noise-2", Text: "travel calendar", Channel: "dense"},
				{DocumentID: "multi-noise-3", Text: "benefits enrollment", Channel: "hotlex"},
				{DocumentID: "multi-noise-4", Text: "office map", Channel: "dense"},
			},
		},
		{
			Name: "citation_locator", Question: "regional failover threshold",
			Gold: map[string]bool{"cite-gold": true},
			Pool: []Passage{
				{DocumentID: "cite-noise-0", Text: "general handbook", Channel: "hotlex"},
				{DocumentID: "cite-near-2", Text: "regional failover", Channel: "dense"},
				{DocumentID: "cite-gold", Text: "regional failover threshold policy", Channel: "hotlex", Locator: Locator{Present: true, PageNumber: 11, Section: "Routing"}},
				{DocumentID: "cite-near-1", Text: "threshold", Channel: "dense"},
				{DocumentID: "cite-noise-1", Text: "expense policy", Channel: "hotlex"},
				{DocumentID: "cite-noise-2", Text: "travel calendar", Channel: "dense"},
				{DocumentID: "cite-noise-3", Text: "benefits enrollment", Channel: "hotlex"},
				{DocumentID: "cite-noise-4", Text: "office map", Channel: "dense"},
			},
		},
	}
}

// TestRerankPinnedBlindMatrix is the executable source for the before/after
// evidence table in RERANK-CACHE-EVIDENCE.md. Gold membership is consulted
// only after both rankers return; neither prefilter nor scorer can receive it.
func TestRerankPinnedBlindMatrix(t *testing.T) {
	fixtureJSON, err := json.Marshal(rerankBlindMatrixCases())
	if err != nil {
		t.Fatal(err)
	}
	fixtureDigest := fmt.Sprintf("%x", sha256.Sum256(fixtureJSON))[:16]
	if fixtureDigest != "1280beb8a8113908" {
		t.Fatalf("blind matrix fixture drifted: digest=%s", fixtureDigest)
	}
	scorer := func(_ context.Context, question string, passages []Passage, _ int) ([]remoteRerankResult, error) {
		tokens := contentTokens(question)
		results := make([]remoteRerankResult, len(passages))
		for i, passage := range passages {
			lower := strings.ToLower(passage.Text)
			score := 0
			for _, token := range tokens {
				if strings.Contains(lower, token) {
					score++
				}
			}
			results[i] = remoteRerankResult{Index: i, RelevanceScore: float64(score)}
		}
		return results, nil
	}

	totalGold := 0
	beforePoolHits, afterPoolHits := 0, 0
	beforeWindowHits, afterWindowHits := 0, 0
	beforeCitationLocators, afterCitationLocators := 0, 0
	beforeScored, afterScored := 0, 0
	beforeChars, afterChars := 0, 0
	for _, fixture := range rerankBlindMatrixCases() {
		t.Setenv("OUROBOROS_ERB_RERANK_PREFILTER_N", "96")
		before, beforeRun, err := rerankRemoteBounded(context.Background(), fixture.Question, fixture.Pool, len(fixture.Pool), "cohere", "hermetic-blind", rerankScoreScope{}, nil, scorer)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("OUROBOROS_ERB_RERANK_PREFILTER_N", "3")
		after, afterRun, err := rerankRemoteBounded(context.Background(), fixture.Question, fixture.Pool, len(fixture.Pool), "cohere", "hermetic-blind", rerankScoreScope{}, nil, scorer)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(docIDs(before[:3]), docIDs(after[:3])) {
			t.Fatalf("%s top-window ordering changed: before=%v after=%v", fixture.Name, docIDs(before[:3]), docIDs(after[:3]))
		}
		if !samePassageCandidates(fixture.Pool, before) || !samePassageCandidates(fixture.Pool, after) {
			t.Fatalf("%s candidate pool changed", fixture.Name)
		}
		beforeByID := make(map[string]Passage, len(before))
		afterByID := make(map[string]Passage, len(after))
		for _, passage := range before {
			beforeByID[passage.DocumentID] = passage
		}
		for _, passage := range after {
			afterByID[passage.DocumentID] = passage
		}
		for goldID := range fixture.Gold {
			totalGold++
			if _, ok := beforeByID[goldID]; ok {
				beforePoolHits++
			}
			if _, ok := afterByID[goldID]; ok {
				afterPoolHits++
			}
			if passageInWindow(before[:3], goldID) {
				beforeWindowHits++
			}
			if passageInWindow(after[:3], goldID) {
				afterWindowHits++
			}
			if beforeByID[goldID].Locator.Present {
				beforeCitationLocators++
			}
			if afterByID[goldID].Locator.Present {
				afterCitationLocators++
			}
			if !reflect.DeepEqual(beforeByID[goldID].Locator, afterByID[goldID].Locator) {
				t.Fatalf("%s citation locator changed for %s", fixture.Name, goldID)
			}
		}
		beforeScored += beforeRun.providerScored
		afterScored += afterRun.providerScored
		beforeChars += beforeRun.ceCharacters
		afterChars += afterRun.ceCharacters
	}
	if totalGold != 4 || beforePoolHits != 4 || afterPoolHits != 4 || beforeWindowHits != 4 || afterWindowHits != 4 || beforeCitationLocators != 4 || afterCitationLocators != 4 {
		t.Fatalf("blind matrix recall/citation regression: gold=%d pool=%d/%d window=%d/%d locators=%d/%d", totalGold, beforePoolHits, afterPoolHits, beforeWindowHits, afterWindowHits, beforeCitationLocators, afterCitationLocators)
	}
	if beforeScored != 24 || afterScored != 9 {
		t.Fatalf("blind matrix provider bound: before=%d after=%d", beforeScored, afterScored)
	}
	t.Logf("pinned blind matrix: cases=3 gold=4 top3_order_equal=3 pool_recall=1.0->1.0 window_recall=1.0->1.0 citation_locator_recall=1.0->1.0 provider_scored=%d->%d ce_characters=%d->%d provider_cost_usd=unknown->unknown", beforeScored, afterScored, beforeChars, afterChars)
}

func passageInWindow(passages []Passage, documentID string) bool {
	for _, passage := range passages {
		if passage.DocumentID == documentID {
			return true
		}
	}
	return false
}

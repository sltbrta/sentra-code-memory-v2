package hosted

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// referenceEntityScan is the pre-#303 per-query linear scan, kept here as the
// semantic oracle for the indexed lookup.
func referenceEntityScan(keys []string, seed string, pred func(string) bool) map[string]struct{} {
	out := map[string]struct{}{}
	for _, k := range keys {
		if pred(k) {
			out[k] = struct{}{}
		}
	}
	return out
}

func offlinePred(s string) func(string) bool {
	return func(k string) bool {
		return strings.Contains(k, s) || (len(s) >= 5 && strings.Contains(s, k))
	}
}

func TestEntityNameIndexMatchesReferenceScan(t *testing.T) {
	keys := []string{
		"acme corp", "acme corporation", "orion", "orion beta", "orion beta launch",
		"postgresql docs", "kvcache metrics", "inc-1234", "billing gate", "auth module",
	}
	ix := newEntityNameIndex(keys)
	for _, seed := range []string{
		"acme",               // whole token of several keys
		"orion",              // whole token + exact key
		"postgres",           // prefix of token "postgresql"
		"acme-corp-incident", // multi-token seed containing key tokens
		"cache",              // mid-token substring of "kvcache" → only fallback finds it
		"inc-1234",           // identifier exact
		"zzz-none",           // no match at all
	} {
		want := referenceEntityScan(ix.keys, seed, offlinePred(seed))
		got := ix.match(seed, len(keys)+1, offlinePred(seed))
		if len(got) != len(want) {
			t.Fatalf("seed %q: got %v want set %v", seed, got, want)
		}
		for _, k := range got {
			if _, ok := want[k]; !ok {
				t.Fatalf("seed %q: unexpected match %q (want %v)", seed, k, want)
			}
		}
	}
}

func TestEntityNameIndexDeterministicLongestFirstOrder(t *testing.T) {
	ix := newEntityNameIndex([]string{"orion", "orion beta launch", "orion beta"})
	want := []string{"orion beta launch", "orion beta", "orion"}
	for i := 0; i < 3; i++ {
		got := ix.match("orion", 10, func(k string) bool { return strings.Contains(k, "orion") })
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("iteration %d: got %v want %v", i, got, want)
			}
		}
	}
}

func TestEntityNameIndexSmallCatalogTopUpMatchesReference(t *testing.T) {
	// "cache" hits "cache metrics" via the token index but "kvcache metrics"
	// only via the mid-token substring scan. When the index result is partial
	// (< max) on a small catalog, the reference scan must top it up: exact
	// pre-#303 key set, exact longest-first order, no duplicates.
	keys := []string{"cache metrics", "kvcache metrics", "auth module"}
	ix := newEntityNameIndex(keys)
	got := ix.match("cache", 8, offlinePred("cache"))
	want := []string{"kvcache metrics", "cache metrics"}
	if len(got) != len(want) {
		t.Fatalf("top-up result: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("top-up order diverges from reference scan: got %v want %v", got, want)
		}
	}
	ref := referenceEntityScan(ix.keys, "cache", offlinePred("cache"))
	if len(ref) != len(got) {
		t.Fatalf("top-up must equal reference oracle: got %v ref %v", got, ref)
	}
	seen := map[string]struct{}{}
	for _, k := range got {
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate key %q after top-up: %v", k, got)
		}
		seen[k] = struct{}{}
	}
	s := ix.stats()
	if s.FallbackScans != 1 {
		t.Fatalf("partial index result must count one fallback scan, got %+v", s)
	}
	if s.IndexMatches != 2 {
		t.Fatalf("token + trigram index hits must be counted, got %+v", s)
	}

	// max caps the topped-up result with old scan priority: the longest key
	// wins even though only the shorter one was reachable via the index.
	ix2 := newEntityNameIndex(keys)
	if got := ix2.match("cache", 2, offlinePred("cache")); len(got) != 2 ||
		got[0] != "kvcache metrics" || got[1] != "cache metrics" {
		t.Fatalf("capped top-up must keep reference-scan order, got %v", got)
	}
}

func TestEntityNameIndexFallbackIsBoundedWithDiagnostics(t *testing.T) {
	// Small catalog: mid-token substring match is only reachable via the
	// bounded fallback scan, which must run and be counted.
	small := newEntityNameIndex([]string{"kvcache metrics", "auth module"})
	got := small.match("cache", 4, offlinePred("cache"))
	if len(got) != 1 || got[0] != "kvcache metrics" {
		t.Fatalf("fallback should find mid-token match, got %v", got)
	}
	if s := small.stats(); s.FallbackScans < 1 {
		t.Fatalf("expected fallback scan diagnostic, got %+v", s)
	}

	// Large catalog (> entityIndexFallbackScanMax): the same mid-token-only
	// seed is served by the trigram index without a full scan.
	keys := make([]string, 0, entityIndexFallbackScanMax+10)
	for i := 0; i < entityIndexFallbackScanMax+10; i++ {
		keys = append(keys, fmt.Sprintf("kvcache%05d metrics", i))
	}
	large := newEntityNameIndex(keys)
	if got := large.match("cache", 4, offlinePred("cache")); len(got) != 4 {
		t.Fatalf("large catalog trigram recall failed, got %v", got)
	}
	s := large.stats()
	if s.FallbackScans != 0 {
		t.Fatalf("large catalog must never linearly scan, got %+v", s)
	}
	if got := large.match("not-present-anywhere", 4, offlinePred("not-present-anywhere")); got != nil {
		t.Fatalf("unrelated seed must remain absent, got %v", got)
	}
	if s = large.stats(); s.FallbackSkips < 1 {
		t.Fatalf("expected bounded no-match skip diagnostic, got %+v", s)
	}

	// Indexed lookups still work on the large catalog without scanning, and
	// oversized posting lists are truncated with a diagnostic.
	hits := large.match("metrics", 4, offlinePred("metrics"))
	if len(hits) != 4 {
		t.Fatalf("indexed whole-token match failed on large catalog: %v", hits)
	}
	if s := large.stats(); s.Truncations < 1 || s.FallbackScans != 0 {
		t.Fatalf("expected posting truncation without scan, got %+v", s)
	}

	// Partial index result on a large catalog: returned as-is (never scanned)
	// and the skipped reference scan is counted — the diagnostics must not
	// pretend the old semantics were fully reproduced.
	skipsBefore := large.stats().FallbackSkips
	partial := large.match("kvcache00001", 8, offlinePred("kvcache00001"))
	if len(partial) != 1 || partial[0] != "kvcache00001 metrics" {
		t.Fatalf("partial indexed match on large catalog failed: %v", partial)
	}
	s = large.stats()
	if s.FallbackSkips != skipsBefore+1 {
		t.Fatalf("partial large-catalog result must count a fallback skip, got %+v", s)
	}
	if s.FallbackScans != 0 {
		t.Fatalf("large catalog must never linearly scan, got %+v", s)
	}
}

func largeEntityFixture(n int) *OfflineEntityCatalog {
	cat := &OfflineEntityCatalog{
		BrainID:     "fixed-corpus",
		Generation:  "gen-fixed",
		Names:       make(map[string]string, n+8),
		NameToDSIDs: make(map[string][]string, n+8),
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("ordinary entity %06d", i)
		cat.Names[key] = fmt.Sprintf("Ordinary Entity %06d", i)
		cat.NameToDSIDs[key] = []string{fmt.Sprintf("doc-noise-%06d", i)}
	}
	for key, display := range map[string]string{
		"orion":                     "Orion",
		"orion migration":           "Orion Migration",
		"orion migration program":   "Orion Migration Program",
		"kvcache retention metrics": "KVCache Retention Metrics",
		"inc-7q9x":                  "INC-7Q9X",
	} {
		cat.Names[key] = display
		cat.NameToDSIDs[key] = []string{"doc-" + strings.ReplaceAll(key, " ", "-")}
	}
	return cat
}

func TestLargeCatalogFixedCorpusTermsRankingAndRecall(t *testing.T) {
	cat := largeEntityFixture(entityIndexFallbackScanMax + 1024)
	nameIdx, _ := cat.indexes()
	if nameIdx.size() <= entityIndexFallbackScanMax {
		t.Fatalf("fixture must exercise large-catalog path: %d", nameIdx.size())
	}

	// The indexed result must preserve the old substring oracle for this fixed
	// corpus, including the mid-token cache -> kvcache match.
	for _, seed := range []string{"orion", "cache", "inc-7q9x"} {
		want := referenceEntityScan(nameIdx.keys, seed, offlinePred(seed))
		got := nameIdx.match(seed, 32, offlinePred(seed))
		if len(got) != len(want) {
			t.Fatalf("seed %q recall: got %v want %v", seed, got, want)
		}
		for _, key := range got {
			if _, ok := want[key]; !ok {
				t.Fatalf("seed %q returned non-oracle key %q", seed, key)
			}
		}
	}

	terms := offlineEntityTermsFromCatalog(cat, "What changed for Orion?", 8)
	wantOrder := []string{"Orion", "Orion Migration Program", "Orion Migration"}
	if len(terms) < len(wantOrder) {
		t.Fatalf("ranked terms too short: %v", terms)
	}
	for i, want := range wantOrder {
		if terms[i] != want {
			t.Fatalf("rank %d = %q, want %q; all=%v", i, terms[i], want, terms)
		}
	}
	cacheTerms := offlineEntityTermsFromCatalog(cat, "Where are the cache retention metrics?", 4)
	if len(cacheTerms) == 0 || cacheTerms[0] != "KVCache Retention Metrics" {
		t.Fatalf("large-catalog mid-token term recall failed: %v", cacheTerms)
	}
}

func TestLargeLiveCatalogFixedCorpusMatchesOracleIncludingReverseContainment(t *testing.T) {
	keys := make([]string, 0, entityIndexFallbackScanMax+515)
	for i := 0; i < entityIndexFallbackScanMax+512; i++ {
		keys = append(keys, fmt.Sprintf("ordinary live entity %06d", i))
	}
	keys = append(keys, "cache", "kvcache", "unrelated")
	ix := newEntityNameIndex(keys)
	if ix.size() <= entityIndexFallbackScanMax {
		t.Fatalf("fixture must exceed fallback limit: %d", ix.size())
	}
	seed := "kvcache-refcount"
	livePred := func(k string) bool {
		return strings.Contains(k, seed) || strings.Contains(seed, k)
	}
	want := referenceEntityScan(ix.keys, seed, livePred)
	got := ix.match(seed, 32, livePred)
	if len(got) != len(want) {
		t.Fatalf("large live reverse-containment recall: got %v want %v", got, want)
	}
	for _, key := range got {
		if _, ok := want[key]; !ok {
			t.Fatalf("large live predicate returned non-oracle key %q", key)
		}
	}
	if len(got) != 2 || got[0] != "kvcache" || got[1] != "cache" {
		t.Fatalf("reverse-containment ranking changed: %v", got)
	}
	if s := ix.stats(); s.FallbackScans != 0 {
		t.Fatalf("large live catalog must not scan: %+v", s)
	}
	t.Logf("large-live fixed-corpus recall=%d/%d", len(got), len(want))
}

func TestLargeCatalogRareIdentifierRetentionAndNoQuestionContamination(t *testing.T) {
	cat := largeEntityFixture(entityIndexFallbackScanMax + 512)
	terms := offlineEntityTermsFromCatalog(cat,
		"For INC-7Q9X and QUESTION_ONLY_CANARY, what changed?", 1)
	if len(terms) != 1 || terms[0] != "INC-7Q9X" {
		t.Fatalf("rare exact identifier must survive maxN=1: %v", terms)
	}
	ids := offlineEntityDSIDsFromCatalog(cat,
		"For INC-7Q9X and QUESTION_ONLY_CANARY, what changed?", 1)
	if len(ids) != 1 || ids[0] != "doc-inc-7q9x" {
		t.Fatalf("rare identifier dsid must survive: %v", ids)
	}
	for _, term := range offlineEntityTermsFromCatalog(cat, "QUESTION_ONLY_CANARY gold-answer-only", 8) {
		if strings.Contains(strings.ToLower(term), "question") || strings.Contains(strings.ToLower(term), "gold") {
			t.Fatalf("question/gold-specific value contaminated corpus catalog result: %v", term)
		}
	}
	if catalogMatchesScope(&OfflineEntityCatalog{Names: map[string]string{"orion": "Orion"}},
		"fixed-corpus", "") {
		t.Fatal("serving path must reject an unscoped catalog with no brain id")
	}
	if catalogMatchesScope(cat, "fixed-corpus", "other-generation") {
		t.Fatal("serving path must reject a catalog from another generation")
	}
}

func TestLargeCatalogLookupP50P95AndAllocationsBounded(t *testing.T) {
	keys := make([]string, 0, 70001)
	for i := 0; i < 70000; i++ {
		keys = append(keys, fmt.Sprintf("ordinary entity %06d", i))
	}
	keys = append(keys, "inc-7q9x", "kvcache retention metrics")
	ix := newEntityNameIndex(keys)
	metrics := ix.metrics()
	if metrics.Keys != len(keys) || metrics.TokenPostings == 0 ||
		metrics.GramPostings == 0 || metrics.PayloadBytes == 0 {
		t.Fatalf("invalid steady-state index metrics: %+v", metrics)
	}
	type lookup struct {
		seed string
		pred func(string) bool
	}
	lookups := []lookup{
		{seed: "inc-7q9x", pred: offlinePred("inc-7q9x")},
		{seed: "cache", pred: offlinePred("cache")},
		{seed: "ordinary", pred: offlinePred("ordinary")},
	}
	for _, q := range lookups {
		_ = ix.match(q.seed, 8, q.pred) // warm all lazy state
	}

	allocs := testing.AllocsPerRun(100, func() {
		for _, q := range lookups {
			if got := ix.match(q.seed, 8, q.pred); len(got) == 0 {
				panic(fmt.Sprintf("lookup %q=%v", q.seed, got))
			}
		}
	})
	if allocs > 192 {
		t.Fatalf("three warm large-catalog lookups allocations = %.1f, want <=192", allocs)
	}
	bench := testing.Benchmark(func(b *testing.B) {
		q := lookups[2] // common posting exercises the bounded 2,048-candidate cap
		for i := 0; i < b.N; i++ {
			_ = ix.match(q.seed, 8, q.pred)
		}
	})
	if bytes := bench.AllocedBytesPerOp(); bytes > 256<<10 {
		t.Fatalf("common warm lookup allocated %d bytes/op, want <=256KiB", bytes)
	}

	const samples = 201
	times := make([]time.Duration, 0, samples)
	before := ix.stats().Candidates
	for i := 0; i < samples; i++ {
		q := lookups[i%len(lookups)]
		start := time.Now()
		_ = ix.match(q.seed, 8, q.pred)
		times = append(times, time.Since(start))
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	p50, p95 := times[samples/2], times[(samples*95+99)/100-1]
	// These are deliberately loose regression ceilings, not a production SLO;
	// the structural candidate cap below is the machine-independent CPU bound.
	if p50 > 20*time.Millisecond || p95 > 100*time.Millisecond {
		t.Fatalf("warm lookup latency p50=%s p95=%s", p50, p95)
	}
	verified := ix.stats().Candidates - before
	if verified > samples*64 {
		t.Fatalf("candidate verification work = %d for %d samples", verified, samples)
	}
	t.Logf("large catalog n=%d logical_payload_bytes=%d token_postings=%d gram_postings=%d allocs/3ops=%.1f common_bytes/op=%d p50=%s p95=%s candidates/op=%.1f",
		ix.size(), metrics.PayloadBytes, metrics.TokenPostings, metrics.GramPostings,
		allocs, bench.AllocedBytesPerOp(), p50, p95, float64(verified)/samples)
}

func TestEntityBrainCacheGenerationAndTTLReuse(t *testing.T) {
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	ec := &entityCatalogCache{byBrain: map[string]*entityBrainSlot{}, now: func() time.Time { return now }}
	first := ec.getOrLoad(context.Background(), nil, "brain-a", "gen-1")
	if again := ec.getOrLoad(context.Background(), nil, "brain-a", "gen-1"); again != first {
		t.Fatal("same generation inside TTL must reuse immutable sample/index")
	}
	if changed := ec.getOrLoad(context.Background(), nil, "brain-a", "gen-2"); changed == first {
		t.Fatal("generation change must replace sample inside TTL")
	}
	current := ec.getOrLoad(context.Background(), nil, "brain-a", "gen-2")
	now = now.Add(entityCatalogTTL)
	if expired := ec.getOrLoad(context.Background(), nil, "brain-a", "gen-2"); expired == current {
		t.Fatal("TTL expiry must replace sample")
	}
	// A clock rollback must not turn a future timestamp into an indefinitely
	// fresh entry.
	current = ec.getOrLoad(context.Background(), nil, "brain-a", "gen-2")
	now = now.Add(-time.Minute)
	if rollback := ec.getOrLoad(context.Background(), nil, "brain-a", "gen-2"); rollback == current {
		t.Fatal("negative cache age must be treated stale")
	}
}

func TestEntityBrainCacheSingleFlightIsPerBrain(t *testing.T) {
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	var mu sync.Mutex
	loads := map[string]int{}
	ec := &entityCatalogCache{
		byBrain: map[string]*entityBrainSlot{},
		load: func(_ context.Context, _ *sql.DB, brain string, _ int) *entityBrainCache {
			mu.Lock()
			loads[brain]++
			mu.Unlock()
			if brain == "brain-a" {
				select {
				case <-startedA:
				default:
					close(startedA)
				}
				<-releaseA
			}
			return &entityBrainCache{names: map[string]string{brain: brain}}
		},
	}
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		_ = ec.getOrLoad(context.Background(), nil, "brain-a", "gen-1")
	}()
	<-startedA
	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		_ = ec.getOrLoad(context.Background(), nil, "brain-b", "gen-1")
	}()
	select {
	case <-doneB:
	case <-time.After(time.Second):
		t.Fatal("brain-b load waited behind blocked brain-a load")
	}
	secondA := make(chan struct{})
	go func() {
		defer close(secondA)
		_ = ec.getOrLoad(context.Background(), nil, "brain-a", "gen-1")
	}()
	close(releaseA)
	<-doneA
	<-secondA
	mu.Lock()
	defer mu.Unlock()
	if loads["brain-a"] != 1 || loads["brain-b"] != 1 {
		t.Fatalf("per-brain single-flight load counts: %v", loads)
	}
}

func TestOfflineCatalogAtomicGenerationRefreshAndScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entity-catalog.json")
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", path)
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	fc := &offlineEntityFileCache{now: func() time.Time { return now }}
	one := &OfflineEntityCatalog{BrainID: "brain-a", Generation: "gen-1",
		Names: map[string]string{"orion": "Orion"}}
	if err := WriteOfflineEntityCatalog(path, one); err != nil {
		t.Fatal(err)
	}
	if one.Generated != "" {
		t.Fatal("atomic writer must not mutate a concurrently reusable catalog")
	}
	got1 := fc.load("brain-a", "gen-1")
	if got1 == nil {
		t.Fatalf("initial scoped load failed: %s", fc.err)
	}
	idx1, _ := got1.indexes()
	gotAgain := fc.load("brain-a", "gen-1")
	idxAgain, _ := gotAgain.indexes()
	if gotAgain != got1 || idxAgain != idx1 {
		t.Fatal("same file generation inside TTL must reuse catalog and index")
	}

	two := &OfflineEntityCatalog{BrainID: "brain-a", Generation: "gen-2",
		Names: map[string]string{"zephyr": "Zephyr"}}
	if err := WriteOfflineEntityCatalog(path, two); err != nil {
		t.Fatal(err)
	}
	got2 := fc.load("brain-a", "gen-2")
	if got2 == nil || got2 == got1 || got2.Names["zephyr"] != "Zephyr" {
		t.Fatalf("generation change must immediately observe atomic replacement: got=%v err=%s", got2, fc.err)
	}
	if wrongGeneration := fc.load("brain-a", "gen-1"); wrongGeneration != nil {
		t.Fatal("stale serving generation must reject newer catalog")
	}
	if wrongBrain := fc.load("brain-b", "gen-2"); wrongBrain != nil {
		t.Fatal("cross-brain catalog must fail closed")
	}
	now = now.Add(offlineEntityFileCheckTTL)
	if afterTTL := fc.load("brain-a", "gen-2"); afterTTL != got2 {
		t.Fatal("unchanged file after TTL identity check must reuse catalog/index")
	}
}

func TestOfflineCatalogFreshHitSkipsDiscoveryAndConfigChangeRechecks(t *testing.T) {
	dirOne := t.TempDir()
	pathOne := filepath.Join(dirOne, "entity-catalog.json")
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", "")
	t.Setenv("OUROBOROS_ERB_HOTLEX_PATH", filepath.Join(dirOne, "path2.gob"))
	if err := WriteOfflineEntityCatalog(pathOne, &OfflineEntityCatalog{
		BrainID: "brain-a", Generation: "gen-1",
		Names: map[string]string{"orion": "Orion"},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	resolveCalls, statCalls := 0, 0
	fc := &offlineEntityFileCache{
		now: func() time.Time { return now },
		resolve: func(config entityCatalogPathConfig) string {
			resolveCalls++
			return resolveEntityCatalogPath(config)
		},
		stat: func(path string) (os.FileInfo, error) {
			statCalls++
			return os.Stat(path)
		},
	}
	first := fc.load("brain-a", "gen-1")
	if first == nil || first.Names["orion"] != "Orion" {
		t.Fatalf("initial discovery failed: got=%v err=%q", first, fc.err)
	}
	if got := fc.load("brain-a", "gen-1"); got != first {
		t.Fatal("fresh scoped load did not reuse the decoded catalog")
	}
	if resolveCalls != 1 || statCalls != 1 {
		t.Fatalf("fresh hit repeated discovery/stat: resolve=%d stat=%d", resolveCalls, statCalls)
	}

	// The TTL must not hide a configuration change, even when the newly
	// selected file has the same brain and generation.
	dirTwo := t.TempDir()
	pathTwo := filepath.Join(dirTwo, "entity-catalog.json")
	if err := WriteOfflineEntityCatalog(pathTwo, &OfflineEntityCatalog{
		BrainID: "brain-a", Generation: "gen-1",
		Names: map[string]string{"zephyr": "Zephyr"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUROBOROS_ERB_HOTLEX_PATH", filepath.Join(dirTwo, "path2.gob"))
	second := fc.load("brain-a", "gen-1")
	if second == nil || second == first || second.Names["zephyr"] != "Zephyr" {
		t.Fatalf("configuration change used stale resolved path: got=%v err=%q", second, fc.err)
	}
	if resolveCalls != 2 || statCalls != 2 {
		t.Fatalf("configuration change did not recheck discovery/stat: resolve=%d stat=%d", resolveCalls, statCalls)
	}
}

func TestWriteOfflineEntityCatalogSyncsDirectoryAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entity-catalog.json")
	cat := &OfflineEntityCatalog{BrainID: "brain-a", Names: map[string]string{"orion": "Orion"}}
	synced := false
	err := writeOfflineEntityCatalog(path, cat, func(gotDir string) error {
		if gotDir != dir {
			t.Fatalf("sync directory = %q, want %q", gotDir, dir)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("directory sync ran before rename published target: %v", err)
		}
		synced = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Fatal("successful atomic write did not sync containing directory")
	}

	durabilityErr := errors.New("injected directory sync failure")
	failedPath := filepath.Join(dir, "entity-catalog-failed.json")
	err = writeOfflineEntityCatalog(failedPath, cat, func(string) error { return durabilityErr })
	if !errors.Is(err, durabilityErr) {
		t.Fatalf("directory sync failure = %v, want %v", err, durabilityErr)
	}
	if _, statErr := os.Stat(failedPath); statErr != nil {
		t.Fatalf("sync failure should occur after rename, stat=%v", statErr)
	}
}

func TestOfflineCatalogDecodeFailureRespectsCheckTTL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entity-catalog.json")
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", path)
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	fc := &offlineEntityFileCache{now: func() time.Time { return now }}
	if got := fc.load("brain-a", "gen-1"); got != nil || fc.err == "" {
		t.Fatalf("malformed catalog must fail soft with cached error: got=%v err=%q", got, fc.err)
	}
	valid := &OfflineEntityCatalog{BrainID: "brain-a", Generation: "gen-1",
		Names: map[string]string{"orion": "Orion"}}
	if err := WriteOfflineEntityCatalog(path, valid); err != nil {
		t.Fatal(err)
	}
	if got := fc.load("brain-a", "gen-1"); got != nil {
		t.Fatal("decode failure must remain cached until the file-check TTL")
	}
	now = now.Add(offlineEntityFileCheckTTL)
	if got := fc.load("brain-a", "gen-1"); got == nil || got.Names["orion"] != "Orion" {
		t.Fatalf("valid atomic replacement was not decoded after TTL: got=%v err=%q", got, fc.err)
	}
}

func TestOfflineCatalogNegativeCacheRechecksGenerationAndPathConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entity-catalog.json")
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", path)
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	fc := &offlineEntityFileCache{now: func() time.Time { return now }}
	if got := fc.load("brain-a", "gen-1"); got != nil || fc.err == "" {
		t.Fatalf("malformed catalog must populate the negative cache: got=%v err=%q", got, fc.err)
	}
	if err := WriteOfflineEntityCatalog(path, &OfflineEntityCatalog{
		BrainID: "brain-a", Generation: "gen-2",
		Names: map[string]string{"zephyr": "Zephyr"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := fc.load("brain-a", "gen-2"); got == nil || got.Names["zephyr"] != "Zephyr" {
		t.Fatalf("generation change did not bypass negative-cache TTL: got=%v err=%q", got, fc.err)
	}

	missing := filepath.Join(t.TempDir(), "missing.json")
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", missing)
	if got := fc.load("brain-a", "gen-3"); got != nil {
		t.Fatalf("missing configured catalog unexpectedly loaded: %v", got)
	}
	configured := filepath.Join(t.TempDir(), "entity-catalog.json")
	if err := WriteOfflineEntityCatalog(configured, &OfflineEntityCatalog{
		BrainID: "brain-a", Generation: "gen-3",
		Names: map[string]string{"orion": "Orion"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", configured)
	if got := fc.load("brain-a", "gen-3"); got == nil || got.Names["orion"] != "Orion" {
		t.Fatalf("path config change did not bypass negative-cache TTL: got=%v err=%q", got, fc.err)
	}
}

func TestRecoveryQueriesUseScopedCatalogOnLivePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entity-catalog.gob")
	t.Setenv("OUROBOROS_ERB_ENTITY_GOB", path)
	cat := &OfflineEntityCatalog{
		BrainID: "brain-a", Generation: "gen-live",
		Names:       map[string]string{"zeph": "Project Zephyr"},
		NameToDSIDs: map[string][]string{"zeph": {"doc-zephyr"}},
	}
	if err := WriteOfflineEntityCatalog(path, cat); err != nil {
		t.Fatal(err)
	}
	oldCache := offlineEntityCache
	offlineEntityCache = &offlineEntityFileCache{}
	defer func() { offlineEntityCache = oldCache }()
	hot := NewHotLex("brain-a")
	hot.Generation = "gen-live"
	hot.AddChunk("chunk-zephyr", "doc-hot-zephyr", "Project Zephyr operational notes", "")
	c := &Client{cfg: Config{BrainID: "brain-a", RRFK: 60}, hot: hot}
	if names, dsids := c.WarmEntityCatalog(); names != 1 || dsids != 1 {
		t.Fatalf("warm scoped index sizes = %d/%d, want 1/1", names, dsids)
	}
	identifierHeavy := `What changed for "ZEPH" under INC-1001 INC-1002 INC-1003 INC-1004 INC-1005 INC-1006 INC-1007?`
	queries := c.recoveryQueriesForClient(context.Background(), identifierHeavy, nil, 6)
	joined := strings.ToLower(strings.Join(queries, "|"))
	if !strings.Contains(joined, "project zephyr") {
		t.Fatalf("identifier extraction consumed the reserved entity slot: %v", queries)
	}
	if !strings.Contains(joined, "inc-1001") {
		t.Fatalf("rare identifier was truncated from live recovery queries: %v", queries)
	}
	bags := c.corpusGrepBags(context.Background(), identifierHeavy, nil, 14)
	if !strings.Contains(strings.ToLower(strings.Join(bags, "|")), "project zephyr") {
		t.Fatalf("live corpus-grep lost its reserved scoped catalog term: %v", bags)
	}
	grepHits := c.corpusGrepFallback(context.Background(), identifierHeavy, ProdProfile{}, nil,
		retrievalFTSState{hotLexAvailable: true, ftsDisabled: true})
	if len(grepHits) == 0 || grepHits[0].DSID != "doc-hot-zephyr" {
		t.Fatalf("live corpus-grep did not search its scoped alias: %+v", grepHits)
	}
	indexDiag := c.entityCatalogIndexDiagnostics()
	namesDiag, ok := indexDiag["offline_names"].(map[string]any)
	if !ok || namesDiag["keys"] != 1 || namesDiag["logical_payload_bytes"] == uint64(0) {
		t.Fatalf("offline index stats were not wired into diagnostics: %#v", indexDiag)
	}
	hits := c.scopedOfflineEntityHits("What changed for ZEPH?", 4)
	if len(hits) != 1 || hits[0].DSID != "doc-zephyr" || hits[0].Text != "" ||
		hits[0].ChunkID != "doc-zephyr#entity" {
		t.Fatalf("live structure stub contract changed: %+v", hits)
	}
	otherHot := NewHotLex("brain-b")
	otherHot.Generation = "gen-live"
	otherHot.AddChunk("chunk-zephyr", "doc-hot-zephyr", "Project Zephyr operational notes", "")
	other := &Client{cfg: Config{BrainID: "brain-b", RRFK: 60}, hot: otherHot}
	if leaked := other.scopedOfflineEntityHits("What changed for ZEPH?", 4); leaked != nil {
		t.Fatalf("cross-brain recovery leaked document identities: %+v", leaked)
	}
	if otherBags := other.corpusGrepBags(context.Background(), "What changed for ZEPH?", nil, 14); strings.Contains(strings.ToLower(strings.Join(otherBags, "|")), "project zephyr") {
		t.Fatalf("live corpus-grep used a cross-brain catalog: %v", otherBags)
	}
	if leaked := other.corpusGrepFallback(context.Background(), "What changed for ZEPH?", ProdProfile{}, nil,
		retrievalFTSState{hotLexAvailable: true, ftsDisabled: true}); len(leaked) != 0 {
		t.Fatalf("live corpus-grep searched a cross-brain catalog alias: %+v", leaked)
	}
}

func TestOfflineEntityTermsGuardEmptyDisplayValues(t *testing.T) {
	// Catalogs stored with non-normalized keys or blank display values (e.g.
	// hand-written JSON): the index normalizes "Acme Corp" → "acme corp",
	// which then misses in Names, and "orion" carries a whitespace display.
	// Neither may emit an empty recovery term.
	cat := &OfflineEntityCatalog{Names: map[string]string{
		"Acme Corp": "Acme Corp", // non-normalized key: Names[<index key>] misses
		"orion":     "   ",       // blank display value
		"kvcache":   "KVCache",
	}}
	got := offlineEntityTermsFromCatalog(cat, "acme orion kvcache status?", 8)
	for _, term := range got {
		if strings.TrimSpace(term) == "" {
			t.Fatalf("empty term leaked: %q in %v", term, got)
		}
	}
	if !strings.Contains(strings.Join(got, "|"), "KVCache") {
		t.Fatalf("valid display must still be returned, got %v", got)
	}
}

func TestEntityNameIndexNilAndEmptySafe(t *testing.T) {
	var nilIx *entityNameIndex
	if got := nilIx.match("acme", 4, func(string) bool { return true }); got != nil {
		t.Fatalf("nil index must return nil, got %v", got)
	}
	empty := newEntityNameIndex(nil)
	if got := empty.match("acme", 4, func(string) bool { return true }); got != nil {
		t.Fatalf("empty index must return nil, got %v", got)
	}
	ix := newEntityNameIndex([]string{"acme corp"})
	if got := ix.match("  ", 4, func(string) bool { return true }); got != nil {
		t.Fatalf("blank seed must return nil, got %v", got)
	}
	if got := ix.match("acme", 0, func(string) bool { return true }); got != nil {
		t.Fatalf("non-positive max must return nil, got %v", got)
	}
}

func TestOfflineEntityTermsFromCatalogIndexed(t *testing.T) {
	cat := &OfflineEntityCatalog{
		BrainID: "b",
		Names: map[string]string{
			"acme corp":       "Acme Corp",
			"acme corp legal": "Acme Corp Legal",
			"postgresql docs": "PostgreSQL Docs",
			"orion":           "Orion",
		},
	}
	// Whole-token + prefix matches, display forms preserved.
	got := offlineEntityTermsFromCatalog(cat, "What is the Acme retention window?", 8)
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "Acme Corp") {
		t.Fatalf("want Acme Corp entity terms, got %v", got)
	}
	// Deterministic across calls (old map-iteration order was random).
	again := offlineEntityTermsFromCatalog(cat, "What is the Acme retention window?", 8)
	if strings.Join(again, "|") != joined {
		t.Fatalf("non-deterministic terms: %v vs %v", got, again)
	}
	// Prefix-of-token seed: "postgres" → "postgresql docs".
	got = offlineEntityTermsFromCatalog(cat, "Where are the postgres runbooks?", 8)
	if !strings.Contains(strings.Join(got, "|"), "PostgreSQL Docs") {
		t.Fatalf("want prefix token match, got %v", got)
	}
	// maxN cap respected.
	if got = offlineEntityTermsFromCatalog(cat, "acme corp orion postgres", 1); len(got) > 1 {
		t.Fatalf("maxN=1 violated: %v", got)
	}
	// Short seeds must not reverse-match (len(s) >= 5 guard preserved).
	cat2 := &OfflineEntityCatalog{Names: map[string]string{"abc": "ABC"}}
	if got = offlineEntityTermsFromCatalog(cat2, "abcd abcd abcd abcd", 8); len(got) != 0 {
		t.Fatalf("reverse containment guard violated: %v", got)
	}
	// Nil catalog fail-soft.
	if got = offlineEntityTermsFromCatalog(nil, "acme", 8); got != nil {
		t.Fatalf("nil catalog must return nil, got %v", got)
	}
}

func TestOfflineEntityDSIDsFromCatalogIndexed(t *testing.T) {
	cat := &OfflineEntityCatalog{
		BrainID: "b",
		Names:   map[string]string{"acme corp": "Acme Corp"},
		NameToDSIDs: map[string][]string{
			"acme corp":  {"dsid_a", "dsid_b"},
			"acme legal": {"dsid_b", "dsid_c"},
			"orion":      {"dsid_o"},
		},
	}
	got := offlineEntityDSIDsFromCatalog(cat, "What changed for Acme this month?", 8)
	set := map[string]struct{}{}
	for _, d := range got {
		if _, dup := set[d]; dup {
			t.Fatalf("duplicate dsid %q in %v", d, got)
		}
		set[d] = struct{}{}
	}
	for _, want := range []string{"dsid_a", "dsid_b", "dsid_c"} {
		if _, ok := set[want]; !ok {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	if _, ok := set["dsid_o"]; ok {
		t.Fatalf("unrelated entity dsid leaked: %v", got)
	}
	// Cap respected.
	if got = offlineEntityDSIDsFromCatalog(cat, "What changed for Acme this month?", 2); len(got) != 2 {
		t.Fatalf("maxN=2 violated: %v", got)
	}
	// Offline hit stubs still carry synthetic #entity chunk ids and empty text
	// (unresolved-stub semantics downstream depend on this shape).
	// Covered structurally: DSID→Hit mapping is unchanged by #303.
}

func TestEntityBrainCacheIndexLivePredicate(t *testing.T) {
	bc := &entityBrainCache{names: map[string]string{
		"acme corp": "Acme Corp",
		"kvcache":   "KVCache",
	}}
	livePred := func(s string) func(string) bool {
		return func(k string) bool {
			return strings.Contains(k, s) || strings.Contains(s, k)
		}
	}
	// Concurrent FIRST use: no single-threaded index() call happens before
	// this block, so the sync.Once initial build itself is exercised under
	// the race detector. Every goroutine must observe the same built index.
	var wg sync.WaitGroup
	indexes := make([]*entityNameIndex, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ix := bc.index()
			indexes[i] = ix
			_ = ix.match("acme", 4, livePred("acme"))
		}(i)
	}
	wg.Wait()
	for i, ix := range indexes {
		if ix == nil || ix != indexes[0] {
			t.Fatalf("goroutine %d observed a different index: %p vs %p", i, ix, indexes[0])
		}
	}
	// Live predicate has no length guard: short seed reverse containment allowed.
	got := bc.index().match("kvcache-refcount", 4, livePred("kvcache-refcount"))
	if len(got) != 1 || got[0] != "kvcache" {
		t.Fatalf("live reverse containment failed: %v", got)
	}
	if got := bc.index().match("acme", 4, livePred("acme")); len(got) != 1 || got[0] != "acme corp" {
		t.Fatalf("cache index match failed: %v", got)
	}
	// Nil cache fail-soft.
	var nilBC *entityBrainCache
	if nilBC.index() != nil {
		t.Fatal("nil cache must yield nil index")
	}
}

func TestOfflineEntityCatalogIndexesLazyAndStable(t *testing.T) {
	cat := &OfflineEntityCatalog{
		Names:       map[string]string{"acme corp": "Acme Corp"},
		NameToDSIDs: map[string][]string{"acme corp": {"dsid_a"}},
	}
	// Concurrent FIRST use exercises the sync.Once initial build under the
	// race detector (no single-threaded indexes() call happens before this).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, d := cat.indexes()
			_ = n.match("acme", 4, offlinePred("acme"))
			_ = d.match("acme", 4, offlinePred("acme"))
		}()
	}
	wg.Wait()
	n1, d1 := cat.indexes()
	n2, d2 := cat.indexes()
	if n1 != n2 || d1 != d2 {
		t.Fatal("indexes must be built once and reused")
	}
	if n1.size() != 1 || d1.size() != 1 {
		t.Fatalf("unexpected index sizes: %d %d", n1.size(), d1.size())
	}
	var nilCat *OfflineEntityCatalog
	n, d := nilCat.indexes()
	if n != nil || d != nil {
		t.Fatal("nil catalog must yield nil indexes")
	}
}

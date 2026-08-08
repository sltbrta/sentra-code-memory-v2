package hosted

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func snapshotFixture() *HotLex {
	h := NewHotLex("brain-a")
	h.Generation = "gen-42"
	// Deliberately add in non-lexicographic order: publication canonicalizes the
	// table, while Search keeps score/chunk-id ranking semantics.
	h.AddChunk("chunk-z", "doc-z", "alpha recovery recovery policy", "file:///z.md")
	h.AddChunk("chunk-a", "doc-a", "alpha recovery policy", "file:///a.md")
	h.AddChunk("chunk-b", "doc-b", "unrelated picnic weather", "file:///b.md")
	return h
}

func TestHotLexSnapshotMMapRoundTripIsDeterministicAndZeroDecode(t *testing.T) {
	h := snapshotFixture()
	want := h.Search("alpha recovery", 10)
	dir := t.TempDir()
	one := filepath.Join(dir, "one.gob")
	two := filepath.Join(dir, "two.gob")
	if err := h.SaveGob(one); err != nil {
		t.Fatal(err)
	}
	if err := h.SaveGob(two); err != nil {
		t.Fatal(err)
	}
	oneBytes, err := os.ReadFile(one)
	if err != nil {
		t.Fatal(err)
	}
	twoBytes, err := os.ReadFile(two)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oneBytes, twoBytes) {
		t.Fatal("canonical publication is not byte deterministic")
	}
	if got := string(oneBytes[:8]); got != hotLexMagic {
		t.Fatalf("magic=%q want=%q", got, hotLexMagic)
	}

	loaded, err := LoadHotLexSnapshot(one, HotLexSnapshotScope{BrainID: "brain-a", Generation: "gen-42"})
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if loaded.mapped == nil || loaded.docs != nil || loaded.postings != nil || loaded.byChunk != nil {
		t.Fatalf("load decoded corpus state: mapped=%v docs=%d postings=%d by_chunk=%d",
			loaded.mapped != nil, len(loaded.docs), len(loaded.postings), len(loaded.byChunk))
	}
	got := loaded.Search("alpha recovery", 10)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapped ranking changed\n got=%#v\nwant=%#v", got, want)
	}
	if len(got) < 2 || got[0].DSID == "" || got[0].SourceURI == "" || got[0].Text == "" || got[0].Channel != "hot_lex" {
		t.Fatalf("citation/hydration identity not preserved: %#v", got)
	}
}

func TestHotLexSnapshotScopeCorruptionAndCloseFailClosed(t *testing.T) {
	h := snapshotFixture()
	path := filepath.Join(t.TempDir(), "hotlex.gob")
	if err := h.SaveGob(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: "brain-b"}); !errors.Is(err, ErrHotLexScope) {
		t.Fatalf("wrong brain err=%v", err)
	}
	if _, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: "brain-a", Generation: "gen-41"}); !errors.Is(err, ErrHotLexStale) {
		t.Fatalf("stale generation err=%v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	corrupt := filepath.Join(filepath.Dir(path), "corrupt.gob")
	if err := os.WriteFile(corrupt, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHotLexSnapshot(corrupt, HotLexSnapshotScope{BrainID: "brain-a"}); !errors.Is(err, ErrHotLexCorrupt) {
		t.Fatalf("corrupt err=%v", err)
	}
	truncated := filepath.Join(filepath.Dir(path), "truncated.gob")
	if err := os.WriteFile(truncated, raw[:len(raw)-17], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHotLexSnapshot(truncated, HotLexSnapshotScope{}); !errors.Is(err, ErrHotLexCorrupt) {
		t.Fatalf("truncated err=%v", err)
	}
	boundedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(boundedRaw[32:40], hotLexMaxDocs+1)
	clear(boundedRaw[hotLexDigestOffset : hotLexDigestOffset+hotLexDigestSize])
	digest := sha256.Sum256(boundedRaw)
	copy(boundedRaw[hotLexDigestOffset:hotLexDigestOffset+hotLexDigestSize], digest[:])
	oversizedCount := filepath.Join(filepath.Dir(path), "oversized-count.gob")
	if err := os.WriteFile(oversizedCount, boundedRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHotLexSnapshot(oversizedCount, HotLexSnapshotScope{}); !errors.Is(err, ErrHotLexCorrupt) {
		t.Fatalf("valid-checksum oversized count err=%v", err)
	}

	loaded, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: "brain-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Close(); err != nil {
		t.Fatal(err)
	}
	if got := loaded.Search("alpha", 3); len(got) != 0 || loaded.Len() != 0 {
		t.Fatalf("closed mapping remained queryable: hits=%v len=%d", got, loaded.Len())
	}
}

func TestHotLexSnapshotAtomicPublicationKeepsLastGoodImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hotlex.gob")
	good := snapshotFixture()
	if err := good.SaveGob(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := NewHotLex("brain-a")
	bad.AddChunk("chunk", "doc", "valid body", "")
	bad.postings[""] = []hotPosting{{Doc: 0, TF: 1}}
	if err := bad.SaveGob(path); err == nil {
		t.Fatal("invalid candidate unexpectedly published")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed publication replaced the last good snapshot")
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".hotlex.gob.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("publication left temp files: %v", temps)
	}
}

func TestHotLexLegacyGobRecoveryAndRepublish(t *testing.T) {
	h := snapshotFixture()
	terms := make([]hotTermSnap, 0, len(h.postings))
	for term, postings := range h.postings {
		terms = append(terms, hotTermSnap{Term: term, Postings: postings})
	}
	legacy := hotLexSnap{
		BrainID: h.BrainID, Generation: h.Generation, N: h.N, AvgDL: h.AvgDL,
		SumLen: h.sumLen, Docs: h.docs, Terms: terms,
	}
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "legacy.gob")
	f, err := os.Create(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(f).Encode(&legacy); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHotLexSnapshot(legacyPath, HotLexSnapshotScope{}); !errors.Is(err, ErrHotLexLegacy) {
		t.Fatalf("legacy without recovery opt-in err=%v", err)
	}
	recovered, err := LoadHotLexSnapshot(legacyPath, HotLexSnapshotScope{
		BrainID: "brain-a", Generation: "gen-42", AllowLegacyGob: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.SnapshotFormat(); got != HotLexFormatLegacyGob {
		t.Fatalf("legacy format diagnostic=%q", got)
	}
	if hits := recovered.Search("alpha recovery", 3); len(hits) == 0 || hits[0].DSID == "" {
		t.Fatalf("legacy recovery hits=%v", hits)
	}
	currentPath := filepath.Join(dir, "current.gob")
	if err := recovered.SaveGob(currentPath); err != nil {
		t.Fatal(err)
	}
	magic := make([]byte, 8)
	current, err := os.Open(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Read(magic); err != nil {
		_ = current.Close()
		t.Fatal(err)
	}
	_ = current.Close()
	if string(magic) != hotLexMagic {
		t.Fatalf("recovery republished magic=%q", magic)
	}
	currentLoaded, err := LoadHotLexSnapshot(currentPath, HotLexSnapshotScope{BrainID: "brain-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer currentLoaded.Close()
	if got := currentLoaded.SnapshotFormat(); got != HotLexFormatHOTLEX2 {
		t.Fatalf("current format diagnostic=%q", got)
	}
}

func TestHotLexMigrationPreservesGobOnlyRollbackAndExplicitDualWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hotlex.gob")
	h := snapshotFixture()
	if err := h.SaveLegacyGob(path); err != nil {
		t.Fatal(err)
	}
	legacyBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SaveGob(path); err != nil {
		t.Fatal(err)
	}
	rollbackPath := LegacyRollbackPath(path)
	rollbackBytes, err := os.ReadFile(rollbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rollbackBytes, legacyBefore) {
		t.Fatal("migration did not preserve the exact legacy gob")
	}
	var gobOnly hotLexSnap
	if err := gob.NewDecoder(bytes.NewReader(rollbackBytes)).Decode(&gobOnly); err != nil {
		t.Fatalf("gob-only rollback binary cannot decode preserved image: %v", err)
	}
	if gobOnly.BrainID != h.BrainID || gobOnly.Generation != h.Generation || len(gobOnly.Docs) != h.Len() {
		t.Fatalf("rollback gob scope/content diagnostic=%+v", gobOnly)
	}
	current, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: h.BrainID, Generation: h.Generation})
	if err != nil {
		t.Fatal(err)
	}
	if got := current.SnapshotFormat(); got != HotLexFormatHOTLEX2 {
		t.Fatalf("migrated format diagnostic=%q", got)
	}
	_ = current.Close()
	// Exercise the documented same-filesystem rollback: stop the current reader,
	// atomically put the preserved gob back at the old binary's exact path, and
	// decode it with the legacy wire shape (without the current loader).
	if err := os.Rename(rollbackPath, path); err != nil {
		t.Fatal(err)
	}
	rolledBackBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rolledBack hotLexSnap
	if err := gob.NewDecoder(bytes.NewReader(rolledBackBytes)).Decode(&rolledBack); err != nil {
		t.Fatalf("gob-only binary cannot decode restored serving path: %v", err)
	}
	if rolledBack.BrainID != h.BrainID || rolledBack.Generation != h.Generation {
		t.Fatalf("restored rollback scope=%q/%q", rolledBack.BrainID, rolledBack.Generation)
	}

	fresh := filepath.Join(dir, "fresh.hlex")
	explicitRollback := filepath.Join(dir, "fresh.rollback.gob")
	if err := h.SaveGobWithRollback(fresh, explicitRollback); err != nil {
		t.Fatal(err)
	}
	rollback, err := LoadHotLexSnapshot(explicitRollback, HotLexSnapshotScope{
		BrainID: h.BrainID, Generation: h.Generation, AllowLegacyGob: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback.Close()
	if got := rollback.SnapshotFormat(); got != HotLexFormatLegacyGob {
		t.Fatalf("explicit rollback format diagnostic=%q", got)
	}
}

func TestHotLexShardMergeRejectsScopeAndGenerationMix(t *testing.T) {
	a := NewHotLex("brain#s0")
	a.Generation = "gen-1"
	a.AddChunkBulk("a", "da", "alpha term", "", false)
	b := NewHotLex("other#s1")
	b.Generation = "gen-1"
	b.AddChunkBulk("b", "db", "beta term", "", false)
	if _, err := MergeHotLexShards("brain", []*HotLex{a, b}); !errors.Is(err, ErrHotLexScope) {
		t.Fatalf("scope merge err=%v", err)
	}
	b.BrainID = "brain#s1"
	b.Generation = "gen-2"
	if _, err := MergeHotLexShards("brain", []*HotLex{a, b}); !errors.Is(err, ErrHotLexStale) {
		t.Fatalf("generation merge err=%v", err)
	}
	if got := MergeShards("brain", []*HotLex{a, b}); got == nil || got.Len() != 0 {
		t.Fatalf("compat merge did not fail closed: %#v", got)
	}
}

func TestWithBrainIDDropsCrossBrainHotLex(t *testing.T) {
	c := &Client{cfg: Config{BrainID: "brain-a"}, hot: snapshotFixture()}
	other := c.WithBrainID("brain-b")
	if other == nil || other.cfg.BrainID != "brain-b" || other.hot != nil {
		t.Fatalf("cross-brain client retained HotLex: %#v", other)
	}
	same := c.WithBrainID("brain-a")
	if same == nil || same.hot != c.hot {
		t.Fatal("same-brain client unexpectedly dropped HotLex")
	}
}

func TestOpenLocalRejectsStaleSnapshotAndRebuildsCurrentGeneration(t *testing.T) {
	dir := t.TempDir()
	c, err := CreateLocal(dir, "brain-local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.BurstIngestLocal(t.Context(), []LocalDocument{{
		ID: "current-doc", Text: "current recovery objective",
	}}, 1); err != nil {
		t.Fatal(err)
	}
	wantGeneration := c.GenerationID()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	stale := NewHotLex("brain-local")
	stale.Generation = "stale-generation"
	stale.AddChunk("stale-chunk", "stale-doc", "malicious stale marker", "")
	if err := stale.SaveGob(filepath.Join(dir, "hotlex.gob")); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLocal(dir, "brain-local")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.HotLex() == nil || reopened.HotLex().Generation != wantGeneration {
		t.Fatalf("generation=%q want=%q", reopened.HotLex().Generation, wantGeneration)
	}
	if hits := reopened.HotLex().Search("malicious stale marker", 3); len(hits) != 0 {
		t.Fatalf("stale snapshot became queryable: %v", hits)
	}
	if hits := reopened.HotLex().Search("current recovery", 3); len(hits) == 0 || hits[0].DSID != "current-doc" {
		t.Fatalf("current rebuild missing: %v", hits)
	}
}

func TestOpenLocalMappedSnapshotRebuildsOnWrite(t *testing.T) {
	dir := t.TempDir()
	c, err := CreateLocal(dir, "brain-write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.BurstIngestLocal(t.Context(), []LocalDocument{{ID: "one", Text: "first marker"}}, 1); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	reopened, err := OpenLocal(dir, "brain-write")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.hot == nil || reopened.hot.mapped == nil {
		t.Fatal("reopen did not use the mapped current snapshot")
	}
	if _, err := reopened.BurstIngestLocal(t.Context(), []LocalDocument{{ID: "two", Text: "second marker"}}, 1); err != nil {
		t.Fatal(err)
	}
	if reopened.hot == nil || reopened.hot.mapped != nil {
		t.Fatal("write did not rebuild mapped image from authoritative chunks")
	}
	if hits := reopened.hot.Search("second marker", 3); len(hits) == 0 || hits[0].DSID != "two" {
		t.Fatalf("rebuilt index missing write: %v", hits)
	}
}

func TestHotLexMMapFixedMemoryLatencyEvidence(t *testing.T) {
	requireHotLexMMap(t)
	if testing.Short() {
		t.Skip("fixed 20k-document evidence fixture")
	}
	const docs = 20_000
	h := NewHotLex("evidence-brain")
	h.Generation = "evidence-gen"
	for i := 0; i < docs; i++ {
		h.AddChunkBulk(
			fmt.Sprintf("chunk-%05d", i), fmt.Sprintf("doc-%05d", i),
			fmt.Sprintf("shared recovery policy unique_%05d", i), "", false,
		)
	}
	h.Finalize()
	path := filepath.Join(t.TempDir(), "fixed-20k.gob")
	if err := h.SaveGob(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	corruptPath := filepath.Join(filepath.Dir(path), "fixed-20k-corrupt.gob")
	if err := os.WriteFile(corruptPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	helperReceipt := filepath.Join(filepath.Dir(path), "helper-receipt.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHotLexMMapEvidenceSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"OUROBOROS_HOTLEX_EVIDENCE_HELPER=1",
		"OUROBOROS_HOTLEX_EVIDENCE_PATH="+path,
		"OUROBOROS_HOTLEX_EVIDENCE_CORRUPT_PATH="+corruptPath,
		"OUROBOROS_HOTLEX_EVIDENCE_RECEIPT="+helperReceipt,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated evidence helper: %v\n%s", err, output)
	}
	var isolated hotLexIsolatedEvidence
	if err := json.Unmarshal(readFileForTest(t, helperReceipt), &isolated); err != nil {
		t.Fatal(err)
	}
	if isolated.PeakRSSDeltaBytes > 64<<20 {
		t.Fatalf("isolated peak RSS delta=%d; fixed ceiling=%d", isolated.PeakRSSDeltaBytes, 64<<20)
	}
	if isolated.CorruptFailureMS > 2000 || isolated.CorruptFailureClass != "ErrHotLexCorrupt" {
		t.Fatalf("cold-failure diagnostic=%+v", isolated)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	loaded, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{
		BrainID: "evidence-brain", Generation: "evidence-gen",
	})
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	heapBytes := after.TotalAlloc - before.TotalAlloc
	if loaded.mapped == nil || len(loaded.docs) != 0 || len(loaded.postings) != 0 {
		t.Fatal("fixed fixture decoded corpus tables")
	}
	if heapBytes > 512<<10 {
		t.Fatalf("load allocated %d heap bytes; fixed ceiling=%d", heapBytes, 512<<10)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("load latency=%s; fixed ceiling=2s", elapsed)
	}
	lookupStarted := time.Now()
	hits := loaded.Search("unique_19999", 3)
	lookupElapsed := time.Since(lookupStarted)
	if len(hits) == 0 || hits[0].ChunkID != "chunk-19999" {
		t.Fatalf("fixed fixture lookup=%v", hits)
	}
	if lookupElapsed > 50*time.Millisecond {
		t.Fatalf("exact-term lookup latency=%s; fixed ceiling=50ms", lookupElapsed)
	}
	t.Logf("hotlex_mmap_evidence docs=%d file_bytes=%d load_ms=%d heap_alloc_bytes=%d lookup_us=%d peak_rss_before_bytes=%d peak_rss_after_bytes=%d peak_rss_delta_bytes=%d corrupt_failure_ms=%d corrupt_failure_class=%s ceiling_ms=2000 ceiling_heap_bytes=%d ceiling_rss_delta_bytes=%d ceiling_lookup_ms=50",
		docs, st.Size(), elapsed.Milliseconds(), heapBytes, lookupElapsed.Microseconds(),
		isolated.PeakRSSBeforeBytes, isolated.PeakRSSAfterBytes, isolated.PeakRSSDeltaBytes,
		isolated.CorruptFailureMS, isolated.CorruptFailureClass, 512<<10, 64<<20)
	runtime.KeepAlive(loaded)
}

type hotLexIsolatedEvidence struct {
	LoadMS               int64  `json:"load_ms"`
	LoadHeapAllocBytes   uint64 `json:"load_heap_alloc_bytes"`
	PeakRSSBeforeBytes   uint64 `json:"peak_rss_before_bytes"`
	PeakRSSAfterBytes    uint64 `json:"peak_rss_after_bytes"`
	PeakRSSDeltaBytes    uint64 `json:"peak_rss_delta_bytes"`
	CorruptFailureMS     int64  `json:"corrupt_failure_ms"`
	CorruptFailureClass  string `json:"corrupt_failure_class"`
	DecodedCorpusTables  bool   `json:"decoded_corpus_tables"`
	IntegrityScanTouches string `json:"integrity_scan_touches"`
}

func TestHotLexMMapEvidenceSubprocess(t *testing.T) {
	requireHotLexMMap(t)
	if os.Getenv("OUROBOROS_HOTLEX_EVIDENCE_HELPER") != "1" {
		return
	}
	path := os.Getenv("OUROBOROS_HOTLEX_EVIDENCE_PATH")
	corruptPath := os.Getenv("OUROBOROS_HOTLEX_EVIDENCE_CORRUPT_PATH")
	receiptPath := os.Getenv("OUROBOROS_HOTLEX_EVIDENCE_RECEIPT")
	runtime.GC()
	beforeRSS := peakRSSBytes(t)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	loaded, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{
		BrainID: "evidence-brain", Generation: "evidence-gen",
	})
	loadElapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	afterRSS := peakRSSBytes(t)
	decoded := loaded.mapped == nil || len(loaded.docs) != 0 || len(loaded.postings) != 0 || len(loaded.byChunk) != 0
	_ = loaded.Close()
	failureStarted := time.Now()
	_, failureErr := LoadHotLexSnapshot(corruptPath, HotLexSnapshotScope{
		BrainID: "evidence-brain", Generation: "evidence-gen",
	})
	failureElapsed := time.Since(failureStarted)
	failureClass := fmt.Sprintf("%T", failureErr)
	if errors.Is(failureErr, ErrHotLexCorrupt) {
		failureClass = "ErrHotLexCorrupt"
	}
	delta := uint64(0)
	if afterRSS > beforeRSS {
		delta = afterRSS - beforeRSS
	}
	receipt := hotLexIsolatedEvidence{
		LoadMS: loadElapsed.Milliseconds(), LoadHeapAllocBytes: after.TotalAlloc - before.TotalAlloc,
		PeakRSSBeforeBytes: beforeRSS, PeakRSSAfterBytes: afterRSS, PeakRSSDeltaBytes: delta,
		CorruptFailureMS: failureElapsed.Milliseconds(), CorruptFailureClass: failureClass,
		DecodedCorpusTables: decoded, IntegrityScanTouches: "all mapped bytes before admission",
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFileForTest(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func BenchmarkHotLexMMapLookup(b *testing.B) {
	h := NewHotLex("bench")
	for i := 0; i < 10_000; i++ {
		h.AddChunkBulk(fmt.Sprintf("chunk-%05d", i), fmt.Sprintf("doc-%05d", i),
			fmt.Sprintf("shared policy token_%05d", i), "", false)
	}
	h.Finalize()
	path := filepath.Join(b.TempDir(), "bench.gob")
	if err := h.SaveGob(path); err != nil {
		b.Fatal(err)
	}
	loaded, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	defer loaded.Close()
	b.ReportAllocs()
	b.SetBytes(int64(len("token_09999")))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if hits := loaded.Search("token_09999", 8); len(hits) == 0 {
			b.Fatal("empty lookup")
		}
	}
}

func TestHotLexSnapshotHasNoEvaluationGoldSurface(t *testing.T) {
	h := snapshotFixture()
	path := filepath.Join(t.TempDir(), "hotlex.gob")
	if err := h.SaveGob(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"gold_document_ids", "reference_answer", "question_type", "benchmark_case"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("snapshot contains evaluation-only field %q", forbidden)
		}
	}
}

func TestHotLexMMapFixedEvidenceReceipt(t *testing.T) {
	if !qualityEvidenceAvailable("docs/stages/stage-09/evidence/hotlex-mmap-fixed.schema.json") ||
		!qualityEvidenceAvailable("docs/stages/stage-09/evidence/hotlex-mmap-fixed.json") {
		t.Skip("optional stage-09 evidence artifacts are not present in this standalone checkout")
	}
	var schema map[string]any
	decodeQualityEvidenceJSON(t, "docs/stages/stage-09/evidence/hotlex-mmap-fixed.schema.json", &schema, false)
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || schema["additionalProperties"] != false {
		t.Fatalf("receipt schema must be draft 2020-12 and closed: %#v", schema)
	}
	var receiptValue any
	if err := json.Unmarshal(readQualityEvidenceFile(t, "docs/stages/stage-09/evidence/hotlex-mmap-fixed.json"), &receiptValue); err != nil {
		t.Fatal(err)
	}
	if err := validateQualityJSONSchema(schema, receiptValue, "$"); err != nil {
		t.Fatalf("HotLex mmap receipt violates schema: %v", err)
	}
	var receipt struct {
		Issue       int `json:"issue"`
		Measurement struct {
			LoadMS              int    `json:"load_ms"`
			LoadHeapAllocBytes  int    `json:"load_heap_alloc_bytes"`
			LookupUS            int    `json:"lookup_us"`
			PeakRSSDeltaBytes   int    `json:"isolated_peak_rss_delta_bytes"`
			CorruptFailureMS    int    `json:"corrupt_failure_ms"`
			CorruptFailureClass string `json:"corrupt_failure_class"`
		} `json:"measurement"`
		Ceilings struct {
			LoadMS             int `json:"load_ms"`
			LoadHeapAllocBytes int `json:"load_heap_alloc_bytes"`
			LookupMS           int `json:"lookup_ms"`
			PeakRSSDeltaBytes  int `json:"isolated_peak_rss_delta_bytes"`
			CorruptFailureMS   int `json:"corrupt_failure_ms"`
		} `json:"ceilings"`
		ClaimDimensions struct {
			LazyLoading struct {
				DecodedCorpusTables        bool `json:"decoded_corpus_tables"`
				OSPageDemandLoadingClaimed bool `json:"os_page_demand_loading_claimed"`
			} `json:"lazy_loading"`
			Full500 struct {
				Measured bool   `json:"measured"`
				Scope    string `json:"scope"`
			} `json:"full500"`
		} `json:"claim_dimensions"`
		ClaimScope string `json:"claim_scope"`
	}
	decodeQualityEvidenceJSON(t, "docs/stages/stage-09/evidence/hotlex-mmap-fixed.json", &receipt, false)
	if receipt.Issue != 300 || receipt.Measurement.LoadMS > receipt.Ceilings.LoadMS ||
		receipt.Measurement.LoadHeapAllocBytes > receipt.Ceilings.LoadHeapAllocBytes ||
		receipt.Measurement.LookupUS > receipt.Ceilings.LookupMS*1000 ||
		receipt.Measurement.PeakRSSDeltaBytes > receipt.Ceilings.PeakRSSDeltaBytes ||
		receipt.Measurement.CorruptFailureMS > receipt.Ceilings.CorruptFailureMS ||
		receipt.Measurement.CorruptFailureClass != "ErrHotLexCorrupt" {
		t.Fatalf("receipt exceeds fixed ceilings: %+v", receipt)
	}
	if receipt.ClaimDimensions.Full500.Measured || receipt.ClaimDimensions.Full500.Scope != "not run" ||
		receipt.ClaimDimensions.LazyLoading.DecodedCorpusTables || receipt.ClaimDimensions.LazyLoading.OSPageDemandLoadingClaimed {
		t.Fatalf("receipt overclaims full500/lazy loading: %+v", receipt.ClaimDimensions)
	}
	if !strings.Contains(receipt.ClaimScope, "not a full-corpus") || !strings.Contains(receipt.ClaimScope, "full500") {
		t.Fatalf("receipt must retain its non-claim: %q", receipt.ClaimScope)
	}
}

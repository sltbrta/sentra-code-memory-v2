package hosted

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// referenceHotLexFullSort reproduces the pre-#289 in-memory algorithm
// (accumulate all matches, full SliceStable sort, truncate) so tests can
// assert that bounded top-k selection returns identical rankings.
func referenceHotLexFullSort(h *HotLex, query string, limit int) []Hit {
	if h == nil || limit <= 0 {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.N == 0 {
		return nil
	}
	qtoks := hotTokenize(query)
	if len(qtoks) == 0 {
		return nil
	}
	seenQ := map[string]struct{}{}
	var terms []string
	for _, t := range qtoks {
		if _, ok := seenQ[t]; ok {
			continue
		}
		seenQ[t] = struct{}{}
		terms = append(terms, t)
	}
	params := defaultBM25()
	scores := map[int]float64{}
	N := float64(h.N)
	avgdl := h.AvgDL
	if avgdl < 1 {
		avgdl = 1
	}
	for _, t := range terms {
		plist := h.postings[t]
		df := float64(len(plist))
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))
		for _, p := range plist {
			di := int(p.Doc)
			if di < 0 || di >= len(h.docs) || h.docs[di].ChunkID == "" {
				continue
			}
			tf := float64(p.TF)
			dl := float64(h.docs[di].Length)
			if dl < 1 {
				dl = 1
			}
			num := tf * (params.K1 + 1)
			den := tf + params.K1*(1-params.B+params.B*dl/avgdl)
			scores[di] += idf * num / den
		}
	}
	type scored struct {
		i int
		s float64
	}
	arr := make([]scored, 0, len(scores))
	for i, s := range scores {
		arr = append(arr, scored{i: i, s: s})
	}
	sort.SliceStable(arr, func(a, b int) bool {
		if arr[a].s != arr[b].s {
			return arr[a].s > arr[b].s
		}
		return h.docs[arr[a].i].ChunkID < h.docs[arr[b].i].ChunkID
	})
	if limit > len(arr) {
		limit = len(arr)
	}
	out := make([]Hit, 0, limit)
	for i := 0; i < limit; i++ {
		d := h.docs[arr[i].i]
		hit := Hit{
			ChunkID:   d.ChunkID,
			DSID:      d.DSID,
			SourceURI: d.SourceURI,
			Score:     arr[i].s,
			Channel:   "hot_lex",
		}
		if d.HasText {
			hit.Text = d.Text
		}
		out = append(out, hit)
	}
	return out
}

// referenceMappedFullSort reproduces the pre-#289 mapped algorithm (full
// sort.Slice over all matches) for snapshot-side comparison benchmarks.
func referenceMappedFullSort(h *HotLex, query string, limit int) []Hit {
	if h == nil || limit <= 0 {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	m := h.mapped
	if m == nil || h.N == 0 {
		return nil
	}
	qtoks := hotTokenize(query)
	if len(qtoks) == 0 {
		return nil
	}
	seenQ := map[string]struct{}{}
	terms := make([]string, 0, len(qtoks))
	for _, term := range qtoks {
		if _, seen := seenQ[term]; seen {
			continue
		}
		seenQ[term] = struct{}{}
		terms = append(terms, term)
	}
	params := defaultBM25()
	scores := map[int]float64{}
	N, avgDL := float64(h.N), h.AvgDL
	if avgDL < 1 {
		avgDL = 1
	}
	for _, term := range terms {
		start, count, ok := m.findTerm(term)
		if !ok {
			continue
		}
		df := float64(count)
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))
		for i := 0; i < count; i++ {
			p := m.postingRecord(start + i)
			doc := int(binary.LittleEndian.Uint32(p[0:4]))
			tf := float64(binary.LittleEndian.Uint32(p[4:8]))
			dl := float64(binary.LittleEndian.Uint32(m.docRecord(doc)[48:52]))
			if dl < 1 {
				dl = 1
			}
			scores[doc] += idf * (tf * (params.K1 + 1)) /
				(tf + params.K1*(1-params.B+params.B*dl/avgDL))
		}
	}
	type scored struct {
		i int
		s float64
	}
	arr := make([]scored, 0, len(scores))
	for i, score := range scores {
		arr = append(arr, scored{i: i, s: score})
	}
	sort.Slice(arr, func(a, b int) bool {
		if arr[a].s != arr[b].s {
			return arr[a].s > arr[b].s
		}
		return bytes.Compare(m.docChunkID(arr[a].i), m.docChunkID(arr[b].i)) < 0
	})
	if limit > len(arr) {
		limit = len(arr)
	}
	out := make([]Hit, 0, limit)
	for i := 0; i < limit; i++ {
		r := m.docRecord(arr[i].i)
		chunk, _ := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
		dsid, _ := m.stringBytes(binary.LittleEndian.Uint64(r[16:24]), binary.LittleEndian.Uint32(r[12:16]))
		uri, _ := m.stringBytes(binary.LittleEndian.Uint64(r[24:32]), binary.LittleEndian.Uint32(r[32:36]))
		hit := Hit{ChunkID: string(chunk), DSID: string(dsid), SourceURI: string(uri), Score: arr[i].s, Channel: "hot_lex"}
		if binary.LittleEndian.Uint32(r[52:56])&hotDocFlagHasText != 0 {
			body, _ := m.stringBytes(binary.LittleEndian.Uint64(r[40:48]), binary.LittleEndian.Uint32(r[36:40]))
			hit.Text = string(body)
		}
		out = append(out, hit)
	}
	return out
}

// buildTopKTestCorpus builds a deterministic corpus with a skewed term
// distribution, one very-high-DF plain term ("common"), one very-high-DF
// identifier term ("sig_77"), and rare identifier tokens (err_XXXX).
func buildTopKTestCorpus(tb testing.TB, docs, vocab int) *HotLex {
	tb.Helper()
	rng := rand.New(rand.NewSource(289))
	h := NewHotLex("topk-bench")
	for i := 0; i < docs; i++ {
		var b strings.Builder
		for j := 0; j < 24; j++ {
			fmt.Fprintf(&b, "t%04d ", int(float64(vocab)*math.Pow(rng.Float64(), 2)))
		}
		if rng.Float64() < 0.6 {
			b.WriteString("common ")
		}
		if rng.Float64() < 0.5 {
			b.WriteString("sig_77 ")
		}
		if i%17 == 0 {
			fmt.Fprintf(&b, "err_%04d ", i%97)
		}
		h.AddChunkBulk(fmt.Sprintf("c%06d", i), fmt.Sprintf("d%06d", i/3), b.String(), "", false)
	}
	h.Finalize()
	return h
}

func assertHitsEqual(tb testing.TB, want, got []Hit) {
	tb.Helper()
	if len(want) != len(got) {
		tb.Fatalf("hit count: want %d got %d\nwant=%v\ngot=%v", len(want), len(got), want, got)
	}
	for i := range want {
		if want[i].ChunkID != got[i].ChunkID || want[i].Score != got[i].Score ||
			want[i].DSID != got[i].DSID || want[i].Text != got[i].Text ||
			want[i].SourceURI != got[i].SourceURI || want[i].Channel != got[i].Channel {
			tb.Fatalf("rank %d: want {%s %.17g} got {%s %.17g}", i,
				want[i].ChunkID, want[i].Score, got[i].ChunkID, got[i].Score)
		}
	}
}

// TestHotLexTopKRankingParityAtScale compares bounded top-k selection against
// the historical full-sort algorithm at corpus scale across queries that
// match many documents (high-DF) and few.
func TestHotLexTopKRankingParityAtScale(t *testing.T) {
	h := buildTopKTestCorpus(t, 20_000, 2_000)
	queries := []string{
		"common",               // ~12k matched docs, far above any limit
		"common sig_77",        // two high-DF terms
		"common sig_77 t0007",  // high-DF plus a rarer term
		"t1999 t1888",          // low-DF: matched below the limit
		"err_0042",             // rare identifier
		"missingterm",          // no matches
		"common common sig_77", // duplicate query tokens dedupe
	}
	for _, q := range queries {
		for _, limit := range []int{1, 3, 8, 64, 500, 100_000} {
			want := referenceHotLexFullSort(h, q, limit)
			got := h.Search(q, limit)
			assertHitsEqual(t, want, got)
			// Zero options must be exactly Search.
			gotOpt, stats := h.SearchWithOptions(q, limit, HotLexSearchOptions{})
			assertHitsEqual(t, want, gotOpt)
			if len(stats.PrunedTerms) != 0 || len(stats.ProtectedTerms) != 0 {
				t.Fatalf("zero options must not prune: %+v", stats)
			}
			if stats.MatchedDocs != len(referenceHotLexFullSort(h, q, 1_000_000)) {
				t.Fatalf("MatchedDocs=%d for %q", stats.MatchedDocs, q)
			}
			if len(got) > limit {
				t.Fatalf("top-k bound violated: %d hits for limit %d", len(got), limit)
			}
		}
	}
}

// TestHotLexTopKDeterministicTies forces a large plate of identical scores
// and requires ascending chunk-id order identical to the full sort, stable
// across repeated calls.
func TestHotLexTopKDeterministicTies(t *testing.T) {
	h := NewHotLex("ties")
	for i := 0; i < 400; i++ {
		h.AddChunkBulk(fmt.Sprintf("chunk-%03d", i), "d", "alpha beta gamma delta", "", false)
	}
	h.Finalize()
	want := referenceHotLexFullSort(h, "alpha", 50)
	got := h.Search("alpha", 50)
	assertHitsEqual(t, want, got)
	if len(got) != 50 {
		t.Fatalf("hits=%d want 50", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score != got[i].Score {
			t.Fatalf("expected a flat tie plate, got %v vs %v", got[i-1].Score, got[i].Score)
		}
		if got[i-1].ChunkID >= got[i].ChunkID {
			t.Fatalf("ties must break by ascending chunk id: %q >= %q", got[i-1].ChunkID, got[i].ChunkID)
		}
	}
	if got[0].ChunkID != "chunk-000" {
		t.Fatalf("first tie hit=%q want chunk-000", got[0].ChunkID)
	}
	for run := 0; run < 20; run++ {
		assertHitsEqual(t, want, h.Search("alpha", 50))
	}
}

// TestHotLexHighDFPruningExplicitAndIdentifierSafe proves the high-DF cut is
// opt-in, observable, and never removes identifier-bearing evidence.
func TestHotLexHighDFPruningExplicitAndIdentifierSafe(t *testing.T) {
	h := NewHotLex("df")
	for i := 0; i < 60; i++ {
		text := "everywhere filler words here"
		if i < 45 {
			text += " err_429"
		}
		if i < 3 {
			text += " zebra"
		}
		h.AddChunkBulk(fmt.Sprintf("c%03d", i), "d", text, "", false)
	}
	h.Finalize()
	opts := HotLexSearchOptions{MaxDocumentFrequency: 50}

	// everywhere df=60 > 50 → pruned; err_429 df=45 ≤ 50 → scored.
	hits, stats := h.SearchWithOptions("everywhere err_429 zebra", 100, opts)
	if !reflect.DeepEqual(stats.PrunedTerms, []string{"everywhere"}) {
		t.Fatalf("PrunedTerms=%v", stats.PrunedTerms)
	}
	if len(stats.ProtectedTerms) != 0 {
		t.Fatalf("ProtectedTerms=%v", stats.ProtectedTerms)
	}
	if stats.MatchedDocs != 45 {
		t.Fatalf("MatchedDocs=%d want 45 (err_429 ∪ zebra docs)", stats.MatchedDocs)
	}
	for _, hit := range hits {
		n := 0
		fmt.Sscanf(hit.ChunkID, "c%03d", &n)
		if n >= 45 {
			t.Fatalf("doc %s only matched pruned term and must be absent", hit.ChunkID)
		}
	}

	// The identifier cut is the critical guard: err_503 df=55 > 50, but as an
	// identifier it is protected and its docs must still rank.
	h2 := NewHotLex("df2")
	for i := 0; i < 70; i++ {
		text := "commonnoise filler words"
		if i < 55 {
			text += " err_503"
		}
		h2.AddChunkBulk(fmt.Sprintf("c%03d", i), "d", text, "", false)
	}
	h2.Finalize()
	hits2, stats2 := h2.SearchWithOptions("commonnoise err_503", 100, opts)
	if !reflect.DeepEqual(stats2.PrunedTerms, []string{"commonnoise"}) {
		t.Fatalf("PrunedTerms=%v", stats2.PrunedTerms)
	}
	if !reflect.DeepEqual(stats2.ProtectedTerms, []string{"err_503"}) {
		t.Fatalf("ProtectedTerms=%v", stats2.ProtectedTerms)
	}
	if len(hits2) != 55 {
		t.Fatalf("identifier evidence lost: %d hits want 55", len(hits2))
	}

	// Disabled by default: zero options keep the high-DF term and match the
	// reference full sort exactly.
	all, stats3 := h2.SearchWithOptions("commonnoise err_503", 100, HotLexSearchOptions{})
	if stats3.MatchedDocs != 70 || len(stats3.PrunedTerms) != 0 {
		t.Fatalf("default must not prune: %+v", stats3)
	}
	assertHitsEqual(t, referenceHotLexFullSort(h2, "commonnoise err_503", 100), all)
}

func TestHotLexTopKUnicodeNumberIdentifierProtected(t *testing.T) {
	h := NewHotLex("unicode-id")
	for i := 0; i < 20; i++ {
		h.AddChunkBulk(fmt.Sprintf("u%02d", i), "d", "case٢", "uri://unicode", false)
	}
	for i := 20; i < 40; i++ {
		h.AddChunkBulk(fmt.Sprintf("u%02d", i), "d", "common", "uri://common", false)
	}
	h.Finalize()
	hits, stats := h.SearchWithOptions("case٢", 100, HotLexSearchOptions{MaxDocumentFrequency: 10})
	if !reflect.DeepEqual(stats.ProtectedTerms, []string{"case٢"}) {
		t.Fatalf("unicode identifier was not protected: %+v", stats)
	}
	if len(hits) != 20 {
		t.Fatalf("unicode identifier hits=%d want 20", len(hits))
	}
}

// TestHotLexTopKSnapshotParity requires the mmap serving path to return
// identical hits and identical pruning stats to the mutable path, including
// the historical full-sort ranking.
func TestHotLexTopKSnapshotParity(t *testing.T) {
	h := buildTopKTestCorpus(t, 5_000, 800)
	path := filepath.Join(t.TempDir(), "hotlex.gob")
	if err := h.SaveGob(path); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: "topk-bench"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	if snap.SnapshotFormat() != HotLexFormatHOTLEX2 {
		t.Fatalf("format=%s", snap.SnapshotFormat())
	}
	queries := []string{"common", "common sig_77 t0007", "t0777 t0123", "err_0042", "missingterm"}
	limits := []int{1, 8, 64, 100_000}
	optSets := []HotLexSearchOptions{{}, {MaxDocumentFrequency: 10}}
	for _, q := range queries {
		for _, limit := range limits {
			for _, opts := range optSets {
				memHits, memStats := h.SearchWithOptions(q, limit, opts)
				snapHits, snapStats := snap.SearchWithOptions(q, limit, opts)
				assertHitsEqual(t, memHits, snapHits)
				if !reflect.DeepEqual(memStats, snapStats) {
					t.Fatalf("stats mismatch q=%q limit=%d opts=%+v: mem=%+v snap=%+v",
						q, limit, opts, memStats, snapStats)
				}
				if opts.MaxDocumentFrequency == 0 {
					assertHitsEqual(t, referenceHotLexFullSort(h, q, limit), snapHits)
					assertHitsEqual(t, referenceMappedFullSort(snap, q, limit), snapHits)
				}
			}
		}
	}
}

func BenchmarkHotLexSearchHighDFTopK(b *testing.B) {
	h := buildTopKTestCorpus(b, 100_000, 4_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Search("common t0007 err_0003", 8)
	}
	b.StopTimer()
	_, stats := h.SearchWithOptions("common t0007 err_0003", 8, HotLexSearchOptions{})
	b.ReportMetric(float64(stats.MatchedDocs), "matched")
}

func BenchmarkHotLexSearchHighDFTopKLimit64(b *testing.B) {
	h := buildTopKTestCorpus(b, 100_000, 4_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Search("common t0007 err_0003", 64)
	}
}

func BenchmarkHotLexSearchHighDFReferenceSort(b *testing.B) {
	h := buildTopKTestCorpus(b, 100_000, 4_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		referenceHotLexFullSort(h, "common t0007 err_0003", 8)
	}
}

func BenchmarkHotLexSearchHighDFPruned(b *testing.B) {
	h := buildTopKTestCorpus(b, 100_000, 4_000)
	opts := HotLexSearchOptions{MaxDocumentFrequency: 1_000}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = h.SearchWithOptions("common t0007 err_0003", 8, opts)
	}
	b.StopTimer()
	_, stats := h.SearchWithOptions("common t0007 err_0003", 8, opts)
	b.ReportMetric(float64(stats.MatchedDocs), "matched")
	b.ReportMetric(float64(len(stats.PrunedTerms)), "pruned")
}

func BenchmarkHotLexSearchLowDFTopK(b *testing.B) {
	h := buildTopKTestCorpus(b, 100_000, 4_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Search("err_0003 t3999", 8)
	}
}

func BenchmarkHotLexSearchSnapshotHighDFTopK(b *testing.B) {
	h := buildTopKTestCorpus(b, 100_000, 4_000)
	path := filepath.Join(b.TempDir(), "hotlex.gob")
	if err := h.SaveGob(path); err != nil {
		b.Fatal(err)
	}
	snap, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: "topk-bench"})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap.Search("common t0007 err_0003", 8)
	}
	b.StopTimer()
	_, stats := snap.SearchWithOptions("common t0007 err_0003", 8, HotLexSearchOptions{})
	b.ReportMetric(float64(stats.MatchedDocs), "matched")
}

func BenchmarkHotLexSearchSnapshotHighDFReferenceSort(b *testing.B) {
	h := buildTopKTestCorpus(b, 100_000, 4_000)
	path := filepath.Join(b.TempDir(), "hotlex.gob")
	if err := h.SaveGob(path); err != nil {
		b.Fatal(err)
	}
	snap, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{BrainID: "topk-bench"})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		referenceMappedFullSort(snap, "common t0007 err_0003", 8)
	}
}

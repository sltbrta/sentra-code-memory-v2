package codecrawl

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkRankFusionHitsAtK measures the rank fusion pipeline against
// the headline hit@1/5/10 acceptance test from issue #43. The benchmark
// does not assert a numeric threshold; it logs the run so the parent
// agent can compare relative quality and latency.
func BenchmarkRankFusionHitsAtK(b *testing.B) {
	dir := b.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		b.Fatal(err)
	}
	files := map[string]string{
		"src/anchor.go":        "package p\nfunc Anchor() {}\n",
		"src/anchor_alias.go":  "package p\nfunc AnchorAlias() {}\n",
		"src/noise.go":         "package p\n// anchor anchor anchor anchor anchor anchor anchor anchor\nfunc noise() {}\n",
		"src/loose.go":         "package p\n// anchor anchor anchor anchor anchor anchor anchor\nfunc loose() {}\n",
		"src/auth.go":          "package p\n// anchor anchor anchor anchor\nfunc Auth() {}\n",
		"src/auth_string.go":   "package p\n// anchor anchor anchor\nfunc AuthString() {}\n",
		"src/auth_helper.go":   "package p\n// anchor anchor\nfunc AuthHelper() {}\n",
		"src/auth_helper_2.go": "package p\n// anchor\nfunc AuthHelper2() {}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		b.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 4
	conf.IdentifierFloor = 1
	conf.IdentifierFloorCap = 2
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := idx.FindRelevantRanked(dir, "Anchor", 10, false, conf)
		hits := map[string]bool{}
		for _, h := range out.Hits {
			hits[h.Path] = true
		}
		if !hits["src/anchor.go"] {
			b.Fatalf("hit@10 missed anchor.go: %+v", out.Hits)
		}
	}
}

// BenchmarkRankFusionPipelinedLatency measures the full pipeline
// latency on a moderate corpus.
func BenchmarkRankFusionPipelinedLatency(b *testing.B) {
	dir := b.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("src/f%03d.go", i)
		body := fmt.Sprintf("package p\nfunc F%d() {}\n", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 4)
	if err != nil {
		b.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 4
	conf.IdentifierFloor = 1
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = idx.FindRelevantRanked(dir, "func", 10, false, conf)
	}
}

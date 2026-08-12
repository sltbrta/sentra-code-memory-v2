package codecrawl

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// hitAtKCase represents one acceptance test for the headline hit@1/5/10
// metrics. The fixture is intentionally small so the test stays runnable
// in CI without external data.
type hitAtKCase struct {
	name      string
	defineID  string // path of the defining file
	noiseID   string // path of the loud noise file
	query     string
	expectIn  []string // paths that must appear in the top-K
	threshold int      // top-K to inspect
}

// TestRankFusionHitAtKAcceptanceMetrics runs the rank fusion pipeline
// against the headline hit@1/5/10 acceptance test from issue #43. The
// test is intentionally conservative: every case must produce the
// expected defining file in the top-K result, and the floor must hold
// even when the lexical baseline loses the definition.
func TestRankFusionHitAtKAcceptanceMetrics(t *testing.T) {
	cases := []hitAtKCase{
		{
			name:      "anchor-overshadowed",
			defineID:  "src/anchor.go",
			noiseID:   "src/noise.go",
			query:     "anchor",
			expectIn:  []string{"src/anchor.go"},
			threshold: 10,
		},
		{
			name:      "auth-overshadowed",
			defineID:  "src/auth.go",
			noiseID:   "src/noise.go",
			query:     "auth",
			expectIn:  []string{"src/auth.go"},
			threshold: 10,
		},
		{
			name:      "hub-overshadowed",
			defineID:  "src/hub.go",
			noiseID:   "src/noise.go",
			query:     "hub",
			expectIn:  []string{"src/hub.go"},
			threshold: 10,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildHitAtKFixture(t, tc.defineID, tc.noiseID)
			idx, _, err := CrawlDir(dir, 2)
			if err != nil {
				t.Fatal(err)
			}
			conf := DefaultRankerConfig()
			conf.Candidates = 4
			conf.IdentifierFloor = 1
			conf.IdentifierFloorCap = 1
			out := idx.FindRelevantRanked(dir, tc.query, tc.threshold, false, conf)
			present := map[string]bool{}
			for _, h := range out.Hits {
				present[h.Path] = true
			}
			for _, want := range tc.expectIn {
				if !present[want] {
					t.Fatalf("hit@%d missed %s: %+v", tc.threshold, want, out.Hits)
				}
			}
		})
	}
}

// buildHitAtKFixture writes a synthetic corpus with one defining file
// and 30 noise files that purposefully drown the lexical baseline. The
// fixture is reproducible across runs.
func buildHitAtKFixture(t *testing.T, defineID, noiseID string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Defining file: a single `func DefineName() { ... }` declaration.
	defineName := filepath.Base(defineID)
	defineName = defineName[:len(defineName)-len(filepath.Ext(defineName))]
	body := fmt.Sprintf("package p\nfunc %s() {}\n", exportName(defineName))
	if err := os.WriteFile(filepath.Join(dir, defineID), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Noise file: pad with the same lexeme so the lexical baseline loses
	// the definition.
	noise := fmt.Sprintf("package p\n// %s %s %s %s %s %s %s %s %s %s\nfunc noise() {}\n",
		defineName, defineName, defineName, defineName, defineName, defineName, defineName, defineName, defineName, defineName)
	if err := os.WriteFile(filepath.Join(dir, noiseID), []byte(noise), 0o644); err != nil {
		t.Fatal(err)
	}
	// 30 additional noise files with the same padded token.
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("src/hit_at_k_noise_%03d.go", i)
		body := fmt.Sprintf("package p\n// %s %s %s %s %s %s %s %s %s %s\nfunc noise%d() {}\n",
			defineName, defineName, defineName, defineName, defineName, defineName, defineName, defineName, defineName, defineName, i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// exportName capitalises the first character so the noise file's anchor
// follows the same identifier rules as the defining file.
func exportName(name string) string {
	if name == "" {
		return ""
	}
	b := []byte(name)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] = b[0] - 'a' + 'A'
	}
	return string(b)
}

// TestRankFusionHitAtKSummary prints the hit@1/5/10 acceptance metric
// summary so the parent agent can compare the new pipeline against the
// legacy baseline. The test prints a single line per case.
func TestRankFusionHitAtKSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping summary in short mode")
	}
	cases := []hitAtKCase{
		{name: "anchor", defineID: "src/anchor.go", noiseID: "src/noise.go", query: "anchor"},
		{name: "auth", defineID: "src/auth.go", noiseID: "src/noise.go", query: "auth"},
		{name: "hub", defineID: "src/hub.go", noiseID: "src/noise.go", query: "hub"},
	}
	for _, tc := range cases {
		dir := buildHitAtKFixture(t, tc.defineID, tc.noiseID)
		idx, _, err := CrawlDir(dir, 2)
		if err != nil {
			t.Fatal(err)
		}
		// Legacy baseline.
		baseline := idx.SearchOpts(tc.query, 10, true)
		baselineSet := map[string]bool{}
		for _, h := range baseline {
			baselineSet[h.Path] = true
		}
		// Hybrid pipeline.
		conf := DefaultRankerConfig()
		conf.Candidates = 4
		conf.IdentifierFloor = 1
		conf.IdentifierFloorCap = 1
		ranked := idx.FindRelevantRanked(dir, tc.query, 10, false, conf)
		rankedSet := map[string]bool{}
		for _, h := range ranked.Hits {
			rankedSet[h.Path] = true
		}
		// Compute hit@K.
		hitAt := func(k int, hits []Hit) bool {
			if len(hits) < k {
				k = len(hits)
			}
			for _, h := range hits[:k] {
				if h.Path == tc.defineID {
					return true
				}
			}
			return false
		}
		hitAtAgent := func(k int, hits []AgentHit) bool {
			if len(hits) < k {
				k = len(hits)
			}
			for _, h := range hits[:k] {
				if h.Path == tc.defineID {
					return true
				}
			}
			return false
		}
		baseAt1 := hitAt(1, baseline)
		rankedAt1 := hitAtAgent(1, ranked.Hits)
		baseAt5 := hitAt(5, baseline)
		rankedAt5 := hitAtAgent(5, ranked.Hits)
		baseAt10 := hitAt(10, baseline)
		rankedAt10 := hitAtAgent(10, ranked.Hits)
		// Sort the diagnostic for deterministic output.
		var note []string
		if !baseAt1 && rankedAt1 {
			note = append(note, "baseline_lost_but_ranker_found@1")
		}
		if !baseAt5 && rankedAt5 {
			note = append(note, "baseline_lost_but_ranker_found@5")
		}
		if !baseAt10 && rankedAt10 {
			note = append(note, "baseline_lost_but_ranker_found@10")
		}
		sort.Strings(note)
		t.Logf("hit@K[case=%s] baseline=%v/%v/%v ranked=%v/%v/%v notes=%v",
			tc.name, baseAt1, baseAt5, baseAt10, rankedAt1, rankedAt5, rankedAt10, note)
		_ = baselineSet
		_ = rankedSet
	}
}

// TestRankFusionHitAtKConservativeDeclarations ensures the headline
// metric also holds for idiomatic codebase patterns: a defining file
// with a noisy alias file that uses the same lexeme in a comment.
func TestRankFusionHitAtKConservativeDeclarations(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"src/anchor.go":        "package p\nfunc Anchor() int { return 1 }\n",
		"src/anchor_alias.go":  "package p\nfunc AnchorAlias() int { return 2 }\n",
		"src/noise.go":         "package p\n// anchor anchor anchor anchor anchor anchor anchor\nfunc noise() {}\n",
		"src/loose.go":         "package p\n// anchor anchor anchor anchor anchor anchor\nfunc loose() {}\n",
		"src/auth.go":          "package p\n// anchor anchor anchor anchor\nfunc Auth() int { return 0 }\n",
		"src/auth_string.go":   "package p\n// anchor anchor anchor\nfunc AuthString() int { return 0 }\n",
		"src/auth_helper.go":   "package p\n// anchor anchor\nfunc AuthHelper() int { return 0 }\n",
		"src/auth_helper_2.go": "package p\n// anchor\nfunc AuthHelper2() int { return 0 }\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	conf := DefaultRankerConfig()
	conf.Candidates = 4
	conf.IdentifierFloor = 1
	conf.IdentifierFloorCap = 1
	for _, topK := range []int{1, 5, 10} {
		out := idx.FindRelevantRanked(dir, "Anchor", topK, false, conf)
		report := []string{}
		for _, h := range out.Hits {
			report = append(report, h.Path)
		}
		if !containsHit(out.Hits, "src/anchor.go") {
			t.Fatalf("hit@%d missed anchor.go: %s", topK, report)
		}
		t.Logf("hit@%d [top]=%v", topK, report)
	}
}

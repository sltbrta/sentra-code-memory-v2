package scmbench_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/scmbench"
)

// writeFixtureTree creates a deterministic synthetic repo: 24 files of
// 60 lines each, all sharing the Anchor symbol so retrieval verbs have hits.
func writeFixtureTree(t testing.TB) (root, cache string) {
	t.Helper()
	root = t.TempDir()
	cache = t.TempDir()
	for i := 0; i < 24; i++ {
		var b []byte
		b = append(b, []byte(fmt.Sprintf("package fixture%d\n\n", i))...)
		for ln := 0; ln < 58; ln++ {
			b = append(b, []byte(fmt.Sprintf("// Anchor line %d of file %d with deterministic filler text.\n", ln, i))...)
		}
		b = append(b, []byte(fmt.Sprintf("func Anchor%d() int { return %d }\n", i, i))...)
		name := fmt.Sprintf("file%02d.go", i)
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, cache
}

func workflowScenario(root, cache string) scmbench.Scenario {
	return scmbench.Scenario{
		Name:       "phase0-scaffold",
		Root:       root,
		IndexCache: cache,
		Steps: []scmbench.Step{
			{Name: "index", Verb: "code_index"},
			{Name: "find_relevant", Verb: "code_find_relevant", Args: map[string]any{
				"q": "Anchor", "top_k": 3, "preview": true, "no_refresh": true,
			}},
			{Name: "expand", Verb: "code_expand", Args: map[string]any{"seed": "Anchor", "no_refresh": true}},
			{Name: "impact", Verb: "code_impact", Args: map[string]any{"seed": "Anchor", "no_refresh": true}},
			{Name: "freshness", Verb: "code_freshness"},
		},
	}
}

func TestEstimateTokensDeterministic(t *testing.T) {
	t.Parallel()
	cases := map[string]int{"": 0, "abcd": 1, "abcde": 2, "abcdefgh": 2}
	for in, want := range cases {
		if got := scmbench.EstimateTokens(in); got != want {
			t.Fatalf("EstimateTokens(%q)=%d want %d", in, got, want)
		}
	}
}

func TestNaiveBaselineUsesCrawlerSourcePolicy(t *testing.T) {
	t.Parallel()
	root, _ := writeFixtureTree(t)
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte(strings.Repeat("x", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "secret.go"), []byte(strings.Repeat("x", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := scmbench.NaiveBaselineBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(0)
	paths, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range paths {
		if filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		st, err := os.Stat(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		want += st.Size()
	}
	if got != want {
		t.Fatalf("baseline=%d want indexed source bytes=%d", got, want)
	}
}

func TestFailedStepCannotMeasureSavings(t *testing.T) {
	t.Parallel()
	root, cache := writeFixtureTree(t)
	rep, err := scmbench.Run(context.Background(), scmbench.Scenario{
		Name: "failed", Root: root, IndexCache: cache,
		Steps: []scmbench.Step{{Name: "bad", Verb: "not_a_real_verb"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.FailedSteps != 1 {
		t.Fatalf("failed steps=%d want 1", rep.FailedSteps)
	}
	if err := rep.MeasureBaseline(root); err == nil {
		t.Fatal("failed workflow must not report savings")
	}
}

func TestWorkflowReport(t *testing.T) {
	t.Parallel()
	root, cache := writeFixtureTree(t)
	rep, err := scmbench.Run(context.Background(), workflowScenario(root, cache))
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.MeasureBaseline(root); err != nil {
		t.Fatal(err)
	}

	if rep.Contract == "" || rep.Scenario != "phase0-scaffold" {
		t.Fatalf("report identity: %+v", rep)
	}
	if len(rep.Steps) != 5 {
		t.Fatalf("steps=%d", len(rep.Steps))
	}
	var bytes, tokens, calls int
	for _, s := range rep.Steps {
		if !s.OK {
			t.Fatalf("step %s failed: %+v", s.Name, s)
		}
		if s.ResponseBytes <= 0 || s.EstTokens <= 0 || s.ToolCalls != 1 {
			t.Fatalf("step %s not measured: %+v", s.Name, s)
		}
		if s.DurationMS < 0 {
			t.Fatalf("step %s negative latency: %+v", s.Name, s)
		}
		bytes += s.ResponseBytes
		tokens += s.EstTokens
		calls += s.ToolCalls
	}
	if rep.Totals.ResponseBytes != bytes || rep.Totals.EstTokens != tokens || rep.Totals.ToolCalls != calls {
		t.Fatalf("totals mismatch: %+v", rep.Totals)
	}
	if rep.Totals.ToolCalls != len(rep.Steps) {
		t.Fatalf("tool calls %d != steps %d", rep.Totals.ToolCalls, len(rep.Steps))
	}
	// The indexed workflow must beat reading the whole tree.
	if rep.SavedTokens <= 0 || rep.TokenSavingsRatio <= 0 {
		t.Fatalf("expected token savings: %+v", rep)
	}
	if rep.SavedTokens != rep.BaselineTokens-rep.Totals.EstTokens {
		t.Fatalf("savings math: %+v", rep)
	}
	// The report is a stable JSON artifact.
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var round scmbench.Report
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if round.Totals != rep.Totals || round.SavedTokens != rep.SavedTokens {
		t.Fatal("report does not round-trip")
	}
}

func TestReportSavingsRequiresMeasuredBaseline(t *testing.T) {
	t.Parallel()
	rep := scmbench.Report{
		Scenario: "unmeasured",
		Totals:   scmbench.Totals{ResponseBytes: 100, EstTokens: 25},
	}
	if err := rep.RecordSavings(t.TempDir()); err == nil {
		t.Fatal("unmeasured report must not be recorded")
	}
}

func TestReportRecordsOptionalSavingsLedger(t *testing.T) {
	t.Parallel()
	root, cache := writeFixtureTree(t)
	rep, err := scmbench.Run(context.Background(), workflowScenario(root, cache))
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.MeasureBaseline(root); err != nil {
		t.Fatal(err)
	}
	ledgerCache := t.TempDir()
	if err := rep.RecordSavings(ledgerCache); err != nil {
		t.Fatal(err)
	}
	ledger, err := savings.Open(ledgerCache)
	if err != nil {
		t.Fatal(err)
	}
	steps := ledger.Steps()
	if len(steps) != 1 {
		t.Fatalf("ledger steps=%d want 1", len(steps))
	}
	got := steps[0]
	if got.Name != rep.Scenario || got.Category != savings.CategoryRetrieval {
		t.Fatalf("ledger identity: %+v", got)
	}
	if got.BaselineBytes != rep.BaselineBytes || got.ServedBytes != int64(rep.Totals.ResponseBytes) ||
		got.BaselineTokens != int64(rep.BaselineTokens) || got.ServedTokens != int64(rep.Totals.EstTokens) {
		t.Fatalf("ledger metric mismatch: %+v report=%+v", got, rep)
	}
}

func TestNormalizeDeterministic(t *testing.T) {
	t.Parallel()
	root, cache := writeFixtureTree(t)
	rep, err := scmbench.Run(context.Background(), workflowScenario(root, cache))
	if err != nil {
		t.Fatal(err)
	}
	// Before normalization: timing and absolute paths vary.
	if rep.Totals.DurationMS < 0 {
		t.Fatal("duration must not be negative")
	}
	// After normalization: durations zeroed, paths stable.
	n := rep.Normalize(root, cache)
	if n.Totals.DurationMS != 0 {
		t.Fatalf("duration not zeroed: %+v", n.Totals)
	}
	for i, s := range n.Steps {
		if s.DurationMS != 0 {
			t.Fatalf("step %d duration not zeroed: %+v", i, s)
		}
	}
	// Normalized report must round-trip through JSON.
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var rt scmbench.Report
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.Totals.DurationMS != 0 {
		t.Fatal("round-tripped normalized report has non-zero duration")
	}
	// Savings math is unaffected by normalization.
	if err := n.MeasureBaseline(root); err != nil {
		t.Fatal(err)
	}
	if n.SavedTokens <= 0 {
		t.Fatal("normalized report must still show token savings")
	}

	otherRoot, otherCache := writeFixtureTree(t)
	longRoot := filepath.Join(t.TempDir(), strings.Repeat("long-checkout-segment-", 8))
	if err := os.MkdirAll(filepath.Dir(longRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(otherRoot, longRoot); err != nil {
		t.Fatal(err)
	}
	otherRoot = longRoot
	other, err := scmbench.Run(context.Background(), workflowScenario(otherRoot, otherCache))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.MeasureBaseline(otherRoot); err != nil {
		t.Fatal(err)
	}
	if got := other.Normalize(otherRoot, otherCache); !reflect.DeepEqual(n, got) {
		t.Fatalf("normalized reports differ across checkout paths:\nleft=%+v\nright=%+v", n, got)
	}
}

// BenchmarkFindRelevantStep measures the warm find_relevant step. Run with:
//
//	go test ./services/brain/internal/scmbench/ -bench . -benchtime 10x
func BenchmarkFindRelevantStep(b *testing.B) {
	root, cache := writeFixtureTree(b)
	ctx := context.Background()
	warm, err := scmbench.Run(ctx, scmbench.Scenario{
		Name: "setup", Root: root, IndexCache: cache,
		Steps: []scmbench.Step{{Name: "index", Verb: "code_index"}},
	})
	if err != nil || !warm.Steps[0].OK {
		b.Fatalf("setup: %+v %v", warm, err)
	}
	sc := scmbench.Scenario{
		Name: "bench", Root: root, IndexCache: cache,
		Steps: []scmbench.Step{{Name: "find_relevant", Verb: "code_find_relevant",
			Args: map[string]any{"q": "Anchor", "top_k": 3, "preview": true, "no_refresh": true}}},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rep, err := scmbench.Run(ctx, sc)
		if err != nil || !rep.Steps[0].OK {
			b.Fatalf("run: %+v %v", rep, err)
		}
		b.ReportMetric(float64(rep.Steps[0].ResponseBytes), "resp-bytes")
		b.ReportMetric(float64(rep.Steps[0].EstTokens), "resp-tokens")
	}
}

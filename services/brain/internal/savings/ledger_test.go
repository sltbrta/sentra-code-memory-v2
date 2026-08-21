package savings_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/savings"
)

func TestLedgerPersistsAndReopens(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	ledger, err := savings.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	step := savings.Step{
		Name: "find-relevant", Category: savings.CategoryRetrieval,
		BaselineBytes: 400, ServedBytes: 100,
		BaselineTokensEst: 100, ServedTokensEst: 25,
		DedupCount: 2, CompressionCount: 1,
		AvoidedReads: 3, ModelCalls: 1, AvoidedModelCalls: 1,
	}
	if err := ledger.Record(step); err != nil {
		t.Fatal(err)
	}

	reopened, err := savings.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Steps(); !reflect.DeepEqual(got, []savings.Step{step}) {
		t.Fatalf("reopened steps = %#v, want %#v", got, []savings.Step{step})
	}
	if _, err := os.Stat(filepath.Join(cache, savings.Filename)); err != nil {
		t.Fatalf("ledger file: %v", err)
	}
}

func TestLedgerRejectsCorruptionWithoutOverwriting(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	path := filepath.Join(cache, savings.Filename)
	corrupt := []byte(`{"version":1,"steps":[`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	// An orphaned temporary write must never be treated as the ledger.
	if err := os.WriteFile(filepath.Join(cache, ".token-savings-orphan.tmp"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := savings.Open(cache); err == nil {
		t.Fatal("Open must reject a corrupt committed ledger")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, corrupt) {
		t.Fatalf("corrupt ledger was overwritten: %q", got)
	}
}

func TestRecordValidationLeavesCommittedLedgerIntact(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	ledger, err := savings.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(savings.Step{Name: "valid", Category: savings.CategoryRetrieval}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, savings.Filename)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(savings.Step{Name: "invalid", BaselineBytes: -1}); err == nil {
		t.Fatal("negative counters must be rejected")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("failed record changed the committed ledger")
	}
}

func TestSummaryIsStableAcrossRecordOrder(t *testing.T) {
	t.Parallel()
	steps := []savings.Step{
		{Name: "phase-a-private-name", Category: savings.CategoryCompression, BaselineBytes: 100, ServedBytes: 40, BaselineTokensEst: 25, ServedTokensEst: 10, CompressionCount: 1},
		{Name: "phase-b-private-name", Category: savings.CategoryRetrieval, BaselineBytes: 50, ServedBytes: 10, BaselineTokensEst: 13, ServedTokensEst: 3, DedupCount: 2, AvoidedReads: 3, ModelCalls: 1, AvoidedModelCalls: 1},
	}
	makeSummary := func(cache string, ordered []savings.Step) savings.Summary {
		ledger, err := savings.Open(cache)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range ordered {
			if err := ledger.Record(step); err != nil {
				t.Fatal(err)
			}
		}
		return ledger.Summary()
	}
	left := makeSummary(t.TempDir(), steps)
	right := makeSummary(t.TempDir(), []savings.Step{steps[1], steps[0]})
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leftJSON, rightJSON) {
		t.Fatalf("summary JSON depends on record order:\n%s\n%s", leftJSON, rightJSON)
	}
	want := "steps=2 baseline_bytes=150 served_bytes=50 saved_bytes=100 baseline_tokens_est=38 served_tokens_est=13 saved_tokens_est=25 dedup=2 compression=1 avoided_reads=3 model_calls=1 avoided_model_calls=1 categories=compression:1,retrieval:1"
	if got := left.String(); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if strings.Contains(string(leftJSON), steps[0].Name) || strings.Contains(string(leftJSON), steps[1].Name) {
		t.Fatalf("summary unexpectedly contains per-step names: %s", leftJSON)
	}
}

func TestLedgerRollsUpBeyondActiveStepCap(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	ledger, err := savings.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < savings.DefaultMaxSteps+17; i++ {
		if err := ledger.Record(savings.Step{
			Name: "step", Category: savings.CategoryRetrieval,
			BaselineBytes: 10, ServedBytes: 4, BaselineTokensEst: 5, ServedTokensEst: 2,
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if len(ledger.Steps()) > savings.DefaultMaxSteps {
		t.Fatalf("active ledger steps exceeded cap: %d", len(ledger.Steps()))
	}
	summary := ledger.Summary()
	want := savings.DefaultMaxSteps + 17
	if summary.Steps != want || summary.Totals.BaselineBytes != int64(want*10) {
		t.Fatalf("rollup lost logical totals: %+v", summary)
	}
	reopened, err := savings.Open(cache)
	if err != nil {
		t.Fatalf("rolled-up ledger is not loadable: %v", err)
	}
	if got := reopened.Summary(); got.Steps != summary.Steps || got.Totals != summary.Totals {
		t.Fatalf("reopened summary drifted: got=%+v want=%+v", got, summary)
	}
}

func TestLedgerSummaryConsistentUnderConcurrentRecord(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	ledger, err := savings.Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	const total = savings.DefaultMaxSteps + 50
	// Hammer Record and Summary concurrently so a fold can interleave with a
	// Summary snapshot. Totals must stay exact regardless of interleaving.
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := ledger.Record(savings.Step{
				Name: "step", Category: savings.CategoryRetrieval,
				BaselineBytes: 10, ServedBytes: 4, BaselineTokensEst: 5, ServedTokensEst: 2,
			}); err != nil {
				t.Errorf("record: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = ledger.Summary()
		}()
	}
	wg.Wait()
	summary := ledger.Summary()
	if summary.Steps != total || summary.Totals.BaselineBytes != int64(total*10) {
		t.Fatalf("concurrent record/summary drifted: steps=%d baseline=%d want steps=%d baseline=%d",
			summary.Steps, summary.Totals.BaselineBytes, total, total*10)
	}
	reopened, err := savings.Open(cache)
	if err != nil {
		t.Fatalf("ledger not loadable after concurrent use: %v", err)
	}
	if got := reopened.Summary(); got.Steps != total || got.Totals.BaselineBytes != int64(total*10) {
		t.Fatalf("reopened summary drifted: %+v", got)
	}
}

package orgscope_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/orgscope"
)

func TestRequiredRecoverySubstrateMatrixIsExact(t *testing.T) {
	want := []orgscope.RecoverySubstrateKind{
		orgscope.RecoverySubstrateFilesystem,
		orgscope.RecoverySubstrateSQL,
		orgscope.RecoverySubstrateVector,
		orgscope.RecoverySubstrateHotLex,
		orgscope.RecoverySubstrateGraph,
		orgscope.RecoverySubstrateClaims,
		orgscope.RecoverySubstrateCache,
		orgscope.RecoverySubstrateObject,
	}
	if got := orgscope.RequiredRecoverySubstrates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("required substrate matrix = %v, want %v", got, want)
	}
}

func TestRecoveryDrillSubstrateMatrixFailsClosedBeforeMutation(t *testing.T) {
	t.Run("missing required substrate", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		scenario.request.Substrates.Adapters = scenario.request.Substrates.Adapters[1:]
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) ||
			!errors.Is(err, orgscope.ErrMissingRecoverySubstrate) ||
			receipt.FailureCode != "missing_substrate" || receipt.Verified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
		assertEmptyRecoveryTarget(t, scenario.target)
	})

	t.Run("duplicate cannot mask missing substrate", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		scenario.request.Substrates.Adapters[len(scenario.request.Substrates.Adapters)-1] =
			orgscope.NewHermeticRecoverySubstrate(orgscope.RecoverySubstrateFilesystem)
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrMissingRecoverySubstrate) || receipt.FailureCode != "missing_substrate" {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
		assertEmptyRecoveryTarget(t, scenario.target)
	})
}

func TestRecoveryDrillFailsOnInconclusiveSubstrateAndQueryPopulation(t *testing.T) {
	t.Run("substrate outage is not absence proof", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		for index, adapter := range scenario.request.Substrates.Adapters {
			if adapter.Kind() == orgscope.RecoverySubstrateGraph {
				scenario.request.Substrates.Adapters[index] = outageRecoverySubstrate{kind: orgscope.RecoverySubstrateGraph}
			}
		}
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoverySubstrateFailed) ||
			receipt.FailureCode != "substrate_verification_failed" || receipt.Verified ||
			receipt.ProductionCertified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
	})

	t.Run("empty representative query result", func(t *testing.T) {
		scenario := newRecoveryScenario(t)
		scenario.request.RepresentativeQuery.Query = "no-such-representative-term"
		receipt, err := orgscope.RunRecoveryDrill(scenario.request, scenario.target)
		if !errors.Is(err, orgscope.ErrRecoveryDrillFailed) ||
			receipt.FailureCode != "query_population_failed" || receipt.RepresentativeQueryRan || receipt.Verified {
			t.Fatalf("receipt = %+v, err=%v", receipt, err)
		}
	})
}

func TestRecoveryDrillRunnerRetainsPositiveAndNegativeReceipts(t *testing.T) {
	dir := t.TempDir()
	retainer, err := orgscope.OpenFileRecoveryReceiptRetainer(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 12, 20, 0, 0, time.UTC)
	runner, err := orgscope.NewRecoveryDrillRunner(orgscope.RecoveryDrillRunnerConfig{
		Timeout: time.Minute, Receipts: retainer, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	passedScenario := newRecoveryScenario(t)
	passed, passedPath, err := runner.Run(context.Background(), orgscope.RecoveryDrillJob{
		ScheduledAt: now.Add(-time.Minute), Request: passedScenario.request, Target: passedScenario.target,
	})
	if err != nil || !passed.Verified || passedPath == "" {
		t.Fatalf("passed receipt = %+v, path=%q, err=%v", passed, passedPath, err)
	}
	assertRetainedReceipt(t, passedPath, "passed", "")

	failedScenario := newRecoveryScenario(t)
	failedScenario.request.FailAt = orgscope.RecoveryFailureAfterBackup
	failed, failedPath, err := runner.Run(context.Background(), orgscope.RecoveryDrillJob{
		ScheduledAt: now, Request: failedScenario.request, Target: failedScenario.target,
	})
	if !errors.Is(err, orgscope.ErrInjectedRecoveryFailure) || failed.Verified || failedPath == "" {
		t.Fatalf("failed receipt = %+v, path=%q, err=%v", failed, failedPath, err)
	}
	assertRetainedReceipt(t, failedPath, "failed", "injected_failure")

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("retained entries = %v, err=%v", entries, err)
	}
}

func TestRecoveryDrillRunnerScheduleAndTimeoutAreBoundedAndRetained(t *testing.T) {
	t.Run("not due", func(t *testing.T) {
		now := time.Date(2026, 8, 4, 12, 20, 0, 0, time.UTC)
		runner, dir := newTestRecoveryRunner(t, now, time.Minute)
		scenario := newRecoveryScenario(t)
		receipt, path, err := runner.Run(context.Background(), orgscope.RecoveryDrillJob{
			ScheduledAt: now.Add(time.Minute), Request: scenario.request, Target: scenario.target,
		})
		if !errors.Is(err, orgscope.ErrRecoveryDrillNotDue) || receipt.FailureCode != "not_due" || path == "" {
			t.Fatalf("receipt = %+v, path=%q, err=%v", receipt, path, err)
		}
		assertRetainedReceipt(t, path, "failed", "not_due")
		assertEmptyRecoveryTarget(t, scenario.target)
		if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 1 {
			t.Fatalf("retained entries = %v, err=%v", entries, readErr)
		}
	})

	t.Run("adapter cancellation", func(t *testing.T) {
		now := time.Date(2026, 8, 4, 12, 20, 0, 0, time.UTC)
		runner, _ := newTestRecoveryRunner(t, now, 20*time.Millisecond)
		scenario := newRecoveryScenario(t)
		for index, adapter := range scenario.request.Substrates.Adapters {
			if adapter.Kind() == orgscope.RecoverySubstrateObject {
				scenario.request.Substrates.Adapters[index] = blockingRecoverySubstrate{kind: orgscope.RecoverySubstrateObject}
			}
		}
		started := time.Now()
		receipt, path, err := runner.Run(context.Background(), orgscope.RecoveryDrillJob{
			Request: scenario.request, Target: scenario.target,
		})
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("bounded run took %v", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) || receipt.FailureCode != "deadline_exceeded" || path == "" {
			t.Fatalf("receipt = %+v, path=%q, err=%v", receipt, path, err)
		}
		assertRetainedReceipt(t, path, "failed", "deadline_exceeded")
	})
}

func TestRecoveryDrillRunnerDowngradesUnretainedSuccess(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 20, 0, 0, time.UTC)
	runner, err := orgscope.NewRecoveryDrillRunner(orgscope.RecoveryDrillRunnerConfig{
		Timeout: time.Minute, Receipts: failingRecoveryRetainer{}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	scenario := newRecoveryScenario(t)
	receipt, path, err := runner.Run(context.Background(), orgscope.RecoveryDrillJob{
		Request: scenario.request, Target: scenario.target,
	})
	if !errors.Is(err, orgscope.ErrRecoveryReceiptRetention) || receipt.Verified ||
		receipt.FailureCode != "receipt_retention_failed" || path != "" {
		t.Fatalf("receipt = %+v, path=%q, err=%v", receipt, path, err)
	}
}

type outageRecoverySubstrate struct {
	kind orgscope.RecoverySubstrateKind
}

func (s outageRecoverySubstrate) Kind() orgscope.RecoverySubstrateKind { return s.kind }
func (outageRecoverySubstrate) ProviderBoundary() orgscope.RecoveryProviderBoundary {
	return orgscope.RecoveryBoundaryHermeticFake
}
func (outageRecoverySubstrate) Restore(context.Context, orgscope.RecoverySubstrateFixture) error {
	return nil
}
func (outageRecoverySubstrate) VerifyErasure(context.Context, orgscope.RecoverySubstrateFixture) (orgscope.RecoverySubstrateObservation, error) {
	return orgscope.RecoverySubstrateObservation{}, errors.New("substrate unavailable")
}

type blockingRecoverySubstrate struct {
	kind orgscope.RecoverySubstrateKind
}

func (s blockingRecoverySubstrate) Kind() orgscope.RecoverySubstrateKind { return s.kind }
func (blockingRecoverySubstrate) ProviderBoundary() orgscope.RecoveryProviderBoundary {
	return orgscope.RecoveryBoundaryHermeticFake
}
func (blockingRecoverySubstrate) Restore(ctx context.Context, _ orgscope.RecoverySubstrateFixture) error {
	<-ctx.Done()
	return ctx.Err()
}
func (blockingRecoverySubstrate) VerifyErasure(context.Context, orgscope.RecoverySubstrateFixture) (orgscope.RecoverySubstrateObservation, error) {
	return orgscope.RecoverySubstrateObservation{}, errors.New("unexpected verification")
}

type failingRecoveryRetainer struct{}

func (failingRecoveryRetainer) Retain(context.Context, orgscope.RecoveryReceiptRecord) (string, error) {
	return "", errors.New("receipt disk unavailable")
}

func newTestRecoveryRunner(t *testing.T, now time.Time, timeout time.Duration) (*orgscope.RecoveryDrillRunner, string) {
	t.Helper()
	dir := t.TempDir()
	retainer, err := orgscope.OpenFileRecoveryReceiptRetainer(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := orgscope.NewRecoveryDrillRunner(orgscope.RecoveryDrillRunnerConfig{
		Timeout: timeout, Receipts: retainer, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner, dir
}

func assertRetainedReceipt(t *testing.T, path, status, failureCode string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record orgscope.RecoveryReceiptRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatal(err)
	}
	if record.Receipt.Status != status || record.Receipt.FailureCode != failureCode ||
		record.Receipt.ProductionCertified || record.RecordedAt.IsZero() {
		t.Fatalf("record = %+v", record)
	}
	if filepath.Dir(path) == "." {
		t.Fatalf("receipt path is not durable: %q", path)
	}
}

func assertEmptyRecoveryTarget(t *testing.T, target *orgscope.Store) {
	t.Helper()
	backup, err := target.CreateBackup()
	if err != nil || len(backup.Items) != 0 || len(backup.Tombstones) != 0 || len(target.Audit()) != 0 {
		t.Fatalf("target mutated: backup=%+v audit=%+v err=%v", backup, target.Audit(), err)
	}
}

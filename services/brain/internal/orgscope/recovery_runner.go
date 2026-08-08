package orgscope

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	// ErrRecoveryDrillNotDue reports a scheduled job invoked before its due
	// instant. The negative result is retained like every other run.
	ErrRecoveryDrillNotDue = errors.New("orgscope: recovery drill is not due")
	// ErrRecoveryReceiptRetention reports that a result could not be durably
	// retained. A successful drill is downgraded to failed in this case.
	ErrRecoveryReceiptRetention = errors.New("orgscope: recovery receipt retention failed")
)

const (
	recoveryReceiptSchemaVersion = "orgscope.recovery-receipt.v1"
	defaultRecoveryRunTimeout    = 60 * time.Minute
	maxRecoveryRunTimeout        = 24 * time.Hour
	maxRecoveryRetentionTimeout  = 30 * time.Second
)

// RecoveryReceiptRecord is the durable, payload-free envelope for both
// positive and negative drill results.
type RecoveryReceiptRecord struct {
	SchemaVersion string               `json:"schema_version"`
	RecordedAt    time.Time            `json:"recorded_at"`
	Receipt       RecoveryDrillReceipt `json:"receipt"`
}

// RecoveryReceiptRetainer durably records one immutable result. Implementors
// must retain failed and cancelled receipts as well as passed receipts.
type RecoveryReceiptRetainer interface {
	Retain(context.Context, RecoveryReceiptRecord) (string, error)
}

// FileRecoveryReceiptRetainer stores content-addressed, immutable JSON files.
// There is intentionally no automatic deletion: retention lifecycle belongs
// to the operator that owns this directory, and negative receipts are never
// silently pruned by the drill runner.
type FileRecoveryReceiptRetainer struct {
	dir string
}

// OpenFileRecoveryReceiptRetainer creates or opens a private receipt
// directory. The returned path is absolute so runner receipts are auditable.
func OpenFileRecoveryReceiptRetainer(dir string) (*FileRecoveryReceiptRetainer, error) {
	if dir == "" {
		return nil, ErrRejected
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, err)
	}
	return &FileRecoveryReceiptRetainer{dir: abs}, nil
}

// Retain writes one fsync'd file through an fsync'd temporary file, then
// publishes it by an immutable hard link and fsyncs the containing directory.
func (s *FileRecoveryReceiptRetainer) Retain(ctx context.Context, record RecoveryReceiptRecord) (string, error) {
	if s == nil || s.dir == "" || ctx == nil || record.SchemaVersion != recoveryReceiptSchemaVersion ||
		record.RecordedAt.IsZero() || record.Receipt.ProductionCertified {
		return "", ErrRecoveryReceiptRetention
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Join(ErrRecoveryReceiptRetention, err)
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, err)
	}
	payload = append(payload, '\n')
	digest := sha256.Sum256(payload)
	name := record.RecordedAt.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(digest[:]) + ".json"
	path := filepath.Join(s.dir, name)
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if bytes.Equal(existing, payload) {
			return path, nil
		}
		return "", ErrRecoveryReceiptRetention
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, readErr)
	}

	temporary, err := os.CreateTemp(s.dir, ".recovery-receipt-*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	failed := func(cause error) (string, error) {
		temporary.Close()
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, cause)
	}
	if err := temporary.Chmod(0o600); err != nil {
		return failed(err)
	}
	if _, err := temporary.Write(payload); err != nil {
		return failed(err)
	}
	if err := temporary.Sync(); err != nil {
		return failed(err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, payload) {
			return path, nil
		}
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, err)
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("%w: %v", ErrRecoveryReceiptRetention, closeErr)
	}
	return path, nil
}

// RecoveryDrillRunnerConfig bounds a run and binds mandatory durable receipt
// retention. Clock is injectable for deterministic schedule and record tests.
type RecoveryDrillRunnerConfig struct {
	Timeout  time.Duration
	Receipts RecoveryReceiptRetainer
	Clock    func() time.Time
}

// RecoveryDrillRunner runs a due job once. It has no background goroutine;
// cron, a queue worker, or another scheduler calls Run when due.
type RecoveryDrillRunner struct {
	timeout  time.Duration
	receipts RecoveryReceiptRetainer
	clock    func() time.Time
}

// RecoveryDrillJob is a runnable unit. ScheduledAt may be zero for an
// immediate run; a future instant fails closed and produces a negative receipt.
type RecoveryDrillJob struct {
	ScheduledAt time.Time
	Request     RecoveryDrillRequest
	Target      *Store
}

// NewRecoveryDrillRunner creates a bounded runner. Zero timeout uses the
// Stage 10 60-minute RTO; larger than 24 hours is rejected as unbounded.
func NewRecoveryDrillRunner(config RecoveryDrillRunnerConfig) (*RecoveryDrillRunner, error) {
	if config.Receipts == nil {
		return nil, ErrRejected
	}
	if config.Timeout == 0 {
		config.Timeout = defaultRecoveryRunTimeout
	}
	if config.Timeout < 0 || config.Timeout > maxRecoveryRunTimeout {
		return nil, ErrRejected
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &RecoveryDrillRunner{timeout: config.Timeout, receipts: config.Receipts, clock: config.Clock}, nil
}

// Run executes one immediate or due job with a wall-clock deadline and
// durably retains its receipt before returning. Negative outcomes are retained
// even when the drill rejects the request or a failure is injected.
func (r *RecoveryDrillRunner) Run(ctx context.Context, job RecoveryDrillJob) (RecoveryDrillReceipt, string, error) {
	if r == nil || r.receipts == nil || r.clock == nil || ctx == nil {
		return RecoveryDrillReceipt{}, "", ErrRejected
	}
	now := r.clock().UTC()
	if !job.ScheduledAt.IsZero() && now.Before(job.ScheduledAt) {
		receipt := initialRecoveryDrillReceipt(job.Request)
		receipt, runErr := failRecoveryDrill(receipt, "schedule", "not_due", ErrRecoveryDrillNotDue)
		path, retainErr := r.retain(context.WithoutCancel(ctx), now, receipt)
		if retainErr != nil {
			return retentionFailure(receipt, retainErr)
		}
		return receipt, path, runErr
	}

	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	receipt, runErr := RunRecoveryDrillContext(runCtx, job.Request, job.Target)
	cancel()
	path, retainErr := r.retain(context.WithoutCancel(ctx), r.clock().UTC(), receipt)
	if retainErr != nil {
		return retentionFailure(receipt, retainErr)
	}
	return receipt, path, runErr
}

func initialRecoveryDrillReceipt(request RecoveryDrillRequest) RecoveryDrillReceipt {
	return RecoveryDrillReceipt{
		DrillID: request.DrillID, TenantID: request.TenantID,
		GenerationID: request.GenerationID, ConfigDigest: request.ConfigDigest,
		StartedAt: request.StartedAt.UTC().Format(time.RFC3339Nano), Status: "failed",
		ProductionCertified: false,
	}
}

func (r *RecoveryDrillRunner) retain(ctx context.Context, at time.Time, receipt RecoveryDrillReceipt) (string, error) {
	retainCtx, cancel := context.WithTimeout(ctx, maxRecoveryRetentionTimeout)
	defer cancel()
	record := RecoveryReceiptRecord{
		SchemaVersion: recoveryReceiptSchemaVersion,
		RecordedAt:    at.UTC(),
		Receipt:       receipt,
	}
	return r.receipts.Retain(retainCtx, record)
}

func retentionFailure(receipt RecoveryDrillReceipt, cause error) (RecoveryDrillReceipt, string, error) {
	receipt.Status = "failed"
	receipt.Verified = false
	receipt.ProductionCertified = false
	receipt.FailureCode = "receipt_retention_failed"
	receipt.Checks = append(receipt.Checks, RecoveryCheck{
		Name: "receipt_retention", Status: "failed", Code: "receipt_retention_failed",
	})
	return receipt, "", errors.Join(ErrRecoveryDrillFailed, ErrRecoveryReceiptRetention, cause)
}

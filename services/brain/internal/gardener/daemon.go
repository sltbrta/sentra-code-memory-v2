package gardener

import (
	"context"
	"fmt"
	"time"
)

// Daemon runs gardener waves against a durable (or memory) queue until
// context cancel. OnWave is optional; called after each non-empty wave with
// receipts (for product WarmSidecars).
type Daemon struct {
	Queue   Queue
	Workers map[JobKind]Worker
	Budget  Budget
	// Poll is idle sleep when the queue is empty (default 500ms).
	Poll time.Duration
	// WorkerID labels claims (default "gardener-daemon").
	WorkerID string
	// OnWave receives receipts after each RunWave with work.
	OnWave func(receipts []Receipt)
}

// RunOnce drains the queue with one Scheduler.RunWave.
func (d *Daemon) RunOnce(ctx context.Context) ([]Receipt, error) {
	if d == nil || d.Queue == nil {
		return nil, fmt.Errorf("gardener: daemon requires Queue")
	}
	workers := d.Workers
	if workers == nil {
		workers = DefaultWorkers()
	}
	budget := d.Budget
	if budget.MaxConcurrent <= 0 {
		budget = DefaultBudget()
	}
	id := d.WorkerID
	if id == "" {
		id = "gardener-daemon"
	}
	sched := &Scheduler{Queue: d.Queue, Workers: workers, Budget: budget}
	receipts, err := sched.RunWave(ctx, id)
	if err != nil {
		return receipts, err
	}
	if d.OnWave != nil && len(receipts) > 0 {
		d.OnWave(receipts)
	}
	return receipts, nil
}

// Run loops until ctx is done. Empty waves sleep Poll then retry.
func (d *Daemon) Run(ctx context.Context) error {
	poll := d.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipts, err := d.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Transient claim/run errors: backoff and continue.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
			}
			continue
		}
		if len(receipts) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
			}
		}
	}
}

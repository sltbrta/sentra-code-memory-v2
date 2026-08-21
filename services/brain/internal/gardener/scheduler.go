package gardener

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"
)

// Scheduler runs workers against a queue under a budget.
type Scheduler struct {
	Queue   Queue
	Workers map[JobKind]Worker
	Budget  Budget
	Clock   func() time.Time
}

// RunWave claims and executes jobs until the queue is empty, budget exhausted,
// or context cancelled. Safe for one wave per generation publish.
func (s *Scheduler) RunWave(ctx context.Context, workerID string) ([]Receipt, error) {
	if s.Queue == nil {
		return nil, fmt.Errorf("gardener: nil queue")
	}
	if s.Clock == nil {
		s.Clock = time.Now
	}
	budget := s.Budget
	if budget.MaxConcurrent <= 0 {
		budget = DefaultBudget()
	}
	start := s.Clock()
	var (
		receipts []Receipt
		mu       sync.Mutex
		wg       sync.WaitGroup
		sem      = make(chan struct{}, budget.MaxConcurrent)
		jobsDone int
	)
	for {
		if err := ctx.Err(); err != nil {
			return receipts, err
		}
		if budget.MaxWallClock > 0 && s.Clock().Sub(start) >= budget.MaxWallClock {
			break
		}
		if budget.MaxJobs > 0 && jobsDone >= budget.MaxJobs {
			break
		}
		batch, err := s.Queue.Claim(ctx, workerID, budget.MaxConcurrent)
		if err != nil {
			return receipts, err
		}
		if len(batch) == 0 {
			break
		}
		for _, job := range batch {
			jobsDone++
			wg.Add(1)
			sem <- struct{}{}
			go func(j Job) {
				defer wg.Done()
				defer func() { <-sem }()
				// A worker panic here takes down the whole host process: this
				// goroutine has no caller to recover it, and the scheduler runs
				// inside long-lived services. A failed job should fail the job.
				rec := runOneRecovered(s, ctx, j, budget)
				if err := s.Queue.Complete(ctx, rec); err != nil {
					// Losing this write strands the job as permanently claimed,
					// so it is recorded on the receipt rather than discarded.
					//
					// Error is the field that carries a cause. Writing into
					// Output only when it was empty put the reason nowhere in
					// the case that matters most: a job that succeeded has
					// output, so a successful job whose completion was never
					// persisted came back OK=false with no explanation at all.
					// A job that already failed keeps its own cause.
					rec.OK = false
					if rec.Error == "" {
						rec.Error = "complete_failed: " + err.Error()
					}
				}
				mu.Lock()
				receipts = append(receipts, rec)
				mu.Unlock()
			}(job)
		}
		wg.Wait()
	}
	return receipts, nil
}

func (s *Scheduler) runOne(ctx context.Context, job Job, budget Budget) Receipt {
	w := s.Workers[job.Kind]
	if w == nil {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: "no_worker", FinishedAt: s.Clock(),
		}
	}
	jobCtx := ctx
	var cancel context.CancelFunc
	if budget.MaxJobDuration > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, budget.MaxJobDuration)
		defer cancel()
	}
	t0 := s.Clock()
	rec, err := w.Run(jobCtx, job, budget)
	if err != nil {
		rec = Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: err.Error(), FinishedAt: s.Clock(),
		}
	}
	if rec.JobID == "" {
		rec.JobID = job.ID
	}
	if rec.FinishedAt.IsZero() {
		rec.FinishedAt = s.Clock()
	}
	if rec.Duration == 0 {
		rec.Duration = rec.FinishedAt.Sub(t0)
	}
	return rec
}

// runOneRecovered converts a worker panic into a failed receipt. Without it a
// single malformed job payload kills every service that runs a gardener wave.
func runOneRecovered(s *Scheduler, ctx context.Context, job Job, budget Budget) (receipt Receipt) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "gardener: panic in job %s (%s): %v\n%s\n",
				job.ID, job.Kind, r, debug.Stack())
			receipt = Receipt{
				JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
				DocumentID: job.DocumentID, OK: false,
				Output: "worker panicked", FinishedAt: time.Now(),
			}
		}
	}()
	return s.runOne(ctx, job, budget)
}

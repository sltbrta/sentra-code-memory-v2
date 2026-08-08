package gardener

import (
	"context"
	"fmt"
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
				rec := s.runOne(ctx, j, budget)
				_ = s.Queue.Complete(ctx, rec)
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

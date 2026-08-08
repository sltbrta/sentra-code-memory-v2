package gardener

import (
	"context"
	"sync"
)

// MemoryQueue is an in-process queue for tests and local single-node gardener.
type MemoryQueue struct {
	mu      sync.Mutex
	pending []Job
	done    []Receipt
}

// Enqueue appends jobs.
func (q *MemoryQueue) Enqueue(_ context.Context, jobs ...Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, jobs...)
	return nil
}

// Claim returns up to n jobs.
func (q *MemoryQueue) Claim(_ context.Context, _ string, n int) ([]Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n <= 0 || len(q.pending) == 0 {
		return nil, nil
	}
	if n > len(q.pending) {
		n = len(q.pending)
	}
	out := append([]Job(nil), q.pending[:n]...)
	q.pending = q.pending[n:]
	return out, nil
}

// Complete stores a receipt.
func (q *MemoryQueue) Complete(_ context.Context, receipt Receipt) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.done = append(q.done, receipt)
	return nil
}

// Receipts returns completed receipts (test helper).
func (q *MemoryQueue) Receipts() []Receipt {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Receipt(nil), q.done...)
}

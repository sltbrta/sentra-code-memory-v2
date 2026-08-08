package companymode

import (
	"context"
	"sync"
)

// OperationClass is one admission class for mixed load.
type OperationClass string

const (
	ClassInteractiveQuery OperationClass = "interactive_query"
	ClassIngestion        OperationClass = "ingestion"
	ClassOperator         OperationClass = "operator"
)

// AdmissionDecision is the bounded decision vocabulary.
type AdmissionDecision string

const (
	Admit  AdmissionDecision = "admit"
	Queue  AdmissionDecision = "queue"
	Reject AdmissionDecision = "reject"
)

// Admission enforces per-class bounded queues and backpressure.
type Admission struct {
	mu       sync.Mutex
	limits   map[OperationClass]int
	inflight map[OperationClass]int
}

// NewAdmission returns admission with the Stage 10 smoke ceilings.
func NewAdmission() *Admission {
	return &Admission{
		limits: map[OperationClass]int{
			ClassInteractiveQuery: 12,
			ClassIngestion:        6,
			ClassOperator:         4,
		},
		inflight: map[OperationClass]int{},
	}
}

// Decide records one attempt and returns admit or reject under saturation.
func (a *Admission) Decide(_ context.Context, class OperationClass) (AdmissionDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	limit, ok := a.limits[class]
	if !ok {
		return Reject, ErrRejected
	}
	if a.inflight[class] >= limit {
		return Reject, ErrQueueFull
	}
	a.inflight[class]++
	return Admit, nil
}

// Release frees one admitted slot.
func (a *Admission) Release(class OperationClass) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight[class] > 0 {
		a.inflight[class]--
	}
}

// Snapshot returns current in-flight counts for operator status.
func (a *Admission) Snapshot() map[OperationClass]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[OperationClass]int, len(a.inflight))
	for k, v := range a.inflight {
		out[k] = v
	}
	return out
}

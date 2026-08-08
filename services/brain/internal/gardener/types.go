package gardener

import (
	"context"
	"time"
)

// JobKind selects an enrichment operator.
type JobKind string

const (
	JobDoc2Query     JobKind = "doc2query"
	JobContextHeader JobKind = "context_header"
	JobEntityExtract JobKind = "entity_extract"
	JobEdgePropose   JobKind = "edge_propose"
	JobSummary       JobKind = "summary"
	JobDenseEmbed    JobKind = "dense_embed"
	JobRAPTORCluster JobKind = "raptor_cluster"
)

// Job is one unit of gardener work against a pinned generation.
type Job struct {
	ID           string
	Kind         JobKind
	GenerationID string
	DocumentID   string
	Payload      map[string]string
	Attempt      int
	CreatedAt    time.Time
}

// Receipt records completion (success or bounded failure).
type Receipt struct {
	JobID        string
	Kind         JobKind
	GenerationID string
	OK           bool
	Error        string
	Tokens       int
	Duration     time.Duration
	Artifacts    int // edges/sidecars/vectors written
	// Output is free-form artifact text (e.g. d2q lines) for product WarmSidecars.
	Output     string
	DocumentID string
	FinishedAt time.Time
}

// Budget bounds concurrent LLM enrichment for one generation wave.
type Budget struct {
	MaxConcurrent   int
	MaxTokensPerJob int
	MaxJobDuration  time.Duration
	MaxJobs         int
	// MaxWallClock caps the entire wave (semantic/graph cold ≤ 20m target).
	MaxWallClock time.Duration
}

// DefaultBudget returns a production-safe concurrent enrichment budget.
func DefaultBudget() Budget {
	return Budget{
		MaxConcurrent:   32,
		MaxTokensPerJob: 2_048,
		MaxJobDuration:  45 * time.Second,
		MaxJobs:         50_000,
		MaxWallClock:    20 * time.Minute,
	}
}

// LocalWorkerBudget sizes concurrent gardener drain for local residual workers
// (in lieu of hosted burst fleet). nWorkers should be GOMAXPROCS or operator override.
func LocalWorkerBudget(nWorkers int) Budget {
	b := DefaultBudget()
	if nWorkers < 1 {
		nWorkers = 1
	}
	if nWorkers > 64 {
		nWorkers = 64
	}
	b.MaxConcurrent = nWorkers
	return b
}

// Queue is the gardener job store (memory or durable later).
type Queue interface {
	Enqueue(ctx context.Context, jobs ...Job) error
	Claim(ctx context.Context, workerID string, n int) ([]Job, error)
	Complete(ctx context.Context, receipt Receipt) error
}

// Worker executes one job kind (deterministic or LLM-backed).
type Worker interface {
	Kind() JobKind
	Run(ctx context.Context, job Job, budget Budget) (Receipt, error)
}

// LLMClient is the optional chat port for LLM-heavy jobs.
// Implementations live outside this package (provider adapters).
type LLMClient interface {
	Complete(ctx context.Context, system, user string, maxTokens int) (text string, tokens int, err error)
}

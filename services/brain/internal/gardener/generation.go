package gardener

import (
	"context"
	"fmt"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

// GraphSink stores ontology graphs produced at publish time (optional).
type GraphSink interface {
	PutGraph(g ontology.Graph) error
}

// GenerationEnricher plans and enqueues gardener jobs when a generation is
// published retrieval_ready. Optionally builds a co-occurrence ontology graph.
type GenerationEnricher struct {
	Queue     Queue
	Budget    Budget
	GraphSink GraphSink
	// MaxTermDocs bounds co-occurrence inverted lists (ontology).
	MaxTermDocs int
}

// OnPublished plans enrichment jobs for documents and enqueues them.
// Also builds deterministic co-occurrence graph when GraphSink is set.
// Returns the number of jobs enqueued.
func (e *GenerationEnricher) OnPublished(ctx context.Context, generationID string, documents map[string]string) (enqueued int, err error) {
	if e == nil || e.Queue == nil {
		return 0, fmt.Errorf("gardener: GenerationEnricher requires Queue")
	}
	if generationID == "" {
		return 0, fmt.Errorf("gardener: empty generation id")
	}
	if len(documents) == 0 {
		return 0, nil
	}

	if e.GraphSink != nil {
		maxTerm := e.MaxTermDocs
		if maxTerm <= 0 {
			maxTerm = 80
		}
		g := ontology.BuildCoOccurrenceGraph(generationID, documents, maxTerm)
		// Merge per-doc cite edges.
		for id, text := range documents {
			g.Edges = append(g.Edges, ontology.ExtractDocumentEdges(generationID, id, text)...)
		}
		if err := e.GraphSink.PutGraph(g); err != nil {
			return 0, fmt.Errorf("gardener: put graph: %w", err)
		}
	}

	budget := e.Budget
	jobs := PlanEnrichmentJobsBudgeted(generationID, documents, nil, budget)
	if len(jobs) == 0 {
		return 0, nil
	}
	if err := e.Queue.Enqueue(ctx, jobs...); err != nil {
		return 0, err
	}
	return len(jobs), nil
}

// DefaultWorkers returns the standard deterministic worker map (no LLM).
func DefaultWorkers() map[JobKind]Worker {
	return map[JobKind]Worker{
		JobDoc2Query:     DeterministicDoc2QueryWorker{},
		JobEdgePropose:   DeterministicEdgeWorker{},
		JobContextHeader: DeterministicContextHeaderWorker{},
		JobDenseEmbed:    DeterministicDenseWorker{},
	}
}

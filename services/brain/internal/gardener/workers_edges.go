package gardener

import (
	"context"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

// DeterministicEdgeWorker proposes document edges via ontology extractors.
// Artifacts is the number of edges extracted for this document alone (cites).
// Full co-occurrence graphs are built at OnPublished via GenerationEnricher.
type DeterministicEdgeWorker struct{}

// Kind implements Worker.
func (DeterministicEdgeWorker) Kind() JobKind { return JobEdgePropose }

// Run implements Worker.
func (DeterministicEdgeWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	text := strings.TrimSpace(job.Payload["text"])
	if text == "" {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: "empty_text", FinishedAt: time.Now(),
		}, nil
	}
	edges := ontology.ExtractDocumentEdges(job.GenerationID, job.DocumentID, text)
	return Receipt{
		JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
		OK: true, Artifacts: len(edges), FinishedAt: time.Now(),
	}, nil
}

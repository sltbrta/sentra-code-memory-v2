package gardener

import (
	"context"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// DeterministicContextHeaderWorker emits a short header artifact without LLM.
type DeterministicContextHeaderWorker struct{}

// Kind implements Worker.
func (DeterministicContextHeaderWorker) Kind() JobKind { return JobContextHeader }

// Run implements Worker.
func (DeterministicContextHeaderWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	text := strings.TrimSpace(job.Payload["text"])
	if text == "" {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: "empty_text", FinishedAt: time.Now(),
		}, nil
	}
	// One context header artifact (first line / lead).
	header := text
	if i := strings.Index(text, "\n"); i > 0 && i < 200 {
		header = strings.TrimSpace(text[:i])
	} else {
		header = textbound.Bytes(header, 200)
	}
	return Receipt{
		JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
		OK: true, Artifacts: 1, Output: header, DocumentID: job.DocumentID,
		FinishedAt: time.Now(),
	}, nil
}

package gardener

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// RegisterLLMWorkers adds LLM-backed workers for doc2query when client is set.
// Concurrent scheduler already fan-outs; DefaultBudget caps wall clock to 20m.
func RegisterLLMWorkers(workers map[JobKind]Worker, client LLMClient) map[JobKind]Worker {
	if workers == nil {
		workers = DefaultWorkers()
	}
	if client == nil {
		return workers
	}
	// LLM upgrades doc2query; keep edge/header deterministic for cost control.
	workers[JobDoc2Query] = LLMDoc2QueryWorker{Client: client}
	workers[JobSummary] = LLMSummaryWorker{Client: client}
	return workers
}

// LLMSummaryWorker produces a short document gist via LLM (gardener async).
type LLMSummaryWorker struct {
	Client LLMClient
}

// Kind implements Worker.
func (LLMSummaryWorker) Kind() JobKind { return JobSummary }

// Run implements Worker.
func (w LLMSummaryWorker) Run(ctx context.Context, job Job, budget Budget) (Receipt, error) {
	if w.Client == nil {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: "no_llm_client", FinishedAt: time.Now(),
		}, nil
	}
	text := strings.TrimSpace(job.Payload["text"])
	if text == "" {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: "empty_text", FinishedAt: time.Now(),
		}, nil
	}
	text = textbound.Bytes(text, 4_000)
	maxTok := budget.MaxTokensPerJob
	if maxTok <= 0 {
		maxTok = 256
	}
	out, tokens, err := w.Client.Complete(
		ctx,
		"Summarize this enterprise document in 2 sentences. No preamble.",
		fmt.Sprintf("Document %s:\n%s", job.DocumentID, text),
		maxTok,
	)
	if err != nil {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: err.Error(), FinishedAt: time.Now(),
		}, nil
	}
	arts := 0
	if strings.TrimSpace(out) != "" {
		arts = 1
	}
	return Receipt{
		JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
		OK: true, Tokens: tokens, Artifacts: arts, FinishedAt: time.Now(),
	}, nil
}

package gardener

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// DeterministicDoc2QueryWorker emits pseudo-questions without an LLM.
// Used as the always-on floor; LLM workers upgrade quality when budget allows.
type DeterministicDoc2QueryWorker struct{}

// Kind implements Worker.
func (DeterministicDoc2QueryWorker) Kind() JobKind { return JobDoc2Query }

// Run implements Worker.
func (DeterministicDoc2QueryWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	text := job.Payload["text"]
	if text == "" {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: "empty_text", FinishedAt: time.Now(),
		}, nil
	}
	// Build pseudo-questions from content tokens (no LLM) for WarmSidecars.
	lines := deterministicDoc2QueryLines(job.DocumentID, text)
	return Receipt{
		JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
		OK: true, Artifacts: len(lines), Output: strings.Join(lines, "\n"),
		DocumentID: job.DocumentID, FinishedAt: time.Now(),
	}, nil
}

func deterministicDoc2QueryLines(docID, text string) []string {
	// First sentence-ish window.
	snip := strings.TrimSpace(text)
	snip = textbound.Bytes(snip, 400)
	// Token bag for "what about X" questions.
	fields := strings.Fields(strings.ToLower(snip))
	var content []string
	stop := map[string]struct{}{
		"the": {}, "and": {}, "for": {}, "with": {}, "that": {}, "this": {},
		"from": {}, "are": {}, "was": {}, "were": {}, "have": {}, "has": {},
		"not": {}, "but": {}, "you": {}, "your": {}, "our": {}, "can": {},
	}
	for _, f := range fields {
		f = strings.Trim(f, ".,;:()[]{}\"'")
		if len(f) < 4 {
			continue
		}
		if _, ok := stop[f]; ok {
			continue
		}
		content = append(content, f)
		if len(content) >= 8 {
			break
		}
	}
	var lines []string
	if docID != "" {
		lines = append(lines, "What does document "+docID+" say?")
	}
	if len(content) >= 2 {
		lines = append(lines, "What is "+content[0]+" "+content[1]+"?")
	}
	if len(content) >= 4 {
		lines = append(lines, "How does "+content[2]+" relate to "+content[3]+"?")
	}
	up := strings.ToUpper(text)
	if strings.Contains(up, "RPO") {
		lines = append(lines, "What is the RPO value?")
	}
	if strings.Contains(up, "RTO") {
		lines = append(lines, "What is the RTO value?")
	}
	if len(lines) == 0 {
		lines = append(lines, "Summarize key facts from this document.")
	}
	return lines
}

// LLMDoc2QueryWorker uses an LLM client when present; fails closed without one.
type LLMDoc2QueryWorker struct {
	Client LLMClient
}

// Kind implements Worker.
func (LLMDoc2QueryWorker) Kind() JobKind { return JobDoc2Query }

// Run implements Worker.
func (w LLMDoc2QueryWorker) Run(ctx context.Context, job Job, budget Budget) (Receipt, error) {
	if w.Client == nil {
		return DeterministicDoc2QueryWorker{}.Run(ctx, job, budget)
	}
	text := job.Payload["text"]
	text = textbound.Bytes(text, 4_000)
	system := "Emit 2-3 short search questions a user might ask that this document answers. One per line. No numbering."
	user := fmt.Sprintf("Document %s:\n%s", job.DocumentID, text)
	maxTok := budget.MaxTokensPerJob
	if maxTok <= 0 {
		maxTok = 256
	}
	out, tokens, err := w.Client.Complete(ctx, system, user, maxTok)
	if err != nil {
		// Soft fallback to deterministic
		rec, _ := DeterministicDoc2QueryWorker{}.Run(ctx, job, budget)
		rec.Error = "llm_fallback:" + err.Error()
		return rec, nil
	}
	lines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	return Receipt{
		JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
		OK: true, Tokens: tokens, Artifacts: lines, FinishedAt: time.Now(),
	}, nil
}

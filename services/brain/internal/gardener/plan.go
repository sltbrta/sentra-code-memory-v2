package gardener

import (
	"sort"
	"time"
)

// payloadTextCap is the max runes of document text attached to a job payload.
const payloadTextCap = 4_000

// defaultEnrichmentKinds are the per-document jobs planned on generation publish.
var defaultEnrichmentKinds = []JobKind{
	JobDoc2Query,
	JobEdgePropose,
	JobContextHeader,
}

// PlanEnrichmentJobs builds one job per (document, kind) with payload text
// truncated to 4k. Job IDs are stable: "{generationID}:{kind}:{docID}".
// Document order is sorted by ID for determinism. kinds may be empty to use
// the default enrichment set (doc2query, edge_propose, context_header).
func PlanEnrichmentJobs(generationID string, documents map[string]string, kinds []JobKind) []Job {
	if len(kinds) == 0 {
		kinds = append([]JobKind(nil), defaultEnrichmentKinds...)
	}
	docIDs := make([]string, 0, len(documents))
	for id := range documents {
		if id != "" {
			docIDs = append(docIDs, id)
		}
	}
	sort.Strings(docIDs)

	now := time.Now()
	out := make([]Job, 0, len(docIDs)*len(kinds))
	for _, docID := range docIDs {
		text := truncatePayload(documents[docID], payloadTextCap)
		for _, kind := range kinds {
			out = append(out, Job{
				ID:           stableJobID(generationID, kind, docID),
				Kind:         kind,
				GenerationID: generationID,
				DocumentID:   docID,
				Payload:      map[string]string{"text": text},
				CreatedAt:    now,
			})
		}
	}
	return out
}

// PlanEnrichmentJobsBudgeted is PlanEnrichmentJobs capped to budget.MaxJobs
// when MaxJobs > 0. Zero MaxJobs means uncapped (same as PlanEnrichmentJobs).
func PlanEnrichmentJobsBudgeted(generationID string, documents map[string]string, kinds []JobKind, budget Budget) []Job {
	jobs := PlanEnrichmentJobs(generationID, documents, kinds)
	if budget.MaxJobs > 0 && len(jobs) > budget.MaxJobs {
		return jobs[:budget.MaxJobs]
	}
	return jobs
}

// stableJobID returns a deterministic job identity safe for idempotent enqueue.
func stableJobID(generationID string, kind JobKind, docID string) string {
	return generationID + ":" + string(kind) + ":" + docID
}

// truncatePayload trims text to at most max runes so multi-byte UTF-8 is not
// split mid-codepoint. max <= 0 leaves text unchanged.
func truncatePayload(text string, max int) string {
	if max <= 0 {
		return text
	}
	// Fast path: under cap by bytes implies under cap by runes.
	if len(text) <= max {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}

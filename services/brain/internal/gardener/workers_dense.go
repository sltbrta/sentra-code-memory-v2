package gardener

import (
	"context"
	"hash/fnv"
	"strings"
	"time"
	"unicode"
)

// DeterministicDenseWorker emits bag-of-words embedding artifacts (offline dense).
// Artifacts counts non-zero vector bins; vectors themselves are written by
// callers that hold a dense.MemoryStore (product LiveCorpus / projections).
type DeterministicDenseWorker struct{}

// Kind implements Worker.
func (DeterministicDenseWorker) Kind() JobKind { return JobDenseEmbed }

// Run implements Worker.
func (DeterministicDenseWorker) Run(_ context.Context, job Job, _ Budget) (Receipt, error) {
	text := strings.TrimSpace(job.Payload["text"])
	if text == "" {
		return Receipt{
			JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
			OK: false, Error: "empty_text", FinishedAt: time.Now(),
		}, nil
	}
	bins := bagBins(text, 64)
	nonzero := 0
	for _, v := range bins {
		if v != 0 {
			nonzero++
		}
	}
	return Receipt{
		JobID: job.ID, Kind: job.Kind, GenerationID: job.GenerationID,
		OK: true, Artifacts: nonzero, FinishedAt: time.Now(),
	}, nil
}

func bagBins(text string, dim int) []float32 {
	v := make([]float32, dim)
	var b strings.Builder
	flush := func() {
		tok := strings.ToLower(b.String())
		b.Reset()
		if len(tok) < 3 {
			return
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		v[int(h.Sum32())%dim] += 1
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return v
}

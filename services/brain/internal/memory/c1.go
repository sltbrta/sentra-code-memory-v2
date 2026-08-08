package memory

import (
	"math"
	"strings"
)

// Probe is a hold-out query used for C1 predict-calibrate.
type Probe struct {
	Question       string
	ExpectedDocIDs []string // documents that should rank highly if memory is healthy
}

// ProbeResult is the outcome of one probe against a ranked hit list.
type ProbeResult struct {
	Question        string
	HitDocIDs       []string
	RecallAtK       float64
	PredictionError float64 // 1 - RecallAtK
}

// MeasureProbe scores whether expected docs appear in top-k hits.
func MeasureProbe(p Probe, hitDocIDs []string, k int) ProbeResult {
	if k <= 0 {
		k = 5
	}
	if len(hitDocIDs) > k {
		hitDocIDs = hitDocIDs[:k]
	}
	if len(p.ExpectedDocIDs) == 0 {
		// Unscored probe: treat as unknown (error 0.5), never perfect — empty gold
		// must not make C1 look healthy (GAP-MEM-C1-QUERY-LOG).
		return ProbeResult{Question: p.Question, HitDocIDs: hitDocIDs, RecallAtK: 0.5, PredictionError: 0.5}
	}
	hit := map[string]struct{}{}
	for _, id := range hitDocIDs {
		hit[id] = struct{}{}
	}
	found := 0
	for _, e := range p.ExpectedDocIDs {
		if _, ok := hit[e]; ok {
			found++
		}
	}
	rec := float64(found) / float64(len(p.ExpectedDocIDs))
	return ProbeResult{
		Question: p.Question, HitDocIDs: hitDocIDs,
		RecallAtK: rec, PredictionError: 1 - rec,
	}
}

// AggregatePredictionError averages probe errors (C1 gate input).
func AggregatePredictionError(results []ProbeResult) float64 {
	if len(results) == 0 {
		return 0.5 // unknown → allow heavy (conservative for skip)
	}
	sum := 0.0
	for _, r := range results {
		sum += r.PredictionError
	}
	return sum / float64(len(results))
}

// ShouldSkipConsolidation is C1: skip heavy rewrites when probes are healthy.
func ShouldSkipConsolidation(predictionError, threshold float64) bool {
	if threshold <= 0 {
		threshold = 0.15
	}
	return predictionError >= 0 && predictionError < threshold
}

// BuildProbesFromDocuments creates probes preferring title-like / first-sentence
// questions ("What about X?") over random tokens (stronger C1).
func BuildProbesFromDocuments(docs map[string]string, maxProbes int) []Probe {
	if maxProbes <= 0 {
		maxProbes = 3
	}
	var ids []string
	for id := range docs {
		ids = append(ids, id)
	}
	// stable-ish order by id
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	var out []Probe
	for _, id := range ids {
		if len(out) >= maxProbes {
			break
		}
		text := docs[id]
		q := probeQuestionFromText(text)
		if q == "" {
			q = firstContentToken(text)
		}
		if q == "" {
			q = id
		}
		out = append(out, Probe{Question: q, ExpectedDocIDs: []string{id}})
	}
	return out
}

// probeQuestionFromText builds a title-like question from the first ~80 chars
// / first sentence (high-precision C1 probe).
func probeQuestionFromText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// First sentence or first 80 chars.
	snippet := text
	if len(snippet) > 80 {
		snippet = snippet[:80]
	}
	// Cut at sentence boundary if present.
	for _, sep := range []string{". ", "! ", "? ", "\n"} {
		if i := strings.Index(snippet, sep); i > 8 {
			snippet = snippet[:i]
			break
		}
	}
	snippet = strings.TrimSpace(snippet)
	snippet = strings.Trim(snippet, ".,:;!?\"'")
	if snippet == "" {
		return ""
	}
	// Prefer noun-phrase head: first 3–8 content tokens.
	fields := strings.Fields(snippet)
	var content []string
	for _, f := range fields {
		f = strings.Trim(f, ".,:;!?\"'()[]")
		if len(f) < 2 {
			continue
		}
		low := strings.ToLower(f)
		switch low {
		case "the", "a", "an", "and", "or", "of", "to", "in", "on", "for", "with", "is", "are", "was", "were":
			// keep short structure words only if we already have content
			if len(content) == 0 {
				continue
			}
		}
		content = append(content, f)
		if len(content) >= 6 {
			break
		}
	}
	if len(content) == 0 {
		return firstContentToken(text)
	}
	if len(content) > 5 {
		content = content[:5]
	}
	phrase := strings.Join(content, " ")
	return "What about " + phrase + "?"
}

func firstContentToken(text string) string {
	fields := strings.Fields(text)
	for _, f := range fields {
		f = strings.Trim(f, ".,:;!?\"'")
		if len(f) >= 4 {
			return f
		}
	}
	return ""
}

// Clamp01 bounds x to [0,1].
func Clamp01(x float64) float64 {
	return math.Max(0, math.Min(1, x))
}

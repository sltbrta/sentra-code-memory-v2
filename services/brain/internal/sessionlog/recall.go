package sessionlog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RecallOptions controls bounded local recall with abstention (issue #30).
type RecallOptions struct {
	// MinConfidence is the minimum provenance confidence to admit a memory.
	MinConfidence float64
	// MinRelevance is the minimum normalized relevance score in [0,1].
	MinRelevance float64
	// TopK bounds the number of returned memories.
	TopK int
	// IncludeSuperseded opts into superseded content (excluded by default).
	IncludeSuperseded bool
}

// DefaultRecallOptions returns conservative thresholds so weak or unrelated
// memory is not injected (#30).
func DefaultRecallOptions() RecallOptions {
	return RecallOptions{
		MinConfidence: 0.5,
		MinRelevance:  0.35,
		TopK:          12,
	}
}

// Memory is one recalled pointer with the score that admitted it.
type Memory struct {
	EventID    string     `json:"event_id,omitempty"`
	Kind       Kind       `json:"kind"`
	Provenance Provenance `json:"provenance,omitempty"`
	Freshness  Freshness  `json:"freshness,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	Relevance  float64    `json:"relevance,omitempty"`
	FreeText   string     `json:"free_text,omitempty"`
}

// RecallResult is the bounded recall answer. When NoMemory is true the recall
// abstained: callers must not inject anything (#30).
type RecallResult struct {
	Memories []Memory `json:"memories,omitempty"`
	// Abstained reports that recall declined to inject memory.
	Abstained bool   `json:"abstained,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Recall searches the event stream for memories matching query. It scores each
// candidate by provenance confidence and a deterministic lexical relevance
// against path/symbol/handle/free-text, then admits only memories above the
// thresholds. Superseded content is excluded by default. Recall abstains
// (returns Abstained=true, empty Memories) when nothing clears the bar.
//
// This is the provenance-first admission gate (#29) applied to recall (#30):
// every admitted memory carries provenance so it is traceable and safely
// invalidatable, and weak/unrelated memory is rejected rather than injected.
func Recall(events []Event, query string, opts RecallOptions) RecallResult {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return RecallResult{Abstained: true, Reason: "empty query"}
	}
	if opts.TopK <= 0 {
		opts.TopK = DefaultRecallOptions().TopK
	}

	var scored []Memory
	for _, ev := range events {
		fresh := ev.Freshness
		if fresh == "" {
			fresh = FreshnessAsOf
		}
		if fresh == FreshnessSuperseded && !opts.IncludeSuperseded {
			continue
		}
		rel := relevance(ev, q)
		conf := ev.Provenance.Confidence
		if conf == 0 {
			// Unspecified confidence is treated as low/unknown rather than 1
			// so recall never silently trusts unprovenanced memory.
			conf = 0.25
		}
		if rel < opts.MinRelevance {
			continue
		}
		if conf < opts.MinConfidence {
			continue
		}
		scored = append(scored, Memory{
			EventID:    ev.ID,
			Kind:       ev.Kind,
			Provenance: ev.Provenance,
			Freshness:  fresh,
			Confidence: conf,
			Relevance:  rel,
			FreeText:   ev.FreeText,
		})
	}

	if len(scored) == 0 {
		return RecallResult{Abstained: true, Reason: fmt.Sprintf(
			"no memory above confidence=%.2f relevance=%.2f", opts.MinConfidence, opts.MinRelevance)}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Relevance != scored[j].Relevance {
			return scored[i].Relevance > scored[j].Relevance
		}
		return scored[i].EventID < scored[j].EventID
	})
	if len(scored) > opts.TopK {
		scored = scored[:opts.TopK]
	}
	return RecallResult{Memories: scored}
}

// relevance is a deterministic normalized lexical score in [0,1]. It rewards
// exact symbol hits and shared tokens across path/symbol/handle/free-text and
// is independent of any model tokenizer.
func relevance(ev Event, q string) float64 {
	symbol := strings.ToLower(ev.Provenance.Symbol)
	path := strings.ToLower(ev.Provenance.Path)
	handle := strings.ToLower(ev.Provenance.Handle)
	text := strings.ToLower(ev.FreeText)
	if symbol == q {
		return 1.0
	}
	var best float64
	if symbol != "" && strings.Contains(symbol, q) {
		best = maxf(best, 0.9)
	}
	if path != "" && strings.Contains(path, q) {
		best = maxf(best, 0.6)
	}
	if handle != "" && strings.Contains(handle, q) {
		best = maxf(best, 0.5)
	}
	if text != "" && strings.Contains(text, q) {
		best = maxf(best, 0.4)
	}
	// Multi-token coverage: reward matching distinct query tokens.
	qTokens := tokenize(q)
	if len(qTokens) > 1 {
		hay := symbol + " " + path + " " + handle + " " + text
		hayTokens := tokenize(hay)
		haySet := map[string]struct{}{}
		for _, t := range hayTokens {
			haySet[t] = struct{}{}
		}
		hit := 0
		for _, t := range qTokens {
			if _, ok := haySet[t]; ok {
				hit++
			}
		}
		coverage := float64(hit) / float64(len(qTokens))
		best = maxf(best, 0.3*coverage)
	}
	return best
}

func tokenize(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '/' || r == '.' || r == '_' || r == '-'
	}) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// MarshalJSON emits canonical recall JSON.
func (r RecallResult) MarshalJSON() ([]byte, error) {
	type alias RecallResult
	return json.Marshal(alias(r))
}

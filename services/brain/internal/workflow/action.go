package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Budget is a bounded resource reservation surfaced to an agent (issue #32).
type Budget struct {
	// Used/Total are in the agent-supplied unit (bytes or tokens).
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// Remaining returns the unused portion, floored at zero.
func (b Budget) Remaining() int64 {
	r := b.Total - b.Used
	if r < 0 {
		return 0
	}
	return r
}

// Exhausted reports whether the reservation is spent.
func (b Budget) Exhausted() bool { return b.Remaining() == 0 && b.Total > 0 }

// Action is one machine-readable next step (issue #32).
type Action struct {
	// Verb is the codeserve verb to invoke (or "noop" for informational steps).
	Verb string `json:"verb"`
	// Summary is a short human-readable label.
	Summary string `json:"summary"`
	// Args is the partial request map the agent should complete and dispatch.
	Args map[string]any `json:"args,omitempty"`
	// Priority orders actions (lower is sooner); ties break on Verb/Summary.
	Priority int `json:"priority,omitempty"`
}

// ExpansionHandle is a resumable context pointer an agent can follow.
type ExpansionHandle struct {
	Kind   string `json:"kind"`
	Symbol string `json:"symbol,omitempty"`
	Path   string `json:"path,omitempty"`
	Handle string `json:"handle,omitempty"`
}

// ActionEnvelope wraps a verb response with the bounded, machine-readable
// metadata an agent needs to plan its next step (issue #32): available
// actions, remaining budget, freshness, expansion handles, confidence,
// coverage warnings, and verification commands.
type ActionEnvelope struct {
	Schema string `json:"schema"`
	// Verb echoes the request verb.
	Verb string `json:"verb"`
	// OK mirrors the codeserve ok flag.
	OK bool `json:"ok"`
	// Actions are the bounded suggested next steps (machine-readable).
	Actions []Action `json:"actions,omitempty"`
	// RemainingBudget reports the context budget after serving the verb.
	RemainingBudget Budget `json:"remaining_budget"`
	// Freshness discloses the freshness class of the served context.
	Freshness string `json:"freshness,omitempty"`
	// ExpansionHandles point at resumable context.
	ExpansionHandles []ExpansionHandle `json:"expansion_handles,omitempty"`
	// Confidence is the caller-supplied confidence in the served context.
	Confidence float64 `json:"confidence,omitempty"`
	// CoverageWarnings flag coverage gaps (unknown symbols, missing tests).
	CoverageWarnings []string `json:"coverage_warnings,omitempty"`
	// VerificationCommands are deterministic commands the agent can run.
	VerificationCommands []string `json:"verification_commands,omitempty"`
	// Payload carries the original verb payload verbatim (already bounded).
	Payload any `json:"payload,omitempty"`
}

// EnvelopeOptions configures envelope assembly.
type EnvelopeOptions struct {
	// Budget is the context reservation.
	Budget Budget
	// Confidence is the caller-supplied confidence.
	Confidence float64
	// Freshness classifies the served context.
	Freshness string
	// MaxActions/MaxHandles/MaxWarnings bound slice lengths.
	MaxActions  int
	MaxHandles  int
	MaxWarnings int
	// VerificationCommands are forwarded verbatim.
	VerificationCommands []string
}

// DefaultEnvelopeOptions returns conservative bounds.
func DefaultEnvelopeOptions() EnvelopeOptions {
	return EnvelopeOptions{
		Budget:      Budget{Total: 8192},
		Confidence:  0.8,
		MaxActions:  8,
		MaxHandles:  8,
		MaxWarnings: 8,
	}
}

// BuildEnvelope wraps payload with bounded action metadata. Actions and
// handles are de-duplicated and deterministically ordered so two builds from
// the same inputs produce byte-identical JSON (issue #32 determinism).
func BuildEnvelope(verb string, ok bool, payload any, actions []Action,
	handles []ExpansionHandle, warnings []string, opts EnvelopeOptions) ActionEnvelope {
	if opts.MaxActions <= 0 {
		opts.MaxActions = DefaultEnvelopeOptions().MaxActions
	}
	if opts.MaxHandles <= 0 {
		opts.MaxHandles = DefaultEnvelopeOptions().MaxHandles
	}
	if opts.MaxWarnings <= 0 {
		opts.MaxWarnings = DefaultEnvelopeOptions().MaxWarnings
	}
	// Estimate the served size from the payload so remaining budget is honest.
	used := estimateBytes(payload)
	opts.Budget.Used = used

	env := ActionEnvelope{
		Schema:               Schema,
		Verb:                 verb,
		OK:                   ok,
		RemainingBudget:      opts.Budget,
		Freshness:            opts.Freshness,
		Confidence:           opts.Confidence,
		Payload:              payload,
		VerificationCommands: dedupStrings(opts.VerificationCommands),
	}
	env.Actions = dedupActions(actions, opts.MaxActions)
	env.ExpansionHandles = dedupHandles(handles, opts.MaxHandles)
	env.CoverageWarnings = dedupStrings(warnings)[:minInt(len(dedupStrings(warnings)), opts.MaxWarnings)]
	return env
}

// MarshalJSON emits canonical envelope JSON (struct field order is stable and
// maps inside Args are normalized through SortMap).
func (e ActionEnvelope) MarshalJSON() ([]byte, error) {
	type alias ActionEnvelope
	return json.Marshal(alias(e))
}

// dedupActions returns a priority-ordered, de-duplicated action slice bounded
// by limit. Ties break on Verb then Summary so output is deterministic.
func dedupActions(in []Action, limit int) []Action {
	seen := map[string]struct{}{}
	out := make([]Action, 0, len(in))
	for _, a := range in {
		key := a.Verb + "|" + a.Summary
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if out[i].Verb != out[j].Verb {
			return out[i].Verb < out[j].Verb
		}
		return out[i].Summary < out[j].Summary
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func dedupHandles(in []ExpansionHandle, limit int) []ExpansionHandle {
	seen := map[string]struct{}{}
	out := make([]ExpansionHandle, 0, len(in))
	for _, h := range in {
		key := fmt.Sprintf("%s|%s|%s|%s", h.Kind, h.Symbol, h.Path, h.Handle)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func dedupStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// estimateBytes returns the JSON-encoded byte length of payload (0 on error).
// It is a cheap, tokenizer-free proxy for served size.
func estimateBytes(payload any) int64 {
	if payload == nil {
		return 0
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return int64(len(raw))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

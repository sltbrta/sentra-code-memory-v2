package sessionlog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Continuation is a resumable session summary (Phase 4, issue #27). It carries
// the pointers, unresolved questions, changed files, failures, next actions,
// and freshness warnings a resumed task needs to avoid repeating context, with
// provenance and an L0–L3 budget accounting (#27, #28, #29).
type Continuation struct {
	Schema string `json:"schema"`
	// Session is the resumed session id.
	Session string `json:"session,omitempty"`
	// Base identifies the repository/tree the continuation was built against.
	Base Provenance `json:"base,omitempty"`
	// BuiltAt is the UTC RFC3339Nano build time.
	BuiltAt string `json:"built_at"`
	// SourceEvents is the number of events folded into the continuation.
	SourceEvents int `json:"source_events"`
	// ReadRanges are unique L0 source pointers a resume should re-fetch.
	ReadRanges []ReadPointer `json:"read_ranges,omitempty"`
	// ChangedFiles are paths touched since the base.
	ChangedFiles []string `json:"changed_files,omitempty"`
	// UnresolvedQuestions are bounded open questions.
	UnresolvedQuestions []string `json:"unresolved_questions,omitempty"`
	// Failures are bounded failure notes to retry or avoid.
	Failures []string `json:"failures,omitempty"`
	// NextActions are bounded suggested next steps (machine-readable).
	NextActions []string `json:"next_actions,omitempty"`
	// FreshnessWarnings disclose stale/superseded content (issue #28).
	FreshnessWarnings []FreshnessWarning `json:"freshness_warnings,omitempty"`
	// Budgets reports the per-tier byte budget used vs reserved.
	Budgets BudgetReport `json:"budgets"`
	// Stale reports whether any included content is stale or as_of.
	Stale bool `json:"stale"`
	// Superseded reports whether superseded content was included by request.
	Superseded bool `json:"superseded,omitempty"`
}

// ReadPointer is a unique (path, range) pointer with its freshness and tier.
type ReadPointer struct {
	Path      string     `json:"path,omitempty"`
	Range     Range      `json:"range,omitempty"`
	Symbol    string     `json:"symbol,omitempty"`
	Freshness Freshness  `json:"freshness,omitempty"`
	Tier      BudgetTier `json:"tier,omitempty"`
}

// FreshnessWarning discloses a stale/as_of/superseded item (issue #28).
type FreshnessWarning struct {
	Freshness Freshness `json:"freshness"`
	Path      string    `json:"path,omitempty"`
	Symbol    string    `json:"symbol,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// BudgetReport accounts per-tier bytes used and the reservation cap.
type BudgetReport struct {
	// Reserved is the caller-supplied per-tier byte budget.
	Reserved Budgets `json:"reserved"`
	// Used is the per-tier bytes actually included.
	Used Budgets `json:"used"`
	// Truncated reports whether any tier exceeded its reservation.
	Truncated bool `json:"truncated,omitempty"`
}

// ContinuationOptions controls admission, freshness disclosure, and budgets.
type ContinuationOptions struct {
	// Budgets reserves bytes per tier. A zero tier is unlimited; lower tiers
	// are admitted before higher tiers.
	Budgets Budgets
	// IncludeSuperseded opts into superseded content (excluded by default).
	IncludeSuperseded bool
	// IncludeStale retains stale content (disclosed in FreshnessWarnings).
	IncludeStale bool
	// MaxQuestions/MaxFailures/MaxActions bound slice lengths.
	MaxQuestions int
	MaxFailures  int
	MaxActions   int
	// MaxReadRanges bounds the number of retained read pointers.
	MaxReadRanges int
}

// DefaultContinuationOptions returns conservative bounds suitable for a resume.
func DefaultContinuationOptions() ContinuationOptions {
	return ContinuationOptions{
		Budgets:       Budgets{L0: 4096, L1: 2048, L2: 1024, L3: 512},
		MaxQuestions:  16,
		MaxFailures:   16,
		MaxActions:    16,
		MaxReadRanges: 64,
	}
}

// BuildContinuation folds events into a resumable continuation. Freshness and
// supersession rules (#28) decide inclusion: timeless/pointer pointers are L0,
// as_of is L1, stale is L2 (only with IncludeStale), superseded is excluded
// unless IncludeSuperseded. Provenance is preserved on every pointer (#29).
func BuildContinuation(events []Event, base Provenance, opts ContinuationOptions, now time.Time) (Continuation, error) {
	if opts.MaxQuestions <= 0 {
		opts.MaxQuestions = 16
	}
	if opts.MaxFailures <= 0 {
		opts.MaxFailures = 16
	}
	if opts.MaxActions <= 0 {
		opts.MaxActions = 16
	}
	if opts.MaxReadRanges <= 0 {
		opts.MaxReadRanges = 64
	}

	c := Continuation{
		Schema:       Schema,
		Base:         base,
		BuiltAt:      now.UTC().Format(time.RFC3339Nano),
		SourceEvents: len(events),
		Budgets:      BudgetReport{Reserved: opts.Budgets},
	}

	type ptrKey struct {
		path  string
		start int
		end   int
	}
	seen := map[ptrKey]struct{}{}
	used := Budgets{}
	addTier := func(tier BudgetTier, bytes int) bool {
		switch tier {
		case TierL0:
			used.L0 += bytes
		case TierL1:
			used.L1 += bytes
		case TierL2:
			used.L2 += bytes
		case TierL3:
			used.L3 += bytes
		}
		cap := 0
		switch tier {
		case TierL0:
			cap = opts.Budgets.L0
		case TierL1:
			cap = opts.Budgets.L1
		case TierL2:
			cap = opts.Budgets.L2
		case TierL3:
			cap = opts.Budgets.L3
		}
		if cap > 0 {
			cur := 0
			switch tier {
			case TierL0:
				cur = used.L0
			case TierL1:
				cur = used.L1
			case TierL2:
				cur = used.L2
			case TierL3:
				cur = used.L3
			}
			if cur > cap {
				c.Budgets.Truncated = true
				return false
			}
		}
		return true
	}

	for _, ev := range events {
		fresh := ev.Freshness
		if fresh == "" {
			fresh = FreshnessAsOf
		}
		if fresh == FreshnessSuperseded && !opts.IncludeSuperseded {
			continue
		}
		if fresh == FreshnessStale && !opts.IncludeStale {
			if ev.Provenance.Path != "" || ev.Provenance.Symbol != "" {
				c.FreshnessWarnings = appendUniqueWarning(c.FreshnessWarnings, FreshnessWarning{
					Freshness: FreshnessStale, Path: ev.Provenance.Path,
					Symbol: ev.Provenance.Symbol, Reason: "excluded by default"})
			}
			continue
		}
		if fresh == FreshnessAsOf || fresh == FreshnessStale {
			c.Stale = true
			if ev.Provenance.Path != "" || ev.Provenance.Symbol != "" {
				c.FreshnessWarnings = appendUniqueWarning(c.FreshnessWarnings, FreshnessWarning{
					Freshness: fresh, Path: ev.Provenance.Path, Symbol: ev.Provenance.Symbol})
			}
		}
		if fresh == FreshnessSuperseded {
			c.Superseded = true
		}
		tier := tierFor(fresh)
		if ev.Provenance.Path != "" && len(c.ReadRanges) < opts.MaxReadRanges {
			key := ptrKey{ev.Provenance.Path, ev.Provenance.Range.Start, ev.Provenance.Range.End}
			if _, ok := seen[key]; !ok {
				bytes := len(ev.Provenance.Path) + ev.Provenance.Range.End
				if addTier(tier, bytes) {
					seen[key] = struct{}{}
					c.ReadRanges = append(c.ReadRanges, ReadPointer{
						Path: ev.Provenance.Path, Range: ev.Provenance.Range,
						Symbol: ev.Provenance.Symbol, Freshness: fresh, Tier: tier,
					})
				}
			}
		}
		switch ev.Kind {
		case KindEdit, KindRefresh:
			if ev.Provenance.Path != "" {
				c.ChangedFiles = appendUniqueString(c.ChangedFiles, ev.Provenance.Path)
			}
		case KindFailure:
			if ev.FreeText != "" && len(c.Failures) < opts.MaxFailures {
				c.Failures = append(c.Failures, ev.FreeText)
			}
		case KindCompletion:
			if ev.FreeText != "" && len(c.NextActions) < opts.MaxActions {
				c.NextActions = append(c.NextActions, "verify completion: "+ev.FreeText)
			}
		}
		if len(ev.CoverageWarnings) > 0 {
			for _, w := range ev.CoverageWarnings {
				if len(c.UnresolvedQuestions) < opts.MaxQuestions {
					c.UnresolvedQuestions = appendUniqueString(c.UnresolvedQuestions, w)
				}
			}
		}
	}
	c.Budgets.Used = used

	// Deterministic ordering so the packet is reproducible.
	sort.Slice(c.ReadRanges, func(i, j int) bool {
		if c.ReadRanges[i].Path != c.ReadRanges[j].Path {
			return c.ReadRanges[i].Path < c.ReadRanges[j].Path
		}
		if c.ReadRanges[i].Range.Start != c.ReadRanges[j].Range.Start {
			return c.ReadRanges[i].Range.Start < c.ReadRanges[j].Range.Start
		}
		return c.ReadRanges[i].Range.End < c.ReadRanges[j].Range.End
	})
	sort.Strings(c.ChangedFiles)
	sort.Strings(c.UnresolvedQuestions)
	sort.Strings(c.Failures)
	sort.Strings(c.NextActions)
	return c, nil
}

// Fresh classifies a fact's freshness against a base digest and observed tree.
// It is the supersession/freshness rule used by continuation and recall (#28):
// a pointer whose base digest differs from the current digest is stale; a
// fact explicitly marked superseded is excluded by default.
func Fresh(mark Freshness, baseDigest, observedDigest string) Freshness {
	if mark == FreshnessSuperseded {
		return FreshnessSuperseded
	}
	if mark == FreshnessTimeless {
		return FreshnessTimeless
	}
	if baseDigest != "" && observedDigest != "" && baseDigest != observedDigest {
		return FreshnessStale
	}
	if mark == FreshnessPointer && baseDigest != "" {
		// Pointer facts are only as fresh as their base; without an observed
		// digest they are as_of the base.
		if observedDigest == "" {
			return FreshnessAsOf
		}
		return FreshnessPointer
	}
	if mark == "" {
		return FreshnessAsOf
	}
	return mark
}

// MarshalJSON emits canonical continuation JSON.
func (c Continuation) MarshalJSON() ([]byte, error) {
	type alias Continuation
	return json.Marshal(alias(c))
}

// appendUniqueString appends s when not already present, preserving order.
func appendUniqueString(dst []string, s string) []string {
	for _, v := range dst {
		if v == s {
			return dst
		}
	}
	return append(dst, s)
}

// appendUniqueWarning appends w when an equal warning is not present.

// appendUniqueWarning appends w when an equal warning is not present.
func appendUniqueWarning(dst []FreshnessWarning, w FreshnessWarning) []FreshnessWarning {
	for _, v := range dst {
		if v == w {
			return dst
		}
	}
	return append(dst, w)
}

// IsStale reports whether a continuation carries any stale or as_of content.
func (c Continuation) IsStale() bool { return c.Stale }

// Describe renders a stable one-line freshness disclosure (issue #28).
func (w FreshnessWarning) Describe() string {
	reason := w.Reason
	if reason == "" {
		reason = string(w.Freshness)
	}
	if w.Path != "" && w.Symbol != "" {
		return fmt.Sprintf("%s: %s (%s)", w.Freshness, w.Symbol, w.Path)
	}
	if w.Path != "" {
		return fmt.Sprintf("%s: %s (%s)", w.Freshness, w.Path, reason)
	}
	return strings.TrimSpace(fmt.Sprintf("%s: %s", w.Freshness, reason))
}

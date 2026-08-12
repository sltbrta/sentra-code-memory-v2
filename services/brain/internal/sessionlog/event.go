package sessionlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Kind is a stable session-event class (snake_case).
type Kind string

// Supported event kinds. The set is closed: an unknown kind is rejected on
// append and on strict replay so silent schema drift fails closed.
const (
	KindTaskStart     Kind = "task_start"
	KindContextServed Kind = "context_served"
	KindRead          Kind = "read"
	KindRefresh       Kind = "refresh"
	KindEdit          Kind = "edit"
	KindTest          Kind = "test"
	KindFailure       Kind = "failure"
	KindCompaction    Kind = "compaction"
	KindCompletion    Kind = "completion"
)

// AllKinds lists every supported event kind in canonical order.
func AllKinds() []Kind {
	return []Kind{
		KindTaskStart, KindContextServed, KindRead, KindRefresh,
		KindEdit, KindTest, KindFailure, KindCompaction, KindCompletion,
	}
}

func init() {
	// Fail fast if a future edit to AllKinds duplicates or reorders a kind in
	// a way that breaks the closed-set guarantee.
	seen := make(map[Kind]struct{}, len(allKindSet))
	for _, k := range AllKinds() {
		if _, ok := seen[k]; ok {
			panic(fmt.Sprintf("sessionlog: duplicate kind %q in AllKinds", k))
		}
		if _, ok := allKindSet[k]; !ok {
			panic(fmt.Sprintf("sessionlog: kind %q missing from set", k))
		}
		seen[k] = struct{}{}
	}
}

var allKindSet = map[Kind]struct{}{
	KindTaskStart: {}, KindContextServed: {}, KindRead: {}, KindRefresh: {},
	KindEdit: {}, KindTest: {}, KindFailure: {}, KindCompaction: {},
	KindCompletion: {},
}

// Valid reports whether k is a supported event kind.
func (k Kind) Valid() bool {
	_, ok := allKindSet[k]
	return ok
}

// Freshness classifies how durable a fact is (Phase 4, issue #28).
type Freshness string

const (
	// FreshnessTimeless facts are unlikely to change (language semantics,
	// stable public symbol identity).
	FreshnessTimeless Freshness = "timeless"
	// FreshnessAsOf facts are valid as of a base ref/time and must disclose it.
	FreshnessAsOf Freshness = "as_of"
	// FreshnessPointer facts are references to source whose freshness follows
	// the referenced file; they carry a base digest that must be rechecked.
	FreshnessPointer Freshness = "pointer"
	// FreshnessStale facts are known outdated but retained for context.
	FreshnessStale Freshness = "stale"
	// FreshnessSuperseded facts have been replaced and are excluded by default.
	FreshnessSuperseded Freshness = "superseded"
)

var freshnessSet = map[Freshness]struct{}{
	FreshnessTimeless: {}, FreshnessAsOf: {}, FreshnessPointer: {},
	FreshnessStale: {}, FreshnessSuperseded: {},
}

// Valid reports whether f is a supported freshness classification.
func (f Freshness) Valid() bool {
	_, ok := freshnessSet[f]
	return ok
}

// ExcludedByDefault reports whether f is suppressed from continuation/recall
// unless the caller explicitly opts into superseded content.
func (f Freshness) ExcludedByDefault() bool {
	return f == FreshnessSuperseded
}

// Range is a half-open source coordinate [Start, End). Start==End==0 means
// "whole file" / "pointer only".
type Range struct {
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`
}

// Valid reports the range is well formed.
func (r Range) Valid() bool {
	return r.Start >= 0 && r.End >= 0 && (r.End == 0 || r.Start <= r.End)
}

// Provenance is the pointer-and-attribution metadata every durable event must
// carry (Phase 4, issue #29). It records where a fact came from so it can be
// rechecked, disclosed, and safely invalidated; it never stores source bytes.
type Provenance struct {
	// Repository is the canonical remote URL or "local" when none is known.
	Repository string `json:"repository,omitempty"`
	// Tree is the base ref the fact was established against (e.g. git HEAD).
	Tree string `json:"tree,omitempty"`
	// BaseDigest is the sha256:hex content digest of Path at Tree (16-byte
	// truncated, matching codecrawl.FileHash). Empty for whole-tree facts.
	BaseDigest string `json:"base_digest,omitempty"`
	// Path is the repo-relative source path the fact points at.
	Path string `json:"path,omitempty"`
	// Range locates the fact inside Path.
	Range Range `json:"range,omitempty"`
	// Symbol names the referenced/edited symbol when known.
	Symbol string `json:"symbol,omitempty"`
	// Handle is a stable opaque expansion handle (e.g. a contextpack handle).
	Handle string `json:"handle,omitempty"`
	// Confidence is the caller-supplied confidence in [0,1].
	Confidence float64 `json:"confidence,omitempty"`
}

// BudgetTier identifies a retention tier used by continuation/compaction
// packets (Phase 4, issue #27). Lower tiers are cheaper and safer to retain.
type BudgetTier string

const (
	// TierL0 immutable pointers — always retained.
	TierL0 BudgetTier = "L0"
	// TierL1 fresh volatile facts — retained while fresh.
	TierL1 BudgetTier = "L1"
	// TierL2 stale-but-valid facts — retained with disclosure.
	TierL2 BudgetTier = "L2"
	// TierL3 compacted summaries — retained as folded summaries only.
	TierL3 BudgetTier = "L3"
)

var tierSet = map[BudgetTier]struct{}{
	TierL0: {}, TierL1: {}, TierL2: {}, TierL3: {},
}

// Budgets are per-tier byte budgets for a packet. Zero means "no reservation".
type Budgets struct {
	L0 int `json:"L0,omitempty"`
	L1 int `json:"L1,omitempty"`
	L2 int `json:"L2,omitempty"`
	L3 int `json:"L3,omitempty"`
}

// Total is the sum of all tiers.
func (b Budgets) Total() int {
	return b.L0 + b.L1 + b.L2 + b.L3
}

// tierFor maps a freshness classification to its default retention tier.
func tierFor(f Freshness) BudgetTier {
	switch f {
	case FreshnessTimeless, FreshnessPointer:
		return TierL0
	case FreshnessAsOf:
		return TierL1
	case FreshnessStale:
		return TierL2
	case FreshnessSuperseded:
		return TierL3
	default:
		return TierL1
	}
}

// Event is one persisted session line.
//
// Privacy contract: Path/Range/Symbol/Handle/Tree are pointers, not content.
// FreeText is the only free-form field and is capped by Sanitize (default
// MaxFreeTextBytes). Events are never required to read source.
type Event struct {
	// Schema is the on-disk contract revision; emitted for forward safety.
	Schema string `json:"schema"`
	// ID is a content-derived deterministic identifier (sha256 prefix).
	ID string `json:"id"`
	// Seq is the monotonic append sequence assigned by the Writer.
	Seq uint64 `json:"seq"`
	// Session groups events for one resumed task.
	Session string `json:"session,omitempty"`
	// Kind is the event class.
	Kind Kind `json:"kind"`
	// Time is the UTC RFC3339Nano write time.
	Time string `json:"time"`
	// Verb is the codeserve verb the step wrapped, when applicable.
	Verb string `json:"verb,omitempty"`
	// Provenance is the pointer/attribution metadata (durable kinds require it).
	Provenance Provenance `json:"provenance,omitempty"`
	// Freshness classifies durability.
	Freshness Freshness `json:"freshness,omitempty"`
	// PredictedDigest is the sha256:hex digest the agent predicted before edit.
	PredictedDigest string `json:"predicted_digest,omitempty"`
	// ObservedDigest is the sha256:hex digest measured after the edit.
	ObservedDigest string `json:"observed_digest,omitempty"`
	// Counts are optional integer side-effects (files changed, tests run, ...).
	Counts map[string]int64 `json:"counts,omitempty"`
	// CoverageWarnings are short, bounded human notes about coverage gaps.
	CoverageWarnings []string `json:"coverage_warnings,omitempty"`
	// FreeText is capped, scrubbed free-form text. It never holds file content.
	FreeText string `json:"free_text,omitempty"`
	// Folded is set on compaction events: the number of source events folded.
	Folded int `json:"folded,omitempty"`
}

// EventID derives a deterministic 16-byte-hex identifier from an event's
// stable fields. It is reproducible from the persisted stream (issue #31).
func EventID(seq uint64, session, kind, timeISO string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s", seq, session, kind, timeISO)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// MaxFreeTextBytes bounds the FreeText field. Privacy-safe events never carry
// raw source; this caps any caller-supplied note.
const MaxFreeTextBytes = 512

// Sanitize returns ev with FreeText trimmed to MaxFreeTextBytes and a
// deterministic Counts map. It does not mutate the receiver.
func (ev Event) Sanitize() Event {
	out := ev
	if len(out.FreeText) > MaxFreeTextBytes {
		out.FreeText = out.FreeText[:MaxFreeTextBytes]
	}
	if len(out.Counts) > 0 {
		out.Counts = canonicalCounts(out.Counts)
	}
	if len(out.CoverageWarnings) > 0 {
		warned := make([]string, 0, len(out.CoverageWarnings))
		for _, w := range out.CoverageWarnings {
			if len(w) > MaxFreeTextBytes {
				w = w[:MaxFreeTextBytes]
			}
			warned = append(warned, w)
		}
		out.CoverageWarnings = warned
	}
	return out
}

// canonicalCounts returns a copy of counts with lexical key order so marshaled
// output is deterministic.
func canonicalCounts(in map[string]int64) map[string]int64 {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]int64, len(in))
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}

// Validate checks an event's internal invariants. It does not check provenance
// requirements (see RequireProvenance) so callers can validate structural
// soundness independently of admission policy.
func (ev Event) Validate() error {
	if err := ev.validateFields(); err != nil {
		return err
	}
	if ev.Seq == 0 {
		return errors.New("sessionlog: seq required")
	}
	if ev.Time == "" {
		return errors.New("sessionlog: time required")
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.Time); err != nil {
		return fmt.Errorf("sessionlog: time must be RFC3339Nano: %w", err)
	}
	return nil
}

// validateFields checks caller-controlled invariants only (kind, schema,
// freshness, range, confidence, free text, path). Seq/Time are sealed by the
// Writer, so Append validates fields before sealing and Validate after.
func (ev Event) validateFields() error {
	if !ev.Kind.Valid() {
		return fmt.Errorf("sessionlog: unknown kind %q", ev.Kind)
	}
	if ev.Schema != "" && ev.Schema != Schema {
		return fmt.Errorf("sessionlog: unsupported schema %q", ev.Schema)
	}
	if ev.Freshness != "" && !ev.Freshness.Valid() {
		return fmt.Errorf("sessionlog: unknown freshness %q", ev.Freshness)
	}
	if !ev.Provenance.Range.Valid() {
		return errors.New("sessionlog: invalid range")
	}
	if ev.Provenance.Confidence < 0 || ev.Provenance.Confidence > 1 {
		return errors.New("sessionlog: confidence must be in [0,1]")
	}
	if len(ev.FreeText) > MaxFreeTextBytes {
		return fmt.Errorf("sessionlog: free_text exceeds %d bytes", MaxFreeTextBytes)
	}
	if ev.Provenance.Path != "" {
		if err := validatePath(ev.Provenance.Path); err != nil {
			return err
		}
	}
	return nil
}

// requireProvenanceKinds are the event classes that must carry provenance on
// admission (durable facts: edits, compactions, context served). Reads and
// refreshes carry pointers when available but are not rejected without them.
var requireProvenanceKinds = map[Kind]struct{}{
	KindEdit: {}, KindCompaction: {}, KindContextServed: {},
}

// RequireProvenance reports whether kind must carry provenance (issue #29).
func RequireProvenance(k Kind) bool {
	_, ok := requireProvenanceKinds[k]
	return ok
}

// ValidateProvenance enforces the provenance-first admission rule for durable
// kinds: a repository/tree identifier and at least one locator (path or symbol
// or handle) are required so the fact is traceable and safely invalidatable.
func ValidateProvenance(ev Event) error {
	if !RequireProvenance(ev.Kind) {
		return nil
	}
	p := ev.Provenance
	if strings.TrimSpace(p.Repository) == "" && strings.TrimSpace(p.Tree) == "" {
		return fmt.Errorf("sessionlog: %s requires repository or tree provenance", ev.Kind)
	}
	if strings.TrimSpace(p.Path) == "" && strings.TrimSpace(p.Symbol) == "" &&
		strings.TrimSpace(p.Handle) == "" {
		return fmt.Errorf("sessionlog: %s requires a path, symbol, or handle locator", ev.Kind)
	}
	if p.Path != "" {
		if err := validatePath(p.Path); err != nil {
			return err
		}
	}
	return nil
}

// validatePath rejects absolute paths and path escapes. Paths are repo-relative
// (forward slashes); the codeserve contract uses filepath.ToSlash everywhere,
// so a cleaned relative path with no ".." segment is the canonical pointer.
func validatePath(p string) error {
	if p == "" {
		return errors.New("sessionlog: empty path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("sessionlog: absolute path rejected: %q", p)
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("sessionlog: backslash path rejected: %q", p)
	}
	// Reject any URL-encoded percent that decodes to an escape — defense in
	// depth for pointers that arrive from untrusted callers.
	if strings.Contains(p, "%") {
		if _, err := url.PathUnescape(p); err != nil {
			return fmt.Errorf("sessionlog: invalid path encoding: %w", err)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("sessionlog: path escape rejected: %q", p)
		}
	}
	return nil
}

// MarshalJSON emits canonical JSON: struct field order is deterministic and
// Counts uses lexical key order via Sanitize.
func (ev Event) MarshalJSON() ([]byte, error) {
	type alias Event
	return json.Marshal(alias(ev.Sanitize()))
}

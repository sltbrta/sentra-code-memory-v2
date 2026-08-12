package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Report is a reproducible, content-safe local task report (issue #33). It
// summarizes what an agent did: context served, impact, tests, edits, graph
// changes, unknown coverage, savings, verification, and next actions.
//
// Content safety: edits and graph changes carry digests and paths only — no
// source bytes. Field order is fixed and slice fields are sorted so the same
// inputs always produce byte-identical JSON.
type Report struct {
	Schema string `json:"schema"`
	// Task identifies the work the report summarizes.
	Task string `json:"task,omitempty"`
	// Session links the report to a sessionlog session.
	Session string `json:"session,omitempty"`
	// Base is the repository/tree the report was built against.
	Base string `json:"base,omitempty"`
	// GeneratedAt is the UTC RFC3339Nano build time.
	GeneratedAt string `json:"generated_at"`
	// ContextServed summarizes bounded context (bytes/tokens/units).
	ContextServed ReportCounts `json:"context_served,omitempty"`
	// Impact summarizes blast-radius findings.
	Impact ImpactSummary `json:"impact,omitempty"`
	// Tests summarizes verification runs.
	Tests TestSummary `json:"tests,omitempty"`
	// Edits lists changed files with digests (no source bytes).
	Edits []FileEdit `json:"edits,omitempty"`
	// GraphChanges lists symbol-graph deltas by file with digests.
	GraphChanges []GraphChange `json:"graph_changes,omitempty"`
	// UnknownCoverage lists symbols/files with no coverage finding.
	UnknownCoverage []string `json:"unknown_coverage,omitempty"`
	// Savings summarizes token/byte savings (savings-ledger compatible).
	Savings ReportCounts `json:"savings,omitempty"`
	// Verification lists deterministic commands and their pass/fail status.
	Verification []Verification `json:"verification,omitempty"`
	// NextActions are bounded suggested next steps.
	NextActions []string `json:"next_actions,omitempty"`
	// Digest is the sha256:hex of the report body (reproducibility receipt).
	Digest string `json:"digest"`
}

// ReportCounts are integer totals with a stable lexical key order.
type ReportCounts map[string]int64

// ImpactSummary is the blast-radius receipt.
type ImpactSummary struct {
	Severity string   `json:"severity,omitempty"`
	Direct   []string `json:"direct,omitempty"`
	Closure  []string `json:"closure,omitempty"`
}

// TestSummary is the verification receipt.
type TestSummary struct {
	Run    int      `json:"run,omitempty"`
	Passed int      `json:"passed,omitempty"`
	Failed int      `json:"failed,omitempty"`
	Files  []string `json:"files,omitempty"`
}

// FileEdit records one changed file with before/after digests (no source).
type FileEdit struct {
	Path         string `json:"path"`
	BeforeDigest string `json:"before_digest,omitempty"`
	AfterDigest  string `json:"after_digest,omitempty"`
	LinesAdded   int    `json:"lines_added,omitempty"`
	LinesRemoved int    `json:"lines_removed,omitempty"`
}

// GraphChange records a per-file symbol-graph delta with a digest.
type GraphChange struct {
	Path         string `json:"path"`
	AddedEdges   int    `json:"added_edges,omitempty"`
	RemovedEdges int    `json:"removed_edges,omitempty"`
	Digest       string `json:"digest,omitempty"`
}

// Verification records one deterministic command and its outcome.
type Verification struct {
	Command string `json:"command"`
	Passed  bool   `json:"passed"`
	Note    string `json:"note,omitempty"`
}

// BuildOptions configure report assembly and digest computation.
type BuildOptions struct {
	Task     string
	Session  string
	Base     string
	MaxSlice int
}

// Build assembles a deterministic report from the supplied evidence. It sorts
// every slice field, normalizes count maps, and computes a content Digest so
// the same inputs always yield byte-identical JSON (issue #33 reproducibility).
func Build(r Report, opts BuildOptions) Report {
	if opts.MaxSlice <= 0 {
		opts.MaxSlice = 32
	}
	r.Schema = Schema
	if opts.Task != "" {
		r.Task = opts.Task
	}
	if opts.Session != "" {
		r.Session = opts.Session
	}
	if opts.Base != "" {
		r.Base = opts.Base
	}
	r.ContextServed = canonical(r.ContextServed)
	r.Savings = canonical(r.Savings)
	sort.Strings(r.Impact.Direct)
	sort.Strings(r.Impact.Closure)
	sort.Strings(r.Tests.Files)
	r.Edits = sortEdits(r.Edits)
	r.GraphChanges = sortGraph(r.GraphChanges)
	sort.Strings(r.UnknownCoverage)
	sort.Strings(r.NextActions)
	r.Verification = sortVerification(r.Verification)
	r = bound(r, opts.MaxSlice)
	r.Digest = digest(r)
	return r
}

// digest computes sha256:hex over the report with an empty Digest field so the
// receipt is independent of itself and reproducible across builds.
func digest(r Report) string {
	r.Digest = ""
	body, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func canonical(in ReportCounts) ReportCounts {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(ReportCounts, len(in))
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}

func sortEdits(in []FileEdit) []FileEdit {
	out := append([]FileEdit(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].AfterDigest < out[j].AfterDigest
	})
	return out
}

func sortGraph(in []GraphChange) []GraphChange {
	out := append([]GraphChange(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func sortVerification(in []Verification) []Verification {
	out := append([]Verification(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Command < out[j].Command })
	return out
}

func bound(r Report, n int) Report {
	boundStr := func(s []string) []string {
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	r.Impact.Direct = boundStr(r.Impact.Direct)
	r.Impact.Closure = boundStr(r.Impact.Closure)
	r.Tests.Files = boundStr(r.Tests.Files)
	r.UnknownCoverage = boundStr(r.UnknownCoverage)
	r.NextActions = boundStr(r.NextActions)
	if len(r.Edits) > n {
		r.Edits = r.Edits[:n]
	}
	if len(r.GraphChanges) > n {
		r.GraphChanges = r.GraphChanges[:n]
	}
	if len(r.Verification) > n {
		r.Verification = r.Verification[:n]
	}
	return r
}

// MarshalJSON emits canonical report JSON (count maps use lexical order).
func (r Report) MarshalJSON() ([]byte, error) {
	type alias Report
	cpy := alias(r)
	cpy.ContextServed = canonical(cpy.ContextServed)
	cpy.Savings = canonical(cpy.Savings)
	return json.Marshal(cpy)
}

// String renders a stable one-line digest line for CLI/log output.
func (r Report) String() string {
	return fmt.Sprintf("report task=%q digest=%s edits=%d tests=%d/%d unknown=%d",
		r.Task, r.Digest, len(r.Edits), r.Tests.Passed, r.Tests.Run, len(r.UnknownCoverage))
}

package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RejectReason is a stable machine-readable ChangeSet rejection class.
type RejectReason string

const (
	// RejectStaleBase: a file's current digest differs from the frozen base.
	RejectStaleBase RejectReason = "stale_base"
	// RejectPathEscape: an edit path escapes the repository root.
	RejectPathEscape RejectReason = "path_escape"
	// RejectOverlap: two edits overlap the same file range.
	RejectOverlap RejectReason = "overlapping_edits"
	// RejectPartialFailure: an edit's predicted digest did not match observed.
	RejectPartialFailure RejectReason = "partial_failure"
	// RejectEmpty: the changeset has no edits.
	RejectEmpty RejectReason = "empty_changeset"
	// RejectBadRange: an edit range is malformed.
	RejectBadRange RejectReason = "bad_range"
	// RejectMissingDigest: required base or predicted/observed evidence is absent.
	RejectMissingDigest RejectReason = "missing_digest"
)

// EditRange is a half-open source coordinate [Start, End).
type EditRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Valid reports the range is well formed.
func (r EditRange) Valid() bool { return r.Start >= 0 && r.End >= 0 && r.Start <= r.End }

// Overlaps reports whether two ranges share any line.
func (r EditRange) Overlaps(o EditRange) bool {
	if r.End <= o.Start || o.End <= r.Start {
		return false
	}
	return true
}

// CandidateEdit is one isolated candidate edit (issue #34).
type CandidateEdit struct {
	// Path is the repo-relative target file (forward slashes, no escapes).
	Path string `json:"path"`
	// Range locates the edited region.
	Range EditRange `json:"range"`
	// Symbol names the edited symbol when known.
	Symbol string `json:"symbol,omitempty"`
	// BaseDigest is the frozen digest of the file at the change-set base.
	BaseDigest string `json:"base_digest,omitempty"`
	// PredictedDigest is the digest the agent expects after applying the edit.
	PredictedDigest string `json:"predicted_digest,omitempty"`
	// ObservedDigest is the digest measured after the edit applied.
	ObservedDigest string `json:"observed_digest,omitempty"`
}

// ChangeSet is a fail-closed candidate validation unit (issue #34). It freezes
// a base (tree ref + per-file base digests), carries isolated candidate edits,
// and validates them atomically. Missing digest evidence is rejected, as are
// stale bases, path escapes, overlaps, and partial failures, so an edit is
// never silently reported as fully verified.
type ChangeSet struct {
	Schema string `json:"schema"`
	// Base is the frozen tree ref (e.g. git HEAD).
	Base string `json:"base,omitempty"`
	// BaseDigests are the frozen per-file content digests at Base.
	BaseDigests map[string]string `json:"base_digests,omitempty"`
	// Edits are the isolated candidate edits.
	Edits []CandidateEdit `json:"edits"`
	// VerificationCommands are deterministic commands for the verification gate.
	VerificationCommands []string `json:"verification_commands,omitempty"`
}

// ValidationResult is the ChangeSet receipt (issue #34 receipts).
type ValidationResult struct {
	Schema string `json:"schema"`
	// Accepted reports whether the whole changeset passed every gate.
	Accepted bool `json:"accepted"`
	// Rejected lists the rejection reasons (empty when Accepted).
	Rejected []RejectReason `json:"rejected,omitempty"`
	// Reasons are human-readable detail strings parallel to Rejected.
	Reasons []string `json:"reasons,omitempty"`
	// PerEdit reports each edit's individual outcome.
	PerEdit []EditResult `json:"per_edit,omitempty"`
	// GraphDelta summarizes predicted vs observed per file.
	GraphDelta []GraphDelta `json:"graph_delta,omitempty"`
	// VerificationCommands forwarded from the ChangeSet.
	VerificationCommands []string `json:"verification_commands,omitempty"`
	// Base echoes the frozen base.
	Base string `json:"base,omitempty"`
	// Digest is a reproducibility receipt over the validation result.
	Digest string `json:"digest"`
}

// EditResult is one edit's outcome.
type EditResult struct {
	Path        string `json:"path"`
	Symbol      string `json:"symbol,omitempty"`
	Stale       bool   `json:"stale,omitempty"`
	DigestMatch bool   `json:"digest_match"`
	Note        string `json:"note,omitempty"`
}

// GraphDelta is the predicted-vs-observed per-file summary.
type GraphDelta struct {
	Path             string `json:"path"`
	PredictedMatches int    `json:"predicted_matches,omitempty"`
	ObservedMatches  int    `json:"observed_matches,omitempty"`
	Diverged         bool   `json:"diverged"`
}

// Digest computes a sha256:hex(16) content digest (matches codecrawl.FileHash
// truncation width). It is the canonical content identity used by base
// freezing and predicted/observed comparison.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// Validate applies every fail-closed gate and returns the receipt. It is pure:
// it never reads or writes files; callers supply base digests and observed
// digests. Validation order is deterministic so the receipt is reproducible.
//
// Gates (issue #34): empty set, path escape, bad range, overlapping edits,
// missing/stale base evidence, and partial failure (predicted != observed).
// Any gate failure rejects the whole set; partial work never lands.
func (cs ChangeSet) Validate() ValidationResult {
	res := ValidationResult{
		Schema:               Schema,
		Base:                 cs.Base,
		VerificationCommands: dedupStrings(cs.VerificationCommands),
	}

	if len(cs.Edits) == 0 {
		res.Rejected = append(res.Rejected, RejectEmpty)
		res.Reasons = append(res.Reasons, "changeset has no edits")
		return finalizeValidation(res)
	}

	// Per-edit checks. We collect all failures (fail closed, but report every
	// problem so the agent can fix them together) before deciding acceptance.
	byPath := map[string][]CandidateEdit{}
	for i, edit := range cs.Edits {
		er := EditResult{Path: edit.Path, Symbol: edit.Symbol}
		if err := validateChangePath(edit.Path); err != nil {
			res.Rejected = append(res.Rejected, RejectPathEscape)
			res.Reasons = append(res.Reasons, fmt.Sprintf("edit %d: %v", i, err))
			er.Note = "path escape rejected"
		}
		if !edit.Range.Valid() {
			res.Rejected = append(res.Rejected, RejectBadRange)
			res.Reasons = append(res.Reasons, fmt.Sprintf("edit %d: bad range %+v", i, edit.Range))
			er.Note = "bad range"
		}
		// Stale base: both the frozen and planned digests are required evidence;
		// absence fails closed rather than being treated as a match.
		frozen := cs.BaseDigests[edit.Path]
		if frozen == "" || edit.BaseDigest == "" {
			res.Rejected = append(res.Rejected, RejectMissingDigest)
			res.Reasons = append(res.Reasons, fmt.Sprintf("edit %d %s: base digest evidence required", i, edit.Path))
			er.Note = "base digest evidence missing"
		} else if frozen != edit.BaseDigest {
			res.Rejected = append(res.Rejected, RejectStaleBase)
			res.Reasons = append(res.Reasons,
				fmt.Sprintf("edit %d %s: stale base (frozen %s != planned %s)",
					i, edit.Path, shortDigest(frozen), shortDigest(edit.BaseDigest)))
			er.Stale = true
			er.Note = "base digest moved since freeze"
		}
		// Partial failure: predicted and observed evidence are both required and
		// must match.
		match := false
		if edit.PredictedDigest == "" || edit.ObservedDigest == "" {
			res.Rejected = append(res.Rejected, RejectMissingDigest)
			res.Reasons = append(res.Reasons, fmt.Sprintf("edit %d %s: predicted and observed digest evidence required", i, edit.Path))
			if er.Note == "" {
				er.Note = "predicted or observed digest evidence missing"
			}
		} else {
			match = true
			if edit.PredictedDigest != edit.ObservedDigest {
				match = false
				res.Rejected = append(res.Rejected, RejectPartialFailure)
				res.Reasons = append(res.Reasons,
					fmt.Sprintf("edit %d %s: predicted %s != observed %s",
						i, edit.Path, shortDigest(edit.PredictedDigest), shortDigest(edit.ObservedDigest)))
				er.Note = "predicted digest did not match observed"
			}
		}
		er.DigestMatch = match && frozen != "" && edit.BaseDigest != ""
		res.PerEdit = append(res.PerEdit, er)
		byPath[edit.Path] = append(byPath[edit.Path], edit)
	}

	// Overlap check: within each file, ranges must not overlap.
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		edits := byPath[p]
		for i := 0; i < len(edits); i++ {
			for j := i + 1; j < len(edits); j++ {
				if edits[i].Range.Overlaps(edits[j].Range) {
					res.Rejected = append(res.Rejected, RejectOverlap)
					res.Reasons = append(res.Reasons,
						fmt.Sprintf("%s: edits %d and %d overlap [%d-%d) vs [%d-%d)",
							p, i, j, edits[i].Range.Start, edits[i].Range.End,
							edits[j].Range.Start, edits[j].Range.End))
				}
			}
		}
	}

	// Predicted-vs-observed graph delta summary per file.
	res.GraphDelta = graphDelta(cs.Edits)
	res.Accepted = len(res.Rejected) == 0
	return finalizeValidation(res)
}

// finalizeValidation sorts fields and computes the reproducibility digest.
func finalizeValidation(res ValidationResult) ValidationResult {
	sort.SliceStable(res.PerEdit, func(i, j int) bool {
		if res.PerEdit[i].Path != res.PerEdit[j].Path {
			return res.PerEdit[i].Path < res.PerEdit[j].Path
		}
		return res.PerEdit[i].Symbol < res.PerEdit[j].Symbol
	})
	sort.SliceStable(res.GraphDelta, func(i, j int) bool {
		return res.GraphDelta[i].Path < res.GraphDelta[j].Path
	})
	reasons := dedupStrings(res.Reasons)
	res.Reasons = reasons
	// Rejected is already in append order; de-duplicate while preserving order.
	res.Rejected = dedupReasons(res.Rejected)
	res.VerificationCommands = dedupStrings(res.VerificationCommands)
	res.Digest = resultDigest(res)
	return res
}

func dedupReasons(in []RejectReason) []RejectReason {
	seen := map[RejectReason]struct{}{}
	out := make([]RejectReason, 0, len(in))
	for _, r := range in {
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}

func graphDelta(edits []CandidateEdit) []GraphDelta {
	byPath := map[string]*GraphDelta{}
	for _, e := range edits {
		gd, ok := byPath[e.Path]
		if !ok {
			gd = &GraphDelta{Path: e.Path}
			byPath[e.Path] = gd
		}
		gd.PredictedMatches++
		if e.PredictedDigest != "" && e.ObservedDigest != "" && e.PredictedDigest == e.ObservedDigest {
			gd.ObservedMatches++
		}
		if e.PredictedDigest != "" && e.ObservedDigest != "" && e.PredictedDigest != e.ObservedDigest {
			gd.Diverged = true
		}
	}
	out := make([]GraphDelta, 0, len(byPath))
	for _, gd := range byPath {
		out = append(out, *gd)
	}
	return out
}

func resultDigest(res ValidationResult) string {
	res.Digest = ""
	body, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func shortDigest(d string) string {
	if len(d) <= 8 {
		return d
	}
	return d[:8]
}

// validateChangePath rejects absolute, backslash, and ".." escapes. Its path
// rules intentionally match the sessionlog contract except that workflow does
// not accept URL-encoded paths.
func validateChangePath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("absolute path rejected: %q", p)
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("backslash path rejected: %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path escape rejected: %q", p)
		}
	}
	return nil
}

// MarshalJSON emits canonical JSON for both ChangeSet and ValidationResult.
func (cs ChangeSet) MarshalJSON() ([]byte, error) {
	type alias ChangeSet
	return json.Marshal(alias(cs))
}

// MarshalJSON emits canonical validation-result JSON.
func (res ValidationResult) MarshalJSON() ([]byte, error) {
	type alias ValidationResult
	return json.Marshal(alias(res))
}

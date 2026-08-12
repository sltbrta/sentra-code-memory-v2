package codecrawl

import (
	"path/filepath"
	"sort"
	"strings"
)

// impactSeverity classifies blast-radius size using deterministic buckets.
// The thresholds were chosen so that:
//   - low:     ≤ 4 files (single-file change with no callers)
//   - medium:  ≤ 16 files (typical symbol edit in a small module)
//   - high:    > 16 files (cross-module or popular-symbol edit)
//
// Severity is intentionally coarse; callers wanting finer granularity
// should inspect Closure and Direct directly. Bounds are independent of
// maxFiles so callers do not see severity flip with their cap choice.
func impactSeverity(closureSize int) string {
	switch {
	case closureSize <= 4:
		return "low"
	case closureSize <= 16:
		return "medium"
	default:
		return "high"
	}
}

// isTestPath matches the conventional test file patterns across Go, Python,
// TypeScript, JavaScript, and Rust. Centralized so impact receipts and the
// codeserve catalog agree on what counts as a test.
//
// Mirrors pathRankMultiplier's tests/ heuristics, but is broader (also
// accepts _test.go style stem suffixes that pathRank demotes).
func isTestPath(p string) bool {
	pl := strings.ToLower(filepath.ToSlash(p))
	if pl == "" {
		return false
	}
	switch {
	case strings.Contains(pl, "/test/"),
		strings.Contains(pl, "/tests/"),
		strings.Contains(pl, "/__tests__/"),
		strings.Contains(pl, ".test."),
		strings.Contains(pl, ".spec."),
		strings.Contains(pl, "_test."),
		strings.HasSuffix(pl, "_test.go"),
		strings.Contains(pl, "/fixtures/"),
		strings.Contains(pl, "/fixture/"),
		strings.Contains(pl, "/mocks/"),
		strings.Contains(pl, "/__mocks__/"):
		return true
	}
	return false
}

// detectAffectedTests returns the deterministic test-file subset of files,
// preserving file order. The cap is generous (maxFiles) since test sets are
// usually small relative to the production closure.
func detectAffectedTests(files []string) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if isTestPath(f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// severityForClosure sizes both direct and closure and returns the highest
// of the two; receivers use whichever dimension dominates the receipt.
func severityForClosure(directN, closureN int) string {
	if directN > closureN {
		return impactSeverity(directN)
	}
	return impactSeverity(closureN)
}

// indexHasGraphReports reports whether the index carries the typed-edge
// projection. Centralized so callers can produce stable coverage notes.
func indexHasGraphReports(idx *Index) bool {
	return idx != nil && idx.HasGraph()
}

// callAwareDirect adds files that the typed-edge graph proves call into
// the seed symbol. This is the call-aware selection the receipt surfaces
// as AffectedTests-style additions to Direct when the graph is present.
//
// maxFiles caps the contribution; the function returns the new files it
// added (so callers can record them in ChangedSymbols if needed) and
// whether the cap was hit.
func callAwareDirect(idx *Index, symbol string, direct map[string]struct{}, maxFiles int) ([]string, bool) {
	if !indexHasGraphReports(idx) || symbol == "" || maxFiles <= 0 {
		return nil, false
	}
	g := idx.Graph()
	if g == nil {
		return nil, false
	}
	edges := g.callersFor(symbol, maxFiles)
	if len(edges) == 0 {
		return nil, false
	}
	added := make([]string, 0, len(edges))
	hitCap := false
	for _, e := range edges {
		if e.Provenance.File == "" {
			continue
		}
		if _, ok := direct[e.Provenance.File]; ok {
			continue
		}
		direct[e.Provenance.File] = struct{}{}
		added = append(added, e.Provenance.File)
		if len(added) >= maxFiles {
			hitCap = true
			break
		}
	}
	if len(added) == 0 {
		return nil, false
	}
	sort.Strings(added)
	return added, hitCap
}

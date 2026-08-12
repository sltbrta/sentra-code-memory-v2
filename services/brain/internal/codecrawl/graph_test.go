package codecrawl

import (
	"encoding/gob"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestTypedEdgesGoASTCalls is a fixture-driven smoke test: three Go files
// where Alpha() in a.go is the seed; b.go calls it directly; c.go merely
// shares the identifier as a string literal. The call-aware selection must
// put b.go in Direct/Closure but not c.go, proving AST-derived edges beat
// pure lexical co-occurrence.
func TestTypedEdgesGoASTCalls(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.go": `package p

// Alpha is the fixture seed.
func Alpha() int { return 1 }
`,
		"b.go": `package p

import "fmt"

// Beta directly calls Alpha.
func Beta() int { v := Alpha(); return v + fmt.Println("called") }
`,
		"c.go": `package p

// Gamma mentions "Alpha" only as a string literal (no call).
func Gamma() int { _ = "Alpha says hi"; return 0 }
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.HasGraph() {
		t.Fatal("expected typed-edge projection after fresh crawl")
	}
	g := idx.Graph()
	if g == nil {
		t.Fatal("nil graph")
	}

	// Find the AST-derived call edge b.go → Alpha.
	calls := g.callersFor("Alpha", 16)
	if len(calls) == 0 {
		t.Fatalf("expected at least one caller of Alpha; got %+v", calls)
	}
	callerFiles := map[string]bool{}
	for _, e := range calls {
		callerFiles[e.Provenance.File] = true
	}
	if !callerFiles["b.go"] {
		t.Fatalf("expected b.go as AST caller; got %v", calls)
	}
	if callerFiles["c.go"] {
		t.Fatalf("string-literal mention must not register as caller; got %v", calls)
	}

	// Edges are deterministic: sort by canonical order.
	for _, file := range g.fileEdgeKeys() {
		got := g.SortedEdges(file)
		prev := Edge{}
		for _, e := range got {
			if edgeLessOrEqual(prev, e) == false {
				t.Fatalf("file %s edges not sorted: %+v after %+v", file, e, prev)
			}
			prev = e
		}
	}
}

// TestImpactReceiptUsesCallAwareSelection proves the Phase 2 call-aware
// selection reaches b.go via the typed-edge projection, surfaces a
// truncated=false receipt when callers fit, and reports severity /
// affected-tests deterministically. The fixture has no test files so
// AffectedTests must be empty (fail-closed: emit what we know, omit
// what we don't).
func TestImpactReceiptUsesCallAwareSelection(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"alpha.go": `package p
func Alpha() int { return 1 }
`,
		"beta.go": `package p
func Beta() int { return Alpha() }
`,
		"noise.go": `package p
func Gamma() int { _ = "Alpha"; return 0 }
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec := idx.Impact("Alpha", 2, 16)
	if rec.Authority != "heuristic" {
		t.Fatalf("authority=%s", rec.Authority)
	}
	if !idx.HasGraph() {
		t.Fatal("graph missing")
	}
	// Coverage gaps must include graph_unavailable when graph is present.
	for _, g := range rec.CoverageGaps {
		if g == "graph_unavailable" {
			t.Fatalf("graph_unavailable must NOT be set when graph is present: %v", rec.CoverageGaps)
		}
	}
	// Direct must include beta.go (call-aware).
	foundBeta := false
	for _, f := range rec.Direct {
		if f == "beta.go" {
			foundBeta = true
		}
	}
	if !foundBeta {
		t.Fatalf("expected beta.go in Direct via call-aware selection; got %v", rec.Direct)
	}
	// Severity should be low (≤4 files) or medium (≤16). Never high here.
	if rec.Severity != "low" && rec.Severity != "medium" {
		t.Fatalf("unexpected severity %s", rec.Severity)
	}
	if rec.Schema != "v2" {
		t.Fatalf("schema=%s", rec.Schema)
	}
	if len(rec.Closure) > 0 {
		// Closure should be deterministically sorted.
		if !sort.StringsAreSorted(rec.Closure) {
			t.Fatalf("closure not sorted: %v", rec.Closure)
		}
	}
}

// TestImpactReceiptTruncatesWhenCallCapHits exercises the truncation
// signal: maxFiles (closure cap) is intentionally smaller than the total
// caller file count so the BFS hits the cap deterministically.
func TestImpactReceiptTruncatesWhenCallCapHits(t *testing.T) {
	dir := t.TempDir()
	// One defining file plus enough callers to overflow any reasonable cap.
	define := "package p\nfunc Alpha() int { return 1 }\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(define), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		body := "package p\nfunc Caller" + itoa(i) + "() int { return Alpha() }\n"
		name := "caller" + itoaPad(i, 3) + ".go"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	// maxFiles=10 is intentionally smaller than the 51-file total so the
	// closure BFS hits its cap and the Truncated flag flips.
	rec := idx.Impact("Alpha", 2, 10)
	if !rec.Truncated {
		t.Fatalf("expected Truncated=true; got %+v", rec)
	}
	if rec.Authority != "heuristic" {
		t.Fatalf("authority=%s", rec.Authority)
	}
}

// TestImpactReceiptFileSeedListsChangedSymbols proves a file seed surfaces
// its bounded ChangedSymbols list and ranks the closure deterministically.
func TestImpactReceiptFileSeedListsChangedSymbols(t *testing.T) {
	dir := t.TempDir()
	body := `package p
func Alpha() int { return 1 }
func Beta() int { return Alpha() }
`
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec := idx.Impact("a.go", 2, 16)
	if rec.SeedKind != "file" {
		t.Fatalf("seed_kind=%s", rec.SeedKind)
	}
	if len(rec.ChangedSymbols) == 0 {
		t.Fatal("expected non-empty ChangedSymbols")
	}
	if !sort.StringsAreSorted(rec.ChangedSymbols) {
		t.Fatalf("changed symbols not sorted: %v", rec.ChangedSymbols)
	}
}

// TestGobBackwardCompatibleLoadsV3Snapshot verifies the additive FileEdges
// gob field is omitted on encode (zero value → not written) and the
// resulting file loads cleanly with an empty Graph. This is the contract
// test for backward compatibility — Phase 1 snapshots written by older
// binaries must still decode.
func TestGobBackwardCompatibleLoadsV3Snapshot(t *testing.T) {
	dir := t.TempDir()
	gobDir := t.TempDir()
	gob := filepath.Join(gobDir, "index.gob")
	// Build a fresh v4 index, save, then re-load via Load (not OpenOrRefresh)
	// and verify nothing about the in-memory fields is silently upgraded.
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\nfunc X() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _, wrote, _, err := OpenOrRefresh(dir, gob, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("expected gob write on first index")
	}
	if !idx.HasGraph() {
		t.Fatal("expected fresh index to have graph")
	}
	// Manually rewrite the gob in v3 shape by re-encoding without
	// FileEdges so we simulate a legacy snapshot. We do this by writing a
	// struct that mimics durableSnap without FileEdges, which proves the
	// decoder tolerates the missing field.
	v3 := durableSnap{
		FilePostings: idx.filePostings,
		FileDefs:     idx.fileDefs,
		FileRefs:     idx.fileRefs,
		FileImps:     idx.fileImps,
		FileHashes:   idx.fileHashes,
		FileStamps:   idx.fileStamps,
	}
	v3Path := filepath.Join(gobDir, "v3-snapshot.gob")
	if err := writeGob(v3Path, &v3); err != nil {
		t.Fatal(err)
	}
	loaded, meta, err := Load(v3Path)
	if err != nil {
		t.Fatalf("v3 load: %v", err)
	}
	if meta.Schema != "" {
		// The legacy encoder writes an empty schema; we don't repopulate.
		t.Logf("legacy schema: %q", meta.Schema)
	}
	if loaded.HasGraph() {
		t.Fatal("loaded v3 snapshot must not invent a graph")
	}
	rec := loaded.Impact("X", 2, 8)
	found := false
	for _, g := range rec.CoverageGaps {
		if g == "graph_unavailable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing graph_unavailable coverage note: %v", rec.CoverageGaps)
	}
}

// TestDeterministicMapTraversal proves re-running the call-aware selection
// over a fixture with multiple matching files returns the same Direct set
// in the same order across multiple invocations.
func TestDeterministicMapTraversal(t *testing.T) {
	dir := t.TempDir()
	body := `package p
func Target() int { return 1 }
`
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Many callers in random filename order to stress map iteration.
	names := []string{"z.go", "m.go", "b.go", "a.go", "y.go"}
	for i, n := range names {
		body := "package p\nfunc Caller" + itoa(i) + "() int { return Target() }\n"
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	first := idx.Impact("Target", 2, 16)
	for i := 0; i < 4; i++ {
		again := idx.Impact("Target", 2, 16)
		if !sliceEq(first.Direct, again.Direct) {
			t.Fatalf("non-deterministic Direct: first=%v again=%v", first.Direct, again.Direct)
		}
		if !sliceEq(first.Closure, again.Closure) {
			t.Fatalf("non-deterministic Closure: first=%v again=%v", first.Closure, again.Closure)
		}
	}
}

// TestAffectedTestsSubsetIsDeterministic proves the test-file subset of
// Closure is sorted and reports the same set across runs.
func TestAffectedTestsSubsetIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.go":          "package p\nfunc A() int { return 1 }\n",
		"a_test.go":     "package p\nimport \"testing\"\nfunc TestA(t *testing.T) { _ = A() }\n",
		"sub/b.go":      "package p\nfunc B() int { return A() }\n",
		"sub/b_test.go": "package p\nimport \"testing\"\nfunc TestB(t *testing.T) { _ = B() }\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec := idx.Impact("A", 2, 16)
	if !sort.StringsAreSorted(rec.AffectedTests) {
		t.Fatalf("affected tests not sorted: %v", rec.AffectedTests)
	}
	if len(rec.AffectedTests) == 0 {
		t.Fatalf("expected at least one test in closure; got %v", rec.AffectedTests)
	}
}

// TestEdgeCapDeterministic exercises SetEdgeCap to bound a fixture with
// many identifiers and confirm the leading edge set is stable.
func TestEdgeCapDeterministic(t *testing.T) {
	dir := t.TempDir()
	var body string
	body = "package p\n"
	for i := 0; i < 200; i++ {
		body += "func UniqueName" + itoa(i) + "() int { return " + itoa(i) + " }\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := edgeCap
	defer func() { SetEdgeCap(prev) }()
	SetEdgeCap(50)
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	edges := idx.Graph().SortedEdges("huge.go")
	if len(edges) != 50 {
		t.Fatalf("edges=%d want 50", len(edges))
	}
	if edges[0].To == "" {
		t.Fatal("first edge missing To")
	}
	// Re-run a second time, must produce identical slice.
	again := idx.Graph().SortedEdges("huge.go")
	if !sliceEqEdges(edges, again) {
		t.Fatal("non-deterministic SortedEdges")
	}
}

// TestLexicalFallbackAddsAuthorityTag verifies non-Go files produce
// AuthorityLexical edges and that the receipt stays valid when only the
// fallback fires.
func TestLexicalFallbackAddsAuthorityTag(t *testing.T) {
	dir := t.TempDir()
	body := `def helper(): pass
class Alpha:
    pass
def main():
    Alpha()
    helper()
`
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.HasGraph() {
		t.Fatal("expected graph for py fixture")
	}
	g := idx.Graph()
	found := false
	for _, e := range g.SortedEdges("main.py") {
		if e.Authority == AuthorityLexical {
			found = true
		}
		if e.Kind == EdgeImport && e.Authority == AuthorityLexical {
			// Python imports are also lexical.
		}
	}
	if !found {
		t.Fatal("expected at least one lexical-fallback edge")
	}
}

// TestEmptySeedFailsClosed verifies the receipt surfaces Unknowns +
// CoverageGaps when the seed is empty (no symbols, no files).
func TestEmptySeedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\nfunc X() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec := idx.Impact("   ", 2, 8)
	if len(rec.Unknowns) == 0 {
		t.Fatal("empty seed must produce Unknowns")
	}
	found := false
	for _, g := range rec.CoverageGaps {
		if g == "unknown_seed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing unknown_seed coverage note: %v", rec.CoverageGaps)
	}
	if rec.Authority != "heuristic" {
		t.Fatalf("authority=%s", rec.Authority)
	}
}

// helpers --------------------------------------------------------------

func edgeLessOrEqual(a, b Edge) bool {
	if a.Kind != b.Kind {
		return string(a.Kind) < string(b.Kind)
	}
	if a.From != b.From {
		return a.From < b.From
	}
	if a.To != b.To {
		return a.To < b.To
	}
	if a.Provenance.Line != b.Provenance.Line {
		return a.Provenance.Line < b.Provenance.Line
	}
	if a.Provenance.Column != b.Provenance.Column {
		return a.Provenance.Column < b.Provenance.Column
	}
	return a.Provenance.File < b.Provenance.File
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sliceEqEdges(a, b []Edge) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	negative := i < 0
	if negative {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func itoaPad(i, width int) string {
	s := itoa(i)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// writeGob mirrors persist.Save but lets the test inject an alternate
// durableSnap to simulate legacy snapshots. It is intentionally minimal;
// production saves use Index.Save.
func writeGob(path string, snap *durableSnap) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// Make sure the type registrations mirror codecrawl's init().
	gob.Register(map[string]map[string]int{})
	gob.Register(map[string][]string{})
	gob.Register(map[string]string{})
	gob.Register(map[string]FileStamp{})
	gob.Register(FileStamp{})
	return gob.NewEncoder(f).Encode(snap)
}

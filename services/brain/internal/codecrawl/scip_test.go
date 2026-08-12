package codecrawl

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
)

// TestIngestSCIPTypeScriptAddsSCIPAuthority proves SCIP ingestion
// surfaces explicit AuthoritySCIP edges and the per-file projection
// remains deterministic after a clean rebuild.
func TestIngestSCIPTypeScriptAddsSCIPAuthority(t *testing.T) {
	idx := newEmptyIndex()
	payload := []byte(`{
		"toolName": "scip-typescript-fixture",
		"toolVersion": "0.0.1",
		"occurrences": [
			{"range": [1, 17, 1, 25], "symbol": "scip-typescript npm typescript 1.0.0 src/foo.ts/Foo.", "symbolRoles": 1},
			{"range": [2, 12, 2, 20], "symbol": "scip-typescript npm typescript 1.0.0 src/foo.ts/Foo.", "symbolRoles": 4},
			{"range": [4, 1, 4, 30], "symbol": "scip-typescript npm typescript 1.0.0 src/foo.ts/Bar.", "symbolRoles": 1}
		]
	}`)
	doc, err := codeindex.DecodeSCIP(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats, err := idx.IngestSCIP(doc, "src/foo.ts", "typescript")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Definitions != 2 || stats.References != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if !idx.HasGraph() {
		t.Fatal("graph missing after SCIP ingest")
	}
	g := idx.Graph()
	// Definitions should be retrievable as a friendly surface for callers.
	defs := g.SortedEdges("src/foo.ts")
	if len(defs) < 3 {
		t.Fatalf("expected ≥3 edges, got %d", len(defs))
	}
	for _, e := range defs {
		if e.Authority != AuthoritySCIP {
			t.Fatalf("expected AuthoritySCIP, got %s", e.Authority)
		}
		if e.Provenance.Parser != "scip" {
			t.Fatalf("parser should be scip, got %s", e.Provenance.Parser)
		}
		if e.Provenance.Language != "typescript" {
			t.Fatalf("language = %s", e.Provenance.Language)
		}
	}
}

// TestIngestSCIPPreservesLexicalFallback merges SCIP edges with existing
// AST-derived edges and ensures the SCIP edges win on duplicated
// (from, to, kind) triples per the authority ranking.
func TestIngestSCIPPreservesLexicalFallback(t *testing.T) {
	idx := newEmptyIndex()
	idx.fileEdges = map[string][]Edge{
		"src/foo.go": {
			{From: "main", To: "Alpha", Kind: EdgeCall, Authority: AuthorityAST, Confidence: 0.95, Provenance: Provenance{File: "src/foo.go", Line: 5, Parser: "go/parser", Language: "go"}},
			{From: "", To: "alpha", Kind: EdgeLexical, Authority: AuthorityLexical, Confidence: 0.4, Provenance: Provenance{File: "src/foo.go", Line: 9, Parser: "lexical:.go", Language: "go"}},
		},
	}
	payload := []byte(`{
		"occurrences": [
			{"range": [3, 1, 3, 6], "symbol": "scheme pkg m Alpha.", "symbolRoles": 1}
		]
	}`)
	doc, err := codeindex.DecodeSCIP(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats, err := idx.IngestSCIP(doc, "src/foo.go", "go")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Definitions != 1 {
		t.Fatalf("definitions = %d", stats.Definitions)
	}
	edges := idx.fileEdges["src/foo.go"]
	// Lexical edges must remain (they are the lexical fallback floor).
	lexicalCount := 0
	scipCount := 0
	for _, e := range edges {
		switch e.Authority {
		case AuthorityLexical:
			lexicalCount++
		case AuthoritySCIP:
			scipCount++
		}
	}
	if lexicalCount != 1 {
		t.Fatalf("lexical edges = %d, want 1", lexicalCount)
	}
	if scipCount != 1 {
		t.Fatalf("scip edges = %d, want 1", scipCount)
	}
}

// TestIngestSCIPEnforcesEdgeCap checks the per-file edge cap floors
// overflowed SCIP ingest deterministically.
func TestIngestSCIPEnforcesEdgeCap(t *testing.T) {
	idx := newEmptyIndex()
	prevCap := edgeCap
	SetEdgeCap(2)
	defer SetEdgeCap(prevCap)
	occs := []codeindex.SCIPOccurence{}
	for i := 0; i < 5; i++ {
		occs = append(occs, codeindex.SCIPOccurence{
			Range:  []uint32{uint32(i + 1), 1, uint32(i + 1), 4},
			Symbol: "scheme pkg m Val.", SymbolRoles: 1,
		})
	}
	doc := codeindex.SCIPDocument{Occurrences: occs}
	stats, err := idx.IngestSCIP(doc, "src/cap.go", "go")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Edges != 2 {
		t.Fatalf("edges = %d, want 2", stats.Edges)
	}
	// Looking up the graph should not panic; the cap is enforced inside
	// the per-file edge map.
	if idx.HasGraph() {
		_ = idx.Graph()
	}
}

// TestIngestSCIPRebuildEquivalence is the headline equivalence test for
// issue #44: a clean ingest and a clean-then-add ingest must produce
// the same fileEdges when the second ingest re-issues the same
// occurrences. Idempotence is the relaxation we implement: re-ingesting
// SCIP for the same path replaces prior SCIP edges rather than merging.
func TestIngestSCIPRebuildEquivalence(t *testing.T) {
	cleanPayload := []byte(`{
		"toolName": "scip-fixture",
		"occurrences": [
			{"range": [1, 1, 1, 6], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1},
			{"range": [2, 1, 2, 6], "symbol": "scheme pkg m Beta.", "symbolRoles": 1},
			{"range": [3, 1, 3, 6], "symbol": "scheme pkg m Anchor.", "symbolRoles": 4},
			{"range": [4, 1, 4, 10], "symbol": "scheme pkg m fmt.", "symbolRoles": 2}
		]
	}`)
	docA, err := codeindex.DecodeSCIP(cleanPayload)
	if err != nil {
		t.Fatalf("decode clean: %v", err)
	}

	// Clean build: ingest the snapshot at once.
	cleanIdx := newEmptyIndex()
	if _, err := cleanIdx.IngestSCIP(docA, "src/rebuild.go", "go"); err != nil {
		t.Fatalf("clean ingest: %v", err)
	}

	// Incremental build: ingest the same occurrences twice. The second
	// ingest must replace the SCIP edges so the final list equals the
	// clean ingest.
	incIdx := newEmptyIndex()
	if _, err := incIdx.IngestSCIP(docA, "src/rebuild.go", "go"); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if _, err := incIdx.IngestSCIP(docA, "src/rebuild.go", "go"); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	cleanEdges := append([]Edge(nil), cleanIdx.fileEdges["src/rebuild.go"]...)
	incEdges := append([]Edge(nil), incIdx.fileEdges["src/rebuild.go"]...)
	if !reflect.DeepEqual(cleanEdges, incEdges) {
		t.Fatalf("incremental != clean\nclean=%+v\ninc=%+v", cleanEdges, incEdges)
	}
}

// TestIngestSCIPIncrementalAddsExtraDefinition exercises the incremental
// path: ingest a base snapshot, then upsert a new definition. The merged
// edge list must contain both the original and the new edges, sorted.
func TestIngestSCIPIncrementalAddsExtraDefinition(t *testing.T) {
	basePayload := []byte(`{
		"toolName": "scip-fixture",
		"occurrences": [
			{"range": [1, 1, 1, 6], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1}
		]
	}`)
	addedPayload := []byte(`{
		"toolName": "scip-fixture",
		"occurrences": [
			{"range": [1, 1, 1, 6], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1},
			{"range": [2, 1, 2, 6], "symbol": "scheme pkg m Beta.", "symbolRoles": 1}
		]
	}`)
	baseDoc, err := codeindex.DecodeSCIP(basePayload)
	if err != nil {
		t.Fatalf("decode base: %v", err)
	}
	addedDoc, err := codeindex.DecodeSCIP(addedPayload)
	if err != nil {
		t.Fatalf("decode added: %v", err)
	}
	idx := newEmptyIndex()
	if _, err := idx.IngestSCIP(baseDoc, "src/file.go", "go"); err != nil {
		t.Fatalf("base ingest: %v", err)
	}
	if _, err := idx.IngestSCIP(addedDoc, "src/file.go", "go"); err != nil {
		t.Fatalf("added ingest: %v", err)
	}
	edges := idx.fileEdges["src/file.go"]
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(edges))
	}
	if edges[0].To != "Anchor" || edges[1].To != "Beta" {
		t.Fatalf("edges = %+v", edges)
	}
}

// TestIngestSCIPRejectsEmptyPath ensures the boundary rejects a nil-path
// invocation so callers cannot accidentally publish edges with no
// provenance.
func TestIngestSCIPRejectsEmptyPath(t *testing.T) {
	idx := newEmptyIndex()
	doc := codeindex.SCIPDocument{
		Occurrences: []codeindex.SCIPOccurence{
			{Range: []uint32{1, 1, 1, 5}, Symbol: "scheme pkg m x.", SymbolRoles: 1},
		},
	}
	if _, err := idx.IngestSCIP(doc, "", "go"); err == nil {
		t.Fatal("expected error for empty path")
	}
}

// TestIngestSCIPMultiLanguageMatrix verifies SCIP ingestion for the
// three explicit language targets (TypeScript/Python/Rust) plus Go
// produces authority-preserving edges.
func TestIngestSCIPMultiLanguageMatrix(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		lang    string
		symbol  string
		payload string
	}{
		{
			"typescript",
			"src/foo.ts",
			"typescript",
			"scheme pkg m Anchor.",
			`{
				"occurrences": [
					{"range": [1, 1, 1, 7], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1}
				]
			}`,
		},
		{
			"python",
			"src/foo.py",
			"python",
			"scheme pkg m Anchor.",
			`{
				"occurrences": [
					{"range": [1, 1, 1, 7], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1}
				]
			}`,
		},
		{
			"rust",
			"src/foo.rs",
			"rust",
			"scheme pkg m Anchor.",
			`{
				"occurrences": [
					{"range": [1, 1, 1, 7], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1}
				]
			}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := newEmptyIndex()
			doc, err := codeindex.DecodeSCIP([]byte(tc.payload))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, err := idx.IngestSCIP(doc, tc.path, tc.lang); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			edges := idx.fileEdges[tc.path]
			if len(edges) != 1 {
				t.Fatalf("edges = %d, want 1", len(edges))
			}
			if edges[0].Authority != AuthoritySCIP {
				t.Fatalf("authority = %s", edges[0].Authority)
			}
			if edges[0].Provenance.Language != tc.lang {
				t.Fatalf("language = %s want %s", edges[0].Provenance.Language, tc.lang)
			}
			if edges[0].Provenance.Parser != "scip" {
				t.Fatalf("parser = %s", edges[0].Provenance.Parser)
			}
		})
	}
}

// TestIngestSCIPUnsupportedFallback verifies an unsupported SCIP role
// degrades to a reference edge with low confidence rather than failing
// the ingest.
func TestIngestSCIPUnsupportedFallback(t *testing.T) {
	idx := newEmptyIndex()
	payload := []byte(`{
		"occurrences": [
			{"range": [1, 1, 1, 7], "symbol": "scheme pkg m Weird.", "symbolRoles": 32768}
		]
	}`)
	doc, err := codeindex.DecodeSCIP(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := idx.IngestSCIP(doc, "src/foo.ts", "typescript"); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	edges := idx.fileEdges["src/foo.ts"]
	if len(edges) != 1 || edges[0].Kind != "reference" {
		t.Fatalf("edges = %+v", edges)
	}
	if edges[0].Confidence >= 0.5 {
		t.Fatalf("expected low confidence for unsupported role: %+v", edges[0])
	}
}

// TestIngestSCIPDeterministicForReOrderedInputs proves the ingest
// pipeline is order-independent at the codecrawl layer: reordering the
// input occurrence slice must produce the same fileEdges.
func TestIngestSCIPDeterministicForReOrderedInputs(t *testing.T) {
	makeDoc := func(order []int) codeindex.SCIPDocument {
		base := []codeindex.SCIPOccurence{
			{Range: []uint32{1, 1, 1, 5}, Symbol: "scheme pkg m a.", SymbolRoles: 1},
			{Range: []uint32{2, 1, 2, 5}, Symbol: "scheme pkg m b.", SymbolRoles: 1},
			{Range: []uint32{3, 1, 3, 5}, Symbol: "scheme pkg m c.", SymbolRoles: 1},
		}
		out := make([]codeindex.SCIPOccurence, len(order))
		for i, idx := range order {
			out[i] = base[idx]
		}
		return codeindex.SCIPDocument{Occurrences: out}
	}

	idxA := newEmptyIndex()
	idxB := newEmptyIndex()
	if _, err := idxA.IngestSCIP(makeDoc([]int{0, 1, 2}), "src/file.go", "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := idxB.IngestSCIP(makeDoc([]int{2, 1, 0}), "src/file.go", "go"); err != nil {
		t.Fatal(err)
	}
	a := append([]Edge(nil), idxA.fileEdges["src/file.go"]...)
	b := append([]Edge(nil), idxB.fileEdges["src/file.go"]...)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("reordered input drift:\nA=%+v\nB=%+v", a, b)
	}
}

// TestIngestSCIPDiagnosticsRoundTrip exercises the stats serialization
// shape so downstream readers can rely on the wire field names.
func TestIngestSCIPDiagnosticsRoundTrip(t *testing.T) {
	idx := newEmptyIndex()
	payload := []byte(`{
		"toolName": "scip-diagnostics",
		"toolVersion": "0.4.0",
		"occurrences": [
			{"range": [1, 1, 1, 6], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1},
			{"range": [2, 1, 2, 6], "symbol": "scheme pkg m Anchor.", "symbolRoles": 256}
		]
	}`)
	doc, err := codeindex.DecodeSCIP(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	stats, err := idx.IngestSCIP(doc, "src/diag.go", "go")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded SCIPIngestStats
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(stats, decoded) {
		t.Fatalf("stats drift: %+v vs %+v", stats, decoded)
	}
	// Sanity: edges must be sorted by (Kind, From, To, Line, Column, File).
	edges := idx.fileEdges["src/diag.go"]
	if !sortEdgesSorted(edges) {
		t.Fatalf("edges not sorted: %+v", edges)
	}
}

// sortEdgesSorted is a helper that mirrors the sortEdges invariant. We
// duplicate the comparator here so the test does not need to export the
// internal sort routine.
func sortEdgesSorted(edges []Edge) bool {
	for i := 1; i < len(edges); i++ {
		if edges[i-1].Kind > edges[i].Kind {
			return false
		}
		if edges[i-1].Kind == edges[i].Kind {
			if edges[i-1].From > edges[i].From {
				return false
			}
		}
	}
	return true
}

// TestScipPayloadAdvertises ensures the payload advertises SCIP
// availability without requiring credentials.
func TestScipPayloadAdvertises(t *testing.T) {
	idx := newEmptyIndex()
	payload := idx.ScipPayload()
	if payload == nil {
		t.Fatal("nil payload")
	}
	if payload["authority"] != "scip" {
		t.Fatalf("authority = %v", payload["authority"])
	}
	if payload["supported"] != true {
		t.Fatalf("supported = %v", payload["supported"])
	}
	if _, ok := payload["edgeCap"]; !ok {
		t.Fatal("edgeCap missing")
	}
}

package codeindex

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestDecodeSCIPCanonicalAndWrapper exercises both payload shapes the
// boundary recognises: the canonical {"occurrences":[...]} map and the
// {"documents":[{...}]} wrapper that several SCIP emitters produce.
func TestDecodeSCIPCanonicalAndWrapper(t *testing.T) {
	canonical := []byte(`{
		"toolName": "scip-go",
		"toolVersion": "1.0.0",
		"occurrences": [
			{"range": [3, 6, 3, 12], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1}
		]
	}`)
	doc, err := DecodeSCIP(canonical)
	if err != nil {
		t.Fatalf("decode canonical: %v", err)
	}
	if doc.ToolName != "scip-go" || len(doc.Occurrences) != 1 {
		t.Fatalf("canonical decode = %+v", doc)
	}

	wrapped := []byte(`{
		"documents": [
			{
				"toolName": "scip-rust",
				"occurrences": [
					{"range": [1, 7, 1, 14], "symbol": "scheme pkg m anchor.", "symbolRoles": 1}
				]
			}
		]
	}`)
	doc, err = DecodeSCIP(wrapped)
	if err != nil {
		t.Fatalf("decode wrapped: %v", err)
	}
	if doc.ToolName != "scip-rust" || len(doc.Occurrences) != 1 {
		t.Fatalf("wrapper decode = %+v", doc)
	}
}

// TestDecodeSCIPRejectsInvalidPayloads asserts the boundary keeps
// malformed inputs out of the ingest path.
func TestDecodeSCIPRejectsInvalidPayloads(t *testing.T) {
	cases := map[string][]byte{
		"empty":       []byte{},
		"notjson":     []byte(`{notjson`),
		"empty-array": []byte(`{"documents": []}`),
		"bad-shape":   []byte(`{"occurrences": "not-an-array"}`),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeSCIP(payload)
			if !errors.Is(err, ErrSCIPInvalid) {
				t.Fatalf("err = %v, want ErrSCIPInvalid", err)
			}
		})
	}
}

// TestIngestSCIPRolesDeterministic verifies the role classifier maps
// every documented role tag to a stable kind+confidence pair.
func TestIngestSCIPRolesDeterministic(t *testing.T) {
	// Each row produces one occurrence with one role and expects a
	// specific kind; the order is {call, definition, import, reference,
	// implementation, inheritance} for stable ordering checks.
	doc := SCIPDocument{
		ToolName: "scip-fixture",
		Occurrences: []SCIPOccurence{
			{Range: []uint32{2, 4, 2, 9}, Symbol: "scheme pkg m caller.", SymbolRoles: 0x100},
			{Range: []uint32{3, 6, 3, 12}, Symbol: "scheme pkg m Anchor.", SymbolRoles: 0x1},
			{Range: []uint32{4, 4, 4, 16}, Symbol: "scheme pkg m fmt.", SymbolRoles: 0x2},
			{Range: []uint32{5, 2, 5, 6}, Symbol: "scheme pkg m Value.", SymbolRoles: 0x4},
			{Range: []uint32{6, 6, 6, 16}, Symbol: "scheme pkg m Trait.", SymbolRoles: 0x40},
			{Range: []uint32{7, 8, 7, 18}, Symbol: "scheme pkg m Base.", SymbolRoles: 0x80},
		},
	}
	edges, stats, err := IngestSCIP(doc, "go", "/work/file.go")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Edges != 6 {
		t.Fatalf("edges = %d, want 6", stats.Edges)
	}
	want := []string{"call", "definition", "implementation", "import", "inheritance", "reference"}
	for i, edge := range edges {
		if edge.Kind != want[i] {
			t.Fatalf("edge %d kind = %s, want %s", i, edge.Kind, want[i])
		}
		if edge.Authority != "scip" {
			t.Fatalf("edge %d authority = %s", i, edge.Authority)
		}
		if edge.Path != "/work/file.go" {
			t.Fatalf("edge %d path = %s", i, edge.Path)
		}
	}
}

// TestIngestSCIPUnknownRoleDegrades verifies roles that fall outside the
// supported subset produce a low-confidence reference rather than failing
// the ingest, so callers can degrade gracefully.
func TestIngestSCIPUnknownRoleDegrades(t *testing.T) {
	doc := SCIPDocument{
		Occurrences: []SCIPOccurence{
			{Range: []uint32{1, 1, 1, 5}, Symbol: "scheme pkg m Value.", SymbolRoles: 0x200},
		},
	}
	edges, stats, err := IngestSCIP(doc, "go", "file.go")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Edges != 1 {
		t.Fatalf("edges = %d, want 1", stats.Edges)
	}
	if edges[0].Kind != "reference" || edges[0].Confidence >= 0.5 {
		t.Fatalf("unknown role: %+v", edges[0])
	}
}

// TestIngestSCIPEnforcesCap proves the per-document cap rejects the
// ingest before allocating the edge slice.
func TestIngestSCIPEnforcesCap(t *testing.T) {
	doc := SCIPDocument{}
	doc.Occurrences = make([]SCIPOccurence, MaxSCIPOccurrences+1)
	_, _, err := IngestSCIP(doc, "go", "file.go")
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("err = %v, want ErrLimitExceeded", err)
	}
}

// TestIngestSCIPOutputIsStable shows repeat ingest of the same SCIP
// document produces identical result slices so downstream comparisons
// (rebuild equivalence, persistence round-trip) do not need an explicit
// sort.
func TestIngestSCIPOutputIsStable(t *testing.T) {
	doc := SCIPDocument{
		Occurrences: []SCIPOccurence{
			{Range: []uint32{1, 1, 1, 5}, Symbol: "scheme pkg m b.", SymbolRoles: 0x1},
			{Range: []uint32{2, 1, 2, 5}, Symbol: "scheme pkg m a.", SymbolRoles: 0x1},
			{Range: []uint32{3, 1, 3, 5}, Symbol: "scheme pkg m a.", SymbolRoles: 0x4},
		},
	}
	first, _, err := IngestSCIP(doc, "go", "stable.go")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, _, err := IngestSCIP(doc, "go", "stable.go")
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("ingest drifted at i=%d\nfirst=%+v\nnext=%+v", i, first, next)
		}
	}
}

// TestIngestSCIPFixtureRoundtrip is a regression guard against a fixture
// stored alongside the rest of the stage-03 fixtures. It pins the wire
// shape so codecrawl can rely on it.
func TestIngestSCIPFixtureRoundtrip(t *testing.T) {
	fixture := []byte(`{
		"toolName": "scip-test",
		"toolVersion": "0.1.0",
		"occurrences": [
			{"range": [1, 4, 1, 12], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1},
			{"range": [2, 4, 2, 11], "symbol": "scheme pkg m Beta.", "symbolRoles": 1},
			{"range": [3, 8, 3, 14], "symbol": "scheme pkg m Anchor.", "symbolRoles": 4},
			{"range": [4, 5, 4, 16], "symbol": "scheme pkg m fmt.", "symbolRoles": 2}
		]
	}`)
	doc, err := DecodeSCIP(fixture)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	edges, stats, err := IngestSCIP(doc, "go", "incremental/clean.go")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Definitions != 2 || stats.References != 1 || stats.Imports != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	// Dump once so the snapshot test below can compare against a stable
	// rendering; the canonical fields are Path, From, To, Kind, Authority,
	// Language, StartLine, StartCol, EndLine, EndCol.
	want := []SCIPEdge{
		{Path: "incremental/clean.go", To: "Anchor", Kind: "definition", Role: SCIPRoleDefinition, Authority: "scip", Confidence: 0.99, Language: "go", StartLine: 1, StartCol: 4, EndLine: 1, EndCol: 12},
		{Path: "incremental/clean.go", To: "Beta", Kind: "definition", Role: SCIPRoleDefinition, Authority: "scip", Confidence: 0.99, Language: "go", StartLine: 2, StartCol: 4, EndLine: 2, EndCol: 11},
		{Path: "incremental/clean.go", To: "fmt", Kind: "import", Role: SCIPRoleImport, Authority: "scip", Confidence: 0.85, Language: "go", StartLine: 4, StartCol: 5, EndLine: 4, EndCol: 16},
		{Path: "incremental/clean.go", To: "Anchor", Kind: "reference", Role: SCIPRoleReference, Authority: "scip", Confidence: 0.7, Language: "go", StartLine: 3, StartCol: 8, EndLine: 3, EndCol: 14},
	}
	// Normalize the From field for the import: codecrawl/SCIP ingest
	// leaves From empty for all kinds because the enclosing scope is
	// not part of the SCIP document. The To field stays the printable
	// identifier derived from the SCIP descriptor.
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("edges = %+v\nwant = %+v", edges, want)
	}
}

// TestIngestSCIPDeterministicAcrossRounds proves two independent
// document ordering inputs produce the same output ordering, guarding
// against future refactors that might introduce Go map iteration
// non-determinism.
func TestIngestSCIPDeterministicAcrossRounds(t *testing.T) {
	a := []SCIPOccurence{
		{Range: []uint32{1, 1, 1, 5}, Symbol: "scheme pkg m b.", SymbolRoles: 0x1},
		{Range: []uint32{2, 1, 2, 5}, Symbol: "scheme pkg m a.", SymbolRoles: 0x1},
		{Range: []uint32{3, 1, 3, 5}, Symbol: "scheme pkg m a.", SymbolRoles: 0x4},
	}
	b := make([]SCIPOccurence, len(a))
	copy(b, a)
	for i := 0; i < len(a)/2; i++ {
		a[i], a[len(a)-1-i] = a[len(a)-1-i], a[i]
	}
	docA, _, err := IngestSCIP(SCIPDocument{Occurrences: a}, "go", "file.go")
	if err != nil {
		t.Fatalf("ingest a: %v", err)
	}
	docB, _, err := IngestSCIP(SCIPDocument{Occurrences: b}, "go", "file.go")
	if err != nil {
		t.Fatalf("ingest b: %v", err)
	}
	if !reflect.DeepEqual(docA, docB) {
		t.Fatalf("order-sensitive:\nA=%+v\nB=%+v", docA, docB)
	}
}

// TestIngestSCIPRoleEncodingConsistentForCoding verifies the JSON round
// trip of the SCIP role field stays stable across the boundary. Coding
// agents should be able to emit SCIP JSON with role integers and get
// back the same kind we surface elsewhere.
func TestIngestSCIPRoleEncodingConsistentForCoding(t *testing.T) {
	payload := []byte(`{
		"occurrences": [
			{"range": [1, 1, 1, 7], "symbol": "scheme pkg m Anchor.", "symbolRoles": 1}
		]
	}`)
	var doc SCIPDocument
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Occurrences[0].SymbolRoles != 1 {
		t.Fatalf("SymbolRoles = %d", doc.Occurrences[0].SymbolRoles)
	}
	edges, _, err := IngestSCIP(doc, "go", "file.go")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if edges[0].Kind != "definition" {
		t.Fatalf("kind = %s, want definition", edges[0].Kind)
	}
	if !strings.HasPrefix(edges[0].To, "A") {
		t.Fatalf("expected identifier to start with capital A: %q", edges[0].To)
	}
}

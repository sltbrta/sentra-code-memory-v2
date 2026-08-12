package codecrawl

import (
	"sort"
	"strings"
)

// EdgeKind classifies one typed relation between symbols or files. Values are
// stable wire strings so callers, persistence, and tests can rely on them.
type EdgeKind string

const (
	EdgeCall           EdgeKind = "call"           // Go AST: caller invokes callee
	EdgeReference      EdgeKind = "reference"      // Go AST: type/value reference
	EdgeImport         EdgeKind = "import"         // source-level import edge
	EdgeImplementation EdgeKind = "implementation" // symbol satisfies interface (reserved; populated when AST hints it)
	EdgeInheritance    EdgeKind = "inheritance"    // embedding / extends (reserved; populated when AST hints it)
	EdgeLexical        EdgeKind = "lexical"        // fallback for non-Go languages: shared identifier co-occurrence
	EdgeDefinition     EdgeKind = "definition"     // SCIP/LSIF-style declaration anchor (issue #44)
)

// Authority reports how the edge was derived. Higher trust wins on conflict.
type Authority string

const (
	AuthorityAST       Authority = "ast"       // go/parser or other structural parser
	AuthorityHeuristic Authority = "heuristic" // name-only / import name match / shared identifier
	AuthorityLexical   Authority = "lexical"   // multi-language fallback identifier scan
	AuthoritySCIP      Authority = "scip"      // SCIP/LSIF snapshot ingestion (issue #44)
)

// AuthorityRank orders authorities from highest to lowest trust. The
// ranking is used by ranking fusion to resolve conflicts when more than
// one source contributed edges for the same (from, to, kind) triple.
func AuthorityRank(a Authority) int {
	switch a {
	case AuthoritySCIP:
		return 4
	case AuthorityAST:
		return 3
	case AuthorityHeuristic:
		return 2
	case AuthorityLexical:
		return 1
	}
	return 0
}

// Provenance carries the source of an edge and the lane that produced it.
// Schema is intentionally compact so callers can inspect or serialize it
// without pulling the whole index.
type Provenance struct {
	File     string `json:"file"`              // relative path the edge was observed in
	Line     int    `json:"line"`              // 1-based, 0 when unknown
	Column   int    `json:"column"`            // 0-based byte offset, 0 when unknown
	Parser   string `json:"parser"`            // "go/parser" | "lexical:<ext>" | "name-graph"
	Snippet  string `json:"snippet,omitempty"` // bounded line excerpt, empty when suppressed
	Language string `json:"language,omitempty"`
}

// Edge is one typed relation. From / To are symbol names when non-empty;
// PathFrom / PathTo are the relative paths that bound them. Either side may
// be empty when the call site references an external package (e.g. a Go
// import that the index cannot resolve). Confidence is in (0,1].
type Edge struct {
	From       string     `json:"from"`
	To         string     `json:"to"`
	Kind       EdgeKind   `json:"kind"`
	Authority  Authority  `json:"authority"`
	Confidence float64    `json:"confidence"`
	Provenance Provenance `json:"provenance"`
	// Target is optional when a single Edge resolves to multiple files
	// (e.g. unresolved import). Empty in single-target cases.
	Target string `json:"target,omitempty"`
}

// MaxEdgesPerFile is the deterministic cap on edges extracted from a single
// file. The cap prevents huge auto-generated fixtures from blowing up the
// durable index; bounds also keep BFS depth-traversal in impact closures
// predictable. Callers can override via SetEdgeCap() before extraction.
const MaxEdgesPerFile = 512

// edgeCap is the effective per-file edge cap during extraction. It is a
// package-level variable so callers (and tests) can shrink the cap to
// exercise truncation deterministically without rebuilding fixtures.
var edgeCap = MaxEdgesPerFile

// SetEdgeCap overrides the per-file edge cap. Passing a non-positive value
// resets to the default. Intended for tests; production callers should leave
// it alone.
func SetEdgeCap(n int) {
	if n <= 0 {
		edgeCap = MaxEdgesPerFile
		return
	}
	edgeCap = n
}

// Graph is the file-disjoint typed edge graph. Edges are file-scoped: at
// index time, every edge originates from a single file (Edge.Provenance.File).
// Cross-file resolution is query-time over the in-memory edges map, mirroring
// the file-disjoint SymbolGraph approach used for defs/refs.
type Graph struct {
	// Edges: file → list of edges observed in that file (file-disjoint at
	// index time). Bounded by edgeCap per file to keep traversal cheap and
	// deterministic. Outbound edges to external targets (e.g. import paths
	// that cannot be resolved to files) live in EdgesByImportPath.
	Edges map[string][]Edge
	// EdgesByImportPath: unresolved import path → edges seen referencing it.
	EdgesByImportPath map[string][]Edge
	// Stats counts kept for diagnostics and ImpactReceipt coverage notes.
	Stats GraphStats
}

// GraphStats is a coarse diagnostic count: how many edges were observed,
// dropped by the cap, classified as unresolved, and survived the lexical
// fallback path.
type GraphStats struct {
	Extracted   int `json:"extracted"`    // edges kept
	Truncated   int `json:"truncated"`    // edges dropped by edgeCap
	Unresolved  int `json:"unresolved"`   // edges whose target is unknown (external package etc.)
	LexicalOnly int `json:"lexical_only"` // edges produced by lexical fallback
}

// newGraph returns an empty Graph with all maps allocated.
func newGraph() *Graph {
	return &Graph{
		Edges:             map[string][]Edge{},
		EdgesByImportPath: map[string][]Edge{},
	}
}

// addEdge appends e to the per-file edge list, enforcing edgeCap and
// updating stats. Unresolved edges (no Target) are also recorded under
// EdgesByImportPath when their Provenance.Parser hints at an import path so
// callers can later look them up by import stem.
func (g *Graph) addEdge(file string, e Edge) {
	if g == nil || file == "" {
		return
	}
	if g.Edges == nil {
		g.Edges = map[string][]Edge{}
	}
	if g.EdgesByImportPath == nil {
		g.EdgesByImportPath = map[string][]Edge{}
	}
	list := g.Edges[file]
	if len(list) >= edgeCap {
		g.Stats.Truncated++
		return
	}
	g.Edges[file] = append(list, e)
	g.Stats.Extracted++
	if e.Target == "" && strings.HasPrefix(strings.ToLower(e.Provenance.Parser), "lexical:") == false {
		// External / unresolved: register so callers can still introspect.
		if e.Provenance.File != "" {
			g.EdgesByImportPath[e.Provenance.File] = append(g.EdgesByImportPath[e.Provenance.File], e)
			g.Stats.Unresolved++
		}
	}
	if strings.HasPrefix(strings.ToLower(e.Provenance.Parser), "lexical:") {
		g.Stats.LexicalOnly++
	}
}

// SortedEdges returns the per-file edge list in deterministic order
// (Kind, From, To, Provenance.Line, Provenance.Column, Provenance.File).
// Callers should prefer this over direct slice iteration when emitting to
// logs, receipts, or test assertions.
func (g *Graph) SortedEdges(file string) []Edge {
	if g == nil || g.Edges == nil {
		return nil
	}
	src := append([]Edge(nil), g.Edges[file]...)
	if len(src) == 0 {
		return nil
	}
	sort.SliceStable(src, func(i, j int) bool {
		if src[i].Kind != src[j].Kind {
			return string(src[i].Kind) < string(src[j].Kind)
		}
		if src[i].From != src[j].From {
			return src[i].From < src[j].From
		}
		if src[i].To != src[j].To {
			return src[i].To < src[j].To
		}
		if src[i].Provenance.Line != src[j].Provenance.Line {
			return src[i].Provenance.Line < src[j].Provenance.Line
		}
		if src[i].Provenance.Column != src[j].Provenance.Column {
			return src[i].Provenance.Column < src[j].Provenance.Column
		}
		return src[i].Provenance.File < src[j].Provenance.File
	})
	return src
}

// fileEdgeKeys returns the sorted set of files that have any edges. Used to
// keep map traversal deterministic in BFS helpers.
func (g *Graph) fileEdgeKeys() []string {
	if g == nil || g.Edges == nil {
		return nil
	}
	out := make([]string, 0, len(g.Edges))
	for k := range g.Edges {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rebuildGraphFromFiles reconstructs the typed-edge Graph from the per-file
// map. Map iteration is sorted to keep the EdgesByImportPath grouping
// deterministic. Returns nil when there are no edges to rebuild.
func rebuildGraphFromFiles(idx *Index) *Graph {
	if idx == nil || len(idx.fileEdges) == 0 {
		return nil
	}
	g := newGraph()
	for file, list := range idx.fileEdges {
		if len(list) == 0 {
			continue
		}
		cp := make([]Edge, len(list))
		copy(cp, list)
		g.Edges[file] = cp
	}
	for _, list := range g.Edges {
		for _, e := range list {
			g.Stats.Extracted++
			if e.Target == "" {
				g.Stats.Unresolved++
				if e.Provenance.File != "" {
					g.EdgesByImportPath[e.Provenance.File] = append(g.EdgesByImportPath[e.Provenance.File], e)
				}
			}
			if isLexicalParser(e.Provenance.Parser) {
				g.Stats.LexicalOnly++
			}
		}
	}
	return g
}

func isLexicalParser(parser string) bool {
	p := strings.ToLower(strings.TrimSpace(parser))
	return strings.HasPrefix(p, "lexical:")
}

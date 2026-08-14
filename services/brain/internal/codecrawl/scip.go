package codecrawl

import (
	"fmt"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
)

// SCIPIngestStats is the codecrawl-side projection of a SCIP ingest run.
// It is exposed on AgentPayload.Notes so callers can see how many edges
// were absorbed, how many were dropped by the cap, and which languages
// participated. The fields are stable wire strings so persisted snapshots
// remain decodable across schema versions.
type SCIPIngestStats struct {
	ToolName        string `json:"tool_name,omitempty"`
	ToolVersion     string `json:"tool_version,omitempty"`
	Occurrences     int    `json:"occurrences"`
	Edges           int    `json:"edges"`
	Definitions     int    `json:"definitions"`
	References      int    `json:"references"`
	Imports         int    `json:"imports"`
	Calls           int    `json:"calls"`
	Implementations int    `json:"implementations"`
	Inheritances    int    `json:"inheritances"`
	// Calls/Implementations/Inheritances stay zero: SCIP occurrence roles
	// do not encode those relationships (scip.proto SymbolRole); retained
	// for wire compatibility with persisted stats.
	Truncated    int `json:"truncated,omitempty"`
	AppliedFiles int `json:"applied_files"`
	DroppedFiles int `json:"dropped_files,omitempty"`
}

// IngestSCIP converts a parsed SCIP document into codecrawl edges and
// merges them into the per-file edge map (fileEdges). The function is
// deterministic, enforces the package-level edge cap, and returns a
// stable stats struct for diagnostics. The pathOverride is applied to
// every occurrence that does not already carry a path; it is the
// caller's responsibility to associate the document with the originating
// file (codecrawl typically receives one SCIP document per file rather
// than a multi-file document).
//
// SCIP edges win against AST/lexical edges when the same target
// appears in multiple sources because AuthoritySCIP outranks the other
// authority encodings (see AuthorityRank).
func (idx *Index) IngestSCIP(doc codeindex.SCIPDocument, path string, language string) (SCIPIngestStats, error) {
	stats := SCIPIngestStats{
		ToolName:    doc.ToolName,
		ToolVersion: doc.ToolVersion,
	}
	if idx == nil {
		return stats, fmt.Errorf("codecrawl: nil index")
	}
	if path == "" {
		return stats, fmt.Errorf("codecrawl: SCIP ingest requires a path")
	}
	edges, scipStats, err := codeindex.IngestSCIP(doc, language, path)
	if err != nil {
		stats.Truncated = scipStats.Truncated
		return stats, err
	}
	if idx.fileEdges == nil {
		idx.fileEdges = map[string][]Edge{}
	}
	if idx.graph == nil && len(idx.fileEdges) > 0 {
		idx.graph = nil // ensure rebuild on next Graph()
	}
	existing := idx.fileEdges[path]
	// Drop existing AST/heuristic edges that conflict with the same target
	// and lower authority. We keep lexical edges because they are the
	// bounded fallback and can co-exist with SCIP edges without
	// dominating the projection. We also drop existing SCIP edges from
	// the same path so re-ingesting the same document is idempotent.
	filtered := make([]Edge, 0, len(existing))
	for _, edge := range existing {
		if edge.Authority == AuthoritySCIP {
			continue
		}
		if AuthorityRank(edge.Authority) < AuthorityRank(AuthoritySCIP) {
			filtered = append(filtered, edge)
		}
	}
	existing = filtered
	applied := 0
	dropped := 0
	for _, edge := range edges {
		converted := convertSCIPEdge(edge, path)
		if len(existing) >= edgeCap {
			dropped++
			continue
		}
		existing = append(existing, converted)
		applied++
	}
	idx.fileEdges[path] = sortEdges(existing)
	stats.Edges = applied
	stats.Occurrences = scipStats.Occurrences
	stats.Definitions = scipStats.Definitions
	stats.References = scipStats.References
	stats.Imports = scipStats.Imports
	stats.Calls = scipStats.Calls
	stats.Implementations = scipStats.Implementations
	stats.Inheritances = scipStats.Inheritances
	stats.Truncated = scipStats.Truncated
	stats.AppliedFiles = 1
	stats.DroppedFiles = dropped
	// Invalidate the lazy graph so the next Graph() call rebuilds with the
	// SCIP-augmented edges.
	idx.graph = nil
	return stats, nil
}

// convertSCIPEdge maps a codeindex.SCIPEdge into the codecrawl Edge type.
// The mapping is deliberately lossy: SCIP roles with no dedicated codecrawl
// kind (write/read access, generated, test, forward definition, unknown
// bits) arrive as Kind=="reference" from the boundary and stay EdgeReference
// so the projection still carries the (path, line) tuple for the caller.
func convertSCIPEdge(in codeindex.SCIPEdge, fallbackPath string) Edge {
	kind := EdgeKind(in.Kind)
	switch kind {
	case EdgeCall, EdgeReference, EdgeImport, EdgeImplementation, EdgeInheritance, EdgeLexical, EdgeDefinition:
	default:
		kind = EdgeReference
	}
	path := in.Path
	if path == "" {
		path = fallbackPath
	}
	return Edge{
		From:       in.From,
		To:         in.To,
		Kind:       kind,
		Authority:  AuthoritySCIP,
		Confidence: in.Confidence,
		Provenance: Provenance{
			File:     path,
			Line:     int(in.StartLine),
			Column:   int(in.StartCol),
			Parser:   "scip",
			Snippet:  in.Snippet,
			Language: in.Language,
		},
	}
}

// ScipPayload advertises SCIP availability without requiring credentials.
// The function is intentionally synchronous: it returns whether the
// codecrawl Index is ready to ingest SCIP documents, which is always
// true when the index is non-nil. Callers that need runtime configuration
// (e.g. an external SCIP producer) can branch on the returned ready flag.
func (idx *Index) ScipPayload() map[string]any {
	if idx == nil {
		return nil
	}
	return map[string]any{
		"authority": "scip",
		"supported": true,
		"edgeCap":   edgeCap,
		"source":    "codeindex.IngestSCIP",
	}
}

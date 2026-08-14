package codeindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SCIP boundary ingestion (issue #44).
//
// This module accepts a bounded snapshot of a SCIP (or LSIF-shaped) document
// and produces the typed edges that the same project exposes as the
// codecrawl typed-edge projection. The codecrawl package converts these
// edges into its own Edge type with AuthoritySCIP set, so callers can
// branch on authority without parsing the parser label.
//
// The decoder is intentionally narrow: it reads the minimum SCIP "occurrences"
// / "symbols" surface that yields def/ref/import edges. It does not require
// any external dependency, deterministic ordering is preserved across
// re-ingest, and any malformed input degrades to a typed error so the
// caller can fall back to the lexical lane.

const (
	// MaxSCIPDocumentBytes bounds one JSON document before decoding.
	MaxSCIPDocumentBytes = 8 << 20
	// MaxSCIPOccurrences is the per-document cap. Exceeding the cap returns
	// ErrLimitExceeded so callers can branch on bounded intake failure.
	MaxSCIPOccurrences = 250_000
	// MaxSCIPSymbols is the per-document symbol cap.
	MaxSCIPSymbols = 1_000_000
)

// SCIPSymbolRole labels the role a symbol plays at one occurrence, per
// the official SCIP protocol definition (scip.proto enum SymbolRole in
// github.com/scip-code/scip, formerly sourcegraph/scip): Definition,
// Import, WriteAccess, ReadAccess, Generated, Test and ForwardDefinition.
// SCIP occurrence roles do NOT encode call/implementation/inheritance
// relationships — those live in SymbolInformation.relationships, which
// this narrow decoder does not model — so no such roles or kinds are
// fabricated here. Unknown role bits are preserved verbatim on the typed
// edge ("unknown(0x…)") so callers can introspect them without re-parsing.
type SCIPSymbolRole string

const (
	SCIPRoleDefinition        SCIPSymbolRole = "definition"
	SCIPRoleImport            SCIPSymbolRole = "import"
	SCIPRoleWriteAccess       SCIPSymbolRole = "write_access"
	SCIPRoleReadAccess        SCIPSymbolRole = "read_access"
	SCIPRoleGenerated         SCIPSymbolRole = "generated"
	SCIPRoleTest              SCIPSymbolRole = "test"
	SCIPRoleForwardDefinition SCIPSymbolRole = "forward_definition"
	SCIPRoleUnspecified       SCIPSymbolRole = "unspecified"
)

// Official SCIP symbol-role bits, verbatim from the SymbolRole enum in
// scip.proto (github.com/scip-code/scip, formerly sourcegraph/scip):
// Definition=0x1, Import=0x2, WriteAccess=0x4, ReadAccess=0x8,
// Generated=0x10, Test=0x20, ForwardDefinition=0x40. There is no Call,
// Implementation or Inheritance role in SCIP. Do not renumber.
const (
	scipRoleBitDefinition        int32 = 0x1
	scipRoleBitImport            int32 = 0x2
	scipRoleBitWriteAccess       int32 = 0x4
	scipRoleBitReadAccess        int32 = 0x8
	scipRoleBitGenerated         int32 = 0x10
	scipRoleBitTest              int32 = 0x20
	scipRoleBitForwardDefinition int32 = 0x40
	// scipRoleKnownMask covers every role bit defined by scip.proto.
	scipRoleKnownMask int32 = 0x7f
)

// SCIPDocument is a bounded slice of a SCIP/LSIF-shaped document. Only
// the fields used to derive edges are modelled. The raw payload is
// preserved for downstream auditing via Raw when supplied.
type SCIPDocument struct {
	ToolName    string          `json:"toolName,omitempty"`
	ToolVersion string          `json:"toolVersion,omitempty"`
	ProjectRoot string          `json:"projectRoot,omitempty"`
	Occurrences []SCIPOccurence `json:"occurrences"`
	Symbols     []SCIPSymbol    `json:"symbols,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

// SCIPOccurence is one row of the SCIP occurrences table. We accept both
// the canonical SCIP field names and the LSIF-shaped "range" JSON object
// that several tools emit as a fallback. Range extraction prefers the
// canonical [startLine, startCol, endLine, endCol] integer array.
type SCIPOccurence struct {
	Range          []uint32 `json:"range"`
	Symbol         string   `json:"symbol"`
	SymbolRoles    int32    `json:"symbolRoles,omitempty"`
	OverrideRange  []uint32 `json:"overrideRange,omitempty"`
	EnclosingRange []uint32 `json:"enclosingRange,omitempty"`
}

// SCIPSymbol describes one SymbolInformation row from the SCIP document.
// We carry just the identifier and the surface form so the typed edge
// can recover the human-readable name.
type SCIPSymbol struct {
	Symbol        string   `json:"symbol"`
	DisplayName   string   `json:"displayName,omitempty"`
	Documentation []string `json:"documentation,omitempty"`
}

// SCIPEdge is the typed edge produced by IngestSCIP. Callers (notably
// codecrawl) map these to their own wire format. Authority is always
// "scip" so the projection is recognisable without inspecting the parser
// label.
type SCIPEdge struct {
	Path       string
	From       string // empty when the source is implicit (e.g. import)
	To         string
	Kind       string // definition | import | reference (SCIP roles carry no call/implementation/inheritance)
	Role       SCIPSymbolRole
	Authority  string  // always "scip" (provenance: scip.proto-backed bits above)
	Confidence float64 // 0..1, derived from role strength
	Language   string
	StartLine  uint32
	StartCol   uint32
	EndLine    uint32
	EndCol     uint32
	Snippet    string
}

// SCIPStats records coarse diagnostics about an ingest run. Calls,
// Implementations and Inheritances are always zero: SCIP occurrence roles
// do not encode those relationships (see SymbolInformation.relationships
// in scip.proto); the counters are retained for wire compatibility with
// the persisted codecrawl stats shape.
type SCIPStats struct {
	Occurrences     int
	Edges           int
	Definitions     int
	References      int
	Imports         int
	Calls           int
	Implementations int
	Inheritances    int
	Truncated       int
	ToolName        string
	ToolVersion     string
}

// ErrSCIPInvalid is the boundary error returned for structurally invalid
// SCIP documents. It is wrapped so callers can errors.Is/match without
// depending on the underlying decoder.
var ErrSCIPInvalid = errors.New("codeindex: invalid SCIP document")

// ErrSCIPUnsupported reports a SCIP document whose entries use symbols or
// roles outside the supported subset. The ingest still produces edges for
// the supported entries so callers can degrade gracefully.
var ErrSCIPUnsupported = errors.New("codeindex: unsupported SCIP entry")

// DecodeSCIP parses a JSON payload into a SCIPDocument. The payload may be
// the canonical {"occurrences":[...]} map or a {"documents":[{...}]} wrapper;
// the wrapper form emits one document per entry and returns the first that
// parses. Errors wrap ErrSCIPInvalid for diagnostics.
func DecodeSCIP(payload []byte) (SCIPDocument, error) {
	if len(payload) == 0 {
		return SCIPDocument{}, fmt.Errorf("%w: empty payload", ErrSCIPInvalid)
	}
	if len(payload) > MaxSCIPDocumentBytes {
		return SCIPDocument{}, fmt.Errorf("%w: payload exceeds %d bytes", ErrSCIPInvalid, MaxSCIPDocumentBytes)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return SCIPDocument{}, fmt.Errorf("%w: %v", ErrSCIPInvalid, err)
	}
	if probe == nil {
		return SCIPDocument{}, fmt.Errorf("%w: object required", ErrSCIPInvalid)
	}
	if raw, ok := probe["documents"]; ok {
		var wrapped []json.RawMessage
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return SCIPDocument{}, fmt.Errorf("%w: documents array: %v", ErrSCIPInvalid, err)
		}
		if len(wrapped) == 0 {
			return SCIPDocument{}, fmt.Errorf("%w: empty documents array", ErrSCIPInvalid)
		}
		first, err := DecodeSCIP(wrapped[0])
		if err != nil {
			return SCIPDocument{}, err
		}
		first.Raw = json.RawMessage(payload)
		return first, nil
	}
	if _, ok := probe["occurrences"]; !ok {
		return SCIPDocument{}, fmt.Errorf("%w: occurrences required", ErrSCIPInvalid)
	}
	var doc SCIPDocument
	if err := json.Unmarshal(payload, &doc); err != nil {
		return SCIPDocument{}, fmt.Errorf("%w: %v", ErrSCIPInvalid, err)
	}
	doc.Raw = json.RawMessage(payload)
	return doc, nil
}

// roleClassifier maps a SCIP symbol_roles bitset to (Kind, Confidence,
// Role) using the official SymbolRole bits from scip.proto. Priority is
// most-specific first: definition > forward definition > import > write
// access > read access > generated > test. Occurrences carrying no
// recognised bit return kind=="" so the caller degrades them to a
// low-confidence reference; the classifier never fabricates
// call/implementation/inheritance kinds, because SCIP occurrence roles do
// not represent those relationships.
func roleClassifier(roles int32) (kind string, confidence float64, role SCIPSymbolRole) {
	switch {
	case roles&scipRoleBitDefinition != 0:
		return "definition", 0.99, SCIPRoleDefinition
	case roles&scipRoleBitForwardDefinition != 0:
		return "definition", 0.95, SCIPRoleForwardDefinition
	case roles&scipRoleBitImport != 0:
		return "import", 0.85, SCIPRoleImport
	case roles&scipRoleBitWriteAccess != 0:
		return "reference", 0.75, SCIPRoleWriteAccess
	case roles&scipRoleBitReadAccess != 0:
		return "reference", 0.7, SCIPRoleReadAccess
	case roles&scipRoleBitGenerated != 0:
		return "reference", 0.5, SCIPRoleGenerated
	case roles&scipRoleBitTest != 0:
		return "reference", 0.5, SCIPRoleTest
	}
	return "", 0, ""
}

// derivePath picks the source path for one occurrence. SCIP emits a single
// identifier per occurrence; the per-file grouping is the caller's
// responsibility because the SCIP document itself does not tag rows with
// a path. We derive a stable path from the symbol's namespace prefix when
// present, otherwise the caller is expected to pass the project root and
// supply the path via the Occurrence metadata.
func derivePathFromSymbol(symbol string) string {
	if symbol == "" {
		return ""
	}
	// SCIP symbols use "scheme package manager package Name." segmentation.
	// We only use the segment between the third "." and the descriptor that
	// follows the language marker; for cross-language refs that is good
	// enough to keep an authority tag.
	parts := strings.Split(symbol, " ")
	if len(parts) < 2 {
		return ""
	}
	descriptor := parts[len(parts)-1]
	idx := strings.Index(descriptor, ".")
	if idx < 0 {
		return ""
	}
	return strings.TrimPrefix(descriptor[idx+1:], "/")
}

// IngestSCIP produces the deterministic, role-classified edge list for one
// SCIP document. The result is sorted by (Kind, Path, From, To, StartLine,
// StartColumn) so callers can iterate without an explicit sort. The cap
// (MaxSCIPOccurrences) is enforced before symbol resolution so callers can
// branch on ErrLimitExceeded without having to inspect the output.
func IngestSCIP(doc SCIPDocument, language string, pathOverride string) ([]SCIPEdge, SCIPStats, error) {
	stats := SCIPStats{
		ToolName:    doc.ToolName,
		ToolVersion: doc.ToolVersion,
		Occurrences: len(doc.Occurrences),
	}
	if stats.Occurrences > MaxSCIPOccurrences {
		stats.Truncated = stats.Occurrences - MaxSCIPOccurrences
		return nil, stats, fmt.Errorf("%w: occurrences", ErrLimitExceeded)
	}
	if len(doc.Symbols) > MaxSCIPSymbols {
		stats.Truncated = len(doc.Symbols) - MaxSCIPSymbols
		return nil, stats, fmt.Errorf("%w: symbols", ErrLimitExceeded)
	}

	displayNames := make(map[string]string, len(doc.Symbols))
	for _, sym := range doc.Symbols {
		if sym.Symbol == "" {
			continue
		}
		if sym.DisplayName != "" {
			displayNames[sym.Symbol] = sym.DisplayName
		}
	}

	edges := make([]SCIPEdge, 0, len(doc.Occurrences))
	for _, occ := range doc.Occurrences {
		kind, confidence, role := roleClassifier(occ.SymbolRoles)
		if kind == "" {
			// No recognised role bit (unspecified or unknown bits only):
			// degrade to a reference with low confidence so callers retain
			// the (path, symbol) mapping without promoting the edge into
			// the projection. Unknown bits are preserved verbatim on Role.
			kind = "reference"
			confidence = 0.4
			if unknown := occ.SymbolRoles &^ scipRoleKnownMask; unknown != 0 {
				role = SCIPSymbolRole(fmt.Sprintf("unknown(0x%X)", unknown))
			} else {
				role = SCIPRoleUnspecified
			}
		}
		path := pathOverride
		if path == "" {
			path = derivePathFromSymbol(occ.Symbol)
		}
		startLine, startCol, endLine, endCol := rangeBounds(occ.Range)
		if prefix, descriptor, ok := splitSCIPDescriptor(occ.Symbol); ok {
			_ = prefix
			_ = descriptor
		}
		display := displayNames[occ.Symbol]
		if display == "" {
			display = lastSCIPName(occ.Symbol)
		}
		// All kinds except import carry the from-side as the last-seen
		// enclosing function or symbol; the SCIP document does not record
		// that, so we leave From empty unless we can derive it from the
		// enclosing range. Definition/Reference callers can derive the
		// enclosing function from the AST; we just keep the To anchor.
		from := ""
		edge := SCIPEdge{
			Path:       path,
			From:       from,
			To:         display,
			Kind:       kind,
			Role:       role,
			Authority:  "scip",
			Confidence: confidence,
			Language:   language,
			StartLine:  startLine,
			StartCol:   startCol,
			EndLine:    endLine,
			EndCol:     endCol,
		}
		edges = append(edges, edge)
		switch kind {
		case "definition":
			stats.Definitions++
		case "reference":
			stats.References++
		case "import":
			stats.Imports++
		}
		// No call/implementation/inheritance cases: the classifier can
		// only produce definition/import/reference from scip.proto bits.
	}
	stats.Edges = len(edges)
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Kind != edges[j].Kind {
			return edges[i].Kind < edges[j].Kind
		}
		if edges[i].Path != edges[j].Path {
			return edges[i].Path < edges[j].Path
		}
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		if edges[i].StartLine != edges[j].StartLine {
			return edges[i].StartLine < edges[j].StartLine
		}
		return edges[i].StartCol < edges[j].StartCol
	})
	return edges, stats, nil
}

// rangeBounds unpacks the four-element range array using a defensive
// fallback for tools that emit shorter or padded arrays.
func rangeBounds(r []uint32) (uint32, uint32, uint32, uint32) {
	switch {
	case len(r) >= 4:
		return r[0], r[1], r[2], r[3]
	case len(r) == 3:
		return r[0], r[1], r[2], r[1]
	case len(r) == 2:
		return r[0], r[1], r[0], r[1]
	}
	return 0, 0, 0, 0
}

// splitSCIPDescriptor returns the prefix ("scheme descriptor ...") and the
// final descriptor (the part after the last " "). It returns ok=false when
// the symbol does not look like a SCIP descriptor.
func splitSCIPDescriptor(symbol string) (prefix, descriptor string, ok bool) {
	idx := strings.LastIndex(symbol, " ")
	if idx < 0 {
		return "", "", false
	}
	return symbol[:idx], symbol[idx+1:], true
}

// lastSCIPName returns the trailing identifier of a SCIP descriptor. The
// descriptor ends with a "."-separated chain; we walk it from the right
// and take the first non-empty fragment that does not begin with a
// digit. SCIP disambiguators ("(", ")", "#", ":") are stripped so the
// human-readable form matches the source declaration.
func lastSCIPName(symbol string) string {
	if symbol == "" {
		return ""
	}
	_, descriptor, ok := splitSCIPDescriptor(symbol)
	if !ok {
		return symbol
	}
	// Strip the trailing "." separator if present.
	descriptor = strings.TrimSuffix(descriptor, ".")
	// Split on "/" and "." to derive the trailing identifier. SCIP
	// descriptors are space-separated "scheme pkg m identifier." chains
	// where identifier is dotted; we take the last non-empty fragment.
	parts := strings.FieldsFunc(descriptor, func(r rune) bool {
		return r == '/' || r == '.'
	})
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	// SCIP disambiguators: "(", ")", "#", ":".
	for _, ch := range "()#:" {
		if i := strings.IndexRune(name, ch); i >= 0 {
			name = name[:i]
		}
	}
	return name
}

package ontology

import "time"

// GenerationID pins the evidence generation an edge was derived from.
type GenerationID string

// EntityID is a stable opaque identifier within a brain.
type EntityID string

// EntityKind classifies an entity (extensible via ontology config).
type EntityKind string

const (
	KindDocument   EntityKind = "document"
	KindPerson     EntityKind = "person"
	KindProject    EntityKind = "project"
	KindTicket     EntityKind = "ticket"
	KindMetric     EntityKind = "metric"
	KindSystem     EntityKind = "system"
	KindClaim      EntityKind = "claim"
	KindCodeSymbol EntityKind = "code_symbol"
)

// RelationKind classifies a directed edge.
type RelationKind string

const (
	RelCites       RelationKind = "cites"
	RelMentions    RelationKind = "mentions"
	RelOwnedBy     RelationKind = "owned_by"
	RelPartOf      RelationKind = "part_of"
	RelImplements  RelationKind = "implements"
	RelContradicts RelationKind = "contradicts"
	RelSupersedes  RelationKind = "supersedes"
	RelSameThread  RelationKind = "same_thread"
	RelCoProject   RelationKind = "co_project"
	RelDerivedFrom RelationKind = "derived_from"
	RelSupports    RelationKind = "supports"
)

// Entity is a named node in the memory graph.
type Entity struct {
	ID           EntityID
	Kind         EntityKind
	Name         string
	Aliases      []string
	DocumentIDs  []string
	GenerationID GenerationID
	Provenance   string
	CreatedAt    time.Time
}

// Edge is a typed directed relation between entities or documents.
// Document-scoped edges may use DocumentSrc/DocumentDst when entity resolution
// has not yet linked nodes (cold brain).
type Edge struct {
	Src          EntityID
	Dst          EntityID
	DocumentSrc  string
	DocumentDst  string
	Rel          RelationKind
	Weight       float64
	GenerationID GenerationID
	Provenance   string // "deterministic" | "gardener_llm" | "import"
	CreatedAt    time.Time
}

// Graph is an immutable generation-scoped ontology slice.
type Graph struct {
	GenerationID GenerationID
	Entities     []Entity
	Edges        []Edge
}

// Schema describes allowed kinds (loaded from YAML in later leaves).
type Schema struct {
	EntityKinds   []EntityKind
	RelationKinds []RelationKind
	Version       string
}

// DefaultSchema returns the v0 built-in ontology kinds.
func DefaultSchema() Schema {
	return Schema{
		Version: "ontology.v0",
		EntityKinds: []EntityKind{
			KindDocument, KindPerson, KindProject, KindTicket,
			KindMetric, KindSystem, KindClaim, KindCodeSymbol,
		},
		RelationKinds: []RelationKind{
			RelCites, RelMentions, RelOwnedBy, RelPartOf, RelImplements,
			RelContradicts, RelSupersedes, RelSameThread, RelCoProject,
			RelDerivedFrom, RelSupports,
		},
	}
}

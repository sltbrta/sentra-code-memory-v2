package ontology

import "sync"

// GenerationStore is an in-memory, thread-safe map of generation-scoped graphs.
// Suitable for tests and single-node product path until a durable projection lands.
type GenerationStore struct {
	mu     sync.RWMutex
	graphs map[GenerationID]Graph
}

// NewGenerationStore returns an empty store.
func NewGenerationStore() *GenerationStore {
	return &GenerationStore{
		graphs: make(map[GenerationID]Graph),
	}
}

// PutGraph replaces the graph for its GenerationID (no-op if GenerationID empty).
func (s *GenerationStore) PutGraph(g Graph) {
	if s == nil || g.GenerationID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Defensive copy of slices so callers cannot mutate stored state.
	s.graphs[g.GenerationID] = cloneGraph(g)
}

// GetGraph returns a copy of the graph for id, if present.
func (s *GenerationStore) GetGraph(id GenerationID) (Graph, bool) {
	if s == nil || id == "" {
		return Graph{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.graphs[id]
	if !ok {
		return Graph{}, false
	}
	return cloneGraph(g), true
}

// MergeEdges appends edges into the graph for generationID, creating the graph
// when missing. Duplicate (DocumentSrc, DocumentDst, Rel) or (Src, Dst, Rel)
// keys keep the higher weight. Empty generationID is ignored.
func (s *GenerationStore) MergeEdges(generationID GenerationID, edges []Edge) {
	if s == nil || generationID == "" || len(edges) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.graphs[generationID]
	if !ok {
		g = Graph{GenerationID: generationID}
	}
	type ekey struct {
		src, dst string
		es, ed   EntityID
		rel      RelationKind
	}
	index := make(map[ekey]int, len(g.Edges)+len(edges))
	for i, e := range g.Edges {
		index[ekey{
			src: e.DocumentSrc, dst: e.DocumentDst,
			es: e.Src, ed: e.Dst, rel: e.Rel,
		}] = i
	}
	for _, e := range edges {
		e.GenerationID = generationID
		k := ekey{
			src: e.DocumentSrc, dst: e.DocumentDst,
			es: e.Src, ed: e.Dst, rel: e.Rel,
		}
		if i, exists := index[k]; exists {
			if e.Weight > g.Edges[i].Weight {
				prev := g.Edges[i]
				if e.Provenance == "" {
					e.Provenance = prev.Provenance
				}
				if e.CreatedAt.IsZero() {
					e.CreatedAt = prev.CreatedAt
				}
				g.Edges[i] = e
			}
			continue
		}
		index[k] = len(g.Edges)
		g.Edges = append(g.Edges, e)
	}
	s.graphs[generationID] = g
}

func cloneGraph(g Graph) Graph {
	out := Graph{GenerationID: g.GenerationID}
	if len(g.Entities) > 0 {
		out.Entities = append([]Entity(nil), g.Entities...)
	}
	if len(g.Edges) > 0 {
		out.Edges = append([]Edge(nil), g.Edges...)
	}
	return out
}

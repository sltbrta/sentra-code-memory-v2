package ontology

// StoreHopper adapts GenerationStore to query.GraphHopper without importing query
// (avoids import cycles). Local authority wires it as query.GraphHopper.
type StoreHopper struct {
	Store *GenerationStore
}

// Expand returns document-id neighbors via PPR over the generation graph.
func (h StoreHopper) Expand(generationID string, seedPaths []string, limit int) []string {
	if h.Store == nil || generationID == "" || len(seedPaths) == 0 {
		return nil
	}
	g, ok := h.Store.GetGraph(GenerationID(generationID))
	if !ok {
		return nil
	}
	if limit <= 0 {
		limit = 16
	}
	// Prefer PPR; fall back to one-hop neighbors.
	ranked := PPR(g, seedPaths, 15, 0.85, limit+len(seedPaths))
	if len(ranked) == 0 {
		return Neighbors(g, seedPaths, limit)
	}
	// Drop pure seeds from expansion list when extras exist.
	seedSet := map[string]struct{}{}
	for _, s := range seedPaths {
		seedSet[s] = struct{}{}
	}
	out := make([]string, 0, limit)
	for _, id := range ranked {
		if _, isSeed := seedSet[id]; isSeed {
			continue
		}
		out = append(out, id)
		if len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return Neighbors(g, seedPaths, limit)
	}
	return out
}

// PutGraph implements gardener.GraphSink.
func (h StoreHopper) PutGraph(g Graph) error {
	if h.Store == nil {
		return nil
	}
	h.Store.PutGraph(g)
	return nil
}

package projections

import (
	"database/sql"
	"fmt"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

const metaKeyGraph = "has_graph"

// GraphRepository stores generation-scoped ontology graphs as edge rows.
// Entities are not persisted; product hop/PPR only needs document endpoints.
type GraphRepository struct {
	DB *sql.DB
}

// PutGraph replaces all edges for g.GenerationID. Empty GenerationID is a no-op.
// An empty edge list still marks the generation present so GetGraph returns true.
func (r *GraphRepository) PutGraph(g ontology.Graph) error {
	if r == nil || r.DB == nil {
		return fmt.Errorf("projections: nil graph repository")
	}
	if g.GenerationID == "" {
		return nil
	}
	gen := string(g.GenerationID)
	tx, err := r.DB.Begin()
	if err != nil {
		return fmt.Errorf("projections: begin put graph: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM ontology_edges WHERE generation_id = ?`, gen); err != nil {
		return fmt.Errorf("projections: clear edges: %w", err)
	}
	stmt, err := tx.Prepare(`
INSERT INTO ontology_edges (generation_id, src, dst, rel, weight, provenance)
VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("projections: prepare edge insert: %w", err)
	}
	defer stmt.Close()

	for i, e := range g.Edges {
		src, dst := edgeEndpoints(e)
		if src == "" || dst == "" || e.Rel == "" {
			return fmt.Errorf("projections: edge %d missing endpoints or rel", i)
		}
		if _, err := stmt.Exec(gen, src, dst, string(e.Rel), e.Weight, e.Provenance); err != nil {
			return fmt.Errorf("projections: insert edge %d: %w", i, err)
		}
	}
	if _, err := tx.Exec(`
INSERT INTO projection_meta (generation_id, key, value) VALUES (?, ?, '1')
ON CONFLICT(generation_id, key) DO UPDATE SET value = excluded.value`,
		gen, metaKeyGraph); err != nil {
		return fmt.Errorf("projections: mark graph present: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("projections: commit put graph: %w", err)
	}
	return nil
}

// GetGraph loads edges for id. ok is false when the generation was never PutGraph'd.
func (r *GraphRepository) GetGraph(id ontology.GenerationID) (ontology.Graph, bool, error) {
	if r == nil || r.DB == nil {
		return ontology.Graph{}, false, fmt.Errorf("projections: nil graph repository")
	}
	if id == "" {
		return ontology.Graph{}, false, nil
	}
	gen := string(id)
	var marker string
	err := r.DB.QueryRow(
		`SELECT value FROM projection_meta WHERE generation_id = ? AND key = ?`,
		gen, metaKeyGraph,
	).Scan(&marker)
	if err == sql.ErrNoRows {
		return ontology.Graph{}, false, nil
	}
	if err != nil {
		return ontology.Graph{}, false, fmt.Errorf("projections: graph meta: %w", err)
	}

	rows, err := r.DB.Query(`
SELECT src, dst, rel, weight, provenance
FROM ontology_edges WHERE generation_id = ?
ORDER BY src, dst, rel`, gen)
	if err != nil {
		return ontology.Graph{}, false, fmt.Errorf("projections: query edges: %w", err)
	}
	defer rows.Close()

	g := ontology.Graph{GenerationID: id}
	for rows.Next() {
		var src, dst, rel, provenance string
		var weight float64
		if err := rows.Scan(&src, &dst, &rel, &weight, &provenance); err != nil {
			return ontology.Graph{}, false, fmt.Errorf("projections: scan edge: %w", err)
		}
		g.Edges = append(g.Edges, ontology.Edge{
			DocumentSrc:  src,
			DocumentDst:  dst,
			Rel:          ontology.RelationKind(rel),
			Weight:       weight,
			GenerationID: id,
			Provenance:   provenance,
		})
	}
	if err := rows.Err(); err != nil {
		return ontology.Graph{}, false, fmt.Errorf("projections: iterate edges: %w", err)
	}
	return g, true, nil
}

// RepoHopper adapts GraphRepository to query.GraphHopper / gardener.GraphSink
// without importing those packages (same shape as ontology.StoreHopper).
type RepoHopper struct {
	Repo *GraphRepository
}

// Expand returns document-id neighbors via PPR over the generation graph.
func (h RepoHopper) Expand(generationID string, seedPaths []string, limit int) []string {
	if h.Repo == nil || generationID == "" || len(seedPaths) == 0 {
		return nil
	}
	g, ok, err := h.Repo.GetGraph(ontology.GenerationID(generationID))
	if err != nil || !ok {
		return nil
	}
	if limit <= 0 {
		limit = 16
	}
	ranked := ontology.PPR(g, seedPaths, 15, 0.85, limit+len(seedPaths))
	if len(ranked) == 0 {
		return ontology.Neighbors(g, seedPaths, limit)
	}
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
		return ontology.Neighbors(g, seedPaths, limit)
	}
	return out
}

// PutGraph implements gardener.GraphSink.
func (h RepoHopper) PutGraph(g ontology.Graph) error {
	if h.Repo == nil {
		return nil
	}
	return h.Repo.PutGraph(g)
}

func edgeEndpoints(e ontology.Edge) (src, dst string) {
	if e.DocumentSrc != "" && e.DocumentDst != "" {
		return e.DocumentSrc, e.DocumentDst
	}
	return string(e.Src), string(e.Dst)
}

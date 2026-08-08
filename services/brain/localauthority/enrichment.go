package localauthority

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/projections"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/query"
)

// ProductMemory holds optional product-path ontology + gardener state for one
// Runtime. Nil-safe: Stage 03/04 code-only paths leave it unset until first use.
type ProductMemory struct {
	mu       sync.RWMutex
	Store    *ontology.GenerationStore
	Queue    *gardener.MemoryQueue
	Enricher *gardener.GenerationEnricher
	// Scheduler is optional; when set, OnPublished may run a best-effort wave.
	Scheduler *gardener.Scheduler
	// Durable optional SQLite projections (ontology edges + dense).
	ProjDB *projections.DB
	Graph  *projections.GraphRepository
}

// EnsureProductMemory installs in-memory ontology+gardener if missing.
// When OUROBOROS_BRAIN_PROJECTIONS_DB is set, also opens durable SQLite graphs.
func (r *Runtime) EnsureProductMemory() *ProductMemory {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.productMemory != nil {
		return r.productMemory
	}
	store := ontology.NewGenerationStore()
	queue := &gardener.MemoryQueue{}
	var graphSink gardener.GraphSink = ontology.StoreHopper{Store: store}
	var projDB *projections.DB
	var graphRepo *projections.GraphRepository
	if path := os.Getenv("OUROBOROS_BRAIN_PROJECTIONS_DB"); path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if db, err := projections.Open(path); err == nil {
			projDB = db
			graphRepo = &projections.GraphRepository{DB: db.SQL}
			// Dual-write: durable primary + memory for fast hopper.
			graphSink = dualGraphSink{mem: ontology.StoreHopper{Store: store}, durable: projections.RepoHopper{Repo: graphRepo}}
		}
	}
	enricher := &gardener.GenerationEnricher{
		Queue:     queue,
		Budget:    gardener.DefaultBudget(),
		GraphSink: graphSink,
	}
	r.productMemory = &ProductMemory{
		Store:    store,
		Queue:    queue,
		Enricher: enricher,
		ProjDB:   projDB,
		Graph:    graphRepo,
		Scheduler: &gardener.Scheduler{
			Queue:   queue,
			Workers: gardener.DefaultWorkers(),
			Budget:  gardener.DefaultBudget(),
		},
	}
	return r.productMemory
}

// dualGraphSink writes to memory and durable projections.
type dualGraphSink struct {
	mem     ontology.StoreHopper
	durable projections.RepoHopper
}

func (d dualGraphSink) PutGraph(g ontology.Graph) error {
	_ = d.mem.PutGraph(g)
	if d.durable.Repo != nil {
		return d.durable.PutGraph(g)
	}
	return nil
}

// generationBodies returns path→text for a published generation when available.
func (r *Runtime) generationBodies(generationID string) map[string]string {
	if r == nil || generationID == "" {
		return nil
	}
	r.ingestionMu.RLock()
	defer r.ingestionMu.RUnlock()
	if r.ingestion == nil || r.ingestion.current == nil {
		return nil
	}
	cur := r.ingestion.current
	if cur.generation.ID != generationID {
		return nil
	}
	out := make(map[string]string, len(cur.files))
	for path, f := range cur.files {
		if len(f.Content) == 0 {
			continue
		}
		text := string(f.Content)
		if len(text) > 12_000 {
			text = text[:12_000]
		}
		out[path] = text
	}
	return out
}

// runtimeDenseSearcher bag-dense over currently published files.
type runtimeDenseSearcher struct {
	runtime *Runtime
}

func (d runtimeDenseSearcher) Search(ctx context.Context, generationID, q string, topK int) []string {
	bodies := d.runtime.generationBodies(generationID)
	if len(bodies) == 0 {
		return nil
	}
	return query.NewBagOfWordsDense(generationID, bodies).Search(ctx, generationID, q, topK)
}

// ProductGraphHopper returns a query.GraphHopper bound to product memory, or nil.
func (r *Runtime) ProductGraphHopper() ontology.StoreHopper {
	pm := r.EnsureProductMemory()
	if pm == nil || pm.Store == nil {
		return ontology.StoreHopper{}
	}
	return ontology.StoreHopper{Store: pm.Store}
}

// EnrichGeneration enqueues gardener jobs and builds ontology for document texts.
// Safe to call after publish; best-effort — errors are returned to the caller.
//
// When OUROBOROS_BRAIN_GARDENER_DB is set, jobs go to the durable product
// SQLite queue and the wave is left for `product-brain gardener` (async).
// Otherwise the in-memory wave runs inline (Stage-compat / tests).
func (r *Runtime) EnrichGeneration(ctx context.Context, generationID string, documents map[string]string) (int, error) {
	pm := r.EnsureProductMemory()
	if pm == nil || pm.Enricher == nil {
		return 0, nil
	}
	// Prefer durable product queue when configured (async background gardener).
	if path := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_GARDENER_DB")); path != "" {
		if q, err := gardener.OpenSQLiteQueue(path); err == nil {
			defer q.Close()
			enr := &gardener.GenerationEnricher{
				Queue:     q,
				Budget:    gardener.DefaultBudget(),
				GraphSink: pm.Enricher.GraphSink,
			}
			return enr.OnPublished(ctx, generationID, documents)
		}
	}
	n, err := pm.Enricher.OnPublished(ctx, generationID, documents)
	if err != nil {
		return n, err
	}
	// Sync wave when no durable queue (tests / small local).
	async := strings.EqualFold(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_ENRICH")), "async")
	if pm.Scheduler != nil && n > 0 && !async {
		_, _ = pm.Scheduler.RunWave(ctx, "product-authority")
	}
	return n, nil
}

// GraphForGeneration returns a copy of the product ontology graph if present.
func (r *Runtime) GraphForGeneration(generationID string) (ontology.Graph, bool) {
	pm := r.EnsureProductMemory()
	if pm == nil || pm.Store == nil {
		return ontology.Graph{}, false
	}
	return pm.Store.GetGraph(ontology.GenerationID(generationID))
}

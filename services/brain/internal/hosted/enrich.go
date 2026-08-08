package hosted

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
)

// EnrichResult is the outcome of product gardener work after ingest.
type EnrichResult struct {
	JobsEnqueued int
	ReceiptsOK   int
	SidecarsWarm int
	GenerationID string
	DurationMS   int64
	Skipped      bool
	Reason       string
	// Async is true when jobs were enqueued for the background daemon.
	Async bool
	// ClaimsAdmitted / RelationsAdmitted set when post-wave cortex ran
	// (sync EnrichAfterIngest or async RunGardenerWave / daemon).
	ClaimsAdmitted    int
	RelationsAdmitted int
}

// enrichMode returns sync | async | off.
//
//	OUROBOROS_BRAIN_ENRICH=0|false|off  → off
//	OUROBOROS_BRAIN_ENRICH=sync         → inline RunWave (tests / CI)
//	OUROBOROS_BRAIN_ENRICH=async        → enqueue only (needs queue)
//	unset / other truthy                → async when durable queue attached, else sync
func enrichMode(hasDurableQueue bool) string {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("OUROBOROS_BRAIN_ENRICH")))
	switch raw {
	case "0", "false", "no", "off":
		return "off"
	case "sync":
		return "sync"
	case "async", "1", "true", "yes", "on":
		return "async"
	case "":
		if hasDurableQueue {
			return "async"
		}
		return "sync"
	default:
		if hasDurableQueue {
			return "async"
		}
		return "sync"
	}
}

func enrichEnabled() bool {
	return enrichMode(false) != "off" || enrichMode(true) != "off"
}

// AttachGardenerQueue sets the product gardener queue (durable preferred).
// When q implements io.Closer, Close() will close it.
func (c *Client) AttachGardenerQueue(q gardener.Queue) {
	if c == nil {
		return
	}
	c.gardenerQ = q
	if cl, ok := q.(io.Closer); ok {
		c.gardenerCloser = cl
	}
}

// GardenerQueue returns the attached queue (may be nil).
func (c *Client) GardenerQueue() gardener.Queue {
	if c == nil {
		return nil
	}
	return c.gardenerQ
}

// EnsureLocalGardener opens <local_dir>/gardener.db when this client is OpenLocal.
func (c *Client) EnsureLocalGardener() error {
	if c == nil {
		return fmt.Errorf("hosted: nil client")
	}
	if c.gardenerQ != nil {
		return nil
	}
	dir := c.LocalDir()
	if dir == "" {
		return fmt.Errorf("hosted: EnsureLocalGardener requires OpenLocal")
	}
	path := filepath.Join(dir, "gardener.db")
	if env := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_GARDENER_DB")); env != "" {
		path = env
	}
	q, err := gardener.OpenSQLiteQueue(path)
	if err != nil {
		return err
	}
	c.AttachGardenerQueue(q)
	return nil
}

// EnqueueEnrichment plans jobs onto the attached queue without running workers.
// retrieval_ready is already true when this is called (primary chunks written).
func (c *Client) EnqueueEnrichment(ctx context.Context, brainID, generationID string, documents map[string]string) (EnrichResult, error) {
	t0 := time.Now()
	out := EnrichResult{GenerationID: generationID, Async: true}
	if c == nil {
		return out, fmt.Errorf("hosted: nil client")
	}
	if enrichMode(c.durableGardener()) == "off" {
		out.Skipped = true
		out.Reason = "enrich_disabled"
		return out, nil
	}
	docs, generationID, brainID := prepareEnrichDocs(c, brainID, generationID, documents)
	out.GenerationID = generationID
	_ = brainID
	if len(docs) == 0 {
		out.Skipped = true
		out.Reason = "empty_docs"
		return out, nil
	}
	q := c.gardenerQ
	if q == nil {
		// Ephemeral queue only useful if caller immediately RunGardenerWave.
		q = &gardener.MemoryQueue{}
		c.gardenerQ = q
	}
	enricher := &gardener.GenerationEnricher{
		Queue:  q,
		Budget: gardener.DefaultBudget(),
	}
	enricher.Budget.MaxJobs = min(enricher.Budget.MaxJobs, len(docs)*4)
	nJobs, err := enricher.OnPublished(ctx, generationID, docs)
	if err != nil {
		return out, err
	}
	out.JobsEnqueued = nJobs
	out.DurationMS = time.Since(t0).Milliseconds()
	out.Reason = "enqueued"
	return out, nil
}

// EnrichAfterIngest enqueues or runs a gardener wave after product ingest.
// Default for local_fs (durable queue): async enqueue — call RunGardenerWave
// or `product-brain gardener` in the background. Memory tests stay sync.
func (c *Client) EnrichAfterIngest(ctx context.Context, brainID, generationID string, documents map[string]string) (EnrichResult, error) {
	if c == nil {
		return EnrichResult{}, fmt.Errorf("hosted: nil client")
	}
	mode := enrichMode(c.durableGardener())
	if mode == "off" {
		return EnrichResult{Skipped: true, Reason: "enrich_disabled", GenerationID: generationID}, nil
	}
	if mode == "async" {
		return c.EnqueueEnrichment(ctx, brainID, generationID, documents)
	}
	return c.runEnrichSync(ctx, brainID, generationID, documents)
}

// RunGardenerWave processes pending queue jobs and WarmSidecars from receipts.
// This is the product background gardener step (daemon / CLI).
// After the enrich drain, runs heavy memory cortex maintenance when Mem is set
// (extract → SeedRelationsFromClaims → edges / pageindex / PR) — off ingest hot path.
func (c *Client) RunGardenerWave(ctx context.Context) (EnrichResult, error) {
	t0 := time.Now()
	out := EnrichResult{}
	if c == nil {
		return out, fmt.Errorf("hosted: nil client")
	}
	if c.gardenerQ == nil {
		out.Skipped = true
		out.Reason = "no_queue"
		return out, nil
	}
	out.GenerationID = c.GenerationID()
	brainID := c.cfg.BrainID

	// Local residual: size gardener concurrency like local burst workers
	// (Postgres queue allows true multi-worker claim; SQLite serializes writers).
	budget := gardener.LocalWorkerBudget(defaultLocalWorkers())
	var receipts []gardener.Receipt
	d := &gardener.Daemon{
		Queue:    c.gardenerQ,
		Workers:  gardener.DefaultWorkers(),
		Budget:   budget,
		WorkerID: "product-local-workers",
		OnWave: func(r []gardener.Receipt) {
			receipts = append(receipts, r...)
		},
	}
	// Drain until empty (bounded waves).
	for i := 0; i < 64; i++ {
		recs, err := d.RunOnce(ctx)
		if err != nil {
			return out, err
		}
		if len(recs) == 0 {
			break
		}
		out.JobsEnqueued += len(recs)
	}
	for _, rec := range receipts {
		if rec.OK {
			out.ReceiptsOK++
		}
	}
	items := sidecarsFromReceipts(receipts)
	if len(items) > 0 {
		n, werr := c.WarmSidecars(ctx, brainID, items)
		if werr != nil {
			out.Reason = "warm_sidecars:" + werr.Error()
		} else {
			out.SidecarsWarm = n
		}
	}
	// Post-wave product hook: heavy cortex build (no queue job — avoids cycles).
	if c.Mem != nil {
		cres := c.RunCortexMaintenance()
		out.RelationsAdmitted = cres.RelationsAdmitted
		out.ClaimsAdmitted = cres.ClaimsAdmitted
	}
	out.DurationMS = time.Since(t0).Milliseconds()
	return out, nil
}

func (c *Client) runEnrichSync(ctx context.Context, brainID, generationID string, documents map[string]string) (EnrichResult, error) {
	t0 := time.Now()
	out := EnrichResult{GenerationID: generationID}
	docs, generationID, brainID := prepareEnrichDocs(c, brainID, generationID, documents)
	out.GenerationID = generationID
	if len(docs) == 0 {
		out.Skipped = true
		out.Reason = "empty_docs"
		return out, nil
	}
	queue := c.gardenerQ
	if queue == nil {
		queue = &gardener.MemoryQueue{}
	}
	enricher := &gardener.GenerationEnricher{
		Queue:  queue,
		Budget: gardener.DefaultBudget(),
	}
	enricher.Budget.MaxJobs = min(enricher.Budget.MaxJobs, len(docs)*4)
	nJobs, err := enricher.OnPublished(ctx, generationID, docs)
	if err != nil {
		return out, err
	}
	out.JobsEnqueued = nJobs
	sched := &gardener.Scheduler{
		Queue:   queue,
		Workers: gardener.DefaultWorkers(),
		Budget:  enricher.Budget,
	}
	receipts, err := sched.RunWave(ctx, "product-hosted")
	if err != nil {
		return out, err
	}
	for _, rec := range receipts {
		if rec.OK {
			out.ReceiptsOK++
		}
	}
	items := sidecarsFromReceipts(receipts)
	if len(items) == 0 {
		items = buildDeterministicSidecars(docs)
	}
	if len(items) > 0 {
		n, werr := c.WarmSidecars(ctx, brainID, items)
		if werr != nil {
			out.Reason = "warm_sidecars:" + werr.Error()
		} else {
			out.SidecarsWarm = n
		}
	}
	// Same post-wave hook as RunGardenerWave: left-shift claims → TemporalRelations
	// so lean ask can ExpandRelationDocuments without a separate gardener CLI.
	if c.Mem != nil {
		cres := c.RunCortexMaintenance()
		out.RelationsAdmitted = cres.RelationsAdmitted
		out.ClaimsAdmitted = cres.ClaimsAdmitted
	}
	out.DurationMS = time.Since(t0).Milliseconds()
	return out, nil
}

func (c *Client) durableGardener() bool {
	if c == nil || c.gardenerQ == nil {
		return false
	}
	_, ok := c.gardenerQ.(*gardener.SQLiteQueue)
	return ok
}

func prepareEnrichDocs(c *Client, brainID, generationID string, documents map[string]string) (map[string]string, string, string) {
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		brainID = c.cfg.BrainID
	}
	if generationID == "" {
		generationID = c.GenerationID()
	}
	if generationID == "" {
		generationID = "gen-enrich"
	}
	docs := documents
	if len(docs) > 500 {
		keys := make([]string, 0, len(documents))
		for k := range documents {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		docs = map[string]string{}
		for _, k := range keys[:500] {
			docs[k] = documents[k]
		}
	}
	return docs, generationID, brainID
}

func sidecarsFromReceipts(receipts []gardener.Receipt) []SidecarWrite {
	var items []SidecarWrite
	for _, rec := range receipts {
		if rec.Kind != gardener.JobDoc2Query && rec.Kind != gardener.JobContextHeader {
			continue
		}
		text := strings.TrimSpace(rec.Output)
		if text == "" || rec.DocumentID == "" {
			continue
		}
		kind := "d2q"
		if rec.Kind == gardener.JobContextHeader {
			kind = "context_header"
		}
		items = append(items, SidecarWrite{DocumentID: rec.DocumentID, Kind: kind, Text: text})
	}
	return items
}

func buildDeterministicSidecars(docs map[string]string) []SidecarWrite {
	out := make([]SidecarWrite, 0, len(docs))
	for id, text := range docs {
		q := "What does " + id + " cover?"
		snip := strings.TrimSpace(text)
		if len(snip) > 200 {
			snip = snip[:200]
		}
		if snip != "" {
			q = q + " Key terms: " + snip
		}
		out = append(out, SidecarWrite{DocumentID: id, Kind: "d2q", Text: q})
	}
	return out
}

// docsFromChunks maps chunk writes to document_id → text for gardener.
func docsFromChunks(chunks []ChunkWrite) map[string]string {
	m := map[string]string{}
	for _, ch := range chunks {
		id := strings.TrimSpace(ch.DocumentID)
		if id == "" {
			id = strings.TrimSpace(ch.ChunkID)
		}
		if id == "" || ch.Text == "" {
			continue
		}
		if prev, ok := m[id]; ok {
			m[id] = prev + "\n" + ch.Text
		} else {
			m[id] = ch.Text
		}
	}
	return m
}

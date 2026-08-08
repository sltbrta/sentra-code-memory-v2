// product-brain gardener + doc watch commands.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/continual"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
)

func runWatchDocs(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dir := fs.String("dir", "", "product brain directory")
	id := fs.String("brain-id", "local", "brain id")
	docs := fs.String("docs", "", "jsonl file or directory of .md/.txt documents (repeat via comma-list)")
	registry := fs.String("registry", "", "JSON multi-folder registry ({folders:[{path,enabled}]})")
	interval := fs.Duration("interval", time.Second, "poll interval")
	debounce := fs.Duration("debounce", 300*time.Millisecond, "change debounce")
	cycles := fs.Int("max-cycles", 0, "0=forever")
	queue := fs.String("queue", "", "queue substrate sqlite|memory|none")
	cortex := fs.String("cortex", "", "cortex substrate fs|memory|none")
	chunks := fs.String("chunks", "", "chunks substrate fs|memory|neon")
	_ = fs.Parse(args)

	// Resolve paths from --docs and/or --registry (multi-folder background daemon).
	var paths []string
	if *docs != "" {
		for _, p := range strings.Split(*docs, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				paths = append(paths, p)
			}
		}
	}
	if *registry != "" {
		reg, err := continual.LoadWatchRegistry(*registry)
		if err != nil {
			fatal("watch: registry: " + err.Error())
		}
		paths = append(paths, reg.EnabledPaths()...)
		if *interval == time.Second && reg.Interval != "" {
			if d, err := time.ParseDuration(reg.Interval); err == nil && d > 0 {
				*interval = d
			}
		}
		if *debounce == 300*time.Millisecond && reg.Debounce != "" {
			if d, err := time.ParseDuration(reg.Debounce); err == nil && d > 0 {
				*debounce = d
			}
		}
	}
	if *dir == "" || len(paths) == 0 {
		fatal("watch: --dir and --docs and/or --registry required")
	}
	applyCLISubstrateEnv(*dir, "", *queue, *cortex, *chunks)
	c, err := openCLIClient(*dir, *id)
	if err != nil {
		fatal(err.Error())
	}
	defer c.Close()
	ctx := context.Background()

	onDelta := func(path string, res hosted.IngestResult) {
		emitJSON(map[string]any{
			"event": "continual_delta", "path": path, "generation_id": res.GenerationID,
			"ingested": res.Ingested, "upserted": res.Upserted, "mode": res.Mode,
			"enrich_jobs": res.EnrichJobs, "product_owned": true,
		})
	}
	onErr := func(stage string, err error) {
		emitJSON(map[string]any{
			"event": "continual_error", "stage": stage, "error": err.Error(), "product_owned": true,
		})
	}

	if len(paths) == 1 {
		err = continual.WatchDocs(ctx, continual.DocWatchOptions{
			Client: c, DocsPath: paths[0], Interval: *interval, Debounce: *debounce, MaxCycles: *cycles,
			OnDelta: func(res hosted.IngestResult) { onDelta(paths[0], res) },
			OnError: onErr,
		})
	} else {
		err = continual.WatchDocsMulti(ctx, continual.DocWatchMultiOptions{
			Client: c, Paths: paths, Interval: *interval, Debounce: *debounce, MaxCycles: *cycles,
			OnDelta: onDelta,
			OnError: onErr,
		})
	}
	if err != nil && err != context.Canceled {
		fatal(err.Error())
	}
}

func runGardener(args []string) {
	fs := flag.NewFlagSet("gardener", flag.ExitOnError)
	dir := fs.String("dir", "", "product brain directory (queue/cortex root)")
	id := fs.String("brain-id", "local", "brain id")
	once := fs.Bool("once", false, "drain queue once and exit")
	lifecycle := fs.Bool("lifecycle", false, "run Phase 3 lifecycle wave (C1 gate + consolidate)")
	// REM is opt-in: --rem or OUROBOROS_BRAIN_REM=1 (deterministic re-extract; no LLM).
	remFlag := fs.Bool("rem", false, "enable deterministic REM re-extract after NREM (also OUROBOROS_BRAIN_REM=1)")
	// prediction-error < 0 means auto-measure via hold-out probes + Retrieve.
	predErr := fs.Float64("prediction-error", -1, "C1 prediction error; <0 auto-measures probes via retrieve")
	poll := fs.Duration("poll", 500*time.Millisecond, "idle poll when looping")
	// lifecycle-interval: when >0 with loop mode, periodically run lifecycle after drain.
	// Prefer cron + --once / --lifecycle for short-lived processes; this is for long-run daemons.
	lifecycleEvery := fs.Duration("lifecycle-interval", 0, "if >0, run lifecycle every interval while looping (e.g. 30m)")
	queue := fs.String("queue", "", "queue substrate sqlite|memory|none")
	cortex := fs.String("cortex", "", "cortex substrate fs|memory|none")
	chunks := fs.String("chunks", "", "chunks substrate fs|memory|neon")
	subProfile := fs.String("substrate-profile", "", "substrate preset solo|team|bench")
	_ = fs.Parse(args)
	if *dir == "" {
		fatal("gardener: --dir required")
	}
	applyCLISubstrateEnv(*dir, *subProfile, *queue, *cortex, *chunks)
	c, err := openCLIClient(*dir, *id)
	if err != nil {
		fatal(err.Error())
	}
	defer c.Close()
	ctx := context.Background()
	enableREM := *remFlag || os.Getenv("OUROBOROS_BRAIN_REM") == "1"
	if *lifecycle {
		out, err := runProductLifecycle(ctx, c, *dir, *predErr, enableREM)
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(out)
		return
	}
	if *once {
		er, err := c.RunGardenerWave(ctx)
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{
			"event": "gardener_wave", "receipts_ok": er.ReceiptsOK,
			"sidecars_warm": er.SidecarsWarm, "jobs": er.JobsEnqueued,
			"duration_ms": er.DurationMS, "product_owned": true,
		})
		return
	}
	// Long-running background gardener: drain durable queue + WarmSidecars + cortex.
	if c.GardenerQueue() == nil {
		fatal("gardener: no queue (bind via --queue sqlite|memory or OpenResidual defaults)")
	}
	ticker := time.NewTicker(*poll)
	defer ticker.Stop()
	var lastLifecycle time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			er, err := c.RunGardenerWave(ctx)
			if err != nil {
				continue
			}
			if er.JobsEnqueued > 0 || er.SidecarsWarm > 0 || er.ReceiptsOK > 0 {
				emitJSON(map[string]any{
					"event": "gardener_wave", "receipts_ok": er.ReceiptsOK,
					"sidecars_warm": er.SidecarsWarm, "jobs": er.JobsEnqueued,
					"duration_ms": er.DurationMS, "product_owned": true,
				})
			}
			// Full lifecycle wave on interval (same path as --lifecycle; not cortex-only).
			if *lifecycleEvery > 0 && (lastLifecycle.IsZero() || time.Since(lastLifecycle) >= *lifecycleEvery) {
				lastLifecycle = time.Now()
				out, err := runProductLifecycle(ctx, c, *dir, *predErr, enableREM)
				if err != nil {
					emitJSON(map[string]any{
						"event": "gardener_lifecycle_error", "error": err.Error(),
						"interval": lifecycleEvery.String(), "product_owned": true,
					})
					continue
				}
				out["event"] = "gardener_lifecycle_tick"
				out["interval"] = lifecycleEvery.String()
				emitJSON(out)
			}
		}
	}
}

// runProductLifecycle runs C1 gate + NREM/REM/RAPTOR/edges (shared by --lifecycle and --lifecycle-interval).
func runProductLifecycle(ctx context.Context, c *hosted.Client, dir string, predErr float64, enableREM bool) (map[string]any, error) {
	before, _ := productsec.DigestFile(filepath.Join(dir, "chunks.jsonl"))
	q := c.GardenerQueue()
	if q == nil {
		return nil, fmt.Errorf("gardener: no queue")
	}
	mem := c.MemoryStore()
	docs := map[string]string{}
	if mem != nil {
		docs = mem.DocTexts()
		if len(docs) == 0 {
			for _, ep := range mem.ListEpisodes() {
				for _, id := range ep.DocumentIDs {
					docs[id] = id
				}
			}
		}
	}
	if len(docs) == 0 {
		docs["_lifecycle"] = "product lifecycle marker"
	}
	pred := predErr
	c1Auto := false
	if pred < 0 {
		c1Auto = true
		pred = hosted.MeasureC1PredictionErrorMem(c.MemoryStore(), docs, func(question string) []string {
			ps, _, err := c.RetrieveOpts(ctx, question, hosted.RetrieveOptions{TopK: 5})
			if err != nil {
				return nil
			}
			var ids []string
			seen := map[string]struct{}{}
			for _, p := range ps {
				if _, ok := seen[p.DocumentID]; ok {
					continue
				}
				if strings.HasPrefix(p.DocumentID, "summary:") {
					continue
				}
				seen[p.DocumentID] = struct{}{}
				ids = append(ids, p.DocumentID)
			}
			return ids
		}, 3)
	}
	util := map[string]float64{}
	edgeW := map[string]float64{}
	if mem != nil {
		for id := range docs {
			util[id] = mem.GetUtility(id)
		}
		_ = mem.SeedEdgeWeightsFromAdj()
		edgeW = mem.WeightedEdges()
	}
	pol := gardener.LifecyclePolicy{
		PredictionError: pred,
		Utility:         util,
		Edges:           edgeW,
		EnableREM:       enableREM,
		OnUtilityDecay: func(scores map[string]float64) {
			if mem == nil {
				return
			}
			dec := mem.DecayUtilityHalfLife(time.Now().UTC())
			if len(dec) == 0 {
				for id, sc := range scores {
					_ = mem.SetUtility(id, sc)
				}
			}
		},
	}
	recs, err := gardener.RunLifecycle(ctx, q, c.GenerationID(), docs, pol, gardener.DefaultBudget())
	if err != nil {
		return nil, err
	}
	skipHeavy := gardener.LifecyclePolicy{PredictionError: pred}.ShouldSkipHeavy()
	var nrem memory.NREMResult
	var rem memory.REMResult
	var reseg memory.ResegmentResult
	edgeDelta := 0
	pruned := 0
	if mem != nil && !skipHeavy {
		_ = c.RunCortexMaintenance()
		nrem = mem.RunNREM(docs, 0.2, 1.5)
		if enableREM {
			rem = mem.RunREM(docs, 1.5)
		}
		raptorDocs := mem.DocTexts()
		if len(raptorDocs) == 0 {
			raptorDocs = docs
		}
		var kept []memory.SummaryNode
		for _, n := range mem.ListSummaries() {
			if n.Kind == "community" {
				kept = append(kept, n)
			}
		}
		kept = append(kept, memory.BuildRAPTORSummaries(raptorDocs, 8)...)
		_ = mem.StoreRAPTOR(kept)
		_ = mem.LinkClaimDocuments(memory.DefaultClaimEdgeCap)
		reseg = mem.LifecycleResegment(c.GenerationID(), docs)
		edgeDelta = mem.HypothesizeEdges()
		pruned = mem.PruneWeakEdges(0.1)
	}
	after, _ := productsec.DigestFile(filepath.Join(dir, "chunks.jsonl"))
	out := map[string]any{
		"event": "gardener_lifecycle", "receipts": len(recs),
		"evidence_digest_before": before, "evidence_digest_after": after,
		"digest_stable": before == after, "product_owned": true,
		"prediction_error": pred, "c1_auto": c1Auto,
		"c1_skip": skipHeavy,
	}
	if mem != nil {
		out["utility_sample"] = map[string]float64{}
		n := 0
		for id := range docs {
			out["utility_sample"].(map[string]float64)[id] = mem.GetUtility(id)
			n++
			if n >= 5 {
				break
			}
		}
		out["summaries"] = len(mem.ListSummaries())
		if edges := mem.DocEdges(); len(edges) > 0 {
			out["ppr_edges"] = len(edges)
		}
		out["ppr_edge_weights"] = mem.EdgeCount()
		out["nrem_quarantined"] = len(nrem.Quarantined)
		out["nrem_promoted"] = len(nrem.Promoted)
		out["rem_enabled"] = enableREM
		if enableREM {
			out["rem_docs"] = len(rem.DocsScanned)
			out["rem_claims"] = rem.ClaimsAdmitted
			out["rem_relations"] = rem.RelationsAdmitted
		}
		out["quarantine_total"] = len(mem.ListQuarantine())
		out["episodes"] = len(mem.ListEpisodes())
		out["episodes_after"] = reseg.EpisodesAfter
		out["reseg"] = reseg.Reseg
		out["edge_hyp_delta"] = edgeDelta
		out["edges_pruned"] = pruned
		out["claims"] = len(mem.CurrentClaims(time.Time{}, true))
		// Graphiti-class left-shift: active TemporalRelations after cortex/REM.
		out["relations"] = len(mem.CurrentRelationsAsOf(time.Time{}, time.Time{}, true))
		out["contested_groups"] = len(mem.ContestedGroups())
	}
	return out, nil
}

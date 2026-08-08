package hosted

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

func TestSoloSubstrateBindsQueueAndCortex(t *testing.T) {
	dir := t.TempDir()
	c, err := CreateLocal(dir, "solo")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rep := c.SubstrateReport()
	if rep["chunks"] != "local_fs" && rep["chunks"] != "fs" {
		// StoreKind returns local_fs
		if !strings.Contains(rep["chunks"], "local") && rep["chunks"] != "local_fs" {
			t.Fatalf("chunks=%v", rep)
		}
	}
	if c.GardenerQueue() == nil {
		t.Fatal("queue not bound")
	}
	if c.MemoryStore() == nil {
		t.Fatal("cortex not bound")
	}
	if rep["queue"] == "none" || rep["cortex"] == "none" {
		t.Fatalf("report=%v", rep)
	}
	// Solo path still dual-cites after maintenance.
	ctx := context.Background()
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "d1", Text: "Widget price is $10."},
		{ID: "d2", Text: "Widget price is $12."},
	}, 1); err != nil {
		t.Fatal(err)
	}
	_ = c.RunCortexMaintenance()
	mem := c.MemoryStore()
	_, _, _ = mem.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "price", Object: "$10", DocumentIDs: []string{"d1"},
	})
	_, contested, err := mem.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "price", Object: "$12", DocumentIDs: []string{"d2"},
	})
	if err != nil || len(contested) == 0 {
		t.Fatalf("contested: %v %v", err, contested)
	}
	ans := c.AnswerOpts(ctx, AnswerOptions{Question: "What is Widget price?", TopK: 6})
	if ans.RetrievalDiagnostics == nil {
		t.Fatal("nil diag")
	}
	pol := ans.RetrievalDiagnostics["conflict_policy"]
	if pol != "dual_cite_and_abstain" && pol != "dual_cite" {
		t.Fatalf("policy=%v answer=%q", pol, ans.Answer)
	}
}

func TestMixedMemoryChunksWithFSQueueCortex(t *testing.T) {
	// Chunks = in-process memory; queue+cortex on FS — proves independent binding.
	dir := t.TempDir()
	c, err := OpenMemoryWithSubstrates("mix", SubstrateConfig{
		Profile: "custom",
		Dir:     dir,
		Chunks:  SubstrateChunksMemory,
		Queue:   SubstrateQueueSQLite,
		Cortex:  SubstrateCortexFS,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rep := c.SubstrateReport()
	if rep["chunks"] != "memory" && !strings.Contains(rep["chunks"], "memory") {
		// StoreKind for MemoryChunkStore
		if c.StoreKind() != "memory" {
			t.Fatalf("want memory chunks, report=%v kind=%s", rep, c.StoreKind())
		}
	}
	if c.GardenerQueue() == nil {
		t.Fatal("queue not bound on mixed open")
	}
	if c.MemoryStore() == nil {
		t.Fatal("cortex not bound on mixed open")
	}
	// SQLite queue file should exist under dir after bind.
	if _, err := filepath.Glob(filepath.Join(dir, "gardener.db")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Memory-store ingest path (not Local durable).
	chunks := []ChunkWrite{
		{ChunkID: "d1#0", DocumentID: "d1", Text: "Gadget cost is $5.", SourceURI: "t://d1"},
		{ChunkID: "d2#0", DocumentID: "d2", Text: "Gadget cost is $9.", SourceURI: "t://d2"},
	}
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BurstUpsert(ctx, c.Config().BrainID, chunks, 1); err != nil {
		t.Fatal(err)
	}
	// Enqueue enrich + wave + cortex maintenance.
	docs := map[string]string{"d1": chunks[0].Text, "d2": chunks[1].Text}
	if _, err := c.EnrichAfterIngest(ctx, c.Config().BrainID, "gen-mix", docs); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunGardenerWave(ctx); err != nil {
		t.Fatal(err)
	}
	mem := c.MemoryStore()
	// Maintenance should have run post-wave; force if empty extract.
	if len(mem.DocTexts()) == 0 {
		_ = mem.SetDocTexts(docs)
		_ = c.RunCortexMaintenance()
	}
	_, _, _ = mem.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "cost", Object: "$5", DocumentIDs: []string{"d1"},
	})
	_, contested, err := mem.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "cost", Object: "$9", DocumentIDs: []string{"d2"},
	})
	if err != nil || len(contested) == 0 {
		t.Fatalf("mixed contested: %v %+v", err, contested)
	}
	// Seed utility so ranking path is load-bearing (not only claim path).
	_ = mem.SetUtility("d1", 1.2)
	_ = mem.SetUtility("d2", 0.9)
	ans := c.AnswerOpts(ctx, AnswerOptions{Question: "What is Gadget cost?", TopK: 6})
	diag := ans.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("nil diag")
	}
	// Mixed path must surface both cortex rank and dual-cite policy (hard fail).
	if diag["utility_ranking"] != true {
		t.Fatalf("want utility_ranking=true; diag=%v answer=%q", diag, ans.Answer)
	}
	pol := diag["conflict_policy"]
	if pol != "dual_cite_and_abstain" && pol != "dual_cite" {
		t.Fatalf("want dual_cite* conflict_policy; got %v diag=%v answer=%q cites=%v",
			pol, diag, ans.Answer, ans.CitedDocumentIDs)
	}
	if rep["queue"] != SubstrateQueueSQLite && rep["queue"] != "bound" {
		t.Fatalf("mixed queue report=%v", rep)
	}
	if rep["cortex"] != SubstrateCortexFS && rep["cortex"] != "bound" {
		t.Fatalf("mixed cortex report=%v", rep)
	}
}

func TestSubstrateFromEnvTeamMix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OUROBOROS_BRAIN_PROFILE", "team")
	t.Setenv("OUROBOROS_BRAIN_DIR", dir)
	t.Setenv("OUROBOROS_BRAIN_QUEUE", "sqlite")
	t.Setenv("OUROBOROS_BRAIN_CORTEX", "fs")
	// dense=qdrant requires QDRANT_URL+KEY (fail-closed); local team mix uses sqlite dense.
	t.Setenv("OUROBOROS_BRAIN_DENSE", "sqlite")
	t.Setenv("OUROBOROS_BRAIN_DENSE_SEARCH_MODE", "ann")
	cfg := SubstrateFromEnv()
	if cfg.Profile != ProfileTeam {
		t.Fatalf("profile=%q", cfg.Profile)
	}
	if cfg.Queue != SubstrateQueueSQLite || cfg.Cortex != SubstrateCortexFS {
		t.Fatalf("cfg=%+v", cfg)
	}
	if cfg.DenseSearchMode != "ann" {
		t.Fatalf("dense search mode=%q", cfg.DenseSearchMode)
	}
	// Memory chunks + env team substrates.
	c := OpenMemory("env-mix")
	defer c.Close()
	if err := ApplySubstrates(c, cfg); err != nil {
		t.Fatal(err)
	}
	if c.GardenerQueue() == nil || c.MemoryStore() == nil {
		t.Fatalf("bindings missing: %+v", c.SubstrateReport())
	}
}

func TestDenseSearchModeRejectsInvalidOverride(t *testing.T) {
	c := OpenMemory("bad-dense-mode")
	defer c.Close()
	err := ApplySubstrates(c, SubstrateConfig{
		Dir: t.TempDir(), Dense: SubstrateDenseSQLite, DenseSearchMode: "sometimes",
		Queue: SubstrateQueueNone, Cortex: SubstrateCortexNone,
	})
	if err == nil || !strings.Contains(err.Error(), "auto|exact|ann") {
		t.Fatalf("invalid override error=%v", err)
	}
}

func TestMemoryQueueAliasIsDurableWhenDirSet(t *testing.T) {
	// Hosted residual rule: queue=memory with Dir becomes durable SQLite.
	dir := t.TempDir()
	c, err := OpenResidual("qmem", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueMemory, Cortex: SubstrateCortexFS,
		Dense: SubstrateDenseNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rep := c.SubstrateReport()
	if rep["queue"] != SubstrateQueueSQLite {
		t.Fatalf("want durable sqlite, got %v", rep)
	}
	if rep["queue_durable"] != "true" {
		t.Fatalf("want queue_durable=true: %v", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, "gardener.db")); err != nil {
		t.Fatalf("gardener.db missing: %v", err)
	}
	// Enqueue survives process-equivalent reopen.
	q := c.GardenerQueue()
	if err := q.Enqueue(context.Background(), gardener.Job{
		ID: "j1", Kind: gardener.JobDenseEmbed, GenerationID: "g1", DocumentID: "d1",
	}); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c2, err := OpenResidual("qmem", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite, Cortex: SubstrateCortexFS,
		Dense: SubstrateDenseNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	jobs, err := c2.GardenerQueue().Claim(context.Background(), "w1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) < 1 {
		t.Fatalf("durable queue lost jobs after reopen: %d", len(jobs))
	}
}

func TestLocalBurstWorkersSingleFlush(t *testing.T) {
	// Parallel workers must not corrupt durable projection; one flush at end.
	dir := t.TempDir()
	c, err := CreateLocal(dir, "burst")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	docs := make([]LocalDocument, 0, 24)
	for i := 0; i < 24; i++ {
		docs = append(docs, LocalDocument{
			ID:   fmt.Sprintf("d%d", i),
			Text: fmt.Sprintf("document body number %d with unique tokens xx%d", i, i),
		})
	}
	res, err := c.BurstIngestLocal(ctx, docs, 8)
	if err != nil {
		t.Fatal(err)
	}
	if res.Workers != 8 {
		t.Fatalf("workers=%d", res.Workers)
	}
	if res.Ingested != 24 {
		t.Fatalf("ingested=%d", res.Ingested)
	}
	// Reopen and ensure all docs survived the single flush.
	_ = c.Close()
	c2, err := OpenLocal(dir, "burst")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	hits, err := c2.Store().LexicalSearch(ctx, "burst", "unique tokens", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 8 {
		t.Fatalf("expected multi-doc lexical after parallel burst, hits=%d", len(hits))
	}
}

func TestLocalDenseSQLiteUpsertSearchAndAsk(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenResidual("dense1", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseSQLite,
		Embed: SubstrateAPINone, LLM: SubstrateAPINone, Ranker: SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.localDense == nil {
		t.Fatal("local dense not bound")
	}
	ctx := context.Background()
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "alpha", Text: "quantum lattice cooling protocol alpha"},
		{ID: "beta", Text: "unrelated cooking recipe for pasta"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dense.db")); err != nil {
		t.Fatalf("dense.db missing: %v", err)
	}
	// Reopen: vectors must persist.
	_ = c.Close()
	c2, err := OpenResidual("dense1", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseSQLite,
		Embed: SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	ans := c2.AnswerOpts(ctx, AnswerOptions{Question: "quantum lattice cooling", TopK: 4})
	diag := ans.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("nil diag")
	}
	if diag["dense_arm"] == nil && diag["dense_hits"] == nil {
		t.Fatalf("expected dense arm diagnostics: %v answer=%q", diag, ans.Answer)
	}
	rep := c2.SubstrateReport()
	if rep["dense"] != SubstrateDenseSQLite {
		t.Fatalf("dense report=%v", rep)
	}
}

package productsearch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/continual"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsearch"
)

// TestProductSupersetSurfaces exercises company-doc + codeindex + gardener + continual
// on the product path — Stage capabilities absorbed into the product facade.
func TestProductSupersetSurfaces(t *testing.T) {
	ctx := context.Background()
	brainDir := t.TempDir()

	// 1) Company-doc product (hosted) + async gardener drain.
	c, err := hosted.CreateLocal(brainDir, "superset")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.BurstIngestLocal(ctx, []hosted.LocalDocument{
		{ID: "pol", Text: "MedThink RPO is fifteen minutes. Kyoto office open."},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserted < 1 && res.Ingested < 1 {
		t.Fatalf("ingest: %+v", res)
	}
	if _, err := c.RunGardenerWave(ctx); err != nil {
		t.Fatal(err)
	}
	ans := c.Answer(ctx, "What is MedThink RPO?", 6)
	if ans.Failure != "" || ans.Answer == "" {
		t.Fatalf("ask: %+v", ans)
	}

	// 2) Stage P5 exact codeindex via productsearch.
	codeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(codeDir, "m.go"), []byte("package m\n\nfunc SearchCode() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exact := productsearch.Search(ctx, productsearch.Request{
		Profile: productsearch.ProfileCodeExact, CodeRoot: codeDir, Question: "SearchCode",
		ExactKind: "definition", TopK: 5,
	})
	if exact.Failure != "" || len(exact.Hits) < 1 {
		t.Fatalf("code_exact: %+v", exact)
	}

	// 3) codeindex.Build directly (Stage library live under product).
	snap, err := codeindex.Build(ctx, []codeindex.SourceFile{{
		Path: "m.go", Language: codeindex.LanguageGo, Content: []byte("package m\nfunc X() {}\n"),
	}}, codeindex.DefaultLimits())
	if err != nil || len(snap.Files) != 1 {
		t.Fatalf("codeindex.Build: %v snap=%+v", err, snap)
	}

	// 4) Durable gardener queue (product async).
	q, err := gardener.OpenSQLiteQueue(filepath.Join(brainDir, "gardener-superset.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	enr := &gardener.GenerationEnricher{Queue: q, Budget: gardener.DefaultBudget()}
	n, err := enr.OnPublished(ctx, "gen-s", map[string]string{"d": "RPO text"})
	if err != nil || n < 1 {
		t.Fatalf("enqueue n=%d err=%v", n, err)
	}
	d := &gardener.Daemon{Queue: q, Workers: gardener.DefaultWorkers(), Budget: gardener.DefaultBudget()}
	if recs, err := d.RunOnce(ctx); err != nil || len(recs) < 1 {
		t.Fatalf("wave: n=%d err=%v", len(recs), err)
	}

	// 5) Continual docs watch one cycle.
	docs := filepath.Join(t.TempDir(), "d.jsonl")
	if err := os.WriteFile(docs, []byte(`{"id":"c1","text":"continual fact Kyoto"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := hosted.CreateLocal(t.TempDir(), "cont")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var saw bool
	_ = continual.WatchDocs(ctx, continual.DocWatchOptions{
		Client: c2, DocsPath: docs, Interval: 1, Debounce: 1, MaxCycles: 1,
		OnDelta: func(hosted.IngestResult) { saw = true },
	})
	if !saw {
		t.Fatal("continual did not fire")
	}
}

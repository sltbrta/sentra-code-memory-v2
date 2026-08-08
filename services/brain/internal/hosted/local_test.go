package hosted

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLocalRoundTripStructureAndAsk(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	c, err := CreateLocal(dir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if c.StoreKind() != "local_fs" {
		t.Fatalf("store=%s", c.StoreKind())
	}
	if c.GenerationID() != "gen-0" {
		t.Fatalf("gen=%s", c.GenerationID())
	}

	docs := []LocalDocument{
		{ID: "d_seed", Title: "Seed", Text: "MedThink recovery PROJ_LOCAL99 for seed only."},
		{ID: "d_linked", Text: "Neighbor PROJ_LOCAL99 carries ZZYXLOCALONLY token."},
		{ID: "d_noise", Text: "Picnic sandwiches unrelated."},
	}
	res, err := c.BurstIngestLocal(ctx, docs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ingested != 3 {
		t.Fatalf("ingested=%d", res.Ingested)
	}
	if res.GenerationID != "gen-1" {
		t.Fatalf("gen after burst=%s", res.GenerationID)
	}
	// Structure hop should know about linked.
	if d, ok := c.store.(*durableStore); ok {
		edge, _, _ := d.StructureExpand("demo", []string{"d_seed"}, 8)
		found := false
		for _, id := range edge {
			if id == "d_linked" {
				found = true
			}
		}
		if !found {
			t.Fatalf("edge expand missing d_linked: %v", edge)
		}
	}

	// Reopen from disk — durable projection.
	c2, err := OpenLocal(dir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if c2.GenerationID() != "gen-1" {
		t.Fatalf("reopen gen=%s", c2.GenerationID())
	}
	ps, diag, err := c2.Retrieve(ctx, "MedThink recovery seed", 6)
	if err != nil {
		t.Fatal(err)
	}
	if diag["store"] != "local_fs" {
		t.Fatalf("diag store=%v", diag["store"])
	}
	if len(ps) == 0 {
		t.Fatal("no passages after reopen")
	}
	// files on disk
	for _, name := range []string{"meta.json", "chunks.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}

	// Delta no-op when unchanged
	res2, err := c2.ContinualDeltaLocal(ctx, docs)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Ingested != 0 {
		t.Fatalf("delta should skip unchanged, got %d", res2.Ingested)
	}

	// CreateLocal refuses existing
	if _, err := CreateLocal(dir, "demo"); err == nil {
		t.Fatal("expected refuse existing brain")
	}
}

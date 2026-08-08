package hosted

import (
	"context"
	"strings"
	"testing"
)

// Residual product path must not fall through to path2 SMF when productOwned.
func TestProductOwnedRetrieveUsesStore(t *testing.T) {
	c := OpenMemory("prod-own")
	if c == nil {
		t.Fatal("OpenMemory nil")
	}
	defer c.Close()
	if !c.ProductOwned() {
		t.Fatal("OpenMemory must set productOwned")
	}
	ctx := context.Background()
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "doc1", Text: "quantum lattice cooling for residual product path"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	ps, diag, err := c.RetrieveOpts(ctx, "quantum lattice", RetrieveOptions{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) == 0 {
		t.Fatal("expected product passages")
	}
	if diag["path"] == "path2" {
		t.Fatalf("unexpected path2: %v", diag)
	}
	joined := strings.ToLower(ps[0].Text)
	if !strings.Contains(joined, "quantum") {
		t.Fatalf("top=%q", ps[0].Text)
	}
}

func TestUpsertDenseFailsClosedWithoutBackend(t *testing.T) {
	c := OpenMemory("no-dense")
	if c == nil {
		t.Fatal("nil")
	}
	defer c.Close()
	err := c.UpsertDense(context.Background(), []DensePoint{{
		ID: "x", Vector: []float32{0.1, 0.2},
	}})
	if err == nil {
		t.Fatal("expected fail-closed UpsertDense")
	}
	if !strings.Contains(err.Error(), "no dense backend") {
		t.Fatalf("msg=%v", err)
	}
}

func TestContinualDeltaSeedsWhenDenseBound(t *testing.T) {
	// Ensures ContinualDeltaLocal calls seedDenseAfterIngest (no panic; dense optional).
	dir := t.TempDir()
	c, err := OpenResidual("delta-dense", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseSQLite,
		Embed: SubstrateAPINone, LLM: SubstrateAPINone, Ranker: SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.ProductOwned() {
		t.Fatal("OpenResidual must set productOwned")
	}
	ctx := context.Background()
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "a", Text: "alpha document one"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContinualDeltaLocal(ctx, []LocalDocument{
		{ID: "a", Text: "alpha document one revised for dense seed"},
		{ID: "b", Text: "beta document two"},
	}); err != nil {
		t.Fatal(err)
	}
	if c.localDense == nil {
		t.Fatal("dense backend should be bound")
	}
}

func TestBindDenseQdrantRequiresEnv(t *testing.T) {
	t.Setenv("QDRANT_URL", "")
	t.Setenv("QDRANT_API_KEY", "")
	dir := t.TempDir()
	_, err := OpenResidual("qfail", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueNone,
		Dense: SubstrateDenseQdrant,
	})
	if err == nil {
		t.Fatal("expected dense=qdrant to fail without QDRANT_URL/KEY")
	}
	if !strings.Contains(err.Error(), "QDRANT") {
		t.Fatalf("msg=%v", err)
	}
}

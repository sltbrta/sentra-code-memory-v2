package hosted

import (
	"context"
	"testing"
)

func TestResidualRetrievePlaneHonesty(t *testing.T) {
	c := OpenMemory("plane-honest")
	if c == nil {
		t.Fatal("nil client")
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "d1", Text: "residual company document about quantum lattice"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	_, diag, err := c.RetrieveOpts(ctx, "quantum lattice", RetrieveOptions{TopK: 4})
	if err != nil {
		t.Fatal(err)
	}
	if diag["plane"] != "residual" {
		t.Fatalf("plane=%v", diag["plane"])
	}
	if diag["not_authority_query"] != true {
		t.Fatalf("must declare not authority: %v", diag)
	}
	if diag["not_path2_smf"] != true {
		t.Fatalf("must declare not path2: %v", diag)
	}
	if diag["graph_truth"] != "memory_edges" {
		t.Fatalf("graph_truth=%v", diag["graph_truth"])
	}
}

func TestSubstrateReportResidualGraphDense(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenResidual("rep", SubstrateConfig{
		Dir: dir, Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseSQLite,
		Embed: SubstrateAPINone, LLM: SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rep := c.SubstrateReport()
	if rep["plane"] != "residual" {
		t.Fatalf("plane=%v", rep["plane"])
	}
	if rep["graph_truth"] != "memory_edges" {
		t.Fatalf("graph_truth=%v", rep)
	}
	if rep["dense"] != SubstrateDenseSQLite {
		t.Fatalf("dense=%v", rep["dense"])
	}
	if rep["dense_plane"] == "path2_eval" {
		t.Fatal("residual must not claim path2 dense plane")
	}
}

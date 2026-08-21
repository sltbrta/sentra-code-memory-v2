package hosted

import (
	"context"
	"testing"
)

// deletion.Purge named `vectors` as skipped because none of the five backends
// behind denseBackend exposed a delete, so erased content stayed answerable by
// vector search after the corpus, the lexical index, the cortex and the query
// log had all dropped it.

func denseBrain(t *testing.T) *Client {
	t.Helper()
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "0")
	t.Setenv("OUROBOROS_ERB_BLIND_PLAN", "0")
	client, err := OpenResidual("purge-dense", SubstrateConfig{
		Dir: t.TempDir(), Chunks: SubstrateChunksFS, Queue: SubstrateQueueSQLite,
		Cortex: SubstrateCortexFS, Dense: SubstrateDenseSQLite,
		Embed: SubstrateAPINone, LLM: SubstrateAPINone, Ranker: SubstrateAPINone,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.BurstIngestLocal(context.Background(), []LocalDocument{
		{ID: "secret-doc", Text: "confidential quarterly figures for the segment", SourceURI: "file://secret"},
		{ID: "public-doc", Text: "public quarterly summary for the segment", SourceURI: "file://public"},
	}, 1); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestPurgeReachesTheDenseVectors(t *testing.T) {
	client := denseBrain(t)
	if client.localDense == nil {
		t.Fatal("no dense backend was configured, so this guard checks nothing")
	}

	before, err := client.localDense.HasDocuments([]string{"secret-doc"})
	if err != nil {
		t.Fatalf("HasDocuments: %v", err)
	}
	if len(before) == 0 {
		t.Skip("the fixture produced no vectors on this build, so there is " +
			"nothing to purge")
	}

	receipt, err := client.PurgeDocuments("purge-dense", []string{"secret-doc"})
	if err != nil {
		t.Fatalf("PurgeDocuments: %v", err)
	}

	after, err := client.localDense.HasDocuments([]string{"secret-doc"})
	if err != nil {
		t.Fatalf("HasDocuments: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("the purged document still has vectors: %v", after)
	}
	if surviving, _ := client.localDense.HasDocuments([]string{"public-doc"}); len(surviving) == 0 {
		t.Fatal("purging one document removed another's vectors")
	}

	// With every substrate reached, the receipt may finally claim completeness
	// -- which it refused to do while the vector store was unreachable.
	for _, skipped := range receipt.Skipped {
		if skipped == "vectors" {
			t.Fatalf("the vector store is still reported as skipped: %+v", receipt)
		}
	}
	if !receipt.VerifiedComplete {
		t.Fatalf("the purge is not verified complete: %+v", receipt)
	}
}

// TestAnUnconfiguredBackendIsNamedAsSkipped keeps the honest disposition for a
// backend that cannot answer at all.
//
// Both remote backends now implement the purge port -- see
// remote_purge_test.go -- so the previous version of this test, which asserted
// they refuse, no longer describes them. What still has to hold is the
// disposition when a backend cannot be reached: it is not wired as a purger,
// and the receipt names `vectors` as skipped rather than claiming completeness
// from zero removals.
func TestAnUnconfiguredBackendIsNamedAsSkipped(t *testing.T) {
	for name, backend := range map[string]denseBackend{
		// No base URL and no collection: nothing to talk to.
		"faiss":  &faissDense{},
		"qdrant": &residualQdrantDense{},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.DeleteDocuments([]string{"doc"}); err == nil {
				t.Fatalf("%s reported a delete against nothing", name)
			}
			if _, err := backend.HasDocuments([]string{"doc"}); err == nil {
				t.Fatalf("%s reported a verification against nothing", name)
			}

			client := &Client{localDense: backend}
			if client.vectorPurger() != nil {
				t.Fatalf("%s was wired as a purger while unreachable; the "+
					"fan-out would report zero removals as an erasure", name)
			}
		})
	}
}

// TestAVerificationFailureIsReadAsAResidual is the fail-closed reading:
// unable to verify is not the same as verified empty.
func TestAVerificationFailureIsReadAsAResidual(t *testing.T) {
	purger := denseVectorPurger{backend: &faissDense{}}
	residual := purger.HasDocumentVectors([]string{"doc-1", "doc-2"})
	if len(residual) != 2 {
		t.Fatalf("a backend that cannot answer was read as empty: %v", residual)
	}
}

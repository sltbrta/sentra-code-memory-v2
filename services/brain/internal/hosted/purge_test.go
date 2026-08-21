package hosted

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// Deleting content left it answering searches.
//
// internal/deletion flipped a manifest to immediate-deny and scheduled a purge
// job for the object store. Nothing removed the projections a query is
// actually answered from, so the document stayed in the corpus, the lexical
// index, the memory cortex and the query log -- and went on being retrieved,
// ranked and cited.
//
// This drives the fan-out through a real local brain: the substrates are the
// product's own, not fakes.

func purgeBrain(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	client, err := OpenLocal(dir, "purge-brain")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	if _, err := client.BurstUpsert(ctx, "purge-brain", []ChunkWrite{
		{DocumentID: "secret-doc", ChunkID: "secret-doc#0",
			Text: "quarterly revenue recognition confidential figures", SourceURI: "file://secret"},
		{DocumentID: "public-doc", ChunkID: "public-doc#0",
			Text: "quarterly revenue recognition public summary", SourceURI: "file://public"},
	}, 1); err != nil {
		t.Fatal(err)
	}

	// Cortex: bodies, adjacency and a claim citing the document.
	if err := client.Mem.SetDocTexts(map[string]string{
		"secret-doc": "confidential figures",
		"public-doc": "public summary",
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Mem.SetDocEdges(map[string][]string{
		"public-doc": {"secret-doc"},
		"secret-doc": {"public-doc"},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, _, err := client.Mem.AdmitClaim(memory.Claim{
		Subject: "revenue", Predicate: "was", Object: "confidential",
		DocumentIDs: []string{"secret-doc"},
		ValidFrom:   now, ObservedAt: now, Status: memory.ClaimActive,
	}); err != nil {
		t.Fatal(err)
	}
	// History: the query log records what each question was answered from.
	if err := client.Mem.AppendQueryLog(memory.QueryLogEntry{
		Question: "what were the quarterly figures?",
		DocIDs:   []string{"secret-doc", "public-doc"},
		At:       now,
	}); err != nil {
		t.Fatal(err)
	}
	return client, dir
}

func TestAPurgedDocumentStopsAnsweringSearches(t *testing.T) {
	client, dir := purgeBrain(t)
	ctx := context.Background()

	before, err := client.store.LexicalSearch(ctx, "purge-brain", "confidential figures", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !hitsDocument(before, "secret-doc") {
		t.Fatal("the fixture document is not retrievable, so this proves nothing")
	}

	receipt, err := client.PurgeDocuments("purge-brain", []string{"secret-doc"})
	if err != nil {
		t.Fatalf("PurgeDocuments: %v", err)
	}

	after, err := client.store.LexicalSearch(ctx, "purge-brain", "confidential figures", 10)
	if err != nil {
		t.Fatal(err)
	}
	if hitsDocument(after, "secret-doc") {
		t.Fatalf("a purged document is still answering searches: %+v", after)
	}
	if !hitsDocument(mustSearch(t, client, "public summary"), "public-doc") {
		t.Fatal("purging one document made another unretrievable")
	}

	// On disk, not just in memory.
	corpus, err := os.ReadFile(filepath.Join(dir, "chunks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(corpus), "confidential figures") {
		t.Fatalf("the purged document's text is still in the corpus:\n%s", corpus)
	}

	if receipt.Purged["corpus"] == 0 {
		t.Fatalf("the corpus reported no purge: %+v", receipt.Purged)
	}
}

func TestAPurgedDocumentLeavesTheCortexAndHistory(t *testing.T) {
	client, dir := purgeBrain(t)

	if _, err := client.PurgeDocuments("purge-brain", []string{"secret-doc"}); err != nil {
		t.Fatal(err)
	}

	if residual := client.Mem.ResidualDocuments([]string{"secret-doc"}); len(residual) != 0 {
		t.Fatalf("the cortex still holds the purged document in %v", residual)
	}
	if residual := client.Mem.ResidualHistory([]string{"secret-doc"}); len(residual) != 0 {
		t.Fatalf("the query log still names the purged document: %v", residual)
	}
	// The surviving document keeps its own cortex entries.
	if residual := client.Mem.ResidualDocuments([]string{"public-doc"}); len(residual) == 0 {
		t.Fatal("purging one document emptied the cortex of another")
	}

	// The cortex is persisted, so the purge has to survive a reopen.
	reopened, err := memory.Open(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if residual := reopened.ResidualDocuments([]string{"secret-doc"}); len(residual) != 0 {
		t.Fatalf("the purge did not persist; the document is back after a reopen: %v", residual)
	}
}

// TestThePurgeReceiptNamesTheVectorStoreAsSkipped is the honesty half. The
// dense backends expose no delete, so this purge does not reach them, and the
// receipt has to say so rather than reporting a completeness it has not
// established.
func TestThePurgeReceiptNamesTheVectorStoreAsSkipped(t *testing.T) {
	client, _ := purgeBrain(t)
	receipt, err := client.PurgeDocuments("purge-brain", []string{"secret-doc"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.VerifiedComplete {
		t.Fatal("the receipt claims a complete erasure while the vector store " +
			"was never reached")
	}
	var named bool
	for _, skipped := range receipt.Skipped {
		if skipped == "vectors" {
			named = true
		}
	}
	if !named {
		t.Fatalf("the unreached substrate is not named: %+v", receipt.Skipped)
	}
}

func mustSearch(t *testing.T, client *Client, q string) []Hit {
	t.Helper()
	hits, err := client.store.LexicalSearch(context.Background(), "purge-brain", q, 10)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func hitsDocument(hits []Hit, docID string) bool {
	for _, hit := range hits {
		if hit.DSID == docID {
			return true
		}
	}
	return false
}

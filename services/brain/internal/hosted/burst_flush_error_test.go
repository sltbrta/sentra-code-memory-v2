package hosted

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D-003. endBatch is the only disk write for a whole burst -- per-upsert
// flushes are deferred while a batch is open -- and BurstUpsert discarded its
// error. A burst whose single flush failed (ENOSPC, a read-only directory, a
// permissions change) returned a fully successful IngestResult with every
// receipt marked OK, having written nothing to disk at all.
//
// The fix was verified by reading; nothing made a flush fail, so reverting it
// left the suite green.

// readOnlyBrainDir opens a durable local brain and then makes its directory
// unwritable, so the next flush fails while every in-memory upsert succeeds.
func readOnlyBrainDir(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	c, err := OpenLocal(dir, "flush-brain")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// One successful burst first, so the failure below is a flush failure and
	// not an "it never worked" result.
	if _, err := c.BurstUpsert(context.Background(), "flush-brain", []ChunkWrite{
		{DocumentID: "doc-1", ChunkID: "doc-1#0", Text: "first write", SourceURI: "file://1"},
	}, 1); err != nil {
		t.Fatalf("baseline burst: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restore before TempDir cleanup, which cannot remove an unwritable dir.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return c
}

func TestBurstUpsertReportsAFailedFlush(t *testing.T) {
	c := readOnlyBrainDir(t)

	result, err := c.BurstUpsert(context.Background(), "flush-brain", []ChunkWrite{
		{DocumentID: "doc-2", ChunkID: "doc-2#0", Text: "never reached disk", SourceURI: "file://2"},
		{DocumentID: "doc-3", ChunkID: "doc-3#0", Text: "nor did this", SourceURI: "file://3"},
	}, 2)
	if err == nil {
		t.Fatalf("a burst whose only disk write failed reported success: %+v", result)
	}
	if !strings.Contains(err.Error(), "flush") {
		t.Fatalf("want a flush failure, got %v", err)
	}
}

// TestBurstUpsertFlushFailureIsNotMaskedByReceipts records the shape of the
// old bug directly: the per-chunk receipts are all OK, because the memory
// upserts really did succeed. Only the flush error distinguishes a burst that
// was persisted from one that was not, which is why discarding it was silent.
func TestBurstUpsertFlushFailureIsNotMaskedByReceipts(t *testing.T) {
	c := readOnlyBrainDir(t)

	result, err := c.BurstUpsert(context.Background(), "flush-brain", []ChunkWrite{
		{DocumentID: "doc-4", ChunkID: "doc-4#0", Text: "in memory only", SourceURI: "file://4"},
	}, 1)
	if err == nil {
		t.Fatal("flush failure was not reported")
	}
	for _, receipt := range result.Receipts {
		if !receipt.OK {
			t.Fatalf("receipt %s reports a chunk failure; this case is about a "+
				"successful fan-out with a failed flush: %+v", receipt.ChunkID, receipt)
		}
	}
}

// TestBurstUpsertStillSucceedsWhenTheFlushSucceeds keeps the guard from
// passing for the wrong reason.
func TestBurstUpsertStillSucceedsWhenTheFlushSucceeds(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenLocal(dir, "ok-brain")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.BurstUpsert(context.Background(), "ok-brain", []ChunkWrite{
		{DocumentID: "doc-1", ChunkID: "doc-1#0", Text: "persisted", SourceURI: "file://1"},
	}, 1); err != nil {
		t.Fatalf("BurstUpsert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chunks.jsonl")); err != nil {
		t.Fatalf("a successful burst wrote no corpus: %v", err)
	}
}

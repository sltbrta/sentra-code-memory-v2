package hosted

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A deletion removed a document's chunks from the corpus and left its derived
// sidecar text on disk forever.
//
// DeleteDocuments sets forceFullFlush and calls flush. flush's corpus branch
// cleared the flag and *then* called flushSidecarsLocked, which read the same
// field to decide whether to rewrite its base -- so the sidecar writer never
// once saw an explicit rewrite request. Its base is only otherwise rewritten
// when it is missing or when the delta passes the compaction threshold, so a
// deleted document's summary stayed in sidecars.jsonl until 512 unrelated
// sidecar writes happened to compact it away.
//
// The in-memory store drops the sidecars correctly, so nothing was visible
// through the store API. Only the file on disk shows it.

func TestDeletingADocumentRewritesTheSidecarBase(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenLocal(dir, "delete-brain")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	if _, err := c.BurstUpsert(ctx, "delete-brain", []ChunkWrite{
		{DocumentID: "keep", ChunkID: "keep#0", Text: "retained body", SourceURI: "file://keep"},
		{DocumentID: "drop", ChunkID: "drop#0", Text: "deleted body", SourceURI: "file://drop"},
	}, 1); err != nil {
		t.Fatalf("burst: %v", err)
	}
	d, ok := c.store.(*durableStore)
	if !ok {
		t.Fatalf("OpenLocal did not produce a durable store, got %T", c.store)
	}
	if _, err := d.WarmSidecars(ctx, "delete-brain", []SidecarWrite{
		{DocumentID: "keep", Kind: "summary", Text: "summary of the retained document"},
		{DocumentID: "drop", Kind: "summary", Text: "confidential summary of the deleted document"},
	}); err != nil {
		t.Fatalf("WarmSidecars: %v", err)
	}

	sidecarPath := filepath.Join(dir, "sidecars.jsonl")
	before, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "confidential summary") {
		t.Fatalf("fixture did not persist the sidecar it is about to delete:\n%s", before)
	}

	if n := d.DeleteDocuments("delete-brain", []string{"drop"}); n == 0 {
		t.Fatal("DeleteDocuments removed nothing")
	}

	after, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "confidential summary") {
		t.Fatalf("the deleted document's sidecar text is still on disk after "+
			"deletion; the explicit full-rewrite request never reached the "+
			"sidecar writer:\n%s", after)
	}
	if !strings.Contains(string(after), "summary of the retained document") {
		t.Fatalf("the surviving document's sidecar was lost:\n%s", after)
	}

	// The delta must not be holding the deleted record either.
	if raw, err := os.ReadFile(filepath.Join(dir, "sidecars.delta.jsonl")); err == nil {
		if strings.Contains(string(raw), "confidential summary") {
			t.Fatalf("the deleted sidecar survives in the delta:\n%s", raw)
		}
	}
}

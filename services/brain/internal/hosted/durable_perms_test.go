package hosted

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D-007. The corpus, sidecar and metadata files were written 0644.
// chunks.jsonl holds the full plaintext of every ingested document and the
// sidecars hold text derived from it, so every local account could read the
// brain. The fix set them to 0600 and nothing checked it -- reverting the
// constant left the suite green.
//
// The guard is written as a property of the whole brain directory rather than
// a list of three filenames, because the interesting failure is a *new* writer
// that does not know about the rule. When first run it found exactly that:
// hotlex.gob, which carries document text for the offline path, was still
// being published 0644.

// assertBrainDirIsPrivate fails for any file under dir that is readable or
// writable by group or other.
func assertBrainDirIsPrivate(t *testing.T, dir string) {
	t.Helper()
	found := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		found++
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			rel, _ := filepath.Rel(dir, path)
			t.Errorf("%s is mode %04o: readable outside the owning account, and a "+
				"brain directory holds ingested plaintext", rel, mode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("no files were produced, so this guard checked nothing")
	}
}

// ingestedBrain produces every durable artefact the local store writes: base
// corpus, delta, sidecars, metadata and the HotLex snapshot.
func ingestedBrain(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	c, err := OpenLocal(dir, "perm-brain")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	if _, err := c.BurstUpsert(ctx, "perm-brain", []ChunkWrite{
		{DocumentID: "doc-1", ChunkID: "doc-1#0", Text: "patient name and account number", SourceURI: "file://1"},
		{DocumentID: "doc-2", ChunkID: "doc-2#0", Text: "second confidential document", SourceURI: "file://2"},
	}, 2); err != nil {
		t.Fatalf("burst: %v", err)
	}
	// A second burst leaves the base alone and appends to the delta, so both
	// corpus files exist.
	if _, err := c.BurstUpsert(ctx, "perm-brain", []ChunkWrite{
		{DocumentID: "doc-3", ChunkID: "doc-3#0", Text: "third confidential document", SourceURI: "file://3"},
	}, 1); err != nil {
		t.Fatalf("second burst: %v", err)
	}
	d, ok := c.store.(*durableStore)
	if !ok {
		t.Fatalf("OpenLocal did not produce a durable store, got %T", c.store)
	}
	if _, err := d.WarmSidecars(ctx, "perm-brain", []SidecarWrite{
		{DocumentID: "doc-1", Kind: "summary", Text: "derived from the plaintext above"},
	}); err != nil {
		t.Fatalf("WarmSidecars: %v", err)
	}
	c.EnsureHotLex()
	if err := d.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return dir
}

func TestDurableBrainFilesAreNotReadableOutsideTheOwner(t *testing.T) {
	dir := ingestedBrain(t)

	// Named explicitly so the guard fails loudly if a rename means it is
	// silently checking an empty directory.
	for _, name := range []string{"meta.json", "chunks.jsonl", "chunks.delta.jsonl", "sidecars.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
	assertBrainDirIsPrivate(t, dir)
}

// TestHotLexSnapshotIsNotWorldReadable pins the site the original fix missed.
// The snapshot carries document text for the offline path, so publishing it
// 0644 leaks the same content chunks.jsonl was tightened to protect.
func TestHotLexSnapshotIsNotWorldReadable(t *testing.T) {
	dir := ingestedBrain(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".gob") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is mode %04o, want 0600", entry.Name(), mode)
		}
	}
	if checked == 0 {
		t.Fatal("no HotLex snapshot was written, so this guard checked nothing")
	}
}

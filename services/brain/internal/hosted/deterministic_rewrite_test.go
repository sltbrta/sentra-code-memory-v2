package hosted

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// D-008. The corpus and sidecar writers ranged over Go maps, so every full
// rewrite emitted the records in a different order. An unchanged corpus
// produced different bytes each time, which makes a digest comparison mean
// nothing: nothing can tell "the content changed" from "the map was walked
// again". The fix sorts before writing, and nothing checked it -- reverting
// the sorts left the suite green.
//
// One rewrite proves nothing here; the guard has to rewrite the same
// unchanged content repeatedly and compare bytes, because map order is
// randomised per iteration rather than fixed per process.

const rewriteRounds = 6

// rewriteSameCorpus forces `rounds` full rewrites of identical content and
// returns the bytes of the named file after each one.
func rewriteSameCorpus(t *testing.T, name string, rounds int) [][]byte {
	t.Helper()
	dir := t.TempDir()
	c, err := OpenLocal(dir, "determinism-brain")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	// Enough documents that map order is overwhelmingly unlikely to repeat by
	// chance: 24! orderings against six samples.
	writes := make([]ChunkWrite, 0, 24)
	sidecars := make([]SidecarWrite, 0, 24)
	for i := 0; i < 24; i++ {
		id := string(rune('a'+i%26)) + "-doc"
		writes = append(writes, ChunkWrite{
			DocumentID: id, ChunkID: id + "#0",
			Text: "stable body for " + id, SourceURI: "file://" + id,
		})
		sidecars = append(sidecars, SidecarWrite{
			DocumentID: id, Kind: "summary", Text: "stable summary for " + id,
		})
	}
	if _, err := c.BurstUpsert(ctx, "determinism-brain", writes, 4); err != nil {
		t.Fatalf("burst: %v", err)
	}
	d, ok := c.store.(*durableStore)
	if !ok {
		t.Fatalf("OpenLocal did not produce a durable store, got %T", c.store)
	}
	if _, err := d.WarmSidecars(ctx, "determinism-brain", sidecars); err != nil {
		t.Fatalf("WarmSidecars: %v", err)
	}

	path := filepath.Join(dir, name)
	out := make([][]byte, 0, rounds)
	for i := 0; i < rounds; i++ {
		// Nothing about the content changes between rounds; only the rewrite
		// is forced.
		d.mu.Lock()
		d.forceFullFlush = true
		d.mu.Unlock()
		if err := d.flush(); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s after round %d: %v", name, i, err)
		}
		if len(raw) == 0 {
			t.Fatalf("%s is empty after round %d", name, i)
		}
		out = append(out, raw)
	}
	return out
}

func TestUnchangedCorpusRewritesToIdenticalBytes(t *testing.T) {
	rounds := rewriteSameCorpus(t, "chunks.jsonl", rewriteRounds)
	for i := 1; i < len(rounds); i++ {
		if !bytes.Equal(rounds[0], rounds[i]) {
			t.Fatalf("rewrite %d of unchanged content produced different bytes "+
				"(%d vs %d bytes): record order depends on map iteration, so a "+
				"digest comparison cannot distinguish a change from a rewrite",
				i, len(rounds[0]), len(rounds[i]))
		}
	}
}

func TestUnchangedSidecarsRewriteToIdenticalBytes(t *testing.T) {
	rounds := rewriteSameCorpus(t, "sidecars.jsonl", rewriteRounds)
	for i := 1; i < len(rounds); i++ {
		if !bytes.Equal(rounds[0], rounds[i]) {
			t.Fatalf("sidecar rewrite %d of unchanged content produced different bytes", i)
		}
	}
}

// TestRewriteOrderIsSortedNotMerelyStable distinguishes "deterministic" from
// "sorted". A writer that happened to be stable for another reason would pass
// the comparisons above; the ordering is part of the contract because it is
// what makes two independently built brains comparable.
func TestRewriteOrderIsSortedNotMerelyStable(t *testing.T) {
	raw := rewriteSameCorpus(t, "chunks.jsonl", 1)[0]
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("want several records, got %d", len(lines))
	}
	for i := 1; i < len(lines); i++ {
		if bytes.Compare(lines[i-1], lines[i]) > 0 {
			t.Fatalf("record %d sorts before record %d: the corpus is not written "+
				"in chunk-id order\n%s\n%s", i, i-1, lines[i-1], lines[i])
		}
	}
}

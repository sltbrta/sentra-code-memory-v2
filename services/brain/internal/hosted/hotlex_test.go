package hosted

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestHotLexSearchAndPersist(t *testing.T) {
	h := NewHotLex("b1")
	h.AddChunk("c1", "d1", "MedThink RPO policy requires fifteen minutes recovery", "")
	h.AddChunk("c2", "d2", "Unrelated picnic sandwiches and weather", "")
	hits := h.Search("MedThink RPO recovery", 5)
	if len(hits) == 0 || hits[0].ChunkID != "c1" {
		t.Fatalf("hits=%v", hits)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hotlex.gob")
	if err := h.SaveGob(path); err != nil {
		t.Fatal(err)
	}
	h2, err := LoadHotLexGob(path)
	if err != nil {
		t.Fatal(err)
	}
	hits2 := h2.Search("MedThink RPO", 3)
	if len(hits2) == 0 || hits2[0].ChunkID != "c1" {
		t.Fatalf("reload hits=%v", hits2)
	}
}

func TestHotLexStrongRequiresDominance(t *testing.T) {
	// Flat top-K must NOT look strong (semantic mush without a clear leader).
	flat := make([]Hit, 12)
	for i := range flat {
		flat[i] = Hit{ChunkID: "c", DSID: "d", Score: 1.0}
	}
	if hotLexStrong(flat, 6, 0.5) {
		t.Fatal("flat scores must not be strong")
	}
	// Dominant top hit is strong.
	dom := make([]Hit, 12)
	dom[0] = Hit{ChunkID: "c0", DSID: "d0", Score: 3.0}
	for i := 1; i < 12; i++ {
		dom[i] = Hit{ChunkID: "c", DSID: "d", Score: 1.0}
	}
	if !hotLexStrong(dom, 6, 0.5) {
		t.Fatal("dominant top score should be strong")
	}
	if hotLexStrong(nil, 6, 0.5) {
		t.Fatal("empty not strong")
	}
}

func TestHotLexMergeShardsAndBulk(t *testing.T) {
	a := NewHotLex("b")
	b := NewHotLex("b")
	a.AddChunkBulk("c1", "d1", "alpha recovery objective medthink", "", false)
	b.AddChunkBulk("c2", "d2", "beta picnic sandwiches only", "", false)
	a.Finalize()
	b.Finalize()
	m := MergeShards("b", []*HotLex{a, b})
	if m.Len() != 2 {
		t.Fatalf("merged len=%d", m.Len())
	}
	hits := m.Search("medthink recovery", 3)
	if len(hits) == 0 || hits[0].ChunkID != "c1" {
		t.Fatalf("hits=%v", hits)
	}
	// O(1) avgdl sanity
	if m.AvgDL < 1 {
		t.Fatalf("avgdl=%v", m.AvgDL)
	}
}

func TestHashShardIDRange(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := HashShardID(string(rune('a'+i%26))+string(rune('0'+i%10)), 16)
		if id < 0 || id >= 16 {
			t.Fatalf("shard %d", id)
		}
	}
}

func TestLocalHotLexInteractiveRetrieve(t *testing.T) {
	dir := t.TempDir()
	c, err := CreateLocal(dir, "hot")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = c.BurstIngestLocal(ctx, []LocalDocument{
		{ID: "d1", Text: "MedThink RPO policy is 15 minutes for active datasets PROJ_HOT99"},
		{ID: "d2", Text: "Noise about cafeteria menus only"},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	c.EnsureHotLex()
	if c.HotLex() == nil || c.HotLex().Len() == 0 {
		t.Fatal("expected hotlex after ingest")
	}
	// Reopen should load hotlex.gob
	c2, err := OpenLocal(dir, "hot")
	if err != nil {
		t.Fatal(err)
	}
	if c2.HotLex() == nil || c2.HotLex().Len() == 0 {
		t.Fatal("expected hotlex on reopen")
	}
	ps, diag, err := c2.Retrieve(ctx, "MedThink RPO active datasets", 4)
	if err != nil {
		t.Fatal(err)
	}
	if diag["retrieve_class"] != "interactive_local" {
		t.Fatalf("retrieve_class=%v want interactive_local diag=%v", diag["retrieve_class"], diag)
	}
	if len(ps) == 0 {
		t.Fatalf("no passages diag=%v", diag)
	}
	n, _ := diag["hot_lex_hits"].(int)
	if n == 0 {
		t.Fatalf("expected hot_lex_hits>0 diag=%v", diag)
	}
	// Soft-empty stopword query must not hard-fail.
	ps2, diag2, err2 := c2.Retrieve(ctx, "a b", 4)
	if err2 != nil {
		t.Fatalf("soft empty err=%v", err2)
	}
	if len(ps2) != 0 {
		t.Fatalf("want empty passages for stopwords, got %d", len(ps2))
	}
	if diag2["soft_empty"] != true {
		t.Fatalf("want soft_empty diag=%v", diag2)
	}
	ans := c2.Answer(ctx, "MedThink RPO PROJ_HOT99", 4)
	if ans.Failure != "" {
		t.Fatalf("answer failure=%s", ans.Failure)
	}
	if ans.RetrievalDiagnostics == nil || ans.RetrievalDiagnostics["interactive"] != true {
		t.Fatalf("want interactive stamp diag=%v", ans.RetrievalDiagnostics)
	}
	stack, _ := ans.RetrievalDiagnostics["product_stack"].(string)
	if !strings.Contains(stack, "interactive") {
		t.Fatalf("product_stack=%q want *interactive*", stack)
	}
}

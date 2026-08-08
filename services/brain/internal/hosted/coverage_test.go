package hosted

import "testing"

func TestCoverageRerankPrefersDiverseDocs(t *testing.T) {
	// Three passages: two near-duplicates of A, one distinct B with unique gold tokens.
	passages := []Passage{
		{DocumentID: "dsid_a", Text: "alpha beta gamma rate limit policy document", Score: 10, ChunkID: "a1"},
		{DocumentID: "dsid_a", Text: "alpha beta gamma rate limit policy again", Score: 9, ChunkID: "a2"},
		{DocumentID: "dsid_b", Text: "omega zeta owner mediation completeness steps", Score: 3, ChunkID: "b1"},
	}
	q := "rate limit and mediation completeness owner"
	out := coverageRerank(q, passages, 2, 0.55)
	if len(out) != 2 {
		t.Fatalf("want 2 got %d", len(out))
	}
	ids := map[string]struct{}{}
	for _, p := range out {
		ids[p.DocumentID] = struct{}{}
	}
	if _, ok := ids["dsid_b"]; !ok {
		t.Fatalf("coverage should keep diverse dsid_b, got %#v", out)
	}
}

func TestDecomposeAndHyde(t *testing.T) {
	subs := decomposeQuery("What is the RPO and what is the RTO for MedThink?", "project_related")
	if len(subs) < 1 {
		t.Fatal("expected subqueries")
	}
	h := hydeStub("What is the default multipart upload limit?")
	if h == "" || len(h) < 20 {
		t.Fatalf("hyde stub weak: %q", h)
	}
}

func TestPruneNeverAllPool(t *testing.T) {
	// Cite cap is residual-parity: never dump 12 ids.
	var cited []string
	for i := 0; i < 12; i++ {
		cited = append(cited, "dsid_"+string(rune('a'+i)))
	}
	// Use simple unique ids
	cited = []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9", "d10", "d11", "d12"}
	out := pruneCitations(cited, nil, "basic")
	if len(out) > 3 {
		t.Fatalf("cite cap broken: %v", out)
	}
}

func TestPhaseADenseQueriesHyDESlot(t *testing.T) {
	q := "What are the rate limits for multipart uploads and total requests per project?"
	variants := []string{q, "rate limits multipart", "total requests project"}

	// Multi-doc rebuild with full query list: HyDE must still occupy a slot
	// (previously rebuilt away — dead guard).
	out := phaseADenseQueries(q, variants, 2, "hyde-stub-text", true)
	if !containsString(out, "hyde-stub-text") {
		t.Fatalf("HyDE must occupy a dense slot when queries are full: %v", out)
	}
	// Cap respected: HyDE replaces the weakest configured slot.
	if len(out) != 2 {
		t.Fatalf("dense query cap exceeded: %v", out)
	}

	// No HyDE: behaves like the previous rebuild (phrases preferred when capped).
	out2 := phaseADenseQueries(q, variants, 2, "", true)
	if containsString(out2, "hyde-stub-text") || len(out2) > 2 {
		t.Fatalf("no-hyde rebuild changed: %v", out2)
	}

	// Non-multi-doc path: variants are capped and HyDE occupies the last slot.
	out3 := phaseADenseQueries(q, variants, 2, "hyde-stub-text", false)
	if len(out3) != 2 || out3[1] != "hyde-stub-text" {
		t.Fatalf("variants path with hyde slot: %v", out3)
	}

	// HyDE already present (via variants) is not duplicated.
	out4 := phaseADenseQueries(q, []string{"hyde-stub-text"}, 2, "hyde-stub-text", false)
	if len(out4) != 1 {
		t.Fatalf("hyde must not duplicate: %v", out4)
	}
}

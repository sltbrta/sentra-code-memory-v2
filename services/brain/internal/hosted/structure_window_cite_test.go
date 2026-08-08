package hosted

import (
	"strings"
	"testing"
)

// S5: left-shifted structure evidence must survive coverage + window and remain
// citable after ground (no full-500 — pure unit path).
func TestStructureChannelSurvivesWindowAndGround(t *testing.T) {
	q := "What is the HealthBridge RPO target?"
	// Dense noise: high score, weak relevance.
	noise := make([]Passage, 0, 12)
	for i := 0; i < 10; i++ {
		noise = append(noise, Passage{
			DocumentID: "noise_" + strings.Repeat("x", 1) + string(rune('a'+i)),
			Text:       "Office catering menu and foosball schedule for the quarterly party.",
			Score:      0.95,
			Channel:    "dense",
		})
	}
	// Left-shifted structure gold (path2 SQL / temporal).
	gold := Passage{
		DocumentID: "dsid_healthbridge_rpo",
		Text:       "HealthBridge requires 1h RPO target for warm-replication profiles.",
		Score:      0.35,
		Channel:    "path2_structure",
	}
	pool := append([]Passage{gold}, noise...)

	covered := coverageRerank(q, pool, 8, 0.7)
	found := false
	for _, p := range covered {
		if p.DocumentID == gold.DocumentID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("coverageRerank dropped path2_structure gold; covered=%v", docIDs(covered))
	}

	window, wdiag := retainWindow(covered, q, 6, 3)
	found = false
	for _, p := range window {
		if p.DocumentID == gold.DocumentID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("retainWindow dropped structure gold; window=%v diag=%v", docIDs(window), wdiag)
	}

	// Ground: model cites structure doc with supported quote.
	g := groundCompletion(
		"HealthBridge RPO target is 1 hour.",
		[]string{gold.DocumentID, "noise_xa"},
		[]Claim{{
			Text: "RPO is 1h", Quote: "1h RPO target", DocumentID: gold.DocumentID,
		}},
		window,
		"basic",
	)
	if len(g.CitedDocumentIDs) == 0 || g.CitedDocumentIDs[0] != gold.DocumentID {
		t.Fatalf("ground must cite structure gold first: cites=%v status=%v",
			g.CitedDocumentIDs, g.Diagnostics)
	}
	if g.Diagnostics["grounding_status"] != "ok" {
		t.Fatalf("want grounding ok, got %v", g.Diagnostics)
	}
}

func TestTemporalRelationChannelSoftFloor(t *testing.T) {
	q := "What is MedThink RPO?"
	// No identifier string "MedThink" in noise; structure passage has it.
	pool := []Passage{
		{DocumentID: "n1", Text: "kitchen snacks and snacks again weekly", Score: 0.9, Channel: "hot_lex"},
		{DocumentID: "n2", Text: "billing invoices and receivables ledger", Score: 0.88, Channel: "dense"},
		{
			DocumentID: "dsid_policy",
			Text:       "MedThink failover policy sets RPO to 15 minutes for gold tier.",
			Score:      0.4,
			Channel:    "temporal_relation",
		},
	}
	// Soft floor: even if extractIdentifiers miss (edge), structure boost helps.
	// Use a question where identifiers include MedThink.
	window, diag := retainWindow(pool, q, 2, 2)
	found := false
	for _, p := range window {
		if p.DocumentID == "dsid_policy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("temporal_relation policy should retain; window=%v diag=%v", docIDs(window), diag)
	}
}

func TestStructureChannelBoostValues(t *testing.T) {
	if structureChannelBoost(Passage{Channel: "path2_structure"}) < structureChannelBoost(Passage{Channel: "dense"}) {
		t.Fatal("path2_structure must boost more than dense")
	}
	if structureChannelBoost(Passage{Channel: "temporal_relation"}) <= 0 {
		t.Fatal("temporal_relation must boost")
	}
}

func docIDs(ps []Passage) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.DocumentID
	}
	return out
}

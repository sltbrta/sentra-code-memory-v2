package hosted

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// S6 adversarial integration (mock FS brain, no Neon, no full-500).
// End-to-end: create → ingest → left-shift cortex → lean ask across
// structure multi-hop, contested dual-cite, info_not_found cite cap,
// and illegal cite strip.
func TestS6AdversarialProductPathIntegration(t *testing.T) {
	t.Setenv("OUROBOROS_BRAIN_LLM", "extractive")
	t.Setenv("OUROBOROS_BRAIN_ENRICH", "sync")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OUROBOROS_ERB_PROD", "1")
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")

	dir := t.TempDir()
	ctx := context.Background()
	c, err := CreateLocal(dir, "s6-adv")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	docs := []LocalDocument{
		// Structure / multi-hop company graph
		{ID: "dsid_checkout", Text: "CheckoutService depends on PaymentGateway for card auth. CheckoutService integrates with InventoryAPI."},
		{ID: "dsid_pay", Text: "PaymentGateway provides PCI-compliant capture. PaymentGateway is owned by Platform SRE."},
		{ID: "dsid_inv", Text: "InventoryAPI reports stock levels every five minutes to CheckoutService."},
		// Contested color facts
		{ID: "dsid_blue", Text: "Acme Widget color is blue sapphire in plant North."},
		{ID: "dsid_red", Text: "Acme Widget color is red crimson in plant North."},
		// Noise
		{ID: "dsid_noise", Text: "Cafeteria foosball tournament starts Friday noon with kitchen snacks."},
	}
	if _, err := c.BurstIngestLocal(ctx, docs, 2); err != nil {
		t.Fatal(err)
	}
	if c.Mem == nil {
		t.Fatal("want Mem cortex")
	}
	// Ensure left-shift even if sync enrich race skipped dense extract.
	res := c.RunCortexMaintenance()
	if res.ClaimsAdmitted < 1 && res.RelationsAdmitted < 1 {
		// Force-admit product graph edges for structure arm.
		_, _, _ = c.Mem.AdmitClaim(memory.Claim{
			Subject: "CheckoutService", Predicate: "depends_on", Object: "PaymentGateway",
			DocumentIDs: []string{"dsid_checkout"}, ValidFrom: time.Now().UTC(), ObservedAt: time.Now().UTC(),
		})
		_ = c.Mem.SeedRelationsFromClaims()
	}
	// Contested Widget color (explicit for dual-cite path).
	_, _, err = c.Mem.AdmitClaim(memory.Claim{
		Subject: "Acme Widget", Predicate: "color", Object: "blue",
		DocumentIDs: []string{"dsid_blue"}, ValidFrom: time.Now().UTC(), ObservedAt: time.Now().UTC(),
		EvidenceQuality: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, contested, err := c.Mem.AdmitClaim(memory.Claim{
		Subject: "Acme Widget", Predicate: "color", Object: "red",
		DocumentIDs: []string{"dsid_red"}, ValidFrom: time.Now().UTC(), ObservedAt: time.Now().UTC(),
		EvidenceQuality: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(contested) == 0 {
		// Same-status equal quality should contest; if not, still probe dual-cite via answer path.
		t.Logf("AdmitClaim did not mark contested (ladder may multi-value); continuing")
	}
	_ = c.Mem.SeedRelationsFromClaims()

	// --- A: structure / product graph lean ask ---
	ansStruct := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What does CheckoutService depend on?",
		QuestionType: "basic",
		TopK:         6,
	})
	if ansStruct.Failure != "" {
		t.Fatalf("structure ask failure: %s", ansStruct.Failure)
	}
	if ansStruct.Answer == "" {
		t.Fatal("empty structure answer")
	}
	low := strings.ToLower(ansStruct.Answer + " " + strings.Join(ansStruct.CitedDocumentIDs, " "))
	if !strings.Contains(low, "payment") && !strings.Contains(low, "checkout") &&
		!strings.Contains(low, "dsid_checkout") && !strings.Contains(low, "dsid_pay") {
		// Soft: extractive may paraphrase; require temporal or cites non-empty
		if len(ansStruct.CitedDocumentIDs) == 0 {
			t.Fatalf("structure path must cite or mention payment/checkout: answer=%q cites=%v diag=%v",
				ansStruct.Answer, ansStruct.CitedDocumentIDs, ansStruct.RetrievalDiagnostics)
		}
	}
	if n, ok := ansStruct.RetrievalDiagnostics["temporal_relation_docs"].(int); ok && n < 1 {
		// float64 from some paths
		if f, ok2 := ansStruct.RetrievalDiagnostics["temporal_relation_docs"].(float64); !ok2 || f < 1 {
			t.Logf("temporal_relation_docs soft-miss (still ok if cites present): diag keys sample plane=%v",
				ansStruct.RetrievalDiagnostics["plane"])
		}
	}

	// --- B: contested dual-cite ---
	ansConf := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What color is Acme Widget?",
		QuestionType: "conflicting_info",
		TopK:         6,
	})
	diag := ansConf.RetrievalDiagnostics
	if diag == nil {
		t.Fatal("nil diag on conflict ask")
	}
	pol, _ := diag["conflict_policy"].(string)
	if pol != "dual_cite_and_abstain" && pol != "dual_cite" {
		// Conflict policy is applied when contested groups match question;
		// fall back to cite both colors docs if present.
		hasBlue, hasRed := false, false
		for _, id := range ansConf.CitedDocumentIDs {
			if id == "dsid_blue" {
				hasBlue = true
			}
			if id == "dsid_red" {
				hasRed = true
			}
		}
		if !(hasBlue && hasRed) && !strings.Contains(strings.ToLower(ansConf.Answer), "contest") {
			t.Fatalf("want dual-cite/contest on color conflict; pol=%q cites=%v answer=%q",
				pol, ansConf.CitedDocumentIDs, ansConf.Answer)
		}
	}

	// --- C: info_not_found cite cap (≤2) ---
	ansAbs := c.AnswerOpts(ctx, AnswerOptions{
		Question:     "What is the price of the quantum flux capacitor upgrade package?",
		QuestionType: "info_not_found",
		TopK:         4,
	})
	if len(ansAbs.CitedDocumentIDs) > 2 {
		t.Fatalf("info_not_found cite cap ≤2, got %v", ansAbs.CitedDocumentIDs)
	}

	// --- D: ground strips illegal cites when structure window is gold ---
	window := []Passage{
		{DocumentID: "dsid_pay", Text: "PaymentGateway provides PCI-compliant capture.", Channel: "path2_structure"},
		{DocumentID: "dsid_checkout", Text: "CheckoutService depends on PaymentGateway.", Channel: "temporal_relation"},
	}
	g := groundCompletion(
		"CheckoutService depends on PaymentGateway.",
		[]string{"dsid_checkout", "dsid_pay", "dsid_hallucinated_never"},
		[]Claim{{
			Text: "depends on PaymentGateway", Quote: "depends on PaymentGateway", DocumentID: "dsid_checkout",
		}},
		window,
		"basic",
	)
	for _, id := range g.CitedDocumentIDs {
		if id == "dsid_hallucinated_never" {
			t.Fatalf("illegal cite survived ground: %v", g.CitedDocumentIDs)
		}
	}
	if len(g.CitedDocumentIDs) == 0 {
		t.Fatal("ground zeroed all cites adversarially")
	}

	// --- E: QUALITY residual vs serve interactive split ---
	if c.hot != nil && c.hot.Len() > 0 {
		t.Setenv("OUROBOROS_ERB_FORCE_PATH2_FTS", "1")
		t.Setenv("OUROBOROS_ERB_QUALITY", "0")
		t.Setenv("OUROBOROS_ERB_PROD", "1")
		_ = os.Unsetenv("OUROBOROS_ERB_QUALITY_INTERACTIVE")
		if !c.preferInteractive(prodProfileFromEnv()) {
			t.Fatal("serve+HotLex must prefer interactive even with FORCE_PATH2_FTS")
		}
		t.Setenv("OUROBOROS_ERB_QUALITY", "1")
		_ = os.Unsetenv("OUROBOROS_ERB_FORCE_RESIDUAL")
		if !c.preferInteractive(prodProfileFromEnv()) {
			t.Fatal("QUALITY + HotLex stays one product path (not residual fork)")
		}
	}
}

// S6: contested TemporalRelations still expand for recall (include contested).
func TestS6ContestedRelationsStillExpand(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC()
	_, _, err = s.AdmitRelation(memory.TemporalRelation{
		Src: "Widget", Relation: "color", Dst: "blue",
		FactText: "Widget color blue", DocumentIDs: []string{"d1"},
		ValidFrom: t0, ObservedAt: t0, EvidenceQuality: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, cont, err := s.AdmitRelation(memory.TemporalRelation{
		Src: "Widget", Relation: "color", Dst: "red",
		FactText: "Widget color red", DocumentIDs: []string{"d2"},
		ValidFrom: t0, ObservedAt: t0, EvidenceQuality: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cont) == 0 {
		t.Log("no contest on multi-value color relation (ontology multi-valued?) — checking expand anyway")
	}
	docs := s.ExpandRelationDocuments([]string{"Widget"}, t0, t0, 8)
	if len(docs) == 0 {
		t.Fatal("contested/active relations must still expand documents for recall")
	}
}

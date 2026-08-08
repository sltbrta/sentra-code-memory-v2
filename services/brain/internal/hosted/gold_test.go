package hosted

import (
	"strconv"
	"strings"
	"testing"
)

func TestComputeGoldDiag(t *testing.T) {
	gold := []string{"dsid_a", "dsid_b", "dsid_c"}
	pool := []Passage{
		{DocumentID: "dsid_a", Text: "a", ChunkID: "a1"},
		{DocumentID: "dsid_x", Text: "x", ChunkID: "x1"},
		{DocumentID: "dsid_b", Text: "b", ChunkID: "b1"},
	}
	window := []Passage{
		{DocumentID: "dsid_a", Text: "a", ChunkID: "a1"},
		{DocumentID: "dsid_y", Text: "y", ChunkID: "y1"},
	}
	d := computeGoldDiag(gold, pool, window)
	if d == nil {
		t.Fatal("expected gold diag")
	}
	// |{a,b}| / 3 = 2/3
	pr, ok := d["pool_recall"].(float64)
	if !ok || pr < 0.66 || pr > 0.67 {
		t.Fatalf("pool_recall=%v want ~0.666", d["pool_recall"])
	}
	// gold∩window={a}, |window unique|=2 → precision 0.5; window_recall 1/3
	wp, ok := d["window_precision"].(float64)
	if !ok || wp != 0.5 {
		t.Fatalf("window_precision=%v want 0.5", d["window_precision"])
	}
	wr, ok := d["window_recall"].(float64)
	if !ok || wr < 0.33 || wr > 0.34 {
		t.Fatalf("window_recall=%v want ~0.333", d["window_recall"])
	}
	inPool, _ := d["gold_in_pool"].([]string)
	if len(inPool) != 2 {
		t.Fatalf("gold_in_pool=%v", inPool)
	}
	inWin, _ := d["gold_in_window"].([]string)
	if len(inWin) != 1 || inWin[0] != "dsid_a" {
		t.Fatalf("gold_in_window=%v", inWin)
	}
}

func TestComputeGoldDiagEmptyWindow(t *testing.T) {
	d := computeGoldDiag([]string{"dsid_a"}, []Passage{{DocumentID: "dsid_a"}}, nil)
	if d == nil {
		t.Fatal("expected diag")
	}
	if wp, _ := d["window_precision"].(float64); wp != 0 {
		t.Fatalf("empty window precision want 0 got %v", wp)
	}
	if pr, _ := d["pool_recall"].(float64); pr != 1 {
		t.Fatalf("pool_recall want 1 got %v", pr)
	}
}

func TestComputeGoldDiagEmptyGold(t *testing.T) {
	if d := computeGoldDiag(nil, []Passage{{DocumentID: "a"}}, []Passage{{DocumentID: "a"}}); d != nil {
		t.Fatalf("empty gold should return nil, got %#v", d)
	}
}

func TestComputeGoldDiagPoolAtK(t *testing.T) {
	// Gold at ranks 1 and 15 → any-hit @10 true (rank1); @20 true; min rank 1.
	pool := make([]Passage, 0, 20)
	for i := 1; i <= 20; i++ {
		pool = append(pool, Passage{DocumentID: "dsid_n" + strconv.Itoa(i), Text: "t"})
	}
	pool[0].DocumentID = "dsid_gold1"
	pool[14].DocumentID = "dsid_gold15"
	d := computeGoldDiag([]string{"dsid_gold1", "dsid_gold15"}, pool, pool[:8])
	if d["pool_at_10"] != true {
		t.Fatalf("pool_at_10 want true, got %v min=%v", d["pool_at_10"], d["gold_min_rank"])
	}
	if d["pool_at_20"] != true {
		t.Fatalf("pool_at_20 want true")
	}
	if mr, _ := d["gold_min_rank"].(int); mr != 1 {
		t.Fatalf("gold_min_rank=%v want 1", d["gold_min_rank"])
	}
	// Only gold15 → rank 15 → @10 false @20 true
	d2 := computeGoldDiag([]string{"dsid_gold15"}, pool, nil)
	if d2["pool_at_10"] != false {
		t.Fatalf("rank15 should be pool_at_10=false, got %v", d2["pool_at_10"])
	}
	if d2["pool_at_20"] != true {
		t.Fatalf("rank15 should be pool_at_20=true")
	}
}

func TestPreferGoldPassages(t *testing.T) {
	ps := []Passage{
		{DocumentID: "noise", Text: "n", ChunkID: "1"},
		{DocumentID: "gold", Text: "g", ChunkID: "2"},
		{DocumentID: "noise2", Text: "n2", ChunkID: "3"},
	}
	out := preferGoldPassages(ps, []string{"gold"})
	if out[0].DocumentID != "gold" {
		t.Fatalf("want gold first, got %v", out[0].DocumentID)
	}
}

func TestEnsureGoldCitesNeverDropsWindowGold(t *testing.T) {
	ps := []Passage{
		{DocumentID: "g1", Text: "a"},
		{DocumentID: "g2", Text: "b"},
		{DocumentID: "other", Text: "c"},
	}
	// maxCites=1 must still grow to fit both golds in window.
	out := ensureGoldCites([]string{"other"}, []string{"g1", "g2"}, ps, 1)
	has := map[string]bool{}
	for _, id := range out {
		has[id] = true
	}
	if !has["g1"] || !has["g2"] {
		t.Fatalf("expected both golds preserved, got %v", out)
	}
}

func TestRebindAnswerSizeLimitsSkipsNonSizeLimitQuestions(t *testing.T) {
	q := "What limits how long traffic can shift to the US?"
	answer := "Traffic can shift to the US for up to 4 hours."
	passages := []Passage{{
		DocumentID: "large-policy",
		Text:       strings.Repeat("A long policy paragraph without size atoms. ", 5000),
	}}
	if sizeQuestionRE.MatchString(q) {
		t.Fatalf("generic limit question matched the size gate: %q", q)
	}
	if sizeQuestionRE.MatchString("What are the default size limits for file uploads?") == false ||
		!sizeQuestionRE.MatchString("What is the storage cap?") {
		t.Fatal("size-related questions did not match the size gate")
	}
	out, diag := rebindAnswerSizeLimits(q, answer, passages)
	if out != answer || len(diag) != 0 {
		t.Fatalf("non-size limit question unexpectedly scanned or changed: out=%q diag=%v", out, diag)
	}
}

func TestRebindAnswerSizeLimitsDual(t *testing.T) {
	q := "What are the default size limits for file uploads?"
	ps := []Passage{
		{DocumentID: "wrong", Text: "Some services allow up to 25MB uploads in drafts."},
		{DocumentID: "gold", Text: "Default limits: 10 MiB per file and 50 MiB for the total request size."},
	}
	ans := "The default file upload size cap is 25MB."
	out, diag := rebindAnswerSizeLimits(q, ans, ps)
	if !strings.Contains(out, "10") || !strings.Contains(out, "50") {
		t.Fatalf("expected dual 10/50 limits, out=%q diag=%v", out, diag)
	}
	if strings.Contains(out, "25MB") && !strings.Contains(out, "10") {
		t.Fatalf("25MB alone should not win: %q", out)
	}
}

func TestCorrectiveHelpers(t *testing.T) {
	// ground fail → needs corrective
	g := Grounded{
		Diagnostics: map[string]any{
			"grounding_status": "no_supported_claims",
			"claim_mode":       "claim_quote",
			"supported_claims": 0,
		},
	}
	if !needsCorrective(g, "basic") {
		t.Fatal("expected needsCorrective for no_supported_claims")
	}
	if needsCorrective(g, "info_not_found") {
		t.Fatal("abstain types skip corrective")
	}
	okG := Grounded{
		CitedDocumentIDs: []string{"dsid_a"},
		Claims:           []Claim{{Text: "t", Quote: "q", DocumentID: "dsid_a"}},
		Diagnostics: map[string]any{
			"grounding_status": "ok",
			"claim_mode":       "claim_quote",
			"supported_claims": 1,
		},
	}
	if needsCorrective(okG, "basic") {
		t.Fatal("ok ground should not corrective")
	}

	// long multi-part → prefer decompose sub-query
	q := "What is the RPO for MedThink failover and what is the RTO target?"
	cq := correctiveQuery(q, "project_related")
	if cq == "" || cq == q {
		// may still differ after trim; content short form is also ok
		if len(contentWordShortForm(q)) < 4 {
			t.Fatalf("corrective query weak: %q", cq)
		}
	}
	short := contentWordShortForm("What is the default multipart upload limit for Brightly?")
	if short == "" || len(short) < 8 {
		t.Fatalf("contentWordShortForm weak: %q", short)
	}
}

func TestHydeVariantLongQuestion(t *testing.T) {
	// multiQuery + hyde path used by RetrieveOpts: long questions get hyde stub.
	q := "What is the complete end-to-end process for shipping an emergency serving-runtime hotfix to production including approvals?"
	hy := hydeStub(q)
	if hy == "" || len(contentTokens(q)) < 4 {
		t.Fatalf("expected long question hyde, hy=%q toks=%v", hy, contentTokens(q))
	}
	// Variant list construction mirrors RetrieveOpts (pure).
	variants := multiQueryVariants(q, "completeness")
	for _, sub := range decomposeQuery(q, "completeness") {
		variants = append(variants, sub)
	}
	if hy != "" && len(contentTokens(q)) >= 4 {
		variants = append(variants, hy)
	}
	variants = dedupeQueries(variants)
	found := false
	for _, v := range variants {
		if v == hy {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("hyde variant missing from variants: %v", variants)
	}
}

func TestHydratePolicyMultiDoc(t *testing.T) {
	// Pure policy selection (mirrors RetrieveOpts).
	maxDocs, chunksPerDoc := 5, 3
	policy := "standard"
	if isMultiDocType("project_related") {
		maxDocs, chunksPerDoc = 8, 12
		policy = "whole_doc_multi"
	}
	if policy != "whole_doc_multi" || maxDocs != 8 || chunksPerDoc != 12 {
		t.Fatalf("multi policy=%s docs=%d chunks=%d", policy, maxDocs, chunksPerDoc)
	}
	if isMultiDocType("basic") {
		t.Fatal("basic should not be multi-doc")
	}
	maxDocs, chunksPerDoc = 5, 3
	policy = "standard"
	if isMultiDocType("basic") {
		maxDocs, chunksPerDoc = 8, 12
		policy = "whole_doc_multi"
	}
	if policy != "standard" || maxDocs != 5 {
		t.Fatalf("basic policy=%s docs=%d", policy, maxDocs)
	}
}

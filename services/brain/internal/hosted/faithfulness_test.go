package hosted

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

func TestAnswerFaithfulnessAcceptsSupportedClaims(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	g := groundCompletion(
		"Audit logs are retained for 90 days.",
		[]string{"policy"},
		[]Claim{{Text: "Audit logs are retained for 90 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy"}},
		passages,
		"basic",
	)

	out, diag := enforceAnswerFaithfulness(context.Background(), "How long are audit logs retained?", "basic", g, passages, nil)
	if diag["decision"] != "accepted" || diag["reason"] != "supported" {
		t.Fatalf("decision=%v reason=%v diag=%v", diag["decision"], diag["reason"], diag)
	}
	if out.Answer != g.Answer || len(out.CitedDocumentIDs) != 1 || out.CitedDocumentIDs[0] != "policy" {
		t.Fatalf("supported answer changed: %#v", out)
	}
	if got := diag["supported_claim_rate"]; got != float64(1) {
		t.Fatalf("supported_claim_rate=%v want 1", got)
	}
}

func TestAnswerFaithfulnessAcceptsAuthorizedExtractiveFallback(t *testing.T) {
	passages := []Passage{
		{DocumentID: "policy", Text: "MedThink gold tier failover has an RPO of 15 minutes."},
		{DocumentID: "runbook", Text: "The MedThink runbook confirms the 15 minute recovery point objective."},
	}
	answer := extractiveForQuestion("What is the MedThink RPO for gold tier failover?", passages)
	g := groundAnswerInPassages(
		"What is the MedThink RPO for gold tier failover?", answer,
		[]string{"policy", "runbook"}, nil, passages, "basic",
	)
	out, diag := enforceAnswerFaithfulness(context.Background(), "What is the MedThink RPO for gold tier failover?", "basic", g, passages, nil)
	if diag["decision"] != "accepted" || !strings.Contains(out.Answer, "15") {
		t.Fatalf("authorized extractive fallback rejected: answer=%q diag=%v ground=%v", out.Answer, diag, g.Diagnostics)
	}
}

func TestFactualConsistencyExtractiveScoresAnswerAgainstAuthorizedEvidence(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The policy states that audit logs are retained for 90 days."}}
	g := Grounded{
		Answer:           "Based on product brain evidence:\n- [policy] Audit logs are retained for 90 days.",
		CitedDocumentIDs: []string{"policy"},
	}
	request := factualConsistencyRequest(g, passages)
	if len(request.Claims) != 1 || request.Claims[0].Statement == request.Claims[0].Supports[0] {
		t.Fatalf("extractive request self-scored or missing: %#v", request)
	}
	result := scoreFactualConsistency(context.Background(), g, passages)
	if result.Status != factualconsistency.StatusScored || !factualconsistency.MeetsDefaultThreshold(result) {
		t.Fatalf("extractive evidence score=%+v", result)
	}
}

func TestFactualConsistencyChunksLargeExtractiveEvidence(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: strings.Repeat("The policy record remains authorized. ", 2200) + "Audit logs are retained for 90 days."}}
	g := Grounded{
		Answer:           "Based on product brain evidence:\n- [policy] Audit logs are retained for 90 days.",
		CitedDocumentIDs: []string{"policy"},
	}
	request := factualConsistencyRequest(g, passages)
	if len(request.Claims) != 1 || len(request.Claims[0].Supports) == 0 {
		t.Fatalf("large evidence was dropped: %#v", request)
	}
	limits := factualconsistency.DefaultLimits()
	total := len(request.Claims[0].Statement)
	for _, support := range request.Claims[0].Supports {
		if len(support) > limits.MaxSupportBytes {
			t.Fatalf("support exceeds bound: %d", len(support))
		}
		total += len(support)
	}
	if total > limits.MaxTotalBytes {
		t.Fatalf("request exceeds total bound: %d", total)
	}
	result := scoreFactualConsistency(context.Background(), g, passages)
	if result.Status != factualconsistency.StatusScored || !factualconsistency.MeetsDefaultThreshold(result) {
		t.Fatalf("late extractive evidence was falsely rejected: %+v", result)
	}
}

func TestCitationOnlyWrongValueCannotUsePackWideOverlap(t *testing.T) {
	passages := []Passage{
		{DocumentID: "policy", Text: "Gold tier RPO is 15 minutes."},
		{DocumentID: "runbook", Text: "Gold tier RTO is 30 minutes."},
	}
	g := Grounded{
		Answer:           "Gold tier RPO is 30 minutes.",
		CitedDocumentIDs: []string{"policy", "runbook"},
	}
	_, diag := enforceAnswerFaithfulness(context.Background(), "What is the gold tier RPO?", "basic", g, passages, nil)
	if diag["decision"] == "accepted" {
		t.Fatalf("citation-only wrong value bypassed grounding: %#v", diag)
	}
}

func TestCitationOnlyNonExtractiveAnswerCannotBypassUnknownScore(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	g := Grounded{
		Answer:           "The policy confirms that audit logs are retained for 90 days.",
		CitedDocumentIDs: []string{"policy"},
	}
	_, diag := enforceAnswerFaithfulness(context.Background(), "How long are audit logs retained?", "basic", g, passages, nil)
	if diag["decision"] == "accepted" || diag["reason"] == "supported_factual_consistency_unavailable" {
		t.Fatalf("citation-only prose bypassed the calibrated floor: %#v", diag)
	}
}

func TestAnswerFaithfulnessAbstainsWhenScoringContextIsCancelled(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	g := groundCompletion(
		"Audit logs are retained for 90 days.",
		[]string{"policy"},
		[]Claim{{Text: "Audit logs are retained for 90 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy"}},
		passages,
		"basic",
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, diag := enforceAnswerFaithfulness(ctx, "How long are audit logs retained?", "basic", g, passages, nil)
	if diag["decision"] != "abstained" || diag["reason"] != "request_cancelled" || out.Answer == g.Answer {
		t.Fatalf("cancelled scoring returned an answer: out=%#v diag=%v", out, diag)
	}
}

func TestAnswerFaithfulnessRepairsPartialUnsupportedAnswer(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	g := groundCompletion(
		"Audit logs are retained for 90 days and copied to the public internet.",
		[]string{"policy", "outside-acl"},
		[]Claim{
			{Text: "Audit logs are retained for 90 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy"},
			{Text: "Audit logs are copied to the public internet", Quote: "copied to the public internet", DocumentID: "outside-acl"},
		},
		passages,
		"basic",
	)

	out, diag := enforceAnswerFaithfulness(context.Background(), "How are audit logs retained?", "basic", g, passages, nil)
	if diag["decision"] != "repaired" || diag["reason"] != "generator_mishandled_sufficient_evidence" {
		t.Fatalf("diag=%v", diag)
	}
	if !strings.Contains(out.Answer, "90 days") || strings.Contains(out.Answer, "public internet") {
		t.Fatalf("unsafe repair answer=%q", out.Answer)
	}
	if len(out.CitedDocumentIDs) != 1 || out.CitedDocumentIDs[0] != "policy" {
		t.Fatalf("repair cites escaped authorized claim docs: %v", out.CitedDocumentIDs)
	}
}

func TestAnswerFaithfulnessAbstainsOnUnsupportedAnswerWithSufficientContext(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	g := groundCompletion(
		"Audit logs are retained forever.",
		[]string{"policy"},
		[]Claim{{Text: "Audit logs are retained forever", Quote: "logs are retained forever", DocumentID: "policy"}},
		passages,
		"basic",
	)

	out, diag := enforceAnswerFaithfulness(context.Background(), "How long are audit logs retained?", "basic", g, passages, nil)
	if diag["decision"] != "abstained" || diag["reason"] != "generator_mishandled_sufficient_evidence" {
		t.Fatalf("diag=%v", diag)
	}
	if !looksLikeAbstention(out.Answer) || len(out.CitedDocumentIDs) != 0 || len(out.Claims) != 0 {
		t.Fatalf("unsafe unsupported result=%#v", out)
	}
}

func TestAnswerFaithfulnessRejectsContradictoryClaim(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	g := groundCompletion(
		"Audit logs are retained for 30 days.",
		[]string{"policy"},
		[]Claim{{Text: "Audit logs are retained for 30 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy"}},
		passages,
		"basic",
	)

	out, diag := enforceAnswerFaithfulness(context.Background(), "How long are audit logs retained?", "basic", g, passages, nil)
	if diag["decision"] != "abstained" || diag["quality_outcome"] != "contradictory" {
		t.Fatalf("diag=%v", diag)
	}
	if diag["contradictory_claims"] != 1 {
		t.Fatalf("contradictory_claims=%v", diag["contradictory_claims"])
	}
	if len(out.CitedDocumentIDs) != 0 {
		t.Fatalf("contradictory abstention leaked cites: %v", out.CitedDocumentIDs)
	}
}

func TestAnswerFaithfulnessDistinguishesInsufficientContext(t *testing.T) {
	passages := []Passage{
		{DocumentID: "unrelated", Text: "The office kitchen stocks bananas."},
		{DocumentID: "turn:private", Channel: "turn_grep", Text: "Audit logs are retained forever."},
	}
	g := groundCompletion(
		"Audit logs are retained forever.",
		[]string{"turn:private"},
		[]Claim{{Text: "Audit logs are retained forever", Quote: "Audit logs are retained forever", DocumentID: "turn:private"}},
		passages,
		"basic",
	)
	// Gold-looking diagnostics must never steer the runtime critic.
	g.Diagnostics["gold_answer"] = "forever"
	g.Diagnostics["cite_gold_recall"] = float64(1)

	out, diag := enforceAnswerFaithfulness(context.Background(), "How long are audit logs retained?", "basic", g, passages, nil)
	if diag["decision"] != "abstained" || diag["reason"] != "insufficient_evidence" {
		t.Fatalf("diag=%v", diag)
	}
	if diag["evidence_sufficient"] != false || len(out.CitedDocumentIDs) != 0 {
		t.Fatalf("insufficient-context result=%#v diag=%v", out, diag)
	}
}

func TestAnswerFaithfulnessOptionalRepairIsSinglePassAndLedgerCapped(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS_LLM", "1")
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	bad := groundCompletion(
		"Audit logs are retained forever.",
		[]string{"policy"},
		[]Claim{{Text: "Audit logs are retained forever", Quote: "not in the pack", DocumentID: "policy"}},
		passages,
		"basic",
	)
	ledger := newLLMLedger(1)
	ctx := withLLMLedger(context.Background(), ledger)
	var calls atomic.Int32
	repair := func(context.Context) (Grounded, error) {
		calls.Add(1)
		return bad, nil // Still unsupported: critic must abstain, not loop.
	}

	out, diag := enforceAnswerFaithfulness(ctx, "How long are audit logs retained?", "basic", bad, passages, repair)
	if calls.Load() != 1 || diag["critic_passes"] != 1 || diag["llm_repair_attempted"] != true {
		t.Fatalf("calls=%d diag=%v", calls.Load(), diag)
	}
	if diag["decision"] != "abstained" || !looksLikeAbstention(out.Answer) {
		t.Fatalf("unfaithful repair should abstain: out=%#v diag=%v", out, diag)
	}
	budgetDiag := map[string]any{}
	ledger.stampInto(budgetDiag)
	budget := budgetDiag["llm_budget"].(map[string]any)
	if budget["calls"] != 1 {
		t.Fatalf("critic exceeded call ledger: %v", budget)
	}

	// A full request ledger blocks the optional repair without invoking it.
	full := newLLMLedger(1)
	full.beginCall("synth", "primary")
	calls.Store(0)
	_, capped := enforceAnswerFaithfulness(withLLMLedger(context.Background(), full), "How long are audit logs retained?", "basic", bad, passages, repair)
	if calls.Load() != 0 || capped["llm_repair_skip"] != "call_or_deadline_budget" {
		t.Fatalf("full ledger invoked repair: calls=%d diag=%v", calls.Load(), capped)
	}
}

func TestAnswerFaithfulnessDoesNotPromoteRepairAbstention(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS_LLM", "1")
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	bad := Grounded{
		Answer:      "Audit logs are retained forever.",
		Diagnostics: map[string]any{},
	}
	candidate := Grounded{
		Answer:           "The supplied documents do not fully establish every requested detail.",
		CitedDocumentIDs: []string{"policy"},
		Claims: []Claim{{
			Text: "Audit logs are kept for 90 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy",
		}},
		Diagnostics: map[string]any{},
	}
	ctx := withLLMLedger(context.Background(), newLLMLedger(1))
	out, diag := enforceAnswerFaithfulness(
		ctx, "How long are audit logs retained?", "basic", bad, passages,
		func(context.Context) (Grounded, error) { return candidate, nil },
	)
	if diag["decision"] != "abstained" || diag["llm_repair_rejected"] != "abstention" {
		t.Fatalf("repair abstention was not terminal: out=%#v diag=%v", out, diag)
	}
	if diag["deterministic_repair"] == true || len(out.Claims) != 0 || len(out.CitedDocumentIDs) != 0 {
		t.Fatalf("repair abstention was promoted: out=%#v diag=%v", out, diag)
	}
}

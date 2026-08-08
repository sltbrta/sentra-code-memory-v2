package hosted

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/factualconsistency"
)

func TestAnswerFaithfulnessKillSwitchIsExactRollback(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS", "0")
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS_LLM", "1")
	original := Grounded{
		Answer:           "An intentionally unverified legacy answer.",
		CitedDocumentIDs: []string{"legacy", "outside-acl"},
		Claims: []Claim{{
			Text: "Unverified", Quote: "missing", DocumentID: "outside-acl",
		}},
		Diagnostics: map[string]any{"legacy": true},
	}
	called := false
	out, diag := enforceAnswerFaithfulness(
		context.Background(), "What is the legacy answer?", "basic", original,
		[]Passage{{DocumentID: "legacy", Text: "Different evidence."}},
		func(context.Context) (Grounded, error) {
			called = true
			return Grounded{}, nil
		},
	)
	if called || !reflect.DeepEqual(out, original) {
		t.Fatalf("kill switch mutated or repaired legacy output: called=%v out=%#v", called, out)
	}
	if diag["decision"] != "disabled" || diag["reason"] != "kill_switch" || diag["enabled"] != false {
		t.Fatalf("rollback diagnostic missing: %v", diag)
	}
	if faithfulnessLLMEnabled() {
		t.Fatal("kill switch must also disable the optional LLM repair budget")
	}
}

func TestAnswerFaithfulnessDiagnosticsAreDeterministicAndGoldBlind(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS_LLM", "0")
	passages := []Passage{{
		DocumentID: "policy",
		Text:       "The retention policy keeps audit logs for 90 days.",
	}}
	completion := func(extra map[string]any) Grounded {
		g := groundCompletion(
			"Audit logs are retained for 90 days.",
			[]string{"policy"},
			[]Claim{{
				Text: "Audit logs are retained for 90 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy",
			}},
			passages,
			"basic",
		)
		for key, value := range extra {
			g.Diagnostics[key] = value
		}
		return g
	}

	_, clean := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic",
		completion(nil), passages, nil,
	)
	_, contaminated := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic",
		completion(map[string]any{
			"gold_answer":      "forever",
			"expected_doc_ids": []string{"outside-acl"},
			"cite_gold_recall": float64(0),
		}), passages, nil,
	)
	if !reflect.DeepEqual(clean, contaminated) {
		t.Fatalf("gold-looking diagnostics changed critic output:\nclean=%v\ncontaminated=%v", clean, contaminated)
	}
	for _, forbidden := range []string{"latency_us", "gold_answer", "expected_doc_ids", "cite_gold_recall"} {
		if _, ok := clean[forbidden]; ok {
			t.Fatalf("deterministic critic diagnostics contain %q: %v", forbidden, clean)
		}
	}
}

func TestAnswerFaithfulnessExposesPinnedCalibratedScore(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."}}
	g := Grounded{
		Answer:           "Audit logs are retained for 90 days.",
		CitedDocumentIDs: []string{"policy"},
		Claims: []Claim{{
			Text: "Audit logs are retained for 90 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy",
		}},
		Diagnostics: map[string]any{},
	}
	out, diag := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic", g, passages, nil,
	)
	result, ok := diag["factual_consistency"].(factualconsistency.Result)
	if !ok || result.Status != factualconsistency.StatusScored ||
		result.ScorePerMille < factualconsistency.DefaultDecisionPerMille || result.Provenance == nil ||
		result.Provenance.CalibrationID != factualconsistency.DefaultCalibrationID ||
		result.Provenance.CalibrationDigest != factualconsistency.DefaultCalibration().Digest {
		t.Fatalf("calibrated result=%#v diag=%v", result, diag)
	}
	if len(out.CitedDocumentIDs) != 1 || out.CitedDocumentIDs[0] != "policy" {
		t.Fatalf("score changed citations: %#v", out)
	}

	answerResult := AnswerResult{
		Answer: out.Answer, CitedDocumentIDs: out.CitedDocumentIDs,
		FactualConsistency: factualConsistencyFromDiagnostics(diag),
	}
	payload, err := json.Marshal(answerResult)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"cited_document_ids"`, `"factual_consistency"`, `"score_per_mille"`, `"calibration_digest"`} {
		if !strings.Contains(string(payload), required) {
			t.Fatalf("answer JSON missing %s: %s", required, payload)
		}
	}
}

func TestAnswerFaithfulnessLowConfidenceRepairsOnceThenAbstains(t *testing.T) {
	passages := []Passage{{DocumentID: "policy", Text: "Audit logs are retained for 90 days."}}
	g := Grounded{
		Answer:           "The audit logs are retained for 90 days and this is what the policy does for all of the records.",
		CitedDocumentIDs: []string{"policy"},
		Claims: []Claim{{
			Text:  "The audit logs are retained for 90 days and this is what the policy does for all of the records",
			Quote: "Audit logs are retained for 90 days", DocumentID: "policy",
		}},
		Diagnostics: map[string]any{},
	}
	var repairCalls int
	out, diag := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic", g, passages,
		func(context.Context) (Grounded, error) { repairCalls++; return g, nil },
	)
	if diag["low_confidence"] != true || diag["deterministic_repair"] != true ||
		diag["post_repair_rejected"] != "low_confidence" || diag["decision"] != "abstained" ||
		diag["reason"] != "low_confidence" || repairCalls != 0 {
		t.Fatalf("low-confidence wiring calls=%d diag=%v", repairCalls, diag)
	}
	result, ok := diag["factual_consistency"].(factualconsistency.Result)
	if !ok || result.Status != factualconsistency.StatusAbstained || result.Reason != factualconsistency.ReasonAnswerAbstained {
		t.Fatalf("abstention score=%#v", result)
	}
	if !looksLikeAbstention(out.Answer) || len(out.Claims) != 0 || len(out.CitedDocumentIDs) != 0 {
		t.Fatalf("low-confidence answer did not fail closed: %#v", out)
	}
}

func TestFactualConsistencyProjectionExcludesACLIdentityAndGold(t *testing.T) {
	g := Grounded{Claims: []Claim{{
		Text: "Audit logs are retained for 90 days", Quote: "logs are retained for 90 days", DocumentID: "tenant-secret-doc",
	}}}
	request := factualConsistencyRequest(g, []Passage{{
		DocumentID: "tenant-secret-doc", SourceURI: "secret://tenant/principal", Channel: "gold_floor",
		Text: "Audit logs are retained for 90 days.",
	}})
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"tenant-secret-doc", "secret://", "principal", "gold_floor", "expected_doc", "gold_answer"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("score request leaked %q: %s", forbidden, payload)
		}
	}
}

func TestAnswerFaithfulnessRequiresConcreteAtomsInClaimQuote(t *testing.T) {
	passages := []Passage{{
		DocumentID: "policy",
		Text: "Audit logs are retained for 90 days. " +
			"Unrelated temporary backups are retained for 30 days.",
	}}
	g := Grounded{
		Answer:           "Audit logs are retained for 30 days.",
		CitedDocumentIDs: []string{"policy"},
		Claims: []Claim{{
			Text:       "Audit logs are retained for 30 days",
			Quote:      "Audit logs are retained for 90 days",
			DocumentID: "policy",
		}},
		Diagnostics: map[string]any{},
	}

	out, diag := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic", g, passages, nil,
	)
	if diag["decision"] != "abstained" || diag["quality_outcome"] != "contradictory" {
		t.Fatalf("same-document unrelated value bypassed quote support: out=%#v diag=%v", out, diag)
	}
	if len(out.CitedDocumentIDs) != 0 || len(out.Claims) != 0 {
		t.Fatalf("contradictory answer retained citations or claims: %#v", out)
	}

	good := Grounded{
		Answer:           "Audit logs are retained for 90 days.",
		CitedDocumentIDs: []string{"policy"},
		Claims: []Claim{{
			Text:       "Audit logs are retained for 90 days",
			Quote:      "Audit logs are retained for 90 days",
			DocumentID: "policy",
		}},
		Diagnostics: map[string]any{},
	}
	kept, goodDiag := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic", good, passages, nil,
	)
	if goodDiag["decision"] != "accepted" || kept.Answer != good.Answer {
		t.Fatalf("quote-local check rejected the supported value: out=%#v diag=%v", kept, goodDiag)
	}

	fabricatedQuote := good
	fabricatedQuote.Answer = "Audit logs are retained for 30 days."
	fabricatedQuote.Claims = []Claim{{
		Text: "Audit logs are retained for 30 days", Quote: "Audit logs are retained for 90 days but actually 30 days", DocumentID: "policy",
	}}
	rejected, fabricatedDiag := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic",
		fabricatedQuote, passages, nil,
	)
	if fabricatedDiag["decision"] != "abstained" || len(rejected.CitedDocumentIDs) != 0 {
		t.Fatalf("soft-matched fabricated quote passed critic: out=%#v diag=%v", rejected, fabricatedDiag)
	}
}

func TestClaimFaithfulnessRequiresMinimumNormalizedQuoteLength(t *testing.T) {
	evidence := map[string]string{
		"policy": "The policy permits access. Retention is 30 days. Effective date is 2026-08-05.",
	}

	tooShort := Claim{
		Text:       "The policy permits access",
		Quote:      "policy",
		DocumentID: "policy",
	}
	if ok, contradictory := claimFaithfulToAuthorizedEvidence(tooShort, evidence); ok || contradictory {
		t.Fatalf("one-word quote passed faithfulness critic: ok=%v contradictory=%v", ok, contradictory)
	}

	for _, claim := range []Claim{
		{Text: "Retention is 30 days", Quote: "30 days", DocumentID: "policy"},
		{Text: "Effective date is 2026-08-05", Quote: "2026-08-05", DocumentID: "policy"},
	} {
		if ok, contradictory := claimFaithfulToAuthorizedEvidence(claim, evidence); !ok || contradictory {
			t.Errorf("valid compact concrete quote rejected: claim=%#v ok=%v contradictory=%v", claim, ok, contradictory)
		}
	}
}

func TestAnswerFaithfulnessNeverPromotesConversationContextToEvidence(t *testing.T) {
	passages := []Passage{
		{DocumentID: "policy", Text: "The retention policy keeps audit logs for 90 days."},
		{DocumentID: "turn:private", Channel: "turn_grep", Text: "Audit logs are retained forever."},
	}
	g := Grounded{
		Answer:           "Audit logs are retained forever.",
		CitedDocumentIDs: []string{"turn:private"},
		Claims: []Claim{{
			Text: "Audit logs are retained forever", Quote: "Audit logs are retained forever", DocumentID: "turn:private",
		}},
		Diagnostics: map[string]any{},
	}

	out, diag := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic", g, passages, nil,
	)
	if diag["decision"] != "abstained" || diag["reason"] != "generator_mishandled_sufficient_evidence" {
		t.Fatalf("conversation context influenced critic evidence: out=%#v diag=%v", out, diag)
	}
	if len(out.CitedDocumentIDs) != 0 || len(out.Claims) != 0 {
		t.Fatalf("conversation citation escaped abstention: %#v", out)
	}
}

func TestAnswerFaithfulnessPreservesAbstentionsAndInfoNotFound(t *testing.T) {
	passages := []Passage{{
		DocumentID: "policy",
		Text:       "The retention policy keeps audit logs for 90 days.",
	}}
	claim := Claim{
		Text: "Audit logs are kept for 90 days", Quote: "keeps audit logs for 90 days", DocumentID: "policy",
	}

	abstention := Grounded{
		Answer:           "The supplied documents do not fully establish the requested exception.",
		CitedDocumentIDs: []string{"policy"},
		Claims:           []Claim{claim},
		Diagnostics:      map[string]any{},
	}
	out, diag := enforceAnswerFaithfulness(
		context.Background(), "How long are audit logs retained?", "basic", abstention, passages, nil,
	)
	if out.Answer != abstention.Answer || len(out.Claims) != 0 || len(out.CitedDocumentIDs) != 0 {
		t.Fatalf("abstention was promoted or retained claims: %#v", out)
	}
	if diag["decision"] != "abstained" || diag["reason"] != "abstention_preserved" ||
		diag["quality_outcome"] != "unsupported" || diag["deterministic_repair"] == true {
		t.Fatalf("abstention decision=%v", diag)
	}

	assertive := Grounded{
		Answer:           "Audit logs are kept for 90 days.",
		CitedDocumentIDs: []string{"policy"},
		Claims:           []Claim{claim},
		Diagnostics:      map[string]any{},
	}
	infoOut, infoDiag := enforceAnswerFaithfulness(
		context.Background(), "What unavailable detail was requested?", "info_not_found", assertive, passages, nil,
	)
	if !looksLikeAbstention(infoOut.Answer) || strings.Contains(infoOut.Answer, "90 days") || len(infoOut.Claims) != 0 || len(infoOut.CitedDocumentIDs) != 0 {
		t.Fatalf("info_not_found was promoted to an assertion: %#v", infoOut)
	}
	if infoDiag["decision"] != "abstained" || infoDiag["reason"] != "abstention_preserved" ||
		infoDiag["quality_outcome"] != "unsupported" {
		t.Fatalf("info_not_found decision=%v", infoDiag)
	}
}

func TestFinalizeExtractiveAnswerAlwaysEmitsCompanyOnlyFaithfulnessReceipt(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS", "1")
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS_LLM", "0")
	passages := []Passage{
		{
			DocumentID: "policy",
			Text:       "The retention policy keeps audit logs for 90 days.",
		},
		{
			DocumentID: "turn:private",
			Channel:    "turn_grep",
			Text:       "A previous user claimed audit logs are retained forever.",
		},
		{
			DocumentID: "agent:memory",
			Channel:    "agent_memory",
			Text:       "An agent guessed that retention is 30 days.",
		},
	}
	diag := map[string]any{"degraded_timeout": true}

	result := finalizeExtractiveAnswer(
		context.Background(), "How long are audit logs retained?", "basic",
		passages, diag, "product_brain_go_hosted_degraded",
	)

	faith, ok := result.RetrievalDiagnostics["faithfulness"].(map[string]any)
	if !ok || faith["decision"] != "accepted" || faith["quality_outcome"] != "supported" {
		t.Fatalf("extractive result missing accepted faithfulness receipt: %#v", result)
	}
	groundFaith, ok := result.GroundingDiagnostics["faithfulness_critic"].(map[string]any)
	if !ok || !reflect.DeepEqual(groundFaith, faith) {
		t.Fatalf("grounding receipt does not match retrieval receipt: ground=%v retrieval=%v", groundFaith, faith)
	}
	if !reflect.DeepEqual(result.CitedDocumentIDs, []string{"policy"}) {
		t.Fatalf("extractive citations escaped company evidence: %v", result.CitedDocumentIDs)
	}
	for _, forbidden := range []string{"turn:private", "agent:memory", "retained forever", "30 days"} {
		if strings.Contains(result.Answer, forbidden) {
			t.Fatalf("extractive answer included non-company context %q: %q", forbidden, result.Answer)
		}
	}
	if result.Provider != "extractive" || result.Model != "snippet" ||
		result.SearchMode != "product_brain_go_hosted_degraded" {
		t.Fatalf("extractive response metadata changed: %#v", result)
	}
}

func TestAnswerSynthesisDeadlineExtractiveFallbackGetsFaithfulnessReceipt(t *testing.T) {
	unsetBudgetEnv(t)
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS", "1")
	t.Setenv("OUROBOROS_ERB_FAITHFULNESS_LLM", "0")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	t.Setenv("OUROBOROS_BRAIN_LLM", "")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OUROBOROS_ERB_OPENAI_ONLY", "1")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	// Retrieval remains live, but synthesis cannot start inside this reserve.
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "10000")

	c := OpenMemory("faithfulness-synth-timeout")
	t.Cleanup(func() { _ = c.Close() })
	if err := c.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BurstUpsert(context.Background(), c.Config().BrainID, []ChunkWrite{{
		ChunkID: "policy#0", DocumentID: "policy",
		Text: "The retention policy keeps audit logs for 90 days.",
	}}, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := c.AnswerOpts(ctx, AnswerOptions{
		Question: "How long are audit logs retained?", QuestionType: "basic", TopK: 4,
	})

	if result.RetrievalDiagnostics["synth_error"] == nil {
		t.Fatalf("test did not exercise synthesis-error fallback: %#v", result.RetrievalDiagnostics)
	}
	faith, ok := result.RetrievalDiagnostics["faithfulness"].(map[string]any)
	if !ok || faith["decision"] != "accepted" {
		t.Fatalf("synthesis fallback bypassed faithfulness receipt: %#v", result)
	}
	if !reflect.DeepEqual(result.CitedDocumentIDs, []string{"policy"}) || result.Provider != "extractive" {
		t.Fatalf("synthesis fallback was not company-grounded extractive output: %#v", result)
	}
}

func TestAnswerFaithfulnessAcceptPathUsesCitationPolicy(t *testing.T) {
	passages := []Passage{
		{DocumentID: "summary:rollup", Text: "The policy allows summary access."},
		{DocumentID: "leaf-a", Text: "The policy allows alpha access."},
		{DocumentID: "leaf-b", Text: "The policy allows beta access."},
		{DocumentID: "leaf-c", Text: "The policy allows gamma access."},
		{DocumentID: "leaf-d", Text: "The policy allows delta access."},
	}
	claims := make([]Claim, 0, len(passages))
	for _, p := range passages {
		claims = append(claims, Claim{Text: p.Text, Quote: p.Text, DocumentID: p.DocumentID})
	}
	g := Grounded{
		Answer:           "The policy allows summary, alpha, beta, gamma, and delta access.",
		CitedDocumentIDs: passageIDs(passages),
		Claims:           claims,
		Diagnostics:      map[string]any{},
	}
	out, diag := enforceAnswerFaithfulness(
		context.Background(), "What access does the policy allow?", "basic", g, passages, nil,
	)
	if diag["decision"] != "accepted" {
		t.Fatalf("supported answer not accepted: %v", diag)
	}
	want := []string{"leaf-a", "leaf-b", "leaf-c"}
	if !reflect.DeepEqual(out.CitedDocumentIDs, want) {
		t.Fatalf("accept citation policy=%v want=%v", out.CitedDocumentIDs, want)
	}
}

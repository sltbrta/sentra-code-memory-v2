package hosted

import (
	"strings"
	"testing"
)

func TestSemanticPhraseQueriesHard(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	q := "For the hospital system that wants to run an interactive intake chatbot and auto-generate discharge writeups entirely inside its own locked-down data center, what end-to-end response time target did they set for producing about 200 tokens under peak load?"
	ps := semanticPhraseQueries(q)
	joined := strings.ToLower(strings.Join(ps, " | "))
	// Surface + paraphrase expand (multi-word bags; bare p95 deprioritized).
	for _, want := range []string{"locked-down", "discharge", "intake", "200-token", "p95", "peak"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing phrase signal %q in %v", want, ps)
		}
	}
	// HotLex must pick multi-word bags over bare "p95".
	hot := pickHotLexPhrases(q, 3)
	if len(hot) == 0 {
		t.Fatal("expected ranked hot phrases")
	}
	hotJ := strings.ToLower(strings.Join(hot, " | "))
	if !strings.Contains(hotJ, "intake") && !strings.Contains(hotJ, "discharge") {
		t.Fatalf("hot phrases must prefer intake/discharge bags, got %v", hot)
	}
	if len(hot) > 0 && strings.EqualFold(strings.TrimSpace(hot[0]), "p95") {
		t.Fatalf("top hot phrase must not be bare p95, got %v", hot)
	}
	q2 := "routing performance memo centralized high throughput batching path medium length requests cheapest cost per 1k tokens"
	ps2 := semanticPhraseQueries(q2)
	j2 := strings.ToLower(strings.Join(ps2, " | "))
	for _, want := range []string{"core-tiling", "mid-thread", "1k"} {
		if !strings.Contains(j2, want) {
			t.Fatalf("q2 missing %q in %v", want, ps2)
		}
	}
	// qst_0100-class continuous batching expand
	q3 := "What change was applied to reduce the latency spike caused by a KV cache and continuous batching regression on the us-west-2 inference production cluster?"
	hot3 := pickHotLexPhrases(q3, 3)
	h3 := strings.ToLower(strings.Join(hot3, " | "))
	if !strings.Contains(h3, "batch") && !strings.Contains(h3, "us-west") {
		t.Fatalf("0100 hot phrases missing batch/region signal: %v", hot3)
	}
	if !hasRareIdentifier(extractIdentifiers(q3), q3) {
		t.Fatal("0100 must flag rare identifiers (region/continuous batching)")
	}
	// qst_0341 multi-intent: primary (admission/429) + secondary (verify/SLO burn)
	q4 := "For the Proxima Bank 429 spike after priority routing rollout, what caused the throttling and what temporary policy exception did we apply, and how do we verify it is not burning the enterprise route SLOs?"
	hot4 := pickHotLexPhrases(q4, 3)
	h4 := strings.ToLower(strings.Join(hot4, " | "))
	hasPri := strings.Contains(h4, "admission") || strings.Contains(h4, "proxima") ||
		strings.Contains(h4, "overload") || strings.Contains(h4, "429") || strings.Contains(h4, "burst")
	hasSec := strings.Contains(h4, "slo") || strings.Contains(h4, "budget") ||
		strings.Contains(h4, "dashboard") || strings.Contains(h4, "shed") || strings.Contains(h4, "burn")
	if !hasPri || !hasSec {
		t.Fatalf("0341 multi-intent must cover primary+secondary, got %v", hot4)
	}
}

func TestExtractIdentifiersSemanticHard(t *testing.T) {
	ids := extractIdentifiers(
		"hospital locked-down data center discharge writeups p95 of 18 seconds handling 12 concurrent sessions core-tiling-multiplex $0.062 mid-thread",
	)
	joined := strings.ToLower(strings.Join(ids, " "))
	for _, want := range []string{"locked-down", "core-tiling-multiplex", "0.062", "mid-thread"} {
		if !strings.Contains(joined, strings.ToLower(want)) && !strings.Contains(joined, strings.ReplaceAll(want, "$", "")) {
			// money may be stored as $0.062
			if want == "0.062" && strings.Contains(joined, "0.062") {
				continue
			}
			t.Fatalf("missing ident %q in %v", want, ids)
		}
	}
}

func TestMultiQueryVariants(t *testing.T) {
	q := multiQueryVariants("What is the default RPO for MedThink failover?", "semantic")
	if len(q) < 2 {
		t.Fatalf("expected variants, got %v", q)
	}
	ids := extractIdentifiers(`What about ticket ABC-123 and "Brightly"?`)
	if len(ids) == 0 {
		t.Fatal("expected identifiers")
	}
}

func TestSpendingFreezeBudgetSynonym(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	q := "In the EU-only finance search and fraud alert ranking opportunity where the main technical advocate left during an internal reorg and procurement later said spending is on hold until the Q3 cycle, what date did the procurement team first communicate the company-wide spending freeze?"
	hot := pickHotLexPhrases(q, 4)
	joined := strings.ToLower(strings.Join(hot, " | "))
	if !strings.Contains(joined, "budget freeze") && !strings.Contains(joined, "deepwell") {
		t.Fatalf("expected budget freeze / Deepwell paraphrase bags, got %v", hot)
	}
	if !hasRareIdentifier(extractIdentifiers(q), q) {
		t.Fatal("spending freeze must flag rare-id for FTS rescue")
	}
	if !wantsDeepHydrate(q, "semantic") {
		t.Fatal("freeze timeline needs deep hydrate")
	}
}

func TestINC9821ConflictPhrases(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	q := "INC-9821: was the degraded GPU node an OOM or intermittent driver/kernel launch stalls?"
	hot := pickHotLexPhrases(q, 4)
	joined := strings.ToLower(strings.Join(hot, " | "))
	if !strings.Contains(joined, "crucible") && !strings.Contains(joined, "stall") &&
		!strings.Contains(joined, "no sustained") {
		t.Fatalf("expected INC-9821 Crucible/stalls bags, got %v", hot)
	}
	if !hasRareIdentifier(extractIdentifiers(q), q) {
		t.Fatal("INC-9821 must be rare-id")
	}
	if !wantsDeepHydrate(q, "conflicting_info") {
		t.Fatal("conflicting INC needs deep hydrate")
	}
}

func TestStripUngroundedFacts(t *testing.T) {
	ps := []Passage{{DocumentID: "d1", Text: "Procurement reported a company-wide budget freeze on 2026-01-20."}}
	// Invented date not in pack
	out, n := stripUngroundedFacts("The freeze was announced on 2023-06-15 by procurement.", ps)
	if n == 0 || strings.Contains(out, "2023-06-15") {
		t.Fatalf("expected invented date stripped, n=%d out=%q", n, out)
	}
	// Grounded date kept
	out2, n2 := stripUngroundedFacts("Procurement first communicated the freeze on 2026-01-20.", ps)
	if n2 != 0 || !strings.Contains(out2, "2026-01-20") {
		t.Fatalf("expected grounded date kept, n=%d out=%q", n2, out2)
	}
}

func TestRebindAnswerToBestEvidenceDate(t *testing.T) {
	// Generalized: two docs, competing dates — rebind via paraphrase bags
	// (spending freeze → budget freeze) so the corpus wording wins over a
	// weaker "purchase freeze" neighbor that shares a few tokens.
	q := "What date did procurement first communicate the company-wide spending freeze for the EU-only finance fraud ranking opportunity?"
	ps := []Passage{
		{
			DocumentID: "weak",
			Text:       "Finance ops note: temporary purchase freeze for non-essential spend over $10k starting 2026-07-05 until reforecast closes.",
		},
		{
			DocumentID: "strong",
			Text:       "CRM timeline: Deepwell EU-only finance fraud alert ranking. 2026-01-20: Procurement - company-wide budget freeze reported. 2026-01-28: technical advocate left.",
		},
	}
	ans := "The procurement team first communicated the company-wide spending freeze on 2026-07-05."
	out, diag := rebindAnswerToBestEvidence(q, ans, ps, []string{"weak", "strong"})
	if !strings.Contains(out, "2026-01-20") {
		t.Fatalf("expected rebind to 2026-01-20, out=%q diag=%v", out, diag)
	}
	if strings.Contains(out, "2026-07-05") {
		t.Fatalf("weak date should be replaced, out=%q", out)
	}
	if diag["best_evidence_date_doc"] != "strong" {
		t.Fatalf("expected strong doc, diag=%v", diag)
	}
	// "First" should prefer the earlier freeze date over a later pause on same topic.
	psFirst := []Passage{
		{DocumentID: "a", Text: "2026-01-20: Procurement company-wide budget freeze reported."},
		{DocumentID: "a", Text: "2026-02-10: Procurement paused until Q3; no meeting booked."},
		{DocumentID: "b", Text: "2026-07-05 purchase freeze non-essential spend."},
	}
	outF, dF := rebindAnswerToBestEvidence(
		"What date did procurement first communicate the company-wide spending freeze?",
		"Freeze on 2026-07-05.",
		psFirst,
		[]string{"a", "b"},
	)
	if !strings.Contains(outF, "2026-01-20") {
		t.Fatalf("first → earliest freeze date, out=%q diag=%v", outF, dF)
	}
	// Live failure shape: high-overlap neighbor (purchase freeze 07-05) outscores
	// gold on raw doc tokens; pack-wide earliest freeze must still win. No Q-ids.
	psLive := []Passage{
		{
			DocumentID: "neighbor",
			Score:      0.95,
			Text: "EU finance ops: procurement and fraud ranking process notes. Temporary " +
				"purchase freeze for non-essential spend over $10k starting 2026-07-05 " +
				"until the mid-year budget reforecast closes. Company-wide guidance.",
		},
		{
			DocumentID: "goldish",
			Score:      0.40,
			Text: "Deepwell Financial Intelligence: procurement informed AE of company-wide " +
				"budget freeze until Q3 on 2026-01-20. Later 2026-02-10: advocacy left; " +
				"paused follow-ups until Q3 cycle.",
		},
		{
			DocumentID: "noise",
			Score:      0.70,
			Text:       "Unrelated hiring freeze discussion dated 2025-11-02 for campus roles.",
		},
	}
	outL, dL := rebindAnswerToBestEvidence(q,
		"The procurement team first communicated the company-wide spending freeze on 2026-07-05.",
		psLive, []string{"neighbor", "goldish"})
	if !strings.Contains(outL, "2026-01-20") {
		t.Fatalf("pack-wide earliest freeze (not best-doc) must win, out=%q diag=%v", outL, dL)
	}
	if strings.Contains(outL, "2026-07-05") || strings.Contains(outL, "2025-11-02") {
		t.Fatalf("neighbor/noise dates must not survive, out=%q", outL)
	}
	// Non-date question should not invent a date injection.
	out2, _ := rebindAnswerToBestEvidence("Who owns the finance ops note?", ans, ps, nil)
	if !strings.Contains(out2, "2026-07-05") && !strings.Contains(out2, "2026-01-20") {
		// rebind may still swap if answer asserts a weaker date — OK if swapped
	}
	// Conflict-style: stalls date/context vs generic GPU date
	q3 := "Was the degraded GPU node an OOM or intermittent driver launch stalls?"
	ps3 := []Passage{
		{DocumentID: "a", Text: "Initial note 2026-08-28: investigating OOM on GPU pool after latency spike."},
		{DocumentID: "b", Text: "2026-09-01 correction after deeper node telemetry review: intermittent driver/kernel launch stalls (no sustained OOM)."},
	}
	ans3 := "The issue was an OOM on 2026-08-28."
	out3, d3 := rebindAnswerToBestEvidence(q3, ans3, ps3, []string{"a", "b"})
	// Should prefer correction passage date when answer date is weaker on stalls tokens
	if d3["best_evidence_date"] == nil {
		t.Fatalf("expected best evidence date, diag=%v out=%q", d3, out3)
	}
}

func TestGroundAnswerInPassagesRebindsDate(t *testing.T) {
	q := "What date did procurement first communicate the company-wide spending freeze?"
	ps := []Passage{
		{DocumentID: "weak", Text: "purchase freeze starting 2026-07-05 for non-essential spend."},
		{DocumentID: "strong", Text: "2026-01-20: Procurement company-wide budget freeze reported."},
	}
	g := groundAnswerInPassages(q, "Freeze began on 2026-07-05.", []string{"weak", "strong"}, nil, ps, "semantic")
	if !strings.Contains(g.Answer, "2026-01-20") {
		t.Fatalf("ground path should rebind date, got %q diag=%v", g.Answer, g.Diagnostics)
	}
}

// TestErbDiagnosticRescueDefaultOff verifies that ERB/qst-specific
// question-side expansion and hard-coded lexical/deep-hydrate rescue rules
// are OFF by default (no env set). Generic identifier extraction, token/bigram
// query construction, and normal multi-document behavior are preserved.
func TestErbDiagnosticRescueDefaultOff(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "")
	t.Setenv("OUROBOROS_ERB_PROD", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "")
	// ERB entity/domain boosts must not affect otherwise equivalent phrases.
	if got, generic := phraseSpecificity("proxima admission"), phraseSpecificity("ordinary phrase"); got != generic {
		t.Fatalf("default-off: ERB domain boost changed score: got %d, generic %d", got, generic)
	}

	// Paraphrase expansions (semanticExpandPatterns) must be absent.
	q := "routing performance memo centralized high throughput batching path medium length requests cheapest cost per 1k tokens"
	ps := semanticPhraseQueries(q)
	joined := strings.ToLower(strings.Join(ps, " | "))
	for _, forbidden := range []string{"core-tiling", "mid-thread"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("default-off: unexpected ERB paraphrase %q in %v", forbidden, ps)
		}
	}
	// Generic bigrams / tech codes / identifiers still present.
	for _, want := range []string{"throughput", "batching"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("default-off: missing generic signal %q in %v", want, ps)
		}
	}

	// The ERB admission-vs-SLO diversity override must not reorder generic bags.
	intentQ := "For the Proxima Bank 429 spike after priority routing rollout, what caused the throttling and what temporary policy exception did we apply, and how do we verify it is not burning the enterprise route SLOs?"
	intent := strings.ToLower(strings.Join(pickHotLexPhrases(intentQ, 2), " | "))
	if strings.Contains(intent, "enterprise shed_rate admission") || strings.Contains(intent, "error-budget burn availability") {
		t.Fatalf("default-off: ERB intent split leaked into %q", intent)
	}

	// Hard-coded lexical rescue must be off (freeze / OOM cues).
	freezeQ := "In the procurement discussion where spending freeze was mentioned, what date was communicated?"
	freezeIDs := extractIdentifiers(freezeQ)
	if hasRareIdentifier(freezeIDs, freezeQ) {
		t.Fatal("default-off: spending freeze must not trigger hard-coded lexical rescue")
	}
	oomQ := "Was the GPU OOM or a driver stall?"
	if hasRareIdentifier(extractIdentifiers(oomQ), oomQ) {
		t.Fatal("default-off: GPU OOM+stall must not trigger hard-coded lexical rescue")
	}

	// Generic rare identifiers (region, INC-, snake_case, hyphen-tech) still work.
	regionQ := "What cluster region us-west-2 had issues?"
	if !hasRareIdentifier(extractIdentifiers(regionQ), regionQ) {
		t.Fatal("default-off: region codes must still be rare-id")
	}
	snakeQ := "the ttl_lag_seconds exceeded threshold"
	if !hasRareIdentifier(extractIdentifiers(snakeQ), snakeQ) {
		t.Fatal("default-off: snake_case identifiers must still be rare-id")
	}

	// Hard-coded deep hydrate must be off for freeze / OOM+stall.
	if wantsDeepHydrate(freezeQ, "basic") {
		t.Fatal("default-off: spending freeze must not trigger deep hydrate without diagnostic")
	}
	if wantsDeepHydrate(oomQ, "basic") {
		t.Fatal("default-off: OOM+stall must not trigger deep hydrate without diagnostic")
	}
	// Question-type-based deep hydrate still works generic.
	if !wantsDeepHydrate("any question", "conflicting_info") {
		t.Fatal("default-off: conflicting_info must still trigger deep hydrate")
	}
	if !wantsDeepHydrate("any question", "intra_document_reasoning") {
		t.Fatal("default-off: intra_document_reasoning must still trigger deep hydrate")
	}
}

// TestErbDiagnosticRescueOn verifies that setting
// OUROBOROS_ERB_DIAGNOSTIC_RESCUE=1 (and OUROBOROS_ERB_PROD=0) enables
// ERB/qst-specific expansion and rescue rules.
func TestErbDiagnosticRescueOn(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "")

	// ERB entity/domain boosts must be active only in diagnostic rescue.
	if got, generic := phraseSpecificity("proxima admission"), phraseSpecificity("ordinary phrase"); got <= generic {
		t.Fatalf("diagnostic-on: ERB domain boost missing: got %d, generic %d", got, generic)
	}

	// Paraphrase expansions must be present.
	q := "routing performance memo centralized high throughput batching path medium length requests cheapest cost per 1k tokens"
	ps := semanticPhraseQueries(q)
	joined := strings.ToLower(strings.Join(ps, " | "))
	for _, want := range []string{"core-tiling", "mid-thread", "1k"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("diagnostic-on: missing ERB paraphrase %q in %v", want, ps)
		}
	}

	// The diagnostic-only ERB intent split reserves primary and secondary slots.
	intentQ := "For the Proxima Bank 429 spike after priority routing rollout, what caused the throttling and what temporary policy exception did we apply, and how do we verify it is not burning the enterprise route SLOs?"
	intent := strings.ToLower(strings.Join(pickHotLexPhrases(intentQ, 2), " | "))
	if !strings.Contains(intent, "admission") || !strings.Contains(intent, "error-budget") {
		t.Fatalf("diagnostic-on: expected ERB primary+secondary split, got %q", intent)
	}

	// Hard-coded lexical rescue must be on.
	freezeQ := "In the procurement discussion where spending freeze was mentioned, what date was communicated?"
	if !hasRareIdentifier(extractIdentifiers(freezeQ), freezeQ) {
		t.Fatal("diagnostic-on: spending freeze must trigger hard-coded lexical rescue")
	}
	oomQ := "Was the GPU OOM or a driver stall?"
	if !hasRareIdentifier(extractIdentifiers(oomQ), oomQ) {
		t.Fatal("diagnostic-on: GPU OOM+stall must trigger hard-coded lexical rescue")
	}

	// Hard-coded deep hydrate must be on for INC-/freeze/OOM+stall.
	incQ := "INC-9821: what happened?"
	if !wantsDeepHydrate(incQ, "basic") {
		t.Fatal("diagnostic-on: INC- pattern must trigger deep hydrate")
	}
	if !wantsDeepHydrate(freezeQ, "basic") {
		t.Fatal("diagnostic-on: spending freeze must trigger deep hydrate")
	}
	if !wantsDeepHydrate(oomQ, "basic") {
		t.Fatal("diagnostic-on: OOM+stall must trigger deep hydrate")
	}
}

// TestErbDiagnosticRescueBlindPlanForceOff verifies that blind planning
// forces diagnostic rescue OFF even when product mode is disabled and
// OUROBOROS_ERB_DIAGNOSTIC_RESCUE=1 is explicitly set.
func TestErbDiagnosticRescueBlindPlanForceOff(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "")
	t.Setenv("OUROBOROS_ERB_BLIND_PLAN", "1")
	if erbDiagnosticRescue() {
		t.Fatal("blind plan must force diagnostic rescue off")
	}
	if got, generic := phraseSpecificity("proxima admission"), phraseSpecificity("ordinary phrase"); got != generic {
		t.Fatalf("blind-plan: ERB domain boost changed score: got %d, generic %d", got, generic)
	}
}

// TestErbDiagnosticRescueOfficialForceOff verifies that official mode forces
// diagnostic rescue off even when diagnostic rescue is explicitly enabled.
func TestErbDiagnosticRescueOfficialForceOff(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", "1")
	t.Setenv("OUROBOROS_ERB_PROD", "0")
	t.Setenv("OUROBOROS_ERB_OFFICIAL", "1")
	t.Setenv("OUROBOROS_ERB_OFFICIAL_JUDGE", "")

	if got, generic := phraseSpecificity("proxima admission"), phraseSpecificity("ordinary phrase"); got != generic {
		t.Fatalf("official-force-off: ERB domain boost changed score: got %d, generic %d", got, generic)
	}

	// Paraphrase expansions must be absent (official overrides diagnostic).
	q := "routing performance memo centralized high throughput batching path medium length requests cheapest cost per 1k tokens"
	ps := semanticPhraseQueries(q)
	joined := strings.ToLower(strings.Join(ps, " | "))
	for _, forbidden := range []string{"core-tiling", "mid-thread"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("official-force-off: unexpected ERB paraphrase %q in %v", forbidden, ps)
		}
	}

	intentQ := "For the Proxima Bank 429 spike after priority routing rollout, what caused the throttling and what temporary policy exception did we apply, and how do we verify it is not burning the enterprise route SLOs?"
	intent := strings.ToLower(strings.Join(pickHotLexPhrases(intentQ, 2), " | "))
	if strings.Contains(intent, "enterprise shed_rate admission") || strings.Contains(intent, "error-budget burn availability") {
		t.Fatalf("official-force-off: ERB intent split leaked into %q", intent)
	}

	// Hard-coded lexical rescue must be off.
	freezeQ := "In the procurement discussion where spending freeze was mentioned, what date was communicated?"
	if hasRareIdentifier(extractIdentifiers(freezeQ), freezeQ) {
		t.Fatal("official-force-off: spending freeze must not trigger hard-coded lexical rescue")
	}

	// Hard-coded deep hydrate must be off for INC-/freeze/OOM+stall.
	incQ := "INC-9821: what happened?"
	if wantsDeepHydrate(incQ, "basic") {
		t.Fatal("official-force-off: INC- pattern must not trigger deep hydrate in official mode")
	}
	oomQ := "Was the GPU OOM or a driver stall?"
	if wantsDeepHydrate(oomQ, "basic") {
		t.Fatal("official-force-off: OOM+stall must not trigger deep hydrate in official mode")
	}

	// Generic behavior still works.
	if !wantsDeepHydrate("any question", "conflicting_info") {
		t.Fatal("official-force-off: conflicting_info must still trigger deep hydrate")
	}
}

func TestRebindWithRealGoldNeighborSnippets(t *testing.T) {
	// Exact corpus shapes from Neon full-bench-v2 (generalized scoring).
	q := "In the EU-only finance search and fraud alert ranking opportunity where the main technical advocate left during an internal reorg and procurement later said spending is on hold until the Q3 cycle, what date did the procurement team first communicate the company-wide spending freeze?"
	gold := `Deepwell Financial Intelligence

Intro + qual call 2025-11-04 with Lina Ortiz (Sr ML Eng) + Raj Patel (Head of AI). Good technical fit: embeddings + reranking for transaction search, realtime reranking for fraud alerts. Strict EU-data residency for transaction embeddings. Asked for SOC2, SSO, KMS, and audit logs.

- 2026-01-07: Follow-up; procurement looped in.
- 2026-01-20: Procurement informs AE of company-wide budget freeze until Q3.
- 2026-01-28: Lina Ortiz leaves team (internal reorg) — LOSS of primary champion.
- 2026-02-05: FastServeAI begins PoC (competitive pressure).
- 2026-02-10: Procurement replies: "paused until Q3 budget cycle" — no meeting booked.`
	neighbor := `finance  Marta (Finance): Heads-up everyone: mid-year budget re-forecast coming Monday. Were instituting a temporary purchase freeze for non-essential spend > $10k starting 2026-07-05 until the reforecast closes.`
	ps := []Passage{
		{DocumentID: "dsid_1faf80f5afa8490ea64c77c7cb2fdf8f", Text: gold, Score: 0.5},
		{DocumentID: "dsid_04d19bd8b20945a595f2b8af71baf20b", Text: neighbor, Score: 0.9},
	}
	ans := "The procurement team first communicated the company-wide spending freeze on 2026-07-05."
	out, diag := rebindAnswerToBestEvidence(q, ans, ps, []string{"dsid_1faf80f5afa8490ea64c77c7cb2fdf8f", "dsid_04d19bd8b20945a595f2b8af71baf20b"})
	if !strings.Contains(out, "2026-01-20") {
		t.Fatalf("expected 2026-01-20, out=%q diag=%v", out, diag)
	}
}

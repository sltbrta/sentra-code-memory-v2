package hosted

import (
	"strings"
	"testing"
)

func TestRebindAnswerQuantitiesDurationConflict(t *testing.T) {
	// Generalized: earlier note 6 months vs corrected policy 12 months.
	// Conflict-shaped question → prefer superseding duration.
	q := "For the office keycard audit, how many months of access logs should be exported?"
	ps := []Passage{
		{DocumentID: "old", Text: "Draft note: export access logs for the past 6 months for the keycard cross-check."},
		{DocumentID: "new", Text: "Security policy (updated): Export the last 12 months of access logs for the initial keycard cross-check. An earlier note suggested 18 months, but the corrected window is 12 months after telemetry review."},
	}
	ans := "Export access logs for the past 6 months."
	out, diag := rebindAnswerQuantities(q, ans, ps, "conflicting_info", []string{"old", "new"})
	if !strings.Contains(out, "12 months") {
		t.Fatalf("expected rebind to 12 months, out=%q diag=%v", out, diag)
	}
	if strings.Contains(out, "6 months") {
		t.Fatalf("old duration should be replaced, out=%q", out)
	}
}

func TestRebindAnswerQuantitiesMoneyCatalog(t *testing.T) {
	q := "What egress cost rate does the cost penalty catalog use for cross-region traffic?"
	ps := []Passage{
		{DocumentID: "weak", Text: "Egress looks elevated; rough estimate around $0.02 per request observed in logs."},
		{DocumentID: "cat", Text: "Cost penalty catalog (EXP-002): use GiB-based billing; catalog rate is +$0.085 per GiB after correction to provider bill attribution."},
	}
	ans := "The catalog uses about $0.02 per request."
	out, diag := rebindAnswerQuantities(q, ans, ps, "conflicting_info", []string{"weak", "cat"})
	if !strings.Contains(strings.ToLower(out), "0.085") {
		t.Fatalf("expected catalog money atom, out=%q diag=%v", out, diag)
	}
}

func TestPrefersSupersedingEvidence(t *testing.T) {
	// Bare "A or B" is too broad for SUPERSEDING pack rewrite (audit #4).
	if prefersSupersedingEvidence("was it A or B?", "basic") {
		t.Fatal("bare A or B must not prefer superseding")
	}
	if !prefersSupersedingEvidence("which is correct after the note supersedes the earlier draft?", "basic") {
		t.Fatal("explicit supersede language should prefer")
	}
	if !prefersSupersedingEvidence("plain", "conflicting_info") {
		t.Fatal("conflicting_info type")
	}
	if prefersSupersedingEvidence("What is the RPO?", "basic") {
		t.Fatal("plain basic should not force supersede")
	}
	if !prefersSupersedingEvidence("How many days of bereavement leave does current policy provide?", "basic") {
		t.Fatal("leave/current policy should prefer superseding day-count")
	}
	if wantsGlobalNewestMark("How many days until deploy?", "") {
		t.Fatal("quantity ask must not want global newest mark")
	}
}

func TestGroundAnswerInPassagesQuantity(t *testing.T) {
	q := "How many months of logs for the keycard audit export?"
	ps := []Passage{
		{DocumentID: "a", Text: "Export past 6 months (draft)."},
		{DocumentID: "b", Text: "Corrected policy after review: last 12 months of access logs for keycard audit."},
	}
	g := groundAnswerInPassages(q, "Export the past 6 months of logs.", []string{"a", "b"}, nil, ps, "conflicting_info")
	if !strings.Contains(g.Answer, "12 months") {
		t.Fatalf("ground path should rebind duration, got %q diag=%v", g.Answer, g.Diagnostics)
	}
}

func TestMergeChecklistSteps(t *testing.T) {
	q := "What steps were listed in the triage checklist?"
	ps := []Passage{
		{DocumentID: "d", Text: "The triage checklist was: (1) suspend partner service account keys, (2) revoke partner Vault tokens using the -prefix partner- option, (3) remove the decrypt IAM binding for the bucket."},
	}
	// Thin/wrong answer missing vault revoke.
	ans := "1) snapshot KMS logs 2) call security."
	out, diag := mergeChecklistStepsIntoAnswer(q, ans, ps)
	if !strings.Contains(strings.ToLower(out), "vault") && !strings.Contains(strings.ToLower(out), "suspend") {
		t.Fatalf("expected pack checklist steps merged, out=%q diag=%v", out, diag)
	}
	if diag["checklist_steps_merged"] == nil && diag["checklist_steps_covered"] == nil {
		t.Fatalf("expected checklist diag, got %v", diag)
	}
}

func TestSeeksChecklistNotBareWhatWasThe(t *testing.T) {
	if seeksChecklist("What was the default pass rate for safest numeric mode?") {
		t.Fatal("factoid must not match checklist")
	}
	if seeksChecklist("What was the RPO for gold tier?") {
		t.Fatal("RPO factoid must not match checklist")
	}
	if !seeksChecklist("What steps were listed in the triage checklist?") {
		t.Fatal("true checklist should match")
	}
}

func TestNoMoneyInjectOnPassRate(t *testing.T) {
	q := "What is the default pass rate used when a machine steps down from safest numeric mode?"
	ps := []Passage{
		{DocumentID: "a", Text: "Default pass rate for step-down is 0.995 after calibration."},
		{DocumentID: "b", Text: "Unrelated billing: catalog rate is +$0.085 per GiB for egress."},
	}
	ans := "The default pass rate is 0.995."
	out, diag := rebindAnswerQuantities(q, ans, ps, "semantic", []string{"a", "b"})
	if strings.Contains(out, "$0.085") || strings.Contains(out, "Rate established") {
		t.Fatalf("must not inject money into pass-rate answer: out=%q diag=%v", out, diag)
	}
	if seeksAtomicQuantity(q) {
		t.Fatal("pass rate question must not open quantity inject path")
	}
	if seeksMoneyQuantity(q) {
		t.Fatal("pass rate is not money surface")
	}
}

func TestConflictInitiallyPrefersLatestCorrection(t *testing.T) {
	q := "Was the node initially an OOM or driver stalls after telemetry review?"
	// Surface must not force earliest solely from "initially".
	if temporalDatePreference(q) == "earliest" {
		t.Fatalf("initially conflict framing must not force earliest, pref=%q", temporalDatePreference(q))
	}
	pref := applyCorrectionDatePolicy(q, "conflicting_info", temporalDatePreference(q))
	if pref != "latest" {
		t.Fatalf("conflict+initially should prefer latest, got %q", pref)
	}
	// Explicit first-event still earliest under conflict.
	first := "What date did procurement first communicate the company-wide spending freeze?"
	if applyCorrectionDatePolicy(first, "conflicting_info", temporalDatePreference(first)) != "earliest" &&
		temporalDatePreference(first) != "earliest" {
		t.Fatalf("first communicate should be earliest intent")
	}
}

func TestClipPassageTextKeepsTailDate(t *testing.T) {
	// Head filler + late freeze date past a 400-char budget head.
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("filler pad sentence about unrelated ops process. ")
	}
	b.WriteString("Timeline: 2026-01-20 Procurement company-wide budget freeze reported. ")
	b.WriteString("Later 2026-02-10 pause until Q3.")
	full := b.String()
	if len(full) < 500 {
		t.Fatalf("setup want long text, got %d", len(full))
	}
	// Budget forces clip; date must still appear.
	out := clipPassageText(full, 600)
	if !strings.Contains(out, "2026-01-20") {
		t.Fatalf("clip must keep tail ISO date, out_len=%d out=%q", len(out), out[:min(200, len(out))])
	}
	if len(out) > 600 {
		t.Fatalf("clip exceeded budget: %d", len(out))
	}
}

func TestRecoverQuoteFromMissingFields(t *testing.T) {
	// Model omits quote → missing_fields; recover from evidence.
	ps := []Passage{
		{DocumentID: "d1", Text: "The default RPO is 15 minutes for MedThink active datasets on the EU primary."},
	}
	claims := []Claim{
		{Text: "RPO is 15 minutes for MedThink", Quote: "", DocumentID: "d1"},
	}
	g := groundCompletion("RPO is 15 minutes.", []string{"d1"}, claims, ps, "basic")
	if g.Diagnostics["grounding_status"] != "ok" {
		t.Fatalf("expected recovered claim ok, diag=%v", g.Diagnostics)
	}
	if n, _ := g.Diagnostics["claims_quote_recovered"].(int); n < 1 {
		// map[string]any may store int
		if g.Diagnostics["claims_quote_recovered"] == nil {
			t.Fatalf("expected claims_quote_recovered, diag=%v claims=%v", g.Diagnostics, g.Claims)
		}
	}
	if len(g.Claims) == 0 {
		t.Fatalf("expected supported claim after quote recover, diag=%v", g.Diagnostics)
	}
}

func TestGroundQuoteAndInfoNotFound(t *testing.T) {
	passages := []Passage{
		{DocumentID: "dsid_a", Text: "The default RPO is 15 minutes for MedThink active datasets."},
		{DocumentID: "dsid_b", Text: "Unrelated billing note about invoices."},
	}
	claims := []Claim{
		{Text: "RPO is 15 minutes", Quote: "15 minutes", DocumentID: "dsid_a"},
		{Text: "bogus", Quote: "not in any document at all xyzzy", DocumentID: "dsid_b"},
	}
	g := groundCompletion(
		"MedThink RPO is 15 minutes.",
		[]string{"dsid_a", "dsid_b", "dsid_fake"},
		claims,
		passages,
		"basic",
	)
	if len(g.CitedDocumentIDs) != 1 || g.CitedDocumentIDs[0] != "dsid_a" {
		t.Fatalf("cites=%v", g.CitedDocumentIDs)
	}
	if len(g.Claims) != 1 {
		t.Fatalf("claims=%d", len(g.Claims))
	}
	ans := forceInfoNotFoundAbstention("Some invented surcharge of $40.")
	if !looksLikeAbstention(ans) {
		t.Fatalf("expected abstention language: %s", ans)
	}
}

func TestGroundNormalizesRaptorSummaryCites(t *testing.T) {
	// Models often emit bare rap-L0-0 while passages use summary:rap-L0-0.
	passages := []Passage{
		{DocumentID: "policy", Text: "The recovery point objective (RPO) is one day."},
		{DocumentID: "summary:rap-L0-0", Text: "RPO one day Kyoto DR."},
	}
	g := groundCompletion(
		"RPO is one day.",
		[]string{"policy", "rap-L0-0", "rap-L1-root"},
		nil,
		passages,
		"basic",
	)
	illegal, _ := g.Diagnostics["illegal_citations"].([]string)
	for _, id := range illegal {
		if id == "rap-L0-0" {
			t.Fatalf("rap-L0-0 should normalize to summary cite, illegal=%v", illegal)
		}
	}
	// allowed cites should include policy and/or summary:rap-L0-0, not bare illegal root
	foundPolicy := false
	for _, c := range g.CitedDocumentIDs {
		if c == "policy" || c == "summary:rap-L0-0" {
			foundPolicy = true
		}
		if c == "rap-L1-root" {
			t.Fatalf("bare illegal root should not appear in cites: %v", g.CitedDocumentIDs)
		}
	}
	if !foundPolicy {
		t.Fatalf("cites=%v illegal=%v", g.CitedDocumentIDs, illegal)
	}
}

func TestGroundNoSupportedClaimsKeepsCappedCites(t *testing.T) {
	// Claim-quotes can all fail validation while the model still named valid
	// passage docs. Official ERB recall uses cited_document_ids — keep a hard
	// cap of allowed cites rather than zeroing the list.
	passages := []Passage{{DocumentID: "dsid_a", Text: "hello world only"}}
	g := groundCompletion(
		"answer",
		[]string{"dsid_a"},
		[]Claim{{Text: "x", Quote: "not present quote text", DocumentID: "dsid_a"}},
		passages,
		"basic",
	)
	if len(g.CitedDocumentIDs) != 1 || g.CitedDocumentIDs[0] != "dsid_a" {
		t.Fatalf("expected fallback cite dsid_a, got %v status=%v", g.CitedDocumentIDs, g.Diagnostics)
	}
	if g.Diagnostics["grounding_status"] != "no_supported_claims" {
		t.Fatalf("status=%v", g.Diagnostics["grounding_status"])
	}
}

func TestKeysOfDeterministicSorted(t *testing.T) {
	m := map[string]struct{}{"dsid_c": {}, "dsid_a": {}, "dsid_b": {}, "dsid_a2": {}}
	first := keysOf(m)
	want := []string{"dsid_a", "dsid_a2", "dsid_b", "dsid_c"}
	if len(first) != len(want) {
		t.Fatalf("len=%d want %d", len(first), len(want))
	}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("keysOf not sorted: %v", first)
		}
	}
	// Repeated calls must be identical (map iteration order is random).
	for range 20 {
		got := keysOf(m)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("nondeterministic keysOf: %v vs %v", got, want)
			}
		}
	}
}

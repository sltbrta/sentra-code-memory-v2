package hosted

import (
	"strings"
	"testing"
)

// Red-proof fixtures for issue #258: type-aware exhaustive slot filling for
// project_related/completeness, qualifier/conflict-aware answer conversion
// (compatible with #270 reranking), strict leaf-span citations, and
// unsupported-extra rejection. All fixtures are pattern-driven; no gold
// labels, no question IDs.

func TestSlotFillCompletenessMultiSlot(t *testing.T) {
	q := "Which customers have dedicated support channels and what are their escalation thresholds?"
	ps := []Passage{
		{DocumentID: "confluence-sop", Text: "Support escalation SOP (updated):\n- Acme Corp: escalate after 4 hours\n- Globex Industries: escalate after 2 hours"},
		{DocumentID: "slack-update", Text: "Escalation thresholds per updated contracts:\n- Initech: escalation threshold is 6 hours per updated contract"},
	}
	g := Grounded{
		Answer:           "Acme Corp escalates after 4 hours.",
		CitedDocumentIDs: []string{"confluence-sop"},
	}
	out := slotFillAnswer(q, "completeness", g, ps, nil)
	if !strings.Contains(out.Answer, "Globex Industries") {
		t.Fatalf("missing uncovered customer Globex Industries, answer=%q", out.Answer)
	}
	if !strings.Contains(out.Answer, "Initech") {
		t.Fatalf("missing uncovered customer Initech, answer=%q", out.Answer)
	}
	if !strings.Contains(out.Answer, "2 hours") || !strings.Contains(out.Answer, "6 hours") {
		t.Fatalf("missing uncovered thresholds, answer=%q", out.Answer)
	}
	// Covered entity must not be duplicated in the appended section.
	appended := out.Answer[len(g.Answer):]
	if strings.Contains(appended, "Acme Corp") {
		t.Fatalf("covered entity re-appended, appended=%q", appended)
	}
	// Strict leaf-span citations: every appended claim quote is a verbatim
	// substring of its cited leaf document.
	pack := map[string]string{}
	for _, p := range ps {
		pack[p.DocumentID] = p.Text
	}
	added := 0
	for _, c := range out.Claims {
		body, ok := pack[c.DocumentID]
		if !ok {
			t.Fatalf("claim cites non-pack doc %q", c.DocumentID)
		}
		if !strings.Contains(body, c.Quote) {
			t.Fatalf("claim quote not a verbatim leaf span: doc=%q quote=%q", c.DocumentID, c.Quote)
		}
		added++
	}
	if added < 2 {
		t.Fatalf("expected ≥2 slot claims, got %d (%v)", added, out.Claims)
	}
	if !containsString(out.CitedDocumentIDs, "slack-update") {
		t.Fatalf("filled doc must join cites, cites=%v", out.CitedDocumentIDs)
	}
	if out.Diagnostics["slot_fill_added"] == nil {
		t.Fatalf("expected slot_fill diagnostics, got %v", out.Diagnostics)
	}
}

func TestSlotFillProjectSynthesisSlots(t *testing.T) {
	q := "What is the current status of the Helios migration, who owns it, and what are the latency targets?"
	ps := []Passage{
		{DocumentID: "wiki-helios", Text: "Project Helios migration\nOwner: Priya Nair\nStatus: at risk\nSLO: p99 250ms for the read path"},
		{DocumentID: "jira-helios", Text: "HELIOS-42 tracks the cutover. Timeline: cutover completes 2026-03-15. Target: TTM 30 days for tenant onboarding"},
	}
	g := Grounded{
		Answer:           "The Helios migration is at risk.",
		CitedDocumentIDs: []string{"wiki-helios"},
	}
	out := slotFillAnswer(q, "project_related", g, ps, nil)
	for _, want := range []string{"Priya Nair", "250ms", "2026-03-15", "30 days"} {
		if !strings.Contains(out.Answer, want) {
			t.Fatalf("missing slot %q, answer=%q", want, out.Answer)
		}
	}
	// Status already covered — never duplicate it in the appended section.
	appended := out.Answer[len(g.Answer):]
	if strings.Contains(appended, "at risk") {
		t.Fatalf("covered status re-appended, appended=%q", appended)
	}
	if !containsString(out.CitedDocumentIDs, "jira-helios") {
		t.Fatalf("cross-doc slot doc must join cites, cites=%v", out.CitedDocumentIDs)
	}
}

func TestSlotFillConstrainedQualifierFilter(t *testing.T) {
	q := "For gold tier tenants, what is the request rate limit?"
	ps := []Passage{
		{DocumentID: "limits-gold", Text: "Tenant rate limits:\n- Gold tier: 1000 requests/minute per tenant"},
		{DocumentID: "limits-silver", Text: "Legacy limits sheet:\n- Silver tier: 250 requests/minute per tenant"},
	}
	g := Grounded{
		Answer:           "Gold tier tenants have a per-tenant request rate limit.",
		CitedDocumentIDs: []string{"limits-gold"},
	}
	out := slotFillAnswer(q, "constrained", g, ps, nil)
	if !strings.Contains(out.Answer, "1000") {
		t.Fatalf("on-qualifier gold limit must be filled, answer=%q", out.Answer)
	}
	if strings.Contains(out.Answer, "250") {
		t.Fatalf("off-qualifier silver limit must be rejected, answer=%q", out.Answer)
	}
	if containsString(out.CitedDocumentIDs, "limits-silver") {
		t.Fatalf("off-qualifier doc must not join cites, cites=%v", out.CitedDocumentIDs)
	}
	if out.Diagnostics["slot_fill_qualifier_dropped"] == nil {
		t.Fatalf("expected qualifier-drop diagnostic, got %v", out.Diagnostics)
	}
}

func TestSlotFillConflictPrefersCurrentValue(t *testing.T) {
	q := "What is the current data retention period for audit logs after the policy update?"
	ps := []Passage{
		{DocumentID: "policy-old", Text: "Retention: 90 days for audit logs. [SUPERSEDED by 2026 policy update]"},
		{DocumentID: "policy-new", Text: "[SUPERSEDING 2026-02-01] Retention: 180 days for audit logs."},
	}
	g := Grounded{
		Answer:           "Audit logs are retained per company policy.",
		CitedDocumentIDs: []string{"policy-new"},
	}
	out := slotFillAnswer(q, "conflicting_info", g, ps, nil)
	if !strings.Contains(out.Answer, "180 days") {
		t.Fatalf("current retention must be filled, answer=%q", out.Answer)
	}
	if strings.Contains(out.Answer, "90 days") {
		t.Fatalf("superseded retention must be rejected, answer=%q", out.Answer)
	}
	if containsString(out.CitedDocumentIDs, "policy-old") {
		t.Fatalf("superseded doc must not join cites via slot fill, cites=%v", out.CitedDocumentIDs)
	}
	if out.Diagnostics["slot_fill_conflict_dropped"] == nil {
		t.Fatalf("expected conflict-drop diagnostic, got %v", out.Diagnostics)
	}
}

func TestSlotFillSkipsAbstainAndUnsupportedTypes(t *testing.T) {
	ps := []Passage{
		{DocumentID: "confluence-sop", Text: "- Globex Industries: escalate after 2 hours"},
	}
	g := Grounded{Answer: "The supplied documents do not establish the answer."}
	out := slotFillAnswer("Which customers have thresholds?", "completeness", g, ps, nil)
	if out.Answer != g.Answer {
		t.Fatalf("abstain answers must be untouched, got %q", out.Answer)
	}
	g2 := Grounded{Answer: "A plain semantic answer."}
	out2 := slotFillAnswer("When did the freeze start?", "semantic", g2, ps, nil)
	if out2.Answer != g2.Answer {
		t.Fatalf("semantic type must not get slot bullets, got %q", out2.Answer)
	}
}

func TestSlotFillKillSwitch(t *testing.T) {
	t.Setenv("OUROBOROS_ERB_SLOT_FILL", "0")
	ps := []Passage{
		{DocumentID: "confluence-sop", Text: "- Globex Industries: escalate after 2 hours"},
	}
	g := Grounded{Answer: "Acme Corp escalates after 4 hours."}
	out := slotFillAnswer("Which customers have thresholds?", "completeness", g, ps, nil)
	if out.Answer != g.Answer {
		t.Fatalf("kill switch must disable slot fill, got %q", out.Answer)
	}
}

func TestRejectUnsupportedExtrasDropsInventedSentence(t *testing.T) {
	ps := []Passage{
		{DocumentID: "policy", Text: "Export the last 12 months of access logs. Retention is 180 days for audit logs."},
	}
	answer := "The export window is 12 months.\nThe surcharge is $9.99 per GiB.\nRetention is 180 days."
	out, diag := rejectUnsupportedExtras(answer, ps, "basic")
	if strings.Contains(out, "$9.99") {
		t.Fatalf("unsupported money sentence must be rejected, out=%q", out)
	}
	if !strings.Contains(out, "12 months") || !strings.Contains(out, "180 days") {
		t.Fatalf("supported sentences must survive, out=%q", out)
	}
	if diag["unsupported_extras_rejected"] != 1 {
		t.Fatalf("expected one rejection, diag=%v", diag)
	}
}

func TestRejectUnsupportedExtrasDropsPlaceholderLine(t *testing.T) {
	ps := []Passage{
		{DocumentID: "chat", Text: "The deploy freeze affected all production deploys last week."},
	}
	answer := "The freeze began on [date not in retrieved documents].\nThe freeze affected all production deploys."
	out, diag := rejectUnsupportedExtras(answer, ps, "semantic")
	if strings.Contains(out, "not in retrieved documents") {
		t.Fatalf("placeholder line must be rejected, out=%q", out)
	}
	if !strings.Contains(out, "deploys") {
		t.Fatalf("atomless supported sentence must survive, out=%q", out)
	}
	if diag["unsupported_extras_rejected"] != 1 {
		t.Fatalf("expected one rejection, diag=%v", diag)
	}
}

func TestRejectUnsupportedExtrasKeepsGroundedAnswer(t *testing.T) {
	ps := []Passage{
		{DocumentID: "policy", Text: "Export the last 12 months of access logs for the keycard audit."},
	}
	answer := "Export the last 12 months of access logs for the keycard audit."
	out, diag := rejectUnsupportedExtras(answer, ps, "basic")
	if out != answer {
		t.Fatalf("fully grounded answer must be unchanged, out=%q", out)
	}
	if diag["unsupported_extras_rejected"] != 0 {
		t.Fatalf("no rejections expected, diag=%v", diag)
	}
}

func TestRejectUnsupportedExtrasNeverEmptiesAnswer(t *testing.T) {
	ps := []Passage{
		{DocumentID: "policy", Text: "Export the last 12 months of access logs."},
	}
	answer := "The surcharge is $9.99 per GiB."
	out, diag := rejectUnsupportedExtras(answer, ps, "basic")
	if strings.TrimSpace(out) == "" {
		t.Fatalf("rejection must never empty the answer, diag=%v", diag)
	}
}

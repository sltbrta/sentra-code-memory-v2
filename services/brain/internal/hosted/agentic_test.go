package hosted

import (
	"context"
	"testing"
)

func TestAgenticKillSwitchHonorsExplicitDisable(t *testing.T) {
	// QUALITY profile with OUROBOROS_BRAIN_AGENTIC=0: prod.Agentic must be
	// false and the kill switch must suppress every escalation.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	prod := prodProfileFromEnv()
	if !prod.Quality {
		t.Fatal("expected QUALITY profile")
	}
	if prod.Agentic {
		t.Fatal("QUALITY + OUROBOROS_BRAIN_AGENTIC=0 must yield prod.Agentic=false")
	}
	diag := map[string]any{}
	on, exh := applyAgenticKillSwitch(prod, true, true, diag)
	if on || exh {
		t.Fatalf("kill switch must disable agentic+exhaustive, got %v %v", on, exh)
	}
	if diag["agentic_disabled_env"] != true {
		t.Fatalf("kill switch must stamp diag, got %v", diag)
	}
	// Default QUALITY (agentic on): pass-through.
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "")
	prod2 := prodProfileFromEnv()
	if !prod2.Agentic {
		t.Fatal("QUALITY default must keep agentic enabled")
	}
	diag2 := map[string]any{}
	on2, exh2 := applyAgenticKillSwitch(prod2, true, false, diag2)
	if !on2 || exh2 {
		t.Fatalf("enabled profile must pass through, got %v %v", on2, exh2)
	}
	if len(diag2) != 0 {
		t.Fatalf("pass-through must not stamp diag, got %v", diag2)
	}
	// Kill switch on already-off inputs stays silent.
	diag3 := map[string]any{}
	on3, exh3 := applyAgenticKillSwitch(prod, false, false, diag3)
	if on3 || exh3 || len(diag3) != 0 {
		t.Fatalf("already-off must stay silent: %v %v %v", on3, exh3, diag3)
	}
}

func TestAgenticExpandMarksTools(t *testing.T) {
	// Default lean: multi-doc agentic is OFF unless QUALITY / deep / explicit env.
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_MODE", "lean")
	t.Setenv("OUROBOROS_BRAIN_AGENTIC", "0")
	for _, qt := range []string{"project_related", "completeness", "conflicting_info", "semantic"} {
		if wantsAgentic(qt) {
			t.Fatalf("wantsAgentic(%q) = true, want false under lean default", qt)
		}
	}
	// QUALITY turns multi-doc agentic on (incl. constrained); basic stays off.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	for _, qt := range []string{"project_related", "completeness", "conflicting_info", "semantic", "constrained"} {
		if !wantsAgentic(qt) {
			t.Fatalf("wantsAgentic(%q) = false, want true under QUALITY", qt)
		}
	}
	if wantsAgentic("basic") {
		t.Fatal("basic must never be agentic under QUALITY")
	}
	// deep mode also agentic for constrained (game-day multi-doc).
	t.Setenv("OUROBOROS_ERB_QUALITY", "0")
	t.Setenv("OUROBOROS_ERB_MODE", "deep")
	if !wantsAgentic("constrained") {
		t.Fatal("deep mode must agentic-expand constrained")
	}
	if !isMultiDocType("constrained") {
		t.Fatal("constrained is multi-doc on ERB")
	}
	// gradeEvidence insufficient on empty window
	grade := gradeEvidence("What is the RPO for MedThink?", "project_related", nil)
	if grade["sufficient"] != false {
		t.Fatalf("empty window should be insufficient: %#v", grade)
	}
	if grade["need_multi"] != true {
		t.Fatalf("project_related need_multi: %#v", grade)
	}
	if n, _ := grade["passage_count"].(int); n != 0 {
		t.Fatalf("passage_count want 0 got %v", grade["passage_count"])
	}

	// non-empty but single-doc multi type → still insufficient (needs ≥3 docs + tight gap)
	single := []Passage{{DocumentID: "dsid_a", Text: "RPO is 15 minutes for MedThink", ChunkID: "c1"}}
	g2 := gradeEvidence("What is the RPO for MedThink failover?", "project_related", single)
	if g2["sufficient"] != false {
		t.Fatalf("single-doc multi type should be insufficient: %#v", g2)
	}
	// Flat multi-doc pack with weak lexical overlap must not look sufficient.
	flat := []Passage{
		{DocumentID: "d1", Text: "unrelated alpha notes", ChunkID: "c1"},
		{DocumentID: "d2", Text: "unrelated beta notes", ChunkID: "c2"},
		{DocumentID: "d3", Text: "unrelated gamma notes", ChunkID: "c3"},
	}
	g3 := gradeEvidence("What is the MedThink RPO for gold tier failover?", "project_related", flat)
	if g3["sufficient"] != false {
		t.Fatalf("weak multi-doc pack should be insufficient: %#v", g3)
	}

	// mergePassages caps total when max binds extras
	base := []Passage{
		{DocumentID: "a", Text: "alpha text body", ChunkID: "a1"},
	}
	extra := []Passage{
		{DocumentID: "b", Text: "beta text body", ChunkID: "b1"},
		{DocumentID: "c", Text: "gamma text body", ChunkID: "c1"},
		{DocumentID: "d", Text: "delta text body", ChunkID: "d1"},
	}
	merged := mergePassages(base, extra, 2)
	if len(merged) != 2 {
		t.Fatalf("mergePassages cap: want 2 got %d %#v", len(merged), merged)
	}
	// base always kept; first extra fills to cap
	if merged[0].DocumentID != "a" {
		t.Fatalf("base first: %#v", merged)
	}
	// dedupe by chunk id
	dup := mergePassages(base, []Passage{{DocumentID: "a", Text: "alpha text body", ChunkID: "a1"}}, 10)
	if len(dup) != 1 {
		t.Fatalf("dedupe failed: %#v", dup)
	}
}

func TestAgenticExpandForceExpandOneRound(t *testing.T) {
	// ForceExpand must ensure at least one reformulate round even when
	// the seed pack is dense and gradeEvidence says sufficient — the
	// low-confidence CE signal means gold may be missing despite many seeds.
	t.Setenv("OUROBOROS_ERB_QUALITY", "1")
	c := OpenMemory("force-expand-round")
	defer c.Close()

	// Dense seed pack (7 unique docs) with good lexical overlap: gradeEvidence
	// will call this sufficient and the local forceOne heuristic would skip.
	window := []Passage{
		{DocumentID: "d1", Text: "MedThink RPO is fifteen minutes for gold tier failover", ChunkID: "c1"},
		{DocumentID: "d2", Text: "MedThink RTO is four hours for active datasets", ChunkID: "c2"},
		{DocumentID: "d3", Text: "MedThink failover policy covers all production tiers", ChunkID: "c3"},
		{DocumentID: "d4", Text: "MedThink backup schedule runs daily at 2am UTC", ChunkID: "c4"},
		{DocumentID: "d5", Text: "MedThink DR plan tested quarterly for compliance", ChunkID: "c5"},
		{DocumentID: "d6", Text: "MedThink security audit findings Q3 2025", ChunkID: "c6"},
		{DocumentID: "d7", Text: "MedThink oncall rotation schedule for SRE team", ChunkID: "c7"},
	}

	plan := QueryPlan{MultiDoc: true}
	out, diag := c.agenticExpand(context.Background(),
		"What is the MedThink RPO for gold tier failover?",
		"project_related",
		window,
		AgenticOptions{Enabled: true, MaxRounds: 1, MaxExtraDocs: 4, ForceExpand: true, Plan: &plan},
	)

	if diag["agentic"] != true {
		t.Fatalf("ForceExpand must enable agentic: %#v", diag)
	}
	if diag["force_expand"] != true {
		t.Fatalf("ForceExpand must set force_expand: %#v", diag)
	}
	r, _ := diag["rounds"].(int)
	if r < 1 {
		t.Fatalf("ForceExpand must run at least 1 round, got %d: %#v", r, diag)
	}
	// Output must include at least the original window.
	if len(out) < len(window) {
		t.Fatalf("output trimmed below input: %d < %d", len(out), len(window))
	}
}

func TestAgenticExpandForceExpandHonorsEnabledFalse(t *testing.T) {
	// Enabled=false must remain a hard off even when ForceExpand is true.
	c := OpenMemory("force-expand-disabled")
	defer c.Close()

	window := []Passage{
		{DocumentID: "d1", Text: "MedThink RPO is fifteen minutes for gold tier failover", ChunkID: "c1"},
		{DocumentID: "d2", Text: "MedThink RTO is four hours for active datasets", ChunkID: "c2"},
		{DocumentID: "d3", Text: "MedThink failover policy covers all production tiers", ChunkID: "c3"},
		{DocumentID: "d4", Text: "MedThink backup schedule runs daily at 2am UTC", ChunkID: "c4"},
		{DocumentID: "d5", Text: "MedThink DR plan tested quarterly for compliance", ChunkID: "c5"},
		{DocumentID: "d6", Text: "MedThink security audit findings Q3 2025", ChunkID: "c6"},
		{DocumentID: "d7", Text: "MedThink oncall rotation schedule for SRE team", ChunkID: "c7"},
	}

	plan := QueryPlan{MultiDoc: true}
	out, diag := c.agenticExpand(context.Background(),
		"What is the MedThink RPO for gold tier failover?",
		"project_related",
		window,
		AgenticOptions{Enabled: false, MaxRounds: 1, MaxExtraDocs: 4, ForceExpand: true, Plan: &plan},
	)

	if diag["agentic"] != false {
		t.Fatalf("Enabled=false must hard-off agentic even with ForceExpand: %#v", diag)
	}
	if diag["rounds"] != 0 {
		t.Fatalf("Enabled=false must leave rounds at 0: %#v", diag)
	}
	if len(out) != len(window) || out[0].DocumentID != window[0].DocumentID {
		t.Fatalf("Enabled=false must return window unchanged: %#v", out)
	}
}

func TestConfidenceTopMean3IgnoresBestLastOrder(t *testing.T) {
	top, mean3 := confidenceTopMean3([]float64{0.25, 0.45, 0.60})
	if top != 0.60 || mean3 < 0.4333 || mean3 > 0.4334 {
		t.Fatalf("top/mean3=%v/%v, want 0.60/0.4333", top, mean3)
	}
}

func TestShouldSignalAgenticLowConfidence(t *testing.T) {
	// Top below 0.50 must trigger low_confidence.
	ok, why := shouldSignalAgentic("What is the MedThink RPO for gold tier failover?",
		[]float64{0.45, 0.30, 0.25})
	if !ok || why != "low_confidence" {
		t.Fatalf("want low_confidence on weak top, got ok=%v why=%q", ok, why)
	}
	// With an explicit higher threshold override, moderate top scores still
	// trigger even when the mean is otherwise above the floor.
	t.Setenv("OUROBOROS_ERB_CONF_TOP", "0.65")
	ok, why = shouldSignalAgentic("What is the metric for finalized streams?",
		[]float64{0.60, 0.56, 0.52})
	t.Setenv("OUROBOROS_ERB_CONF_TOP", "")
	if !ok || why != "low_confidence" {
		t.Fatalf("want low_confidence on moderate top, got ok=%v why=%q", ok, why)
	}
	// Top above the threshold but mean3 below 0.35 must still trigger.
	ok, why = shouldSignalAgentic("What is the MedThink failover policy?",
		[]float64{0.70, 0.20, 0.10})
	if !ok || why != "low_confidence" {
		t.Fatalf("want low_confidence on weak mean3, got ok=%v why=%q", ok, why)
	}
	// Both above thresholds must not trigger.
	ok, _ = shouldSignalAgentic("What is the MedThink failover policy?",
		[]float64{0.85, 0.55, 0.40})
	if ok {
		t.Fatal("high conf non-agg must not escalate")
	}
	// With raised thresholds (matching production overrides), scores like
	// top≈0.587 mean3≈0.556 that are above defaults but below prod thresholds
	// must also trigger low_confidence.
	t.Setenv("OUROBOROS_ERB_CONF_TOP", "0.60")
	t.Setenv("OUROBOROS_ERB_CONF_MEAN3", "0.58")
	ok, why = shouldSignalAgentic("What is the MedThink RPO for gold tier failover?",
		[]float64{0.587, 0.556, 0.525})
	if !ok || why != "low_confidence" {
		t.Fatalf("want low_confidence under prod thresholds, got ok=%v why=%q", ok, why)
	}
}

func TestAgenticSignalActionDenseLowConfidence(t *testing.T) {
	escalate, force, reason, suppressed := agenticSignalAction(12, "low_confidence")
	if !escalate || !force || reason != "forced_low_confidence" || suppressed != "" {
		t.Fatalf("dense low-confidence action = %v %v %q %q", escalate, force, reason, suppressed)
	}

	escalate, force, reason, suppressed = agenticSignalAction(12, "aggregation_heuristic")
	if !escalate || force || reason != "aggregation_heuristic" || suppressed != "" {
		t.Fatalf("aggregation action changed = %v %v %q %q", escalate, force, reason, suppressed)
	}

	escalate, force, reason, suppressed = agenticSignalAction(12, "forced_all")
	if !escalate || force || reason != "forced_all" || suppressed != "" {
		t.Fatalf("forced-all action changed = %v %v %q %q", escalate, force, reason, suppressed)
	}

	escalate, force, reason, suppressed = agenticSignalAction(12, "other")
	if escalate || force || reason != "" || suppressed != "seed_dense_other" {
		t.Fatalf("dense unknown action = %v %v %q %q", escalate, force, reason, suppressed)
	}

	escalate, force, reason, suppressed = agenticSignalAction(11, "low_confidence")
	if !escalate || force || reason != "low_confidence" || suppressed != "" {
		t.Fatalf("non-dense low-confidence action = %v %v %q %q", escalate, force, reason, suppressed)
	}
}

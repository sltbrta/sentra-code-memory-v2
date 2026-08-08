package hosted

import (
	"strings"
	"testing"
)

func TestInferQueryPlanConflict(t *testing.T) {
	p := InferQueryPlan("Was the degraded GPU node an OOM or intermittent driver launch stalls?")
	if !p.Conflict {
		t.Fatalf("expected conflict, plan=%+v", p)
	}
	if p.EffectiveType != "conflicting_info" {
		t.Fatalf("effective=%s", p.EffectiveType)
	}
	if !p.MultiDoc || !p.Agentic {
		t.Fatalf("conflict should multi-doc+agentic, plan=%+v", p)
	}
}

func TestInferQueryPlanCompleteness(t *testing.T) {
	p := InferQueryPlan("Which customers have been granted an exception to log retention, and what period for each?")
	if !p.Completeness {
		t.Fatalf("expected completeness, plan=%+v", p)
	}
	if p.EffectiveType != "completeness" {
		t.Fatalf("effective=%s", p.EffectiveType)
	}
}

func TestInferQueryPlanTemporalFreeze(t *testing.T) {
	p := InferQueryPlan("What date did procurement first communicate the company-wide spending freeze?")
	if !p.Temporal || !p.AtomicFact {
		t.Fatalf("expected temporal atomic, plan=%+v", p)
	}
	// Spending freeze cues deep hydrate.
	if !p.DeepHydrate {
		t.Fatalf("expected deep hydrate for freeze timeline, plan=%+v", p)
	}
}

func TestInferQueryPlanChecklist(t *testing.T) {
	p := InferQueryPlan("What steps were listed in the triage checklist for the partner vault incident?")
	if !p.Checklist {
		t.Fatalf("expected checklist, plan=%+v", p)
	}
}

func TestInferQueryPlanSemanticLong(t *testing.T) {
	p := InferQueryPlan("In the recent change that made low bit math safer for inference, what is the default pass rate used to decide when a machine is allowed to step down from the safest numeric mode?")
	if p.EffectiveType != "semantic" && !p.SemanticExpand {
		t.Fatalf("long paraphrase should semantic-expand, plan=%+v", p)
	}
}

func TestResolveQuestionTypeLabeledWins(t *testing.T) {
	typ, plan := ResolveQuestionType("What date was the freeze?", "basic")
	if typ != "basic" {
		t.Fatalf("labeled basic must win, got %s", typ)
	}
	if plan.Source != "labeled" {
		t.Fatalf("source=%s", plan.Source)
	}
	// Surface temporal still recorded.
	if !plan.Temporal {
		t.Fatalf("still want temporal flag from surface, plan=%+v", plan)
	}
}

func TestResolveQuestionTypeInferredWhenEmpty(t *testing.T) {
	typ, plan := ResolveQuestionType(
		"Was INC-9821 an OOM or driver stalls after telemetry review?",
		"",
	)
	if typ != "conflicting_info" {
		t.Fatalf("inferred type=%s plan=%+v", typ, plan)
	}
	if plan.Source != "inferred" {
		t.Fatalf("source=%s", plan.Source)
	}
	if !strings.Contains(strings.ToLower(typ), "conflict") {
		t.Fatalf("want conflict type")
	}
}

func TestApplyServeModeLightDemotesAgentic(t *testing.T) {
	p := InferQueryPlan("In the recent change that made low bit math safer for inference, what is the default pass rate?")
	p = ApplyServeMode(p, "light")
	if p.Mode != "light" {
		t.Fatalf("mode=%s", p.Mode)
	}
	// Light demotes open-ended agentic unless conflict/completeness/rare.
	if p.Agentic && !p.Conflict && !p.Completeness && !p.RareID {
		t.Fatalf("light should demote agentic for semantic-ish, plan=%+v", p)
	}
}

func TestApplyServeModeResearchOpensArms(t *testing.T) {
	p := InferQueryPlan("What is the widget price?")
	p = ApplyServeMode(p, "research")
	if !p.Agentic || !p.MultiDoc {
		t.Fatalf("research should force multi+agentic, plan=%+v", p)
	}
}

func TestPlanFloors(t *testing.T) {
	comp := InferQueryPlan("Which customers have been granted an exception to log retention?")
	if comp.TopKFloor() < 14 || comp.PoolKFloor() < 72 {
		t.Fatalf("completeness floors top=%d pool=%d", comp.TopKFloor(), comp.PoolKFloor())
	}
	if !comp.WantCompletenessRescue() || !comp.WantHardPoolExpand() {
		t.Fatalf("completeness rescue/hardPool flags, plan=%+v", comp)
	}
	if comp.CoverageLambda() != 0.55 {
		t.Fatalf("lambda=%v", comp.CoverageLambda())
	}
	sem := InferQueryPlan("In the recent change that made low bit math safer for inference, what is the default pass rate used to decide when a machine is allowed to step down from the safest numeric mode?")
	if sem.FTSBagN() < 3 || sem.FTSCap() < 3 {
		t.Fatalf("semantic FTS bag/cap too low bag=%d cap=%d", sem.FTSBagN(), sem.FTSCap())
	}
}

func TestIsThinShortDefault(t *testing.T) {
	p := InferQueryPlan("What is Alpha?")
	if !p.IsThin("What is Alpha?") {
		t.Fatalf("short thin default should be thin, plan=%+v", p)
	}
	long := "In the recent change that made low bit math safer for inference, what is the default pass rate used to decide when a machine is allowed to step down?"
	p2 := InferQueryPlan(long)
	if p2.IsThin(long) {
		t.Fatalf("long question should not be thin")
	}
	// Strong surface cues are never thin.
	conf := InferQueryPlan("Was it OOM or stalls after telemetry review?")
	if conf.IsThin("Was it OOM or stalls after telemetry review?") {
		t.Fatalf("conflict should not be thin")
	}
}

func TestWantsAgenticPlanRespectsMode(t *testing.T) {
	p := InferQueryPlan("Which customers have retention exceptions?")
	p = ApplyServeMode(p, "light")
	// Completeness keeps agentic under light.
	if !wantsAgenticPlan(p, p.EffectiveType) {
		t.Fatalf("completeness under light should still agentic, plan=%+v", p)
	}
	p2 := InferQueryPlan("What color is the sky?")
	p2 = ApplyServeMode(p2, "light")
	if wantsAgenticPlan(p2, p2.EffectiveType) {
		t.Fatalf("basic light should not agentic")
	}
}

func TestPlanFromOptsMode(t *testing.T) {
	typ, plan := PlanFromOpts(
		"Was INC-9821 an OOM or driver stalls after telemetry review?",
		"",
		"deep",
	)
	if typ != "conflicting_info" {
		t.Fatalf("typ=%s", typ)
	}
	if plan.Mode != "deep" || !plan.Agentic {
		t.Fatalf("deep conflict plan=%+v", plan)
	}
}

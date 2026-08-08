package hosted

import (
	"regexp"
	"strings"
)

// QueryPlan is product-level routing derived from the query surface only.
// ERB question_type labels are eval conveniences — product Ask never has them.
// Capabilities (not taxonomy names) drive retrieve/synth/ground budgets.
type QueryPlan struct {
	MultiDoc         bool   `json:"multi_doc"`
	SemanticExpand   bool   `json:"semantic_expand"`
	DeepHydrate      bool   `json:"deep_hydrate"`
	Agentic          bool   `json:"agentic"`
	Conflict         bool   `json:"conflict"`
	Checklist        bool   `json:"checklist"`
	Completeness     bool   `json:"completeness"`
	Temporal         bool   `json:"temporal"`
	AtomicFact       bool   `json:"atomic_fact"`
	RareID           bool   `json:"rare_id"`
	InfoNotFoundRisk bool   `json:"info_not_found_risk"`
	EffectiveType    string `json:"effective_type,omitempty"`
	// Source: labeled | inferred | labeled+inferred | inferred+llm
	Source string `json:"source"`
	// Mode: light | deep | research | bench | "" (from product UI)
	Mode string `json:"mode,omitempty"`
	// LLMRefined: thin-plan path asked a cheap classifier.
	LLMRefined bool `json:"llm_refined,omitempty"`
	// LLMSkipped: bounded fail-closed skip of the optional plan refine
	// (issue #282): deadline_reserve | context_done. Empty when not skipped.
	LLMSkipped string `json:"llm_skipped,omitempty"`
}

// --- capability helpers (prefer these over string-equality on EffectiveType) ---

func (p QueryPlan) WantBroadLex() bool {
	return p.MultiDoc || p.SemanticExpand || p.Completeness || p.Conflict ||
		p.RareID || p.DeepHydrate || p.AtomicFact || p.Checklist
}

func (p QueryPlan) WantSemanticFTS() bool {
	return p.SemanticExpand || p.MultiDoc || p.Completeness || p.Conflict
}

func (p QueryPlan) WantCompletenessRescue() bool {
	return p.Completeness
}

func (p QueryPlan) WantHardPoolExpand() bool {
	return p.Completeness || p.SemanticExpand || p.Conflict
}

func (p QueryPlan) WantAgentic() bool {
	return p.Agentic
}

func (p QueryPlan) FTSBagN() int {
	if p.SemanticExpand {
		return 4
	}
	if p.WantBroadLex() || p.RareID {
		return 3
	}
	return 2
}

func (p QueryPlan) FTSCap() int {
	if p.SemanticExpand {
		return 4
	}
	if p.WantBroadLex() {
		return 3
	}
	return 2
}

func (p QueryPlan) TopKFloor() int {
	if p.Completeness {
		return 14
	}
	if p.Conflict || p.SemanticExpand {
		return 12
	}
	if p.MultiDoc {
		return 10
	}
	return 0
}

func (p QueryPlan) PoolKFloor() int {
	if p.Completeness {
		return 72
	}
	if p.SemanticExpand || p.Conflict {
		return 64
	}
	if p.MultiDoc {
		return 56
	}
	return 0
}

func (p QueryPlan) CoverageLambda() float64 {
	if p.Completeness {
		return 0.55
	}
	return 0.7
}

func (p QueryPlan) CENFloor() int {
	if p.Completeness {
		return 64
	}
	if p.MultiDoc || p.SemanticExpand || p.Conflict {
		return 56
	}
	return 48
}

// IsThin reports plans with little surface signal — candidate for LLM refine.
func (p QueryPlan) IsThin(question string) bool {
	toks := contentTokens(question)
	if len(toks) >= 12 || len(question) >= 100 {
		return false
	}
	// Strong surface cues → not thin.
	if p.Conflict || p.Completeness || p.Checklist || p.Temporal || p.RareID || p.DeepHydrate {
		return false
	}
	// Only weak semantic/default.
	return len(toks) < 10
}

var (
	enumRE         = regexp.MustCompile(`(?i)\b(which|what)\s+(customers?|partners?|teams?|channels?|exceptions?|steps?|fields?|metrics?|actions?)\b`)
	listAllRE      = regexp.MustCompile(`(?i)\b(list|enumerate|every|all of|complete (required|list)|each of)\b`)
	orConflictRE   = regexp.MustCompile(`(?i)\b(or|rather than|instead of|as opposed to)\b`)
	conflictCueRE  = regexp.MustCompile(`(?i)\b(correction|corrected|supersed|actually|telemetry review|conflicting|wrongly|initially (said|noted)|earlier note)\b`)
	multiEntityRE  = regexp.MustCompile(`(?i)\b(and also|as well as|in addition|both .+ and|across (docs?|documents?|teams?))\b`)
	projectRE      = regexp.MustCompile(`(?i)\b(how (do|should|can) we|what is the (recommended|suggested)|remediation|rollout|runbook|playbook|process for)\b`)
	constrainedRE  = regexp.MustCompile(`(?i)\b(only for|limited to|in (us|eu|ap)-|for the .+ tier|under the .+ plan|when using)\b`)
	infoNotFoundRE = regexp.MustCompile(`(?i)\b(is there any|are there any|does .+ (exist|support)|has anyone|any customer other than)\b`)
)

// InferQueryPlan derives capabilities from the question text alone.
func InferQueryPlan(question string) QueryPlan {
	q := strings.TrimSpace(question)
	p := QueryPlan{Source: "inferred"}
	if q == "" {
		p.EffectiveType = "basic"
		return p
	}
	low := strings.ToLower(q)

	p.Checklist = seeksChecklist(q)
	p.Temporal = seeksAtomicDate(q) || temporalDatePreference(q) != ""
	p.AtomicFact = p.Temporal || seeksAtomicQuantity(q)
	p.RareID = hasRareIdentifier(extractIdentifiers(q), q)
	p.DeepHydrate = wantsDeepHydrate(q, "")
	p.Conflict = prefersSupersedingEvidence(q, "") ||
		(orConflictRE.MatchString(q) && (conflictCueRE.MatchString(q) ||
			strings.Contains(low, "oom") || strings.Contains(low, "stall") ||
			strings.Contains(low, "was it") || strings.Contains(low, "is it")))
	p.Completeness = listAllRE.MatchString(q) || enumRE.MatchString(q)
	p.InfoNotFoundRisk = infoNotFoundRE.MatchString(q)

	toks := contentTokens(q)
	long := len(toks) >= 12 || len(q) >= 120
	shortFactoid := regexp.MustCompile(`(?i)^(what|who|when|where|how many|how much)\b`).MatchString(q) &&
		len(toks) <= 14 && !p.Completeness && !p.Conflict
	p.SemanticExpand = (long && !shortFactoid) || p.DeepHydrate ||
		(len(pickHotLexPhrases(q, 1)) > 0 && long)

	p.MultiDoc = p.Completeness || p.Conflict || multiEntityRE.MatchString(q) ||
		projectRE.MatchString(q) || (p.SemanticExpand && long)

	p.Agentic = p.MultiDoc || p.Conflict || p.Completeness || p.DeepHydrate ||
		p.SemanticExpand || p.RareID

	constrained := constrainedRE.MatchString(q)

	switch {
	case p.InfoNotFoundRisk && !p.Completeness:
		if strings.Contains(low, "other than") || strings.Contains(low, "besides") {
			p.EffectiveType = "completeness"
			p.Completeness = true
			p.MultiDoc = true
		} else {
			p.EffectiveType = "info_not_found"
		}
	case p.Conflict:
		p.EffectiveType = "conflicting_info"
		p.MultiDoc = true
	case p.Completeness:
		p.EffectiveType = "completeness"
		p.MultiDoc = true
	case projectRE.MatchString(q) && p.MultiDoc:
		p.EffectiveType = "project_related"
	case constrained && p.MultiDoc:
		p.EffectiveType = "constrained"
	case p.SemanticExpand && !shortFactoid:
		p.EffectiveType = "semantic"
		p.MultiDoc = true
	case p.Checklist || p.AtomicFact || shortFactoid:
		p.EffectiveType = "basic"
	default:
		p.EffectiveType = "semantic"
		p.SemanticExpand = true
		p.MultiDoc = true
		p.Agentic = true
	}

	if p.Conflict || p.Temporal || p.DeepHydrate {
		p.DeepHydrate = true
	}
	return p
}

// ResolveQuestionType returns the type string for legacy gates + full plan.
func ResolveQuestionType(question, labeledType string) (string, QueryPlan) {
	labeled := strings.ToLower(strings.TrimSpace(labeledType))
	plan := InferQueryPlan(question)
	if labeled != "" {
		plan.Source = "labeled"
		plan.EffectiveType = labeled
		if seeksAtomicDate(question) {
			plan.Temporal = true
			plan.AtomicFact = true
		}
		if seeksChecklist(question) {
			plan.Checklist = true
		}
		if prefersSupersedingEvidence(question, labeled) {
			plan.Conflict = true
		}
		if isMultiDocType(labeled) {
			plan.MultiDoc = true
			plan.Agentic = true
		}
		if labeled == "semantic" {
			plan.SemanticExpand = true
		}
		if labeled == "completeness" {
			plan.Completeness = true
			plan.MultiDoc = true
		}
		if labeled == "conflicting_info" {
			plan.Conflict = true
			plan.MultiDoc = true
		}
		if labeled == "info_not_found" {
			plan.InfoNotFoundRisk = true
		}
		return labeled, plan
	}
	plan.Source = "inferred"
	return plan.EffectiveType, plan
}

// ApplyServeMode modulates plan budgets from product UI mode.
// light → demote agentic / multi-arm for speed
// deep/bench → keep plan flags; ensure agentic on multi-doc
// research → force open multi-arm
func ApplyServeMode(p QueryPlan, mode string) QueryPlan {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "lean":
		m = "light"
	case "agentic":
		m = "deep"
	case "":
		// Fall back to env mode.
		m = strings.ToLower(envOr("OUROBOROS_ERB_MODE", ""))
		if m == "lean" {
			m = "light"
		}
	}
	p.Mode = m
	switch m {
	case "light":
		// Demo-fast: keep surface-critical arms, drop open-ended agentic.
		if !p.Conflict && !p.Completeness && !p.RareID {
			p.Agentic = false
		}
		// Still allow semantic expand FTS for long questions.
	case "deep", "bench":
		if p.MultiDoc || p.SemanticExpand || p.Conflict || p.Completeness {
			p.Agentic = true
		}
	case "research":
		p.Agentic = true
		p.MultiDoc = true
		if !p.SemanticExpand && !p.Completeness && !p.Conflict {
			p.SemanticExpand = true
		}
	}
	return p
}

// PlanFromOpts resolves type+plan for retrieve, applying mode and optional thin LLM.
// questionTypeOut is the string for prompts/legacy; plan is authoritative for flags.
func PlanFromOpts(question, labeledType, mode string) (string, QueryPlan) {
	typ, plan := ResolveQuestionType(question, labeledType)
	plan = ApplyServeMode(plan, mode)
	return typ, plan
}

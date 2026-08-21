package hosted

import (
	"regexp"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// seeksChecklist: triage steps, acceptance criteria, required content lists.
// Keep cues tight — bare "what was the" / "triage" match ordinary factoids and
// would dump pack numbered lists onto free-form product answers.
func seeksChecklist(question string) bool {
	q := strings.ToLower(question)
	for _, c := range []string{
		"checklist", "what steps", "which steps", "what were the steps",
		"acceptance criteria", "required content", "required fields",
		"enumerate", "list every", "complete required", "triage checklist",
		"triage steps", "steps were listed", "step-by-step",
	} {
		if strings.Contains(q, c) {
			return true
		}
	}
	return false
}

var (
	// (1) step / 1. step / 1) step / - step with verb-ish length
	stepLineRE = regexp.MustCompile(`(?m)^\s*(?:\(?\d{1,2}\)?[.)]|[-*•])\s+(.{12,200})$`)
	// Inline: (1) foo (2) bar
	inlineStepRE = regexp.MustCompile(`\((\d{1,2})\)\s+([^;(]{8,160})`)
)

// extractNumberedSteps pulls checklist-like lines from passage text.
func extractNumberedSteps(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		s = strings.TrimRight(s, ".;")
		if len(s) < 12 {
			return
		}
		key := strings.ToLower(s)
		key = textbound.Bytes(key, 80)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	for _, m := range stepLineRE.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	for _, m := range inlineStepRE.FindAllStringSubmatch(text, -1) {
		if len(m) > 2 {
			add(m[2])
		}
	}
	return out
}

// mergeChecklistStepsIntoAnswer appends pack-derived steps when the model
// answer is thin or invents a different checklist (common basic/intra miss).
// Only fires for checklist-shaped questions; does not rewrite free prose.
func mergeChecklistStepsIntoAnswer(question, answer string, passages []Passage) (string, map[string]any) {
	diag := map[string]any{}
	if !seeksChecklist(question) || len(passages) == 0 {
		return answer, diag
	}
	var steps []string
	seen := map[string]struct{}{}
	// Prefer higher-score / leaf passages first.
	ordered := rankPassagesForExtractive(question, passages)
	for _, p := range ordered {
		for _, s := range extractNumberedSteps(p.Text) {
			key := strings.ToLower(s)
			key = textbound.Bytes(key, 60)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			steps = append(steps, s)
			if len(steps) >= 8 {
				break
			}
		}
		if len(steps) >= 8 {
			break
		}
	}
	if len(steps) < 2 {
		return answer, diag
	}
	// Count how many pack steps already appear (loosely) in the answer.
	lowAns := strings.ToLower(answer)
	hit := 0
	for _, s := range steps {
		// Use first 4 content tokens as fingerprint.
		toks := contentTokens(s)
		if len(toks) > 4 {
			toks = toks[:4]
		}
		ok := true
		for _, t := range toks {
			if len(t) >= 4 && !strings.Contains(lowAns, t) {
				ok = false
				break
			}
		}
		if ok && len(toks) > 0 {
			hit++
		}
	}
	// Already covers most pack steps — leave answer alone.
	if hit*2 >= len(steps) {
		diag["checklist_steps_covered"] = hit
		return answer, diag
	}
	out := strings.TrimSpace(answer)
	if out != "" && !strings.HasSuffix(out, ".") {
		out += "."
	}
	out += "\n\nEvidence checklist steps from the supporting documents:\n"
	for i, s := range steps {
		out += strings.TrimSpace(s)
		if !strings.HasSuffix(s, ".") {
			out += "."
		}
		if i+1 < len(steps) {
			out += "\n"
		}
	}
	diag["checklist_steps_merged"] = len(steps)
	diag["checklist_steps_prior_hits"] = hit
	return out, diag
}

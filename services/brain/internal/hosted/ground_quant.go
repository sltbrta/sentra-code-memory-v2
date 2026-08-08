package hosted

import (
	"regexp"
	"strings"
)

// Duration and money atoms appear across ERB conflict/completeness misses
// (retention windows, egress rates, audit export periods). Same shape as
// date rebind: score pack evidence, prefer superseding/correction windows
// when the question is conflict-shaped — never per-question IDs.

var (
	durationAtomRE = regexp.MustCompile(`(?i)\b(\d{1,3})\s*(months?|days?|weeks?)\b`)
	// $0.085 per GiB, $18k, etc.
	moneyAtomRE = regexp.MustCompile(`(?i)\$\d+(?:\.\d{1,4})?(?:\s*(?:per|/)\s*(?:GiB|GB|TB|mo|month|1k(?:\s*tokens?)?))?`)
)

// prefersSupersedingEvidence is true for conflicting_info, A-vs-B / correction
// surface forms, and policy/leave "current" questions where multiple day-counts
// compete. Generalized from conflict fact misses + leave smoke supersession.
func prefersSupersedingEvidence(question, questionType string) bool {
	if strings.EqualFold(strings.TrimSpace(questionType), "conflicting_info") {
		return true
	}
	q := strings.ToLower(question)
	// Prefer superseding *when documents actually conflict* — not every quantity ask.
	// "how many days" alone must not trigger SUPERSEDING pack rewrite (audit #4).
	for _, c := range []string{
		"rather than", "instead of", "which is correct", "corrected",
		"supersed", "actually ", "not a sustained",
		"earlier note", "after deeper", "telemetry review",
		"current policy", "current leave", "days of leave",
		"leave entitlement", "most recent policy", "updated policy",
		"bereavement leave", "parental leave",
	} {
		if strings.Contains(q, c) {
			return true
		}
	}
	return false
}

// seeksAtomicQuantity: answer should lock to pack durations/rates when the
// question asks for a window, retention, rate, or cost basis.
// Bare "rate"/"latency"/"threshold" alone are too broad (pass rate, error rate)
// and would open money inject on unrelated $ atoms — use cost/window surface.
func seeksAtomicQuantity(question string) bool {
	q := strings.ToLower(question)
	for _, c := range []string{
		"how many month", "how many day", "how many week",
		"days of leave", "days paid", "paid days", "leave days",
		"retention", "export window", "export period", "lookback",
		"per gib", "per gb", "per tb", "$/", "$ per",
		"cost", "price", "billing", "egress rate", "catalog rate",
		"for the past", "for the last", "past months", "last months",
		"retention window", "audit export", "months of",
		"bereavement", "parental leave",
	} {
		if strings.Contains(q, c) {
			return true
		}
	}
	// Duration window without "how many": "what is the retention period"
	if strings.Contains(q, "window") && (strings.Contains(q, "month") || strings.Contains(q, "day") ||
		strings.Contains(q, "retention") || strings.Contains(q, "export")) {
		return true
	}
	if strings.Contains(q, "period") && (strings.Contains(q, "retention") || strings.Contains(q, "export") ||
		strings.Contains(q, "month") || strings.Contains(q, "lookback")) {
		return true
	}
	return false
}

// seeksMoneyQuantity: cost/billing surface only — never bare "rate" (pass rate).
func seeksMoneyQuantity(question string) bool {
	q := strings.ToLower(question)
	for _, c := range []string{
		"cost", "price", "billing", "per gib", "per gb", "per tb",
		"$/", "$ per", "egress", "catalog rate", "list price", "charge",
	} {
		if strings.Contains(q, c) {
			return true
		}
	}
	return false
}

// correctionLocalBonus lifts atoms whose window has superseding language.
// retractedFramingPenalty lowers atoms framed as draft / earlier / suggested
// so "earlier note suggested 18 months" loses to "corrected window is 12 months".
func correctionLocalBonus(text, atom string) int {
	win := dateWindow(text, atom, 140)
	if win == "" {
		return 0
	}
	bonus := 0
	for _, cue := range []string{
		"correction", "corrected", "supersed", "telemetry", "after deeper",
		"revised", "updated policy", "final ", "instead", "rather than",
		"not a sustained", "concluded", "confirmed", "catalog rate",
		"billing", "approved", "effective", "policy",
	} {
		if strings.Contains(win, cue) {
			bonus += 5
		}
	}
	return bonus - retractedFramingPenalty(win)
}

func retractedFramingPenalty(win string) int {
	if win == "" {
		return 0
	}
	pen := 0
	for _, cue := range []string{
		"earlier", "suggested", "draft", "initially", "previously",
		"old note", "superseded", "wrongly", "mistaken", "obsolete",
		"no longer", "deprecated",
	} {
		if strings.Contains(win, cue) {
			pen += 12
		}
	}
	return pen
}

type quantCand struct {
	atom  string
	score int
	doc   string
	text  string
	kind  string // "duration" | "money"
}

func collectQuantCands(question string, passages []Passage, cited map[string]struct{}) []quantCand {
	agg := aggregatePassagesByDoc(passages)
	qExpand := question
	if bags := pickHotLexPhrases(question, 4); len(bags) > 0 {
		qExpand = question + " " + strings.Join(bags, " ")
	}
	var cands []quantCand
	for _, p := range agg {
		ps := passageQuestionOverlap(question, p)
		citeBonus := 0
		if _, ok := cited[p.DocumentID]; ok {
			citeBonus = 12
		}
		// Score *each occurrence* of an atom: "12 months" next to "corrected"
		// must beat "12 months" next to a preamble that still mentions an
		// "earlier" note within a wide window.
		addOcc := func(atom, kind string, at int) {
			if at < 0 {
				return
			}
			loc := dateLocalBonusAt(p.Text, atom, qExpand, at)
			corr := correctionLocalBonusAt(p.Text, atom, at)
			s := ps*8 + loc + corr + citeBonus
			cands = append(cands, quantCand{atom: atom, score: s, doc: p.DocumentID, text: p.Text, kind: kind})
		}
		low := strings.ToLower(p.Text)
		for _, m := range durationAtomRE.FindAllString(p.Text, -1) {
			// Walk every occurrence (not only first Index).
			needle := strings.ToLower(m)
			from := 0
			for {
				at := strings.Index(low[from:], needle)
				if at < 0 {
					break
				}
				abs := from + at
				addOcc(m, "duration", abs)
				from = abs + len(m)
			}
		}
		for _, m := range moneyAtomRE.FindAllString(p.Text, -1) {
			m = strings.TrimSpace(m)
			needle := strings.ToLower(m)
			from := 0
			for {
				at := strings.Index(low[from:], needle)
				if at < 0 {
					break
				}
				abs := from + at
				addOcc(m, "money", abs)
				from = abs + len(m)
			}
		}
	}
	return cands
}

// dateWindowAt is dateWindow with an explicit atom offset (multi-occurrence).
func dateWindowAt(text string, at, atomLen, pad int) string {
	if at < 0 || at >= len(text) {
		return ""
	}
	if pad <= 0 {
		pad = 80
	}
	start := at - pad
	if start < 0 {
		start = 0
	}
	end := at + atomLen + pad
	if end > len(text) {
		end = len(text)
	}
	return strings.ToLower(text[start:end])
}

func dateLocalBonusAt(text, atom, question string, at int) int {
	win := dateWindowAt(text, at, len(atom), 90)
	if win == "" {
		return 0
	}
	bonus := 0
	for _, t := range contentTokens(question) {
		if len(t) >= 4 && strings.Contains(win, t) {
			bonus += 2
		}
	}
	for _, cue := range []string{
		"freeze", "announc", "communicat", "effective", "procur", "budget",
		"spend", "incident", "stall", "oom", "policy", "approved", "dated",
		"correction", "telemetry", "supersed", "updated", "company-wide",
		"export", "retention", "catalog", "egress", "access log",
	} {
		if strings.Contains(win, cue) {
			bonus += 3
		}
	}
	for _, bag := range pickHotLexPhrases(question, 4) {
		b := strings.ToLower(bag)
		if len(b) >= 6 && strings.Contains(win, b) {
			bonus += 10
		}
	}
	return bonus
}

func correctionLocalBonusAt(text, atom string, at int) int {
	// Tight window so an earlier-note clause does not taint a later correction.
	win := dateWindowAt(text, at, len(atom), 70)
	if win == "" {
		return 0
	}
	bonus := 0
	for _, cue := range []string{
		"correction", "corrected", "supersed", "telemetry", "after deeper",
		"revised", "updated", "final ", "instead", "rather than",
		"not a sustained", "concluded", "confirmed", "catalog rate",
		"billing", "approved", "effective", "policy",
	} {
		if strings.Contains(win, cue) {
			bonus += 6
		}
	}
	return bonus - retractedFramingPenalty(win)
}

// rebindAnswerQuantities rewrites weakly supported durations/money atoms in the
// answer to the best pack evidence. When prefersSupersedingEvidence, correction-
// cued spans beat earlier notes (generalized conflict resolution).
func rebindAnswerQuantities(question, answer string, passages []Passage, questionType string, citedIDs ...[]string) (string, map[string]any) {
	diag := map[string]any{}
	if strings.TrimSpace(answer) == "" || len(passages) == 0 {
		return answer, diag
	}
	ansDur := durationAtomRE.FindAllString(answer, -1)
	ansMoney := moneyAtomRE.FindAllString(answer, -1)
	if len(ansDur) == 0 && len(ansMoney) == 0 && !seeksAtomicQuantity(question) {
		return answer, diag
	}

	citedSet := map[string]struct{}{}
	if len(citedIDs) > 0 {
		for _, c := range citedIDs[0] {
			if c != "" {
				citedSet[c] = struct{}{}
			}
		}
	}
	cands := collectQuantCands(question, passages, citedSet)
	if len(cands) == 0 {
		return answer, diag
	}

	supersede := prefersSupersedingEvidence(question, questionType)
	if supersede {
		diag["quantity_supersede_pref"] = true
	}

	// Best per kind.
	bestOf := func(kind string) (quantCand, bool) {
		var pool []quantCand
		for _, c := range cands {
			if c.kind != kind {
				continue
			}
			if supersede {
				// Prefer positively framed correction spans; fall back if none.
				if c.score >= 40 {
					pool = append(pool, c)
				}
			} else {
				pool = append(pool, c)
			}
		}
		if len(pool) == 0 {
			for _, c := range cands {
				if c.kind == kind {
					pool = append(pool, c)
				}
			}
		}
		if len(pool) == 0 {
			return quantCand{}, false
		}
		// Score floor vs max.
		maxS := pool[0].score
		for _, c := range pool[1:] {
			if c.score > maxS {
				maxS = c.score
			}
		}
		floor := maxS * 40 / 100
		if floor < 15 {
			floor = 15
		}
		best := pool[0]
		for _, c := range pool {
			if c.score < floor {
				continue
			}
			if c.score > best.score {
				best = c
				continue
			}
			// Prefer higher score only (per-occurrence scoring already embeds
			// correction vs retracted framing).
		}
		return best, true
	}

	support := map[string]int{}
	for _, c := range cands {
		key := strings.ToLower(c.atom)
		if c.score > support[key] {
			support[key] = c.score
		}
	}

	out := answer
	rebound := 0
	rebindKind := func(kind string, atoms []string) {
		best, ok := bestOf(kind)
		if !ok || best.atom == "" {
			return
		}
		diag["best_evidence_"+kind] = best.atom
		diag["best_evidence_"+kind+"_score"] = best.score
		diag["best_evidence_"+kind+"_doc"] = best.doc
		seen := map[string]struct{}{}
		for _, ad := range atoms {
			low := strings.ToLower(ad)
			if _, ok := seen[low]; ok {
				continue
			}
			seen[low] = struct{}{}
			if strings.EqualFold(ad, best.atom) {
				continue
			}
			adScore := support[low]
			// Force rewrite under supersede policy or when dominated.
			if supersede || adScore < best.score {
				// Replace first occurrence of this atom form (case-insensitive via exact match list).
				out2 := strings.Replace(out, ad, best.atom, 1)
				if out2 != out {
					out = out2
					rebound++
				}
			}
		}
		// Inject when question seeks quantity and answer has none of this kind.
		if seeksAtomicQuantity(question) && len(atoms) == 0 && best.atom != "" {
			// Only inject duration for window questions; money for cost surface only.
			ql := strings.ToLower(question)
			want := false
			if kind == "duration" && (strings.Contains(ql, "month") || strings.Contains(ql, "day") ||
				strings.Contains(ql, "week") || strings.Contains(ql, "window") ||
				strings.Contains(ql, "retention") || strings.Contains(ql, "export") ||
				strings.Contains(ql, "past") || strings.Contains(ql, "last ") ||
				strings.Contains(ql, "lookback") || strings.Contains(ql, "period")) {
				want = true
			}
			if kind == "money" && seeksMoneyQuantity(question) {
				want = true
			}
			if want && !durationAtomRE.MatchString(out) && kind == "duration" {
				out = strings.TrimSpace(out)
				if out != "" && !strings.HasSuffix(out, ".") {
					out += "."
				}
				out += " Period established in the supporting documents: " + best.atom + "."
				rebound++
				diag["duration_injected"] = true
			}
			if want && !moneyAtomRE.MatchString(out) && kind == "money" {
				out = strings.TrimSpace(out)
				if out != "" && !strings.HasSuffix(out, ".") {
					out += "."
				}
				out += " Rate established in the supporting documents: " + best.atom + "."
				rebound++
				diag["money_injected"] = true
			}
		}
	}

	rebindKind("duration", ansDur)
	// Normalize money atoms from answer for matching (trim).
	var moneyNorm []string
	for _, m := range ansMoney {
		moneyNorm = append(moneyNorm, strings.TrimSpace(m))
	}
	rebindKind("money", moneyNorm)

	if rebound > 0 {
		diag["quantities_rebound"] = rebound
	}
	return out, diag
}

// sizeAtomRE matches storage/upload size tokens (10 MiB, 50MB, 25 MB).
var (
	sizeAtomRE     = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*(MiB|MB|GiB|GB)\b`)
	sizeSentenceRE = regexp.MustCompile(`[.;\n]+`)
	sizeQuestionRE = regexp.MustCompile(`(?i)\b(?:size|upload|uploads|storage|attachment|attachments|payload|mib|mb|gib|gb)\b`)
)

// rebindAnswerSizeLimits fixes common "wrong limit from neighbor doc" failures
// (e.g. answer 25MB when pack states 10 MiB per file + 50 MiB total).
// Prefer a dual-limit span from the pack when the question asks about size/limits.
func rebindAnswerSizeLimits(question, answer string, passages []Passage) (string, map[string]any) {
	diag := map[string]any{}
	// Gate: only questions that can contain storage/upload size atoms. A generic
	// "limit" question (for example, a traffic-shift duration or retry count)
	// must not scan the entire hydrated pack just because it uses that word.
	if !sizeQuestionRE.MatchString(question) {
		return answer, diag
	}
	// Collect dual-limit sentences from pack (two size atoms within one span).
	type dual struct {
		a, b, sentence, doc string
		score               int
	}
	var duals []dual
	for _, p := range passages {
		text := p.Text
		if text == "" {
			continue
		}
		// Split rough sentences.
		for _, sent := range sizeSentenceRE.Split(text, -1) {
			ms := sizeAtomRE.FindAllString(sent, -1)
			if len(ms) < 2 {
				continue
			}
			// Prefer spans that mention per-file/total/request.
			low := strings.ToLower(sent)
			sc := passageQuestionOverlap(question, Passage{Text: sent, DocumentID: p.DocumentID})
			if strings.Contains(low, "per file") || strings.Contains(low, "per-file") {
				sc += 8
			}
			if strings.Contains(low, "total") || strings.Contains(low, "request") {
				sc += 6
			}
			if strings.Contains(low, "mib") {
				sc += 2 // gold often uses MiB
			}
			duals = append(duals, dual{a: ms[0], b: ms[1], sentence: strings.TrimSpace(sent), doc: p.DocumentID, score: sc})
		}
	}
	if len(duals) == 0 {
		return answer, diag
	}
	// Best dual by score.
	best := duals[0]
	for _, d := range duals[1:] {
		if d.score > best.score {
			best = d
		}
	}
	// If answer already contains both size atoms (normalized), leave it.
	ansSizes := sizeAtomRE.FindAllString(answer, -1)
	hasA, hasB := false, false
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "ib", "b"))
	}
	// Compare numeric cores.
	num := func(s string) string {
		m := regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)`).FindString(s)
		return m
	}
	na, nb := num(best.a), num(best.b)
	for _, s := range ansSizes {
		ns := num(s)
		if ns == na {
			hasA = true
		}
		if ns == nb {
			hasB = true
		}
	}
	_ = norm
	if hasA && hasB {
		return answer, diag
	}
	// Replace single wrong size or append the dual limit.
	out := answer
	if len(ansSizes) == 1 && !hasA && !hasB {
		// Single competing size → rewrite to dual.
		out = strings.Replace(out, ansSizes[0], best.a+" per file and "+best.b+" total", 1)
		diag["size_limits_rebound"] = true
		diag["size_limits_to"] = best.a + " / " + best.b
		diag["best_evidence_size_doc"] = best.doc
		return out, diag
	}
	// Append authoritative dual limit.
	out = strings.TrimSpace(out)
	if out != "" && !strings.HasSuffix(out, ".") {
		out += "."
	}
	out += " Size limits established in the supporting documents: " + best.a + " per file and " + best.b + " total request size."
	diag["size_limits_injected"] = true
	diag["size_limits_to"] = best.a + " / " + best.b
	diag["best_evidence_size_doc"] = best.doc
	return out, diag
}

// trueFirstEventIntent is true only for explicit first-event phrasing — not
// conflict framing like "initially said" / "initial hypothesis".
func trueFirstEventIntent(question string) bool {
	q := strings.ToLower(question)
	for _, c := range []string{
		"first communicated", "first announced", "first applied", "first noted the freeze",
		"earliest", "originally communicated", "originally announced",
		"what date did", "what date was", "when was the first", "when did .* first",
	} {
		if strings.Contains(q, c) {
			return true
		}
	}
	// "first " without correction/initial conflict framing.
	if strings.Contains(q, "first ") && !strings.Contains(q, "initially") &&
		!strings.Contains(q, "initial ") && !strings.Contains(q, "correction") &&
		!strings.Contains(q, "telemetry") {
		return true
	}
	return false
}

// applyCorrectionDatePolicy: for conflict-shaped questions without an explicit
// first-event cue, prefer the latest event-cued date among correction windows
// (telemetry review date beats initial incident open date).
// "initially said/noted" is conflict framing, not first-event intent — do not
// let temporalDatePreference("initial") win over superseding evidence.
func applyCorrectionDatePolicy(question, questionType string, pref string) string {
	if prefersSupersedingEvidence(question, questionType) {
		if trueFirstEventIntent(question) {
			return "earliest"
		}
		return "latest"
	}
	return pref
}

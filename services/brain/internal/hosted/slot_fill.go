package hosted

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// Type-aware exhaustive slot filling + strict unsupported-extra rejection
// (issue #258). Retrieval diagnosis (#259) showed project_related /
// completeness answers lose multi-gold facts after a correct retrieve: the
// single-shot or map-reduce synthesis under-enumerates pack-established
// slots. This is a deterministic post-grounding pass — zero LLM calls, so
// the bounded budget ledger (#278) is untouched and no new pipeline or
// unbounded loop is introduced.
//
// Guarantees:
//   - Type-aware: each question type fills only its own slot schema
//     (project: owner/status/target/timeline; completeness + constrained:
//     enumerated entity items; conflicting: quantity atoms).
//   - Qualifier/conflict-aware: constrained candidates conflict with #270
//     question qualifiers are rejected; conflicting_info quantity groups
//     resolve to the current/superseding value using the same validity,
//     currency, and authority signals as the #270 reranker.
//   - Strict leaf-span citations: filled facts are verbatim pack spans from
//     leaf documents only (never summary:/agent:/conversation passages) and
//     are recorded as claims so citation pruning keeps the leaf doc.
//   - Unsupported-extra rejection: answer lines whose fact atoms (dates,
//     amounts, durations, identifiers, numbers) appear nowhere in the pack
//     are dropped; a rejection never empties the answer.
//
// No gold labels, no question IDs, no query-specific steering.
//
// Env knobs:
//
//	OUROBOROS_ERB_SLOT_FILL       kill switch (default on)
//	OUROBOROS_ERB_SLOT_FILL_MAX   max appended facts (default 8, clamp 1..12)

var (
	slotOwnerRE  = regexp.MustCompile(`(?i)\b(?:owner|owned by|dri|lead|assignee)\s*[:\-]\s*([A-Z][\w.'-]+(?: [A-Z][\w.'-]+){0,2})`)
	slotStatusRE = regexp.MustCompile(`(?i)\bstatus\s*[:\-]\s*(on track|at risk|blocked|complete|completed|done|in progress|green|yellow|red|ga|beta)\b`)
	slotTargetRE = regexp.MustCompile(`(?i)\b(?:slo|sla|ttm|ttf|target|p50|p95|p99)\b\s*[:\-]?\s*((?:\w+\s+)?<?\s?\d+(?:\.\d+)?\s?(?:ms|s|sec|seconds|minutes|hours|days|weeks|%))\b`)
	bulletLineRE = regexp.MustCompile(`^\s*(?:[-*•]|\d{1,2}[.)])\s+(.+?)\s*$`)
	colonPairRE  = regexp.MustCompile(`^\s*([A-Z][\w.&'-]{2,}(?: [A-Z][\w.&'-]{2,}){0,2})\s*:\s+(\S.{2,120}?)\s*$`)
	atomNumberRE = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	atomIdentRE  = regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`)
	listMarkerRE = regexp.MustCompile(`^\s*(?:[-*•#]|\d{1,2}[.)])\s+`)
)

const slotFillHardCap = 12

func slotFillEnabled() bool {
	return envTruthy("OUROBOROS_ERB_SLOT_FILL", true)
}

func slotFillMaxAdditions() int {
	n := envInt("OUROBOROS_ERB_SLOT_FILL_MAX", 8)
	if n < 1 {
		n = 1
	}
	if n > slotFillHardCap {
		n = slotFillHardCap
	}
	return n
}

// slotCandidate is one pack-established fact that the answer may have missed.
type slotCandidate struct {
	kind    string // owner|status|target|timeline|entity|duration|money|date
	value   string // canonical value used for coverage checks
	quote   string // verbatim span from the leaf document (strict citation)
	display string // appended answer text for this fact
	docID   string
	blob    string // passage blob for qualifier checks
	date    time.Time
	hasDate bool
	cur     float64
	auth    float64
	ord     int
}

// slotFillAnswer appends pack-established facts the grounded answer missed.
// It only runs for multi-document/constrained/conflict types, never on
// abstentions, and only from leaf documents. Returns g unchanged otherwise.
func slotFillAnswer(question, questionType string, g Grounded, passages []Passage, sourceTypes []string) Grounded {
	if g.Diagnostics == nil {
		g.Diagnostics = map[string]any{}
	}
	qt := strings.ToLower(strings.TrimSpace(questionType))
	if !slotFillEnabled() || looksLikeAbstention(g.Answer) || shouldClearCitesOnAbstain(g.Answer) {
		return g
	}
	cands := slotCandidatesFor(question, qt, passages, sourceTypes)
	if len(cands) == 0 {
		return g
	}
	g.Diagnostics["slot_fill_candidates"] = len(cands)

	// Qualifier-aware rejection (#270-compatible): constrained questions keep
	// only candidates whose evidence does not declare a conflicting value for
	// a qualifier the question pins (tier/region/product/tenant/environment).
	if qt == "constrained" {
		cands, g.Diagnostics = filterQualifierConflicts(question, cands, g.Diagnostics)
	}
	// Conflict-aware resolution (#270-compatible): conflicting questions keep
	// only the current/superseding value per quantity kind.
	if qt == "conflicting_info" {
		cands, g.Diagnostics = resolveSlotConflicts(question, cands, g.Diagnostics)
	}

	var b strings.Builder
	added, covered := 0, 0
	seen := map[string]struct{}{}
	for _, c := range cands {
		if added >= slotFillMaxAdditions() {
			break
		}
		key := c.kind + "|" + normText(c.value)
		if _, dup := seen[key]; dup {
			continue
		}
		if candidateCovered(g.Answer, c) {
			covered++
			continue
		}
		seen[key] = struct{}{}
		if added == 0 {
			b.WriteString("\n\nAdditional facts established in the documents:")
		}
		b.WriteString("\n- ")
		b.WriteString(c.display)
		g.Claims = append(g.Claims, Claim{Text: c.value, Quote: c.quote, DocumentID: c.docID})
		g.CitedDocumentIDs = uniqueStringsStable(append(g.CitedDocumentIDs, c.docID))
		added++
	}
	if added > 0 {
		g.Answer = strings.TrimRight(g.Answer, "\n ") + b.String()
	}
	g.Diagnostics["slot_fill_added"] = added
	if covered > 0 {
		g.Diagnostics["slot_fill_covered"] = covered
	}
	return g
}

// slotCandidatesFor mines type-scoped slot candidates from leaf passages.
func slotCandidatesFor(question, qt string, passages []Passage, sourceTypes []string) []slotCandidate {
	var kinds map[string]bool
	switch qt {
	case "project_related":
		kinds = map[string]bool{"owner": true, "status": true, "target": true, "timeline": true}
	case "completeness", "constrained":
		kinds = map[string]bool{"entity": true}
	case "conflicting_info":
		kinds = map[string]bool{"duration": true, "money": true}
	default:
		return nil
	}
	var out []slotCandidate
	ord := 0
	for _, p := range passages {
		if p.DocumentID == "" || isConversationPassage(p) ||
			strings.HasPrefix(p.DocumentID, "summary:") || strings.HasPrefix(p.DocumentID, "agent:") {
			continue // strict leaf-span only
		}
		text := p.Text
		if text == "" {
			continue
		}
		d, hasDate := passageTime(p)
		base := slotCandidate{
			docID: p.DocumentID,
			blob:  p.DocumentID + " " + p.SourceURI + " " + p.Channel + " " + text,
			date:  d, hasDate: hasDate,
			cur:  currencyScore(text),
			auth: authorityForPassage(p, sourceTypes),
		}
		add := func(kind, value, quote, display string) {
			value = strings.TrimSpace(value)
			quote = strings.TrimSpace(quote)
			if value == "" || len(quote) < 8 || !strings.Contains(text, quote) {
				return // quote must be a verbatim leaf span
			}
			c := base
			c.kind, c.value, c.quote, c.display = kind, value, quote, display
			c.ord = ord
			ord++
			out = append(out, c)
		}
		if kinds["owner"] {
			if m := slotOwnerRE.FindStringSubmatch(text); len(m) > 1 {
				add("owner", m[1], quoteAround(text, m[1]), fmt.Sprintf("Owner: %s — \"%s\"", m[1], quoteAround(text, m[1])))
			}
		}
		if kinds["status"] {
			if m := slotStatusRE.FindStringSubmatch(text); len(m) > 1 {
				add("status", m[1], quoteAround(text, m[0]), fmt.Sprintf("Status: %s — \"%s\"", m[1], quoteAround(text, m[0])))
			}
		}
		if kinds["target"] {
			for _, m := range slotTargetRE.FindAllStringSubmatch(text, 4) {
				if len(m) > 1 {
					add("target", m[1], quoteAround(text, m[0]), fmt.Sprintf("Target: %s — \"%s\"", m[1], quoteAround(text, m[0])))
				}
			}
		}
		if kinds["timeline"] {
			for _, dstr := range isoDateRE.FindAllString(text, 4) {
				add("timeline", dstr, quoteAround(text, dstr), fmt.Sprintf("Timeline: %s — \"%s\"", dstr, quoteAround(text, dstr)))
			}
		}
		if kinds["entity"] {
			for _, line := range strings.Split(text, "\n") {
				if m := bulletLineRE.FindStringSubmatch(line); len(m) > 1 {
					item := m[1]
					if len(factAtoms(item)) == 0 {
						continue // entity items must carry a concrete fact atom
					}
					quote := strings.TrimSpace(line)
					add("entity", item, quote, strings.TrimSpace(line))
					continue
				}
				if m := colonPairRE.FindStringSubmatch(line); len(m) > 2 {
					if len(factAtoms(m[2])) == 0 {
						continue
					}
					quote := strings.TrimSpace(line)
					add("entity", m[1]+": "+m[2], quote, quote)
				}
			}
		}
		if kinds["duration"] {
			for _, m := range durationAtomRE.FindAllString(text, 6) {
				add("duration", m, quoteAround(text, m), fmt.Sprintf("%s — \"%s\"", m, quoteAround(text, m)))
			}
		}
		if kinds["money"] {
			for _, m := range moneyRE.FindAllString(text, 6) {
				add("money", m, quoteAround(text, m), fmt.Sprintf("%s — \"%s\"", m, quoteAround(text, m)))
			}
		}
	}
	return out
}

// quoteAround returns a verbatim ≤120-char window of text containing value.
func quoteAround(text, value string) string {
	at := strings.Index(text, value)
	if at < 0 {
		return ""
	}
	start := at - 40
	if start < 0 {
		start = 0
	}
	end := at + len(value) + 60
	if end > len(text) {
		end = len(text)
	}
	span := strings.TrimSpace(text[start:end])
	if nl := strings.Index(span, "\n"); nl >= 0 {
		// Keep the line containing the value when the window spans lines.
		for _, line := range strings.Split(text[start:end], "\n") {
			if strings.Contains(line, value) {
				span = strings.TrimSpace(line)
				break
			}
		}
	}
	if len(span) > 120 {
		span = strings.TrimSpace(textbound.Bytes(span, 120))
	}
	return span
}

// factAtoms extracts verifiable fact atoms: ISO dates, verbose dates, money,
// durations, ticket-style identifiers, and bare numbers.
func factAtoms(s string) []string {
	var out []string
	out = append(out, isoDateRE.FindAllString(s, -1)...)
	out = append(out, yearMonthRE.FindAllString(s, -1)...)
	out = append(out, moneyRE.FindAllString(s, -1)...)
	out = append(out, durationAtomRE.FindAllString(s, -1)...)
	out = append(out, atomIdentRE.FindAllString(s, -1)...)
	out = append(out, atomNumberRE.FindAllString(s, -1)...)
	return uniqueStringsStable(out)
}

// candidateCovered is true when every fact atom of the candidate already
// appears in the answer (word-bounded for numbers), or — for atomless values
// like owner/status — when the value text is present. Entity candidates
// additionally require the leading name to be present so completeness
// enumeration re-adds a fact whose threshold leaked in without its customer.
func candidateCovered(answer string, c slotCandidate) bool {
	low := normText(answer)
	if c.kind == "entity" {
		if idx := strings.Index(c.value, ":"); idx > 0 && idx <= 60 {
			name := strings.TrimSpace(c.value[:idx])
			if name != "" && !strings.Contains(low, normText(name)) {
				return false
			}
		}
	}
	atoms := factAtoms(c.value)
	if len(atoms) == 0 {
		return strings.Contains(low, normText(c.value))
	}
	for _, a := range atoms {
		if !atomInText(a, low) {
			return false
		}
	}
	return true
}

// atomInText checks pack/answer support for one atom. Numbers match on word
// boundaries so "2" does not ride on "2026" or "250".
func atomInText(atom, bagNorm string) bool {
	a := strings.TrimSpace(atom)
	if a == "" {
		return true
	}
	if atomNumberRE.MatchString(a) && !strings.ContainsAny(a, "$%-:") {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(a) + `\b`)
		return re.MatchString(bagNorm)
	}
	return strings.Contains(bagNorm, normText(a))
}

// filterQualifierConflicts drops candidates whose evidence declares a
// different value for a qualifier the question pins — the answer-side
// analogue of the #270 constrained reranker.
func filterQualifierConflicts(question string, cands []slotCandidate, diag map[string]any) ([]slotCandidate, map[string]any) {
	want := extractPassageQualifiers(question)
	if len(want) == 0 {
		return cands, diag
	}
	var kept []slotCandidate
	dropped := 0
	for _, c := range cands {
		conflict := false
		have := map[string][]string{}
		for _, q := range extractPassageQualifiers(c.blob) {
			have[q.key] = append(have[q.key], q.value)
		}
		for _, q := range want {
			if containsQualifierValue(c.blob, q.value) {
				continue
			}
			if len(have[q.key]) > 0 {
				conflict = true
				break
			}
		}
		if conflict {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	if dropped > 0 {
		diag["slot_fill_qualifier_dropped"] = dropped
	}
	return kept, diag
}

// resolveSlotConflicts keeps one current value per quantity kind when the
// pack disagrees, ranking by the #270 signals: validity window at the
// question's date anchor, document date, currency (superseding/draft), and
// authority. Losers are rejected, never appended.
func resolveSlotConflicts(question string, cands []slotCandidate, diag map[string]any) ([]slotCandidate, map[string]any) {
	anchor, hasAnchor := questionDate(question)
	groups := map[string][]int{}
	var order []string
	for i, c := range cands {
		if _, ok := groups[c.kind]; !ok {
			order = append(order, c.kind)
		}
		groups[c.kind] = append(groups[c.kind], i)
	}
	drop := map[int]bool{}
	for _, kind := range order {
		idxs := groups[kind]
		if len(idxs) < 2 {
			continue
		}
		values := map[string]bool{}
		for _, i := range idxs {
			values[normText(cands[i].value)] = true
		}
		if len(values) < 2 {
			continue // same value repeated — dedupe handles it, not a conflict
		}
		score := func(i int) (int, float64, float64, float64) {
			c := cands[i]
			validity := 0
			if hasAnchor {
				validity = passageValidityAt(Passage{Text: c.blob, DocumentID: c.docID, SourceURI: c.blob}, anchor)
			}
			dateKey := float64(0)
			if c.hasDate {
				dateKey = float64(c.date.Unix())
			}
			return validity, dateKey, c.cur, c.auth
		}
		best := idxs[0]
		bv, bd, bc, ba := score(best)
		for _, i := range idxs[1:] {
			iv, id, ic, ia := score(i)
			if iv > bv || iv == bv && (id > bd || id == bd && (ic > bc || ic == bc && ia > ba)) {
				best, bv, bd, bc, ba = i, iv, id, ic, ia
			}
		}
		for _, i := range idxs {
			if i != best {
				drop[i] = true
			}
		}
	}
	if len(drop) == 0 {
		return cands, diag
	}
	var kept []slotCandidate
	for i, c := range cands {
		if !drop[i] {
			kept = append(kept, c)
		}
	}
	diag["slot_fill_conflict_dropped"] = len(drop)
	return kept, diag
}

// rejectUnsupportedExtras drops answer lines that assert fact atoms absent
// from every pack passage (unsupported extras), including lines left with
// stripUngroundedFacts placeholders. Atomless lines always survive, and a
// rejection never empties the answer. Returns the cleaned answer and the
// rejection diagnostics.
func rejectUnsupportedExtras(answer string, passages []Passage, questionType string) (string, map[string]any) {
	diag := map[string]any{"unsupported_extras_rejected": 0}
	if strings.TrimSpace(answer) == "" || len(passages) == 0 ||
		strings.EqualFold(questionType, "info_not_found") || looksLikeAbstention(answer) {
		return answer, diag
	}
	bag := strings.Builder{}
	for _, p := range passages {
		if p.DocumentID == "" || isConversationPassage(p) {
			continue
		}
		bag.WriteString(p.Text)
		bag.WriteByte('\n')
	}
	bagNorm := normText(bag.String())
	if bagNorm == "" {
		return answer, diag
	}
	lines := strings.Split(answer, "\n")
	kept := make([]string, 0, len(lines))
	rejected := 0
	var rejectedAtoms []string
	for _, line := range lines {
		body := listMarkerRE.ReplaceAllString(line, "")
		atoms := factAtoms(body)
		placeholder := strings.Contains(line, "not in retrieved documents]")
		unsupported, supported := 0, 0
		for _, a := range atoms {
			if atomInText(a, bagNorm) {
				supported++
			} else {
				unsupported++
				if len(rejectedAtoms) < 8 {
					rejectedAtoms = append(rejectedAtoms, a)
				}
			}
		}
		drop := placeholder && strings.TrimSpace(body) != ""
		if !drop && unsupported > 0 && supported == 0 && strings.TrimSpace(body) != "" {
			drop = true
		}
		if drop {
			rejected++
			continue
		}
		kept = append(kept, line)
	}
	if rejected == 0 {
		return answer, diag
	}
	out := strings.Join(kept, "\n")
	if strings.TrimSpace(out) == "" {
		// Never let rejection empty the answer.
		return answer, diag
	}
	diag["unsupported_extras_rejected"] = rejected
	if len(rejectedAtoms) > 0 {
		diag["unsupported_extras_atoms"] = rejectedAtoms
	}
	return out, diag
}

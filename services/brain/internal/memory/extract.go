package memory

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

// High-precision deterministic claim extract patterns (not noisy OpenIE).
// Patterns: "X is Y", "X costs $N", "X has role Y", "price of X is Y",
// "X price is $N", "X painted/colored Y", "X measures/equals Y",
// "X has Y", "X: Y" short lines, "CEO of X is Y", "X = Y", revenue/target.
var (
	reIs       = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+is\s+([A-Za-z0-9][\w\s\.\-%$]{0,60}?)(?:[.!?;,\n]|$)`)
	reCosts    = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+costs\s+(\$?\d[\d,]*(?:\.\d+)?(?:\s*(?:USD|usd|dollars?))?)\b`)
	reHasRole  = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+has\s+role\s+([A-Za-z][\w\s\-]{1,40}?)\b`)
	rePriceOf  = regexp.MustCompile(`(?i)\bprice\s+of\s+([A-Za-z][\w\s\-]{1,40}?)\s+is\s+(\$?\d[\d,]*(?:\.\d+)?(?:\s*(?:USD|usd|dollars?))?)\b`)
	reXPriceIs = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+price\s+is\s+(\$?\d[\d,]*(?:\.\d+)?(?:\s*(?:USD|usd|dollars?))?)\b`)
	rePainted  = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:are|is)\s+painted\s+([A-Za-z][\w\-]{2,30})\b`)
	reColorIs  = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:color|colour)\s+is\s+([A-Za-z][\w\-]{2,30})\b`)
	reEquals   = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:equals|measures)\s+([A-Za-z0-9][\w\s\.\-%$]{0,40}?)\b`)
	reRoleOf   = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+role\s+is\s+([A-Za-z][\w\s\-]{1,40}?)\b`)
	// Stronger high-precision patterns.
	reHasY  = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+has\s+([A-Za-z0-9][\w\s\.\-%$]{1,40}?)(?:[.!?;,\n]|$)`)
	reCEOOf = regexp.MustCompile(`(?i)\bCEO\s+of\s+([A-Za-z][\w\s\-]{1,40}?)\s+is\s+([A-Za-z][\w\s\-]{1,40}?)\b`)
	// "Ada is CEO of Acme" → Acme ceo Ada
	reIsCEOOf = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+is\s+CEO\s+of\s+([A-Za-z][\w\s\-]{1,40}?)\b`)
	reEqSign  = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s*=\s*([A-Za-z0-9][\w\s\.\-%$]{0,40}?)(?:[.!?;,\n]|$)`)
	reRevenue = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:revenue|ARR|MRR)\s+(?:is|of|reaches)?\s*(\$?\d[\d,]*(?:\.\d+)?(?:\s*(?:USD|usd|M|B|million|billion)?)?)\b`)
	reTarget  = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+target\s+(?:is|of)?\s*([A-Za-z0-9][\w\s\.\-%$]{1,40}?)(?:[.!?;,\n]|$)`)
	reColonKV = regexp.MustCompile(`^([A-Za-z][\w\s\-]{1,40}?):\s*([A-Za-z0-9][\w\s\.\-%$]{1,60})$`)
	// OpenIE-light denser patterns (still deterministic — not neural OpenIE).
	reLocatedIn = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:is\s+)?(?:located|based|headquartered)\s+in\s+([A-Za-z][\w\s\-,]{1,40}?)\b`)
	reOwns      = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:owns|acquired|acquired by)\s+([A-Za-z][\w\s\-]{1,40}?)\b`)
	reFounded   = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:was\s+)?founded\s+(?:in|by)\s+([A-Za-z0-9][\w\s\-]{1,40}?)\b`)
	reWorksAt   = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:works|worked)\s+at\s+([A-Za-z][\w\s\-]{1,40}?)\b`)
	reReported  = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+reported\s+([A-Za-z0-9][\w\s\.\-%$]{1,40}?)\b`)
	reBetween   = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,30}?)\s+between\s+([A-Za-z][\w\s\-]{1,30}?)\s+and\s+([A-Za-z][\w\s\-]{1,30}?)\b`)
	reOfIs      = regexp.MustCompile(`(?i)\b([A-Za-z][\w\-]{2,30})\s+of\s+([A-Za-z][\w\s\-]{1,40}?)\s+is\s+([A-Za-z0-9][\w\s\.\-%$]{0,40}?)(?:[.!?;,\n]|$)`)
	reAnnounced = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+announced\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	// Ops / SLA / product denser OpenIE-light (GAP-MEM-OPENIE quality).
	reRPO = regexp.MustCompile(`(?i)\b(?:RPO|recovery\s+point\s+objective)\s+(?:is|of|=)\s*(\d[\d\s]*(?:hours?|days?|minutes?|h|d|m)?)\b`)
	reRTO = regexp.MustCompile(`(?i)\b(?:RTO|recovery\s+time\s+objective)\s+(?:is|of|=)\s*(\d[\d\s]*(?:hours?|days?|minutes?|h|d|m)?)\b`)
	// Entity-scoped recovery objectives: "MedThink ... sets RPO to 15 minutes".
	reEntityRPO = regexp.MustCompile(`(?i)\b([A-Za-z][\w\-]{1,40})\b(?:[^.\n]{0,80}?)\b(?:sets?\s+)?(?:RPO|recovery\s+point\s+objective)\s+(?:to|at|of|=|is)\s*(\d[\d\s]*(?:hours?|days?|minutes?|h|d|m)?)\b`)
	reEntityRTO = regexp.MustCompile(`(?i)\b([A-Za-z][\w\-]{1,40})\b(?:[^.\n]{0,80}?)\b(?:sets?\s+)?(?:RTO|recovery\s+time\s+objective)\s+(?:to|at|of|=|is)\s*(\d[\d\s]*(?:hours?|days?|minutes?|h|d|m)?)\b`)
	// Credit / SLA ladder: "X: 10 percent credit for …" / "X SLA: N percent for …"
	rePercentCredit = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:SLA[:\s]+)?(\d{1,3}\s*percent)\s+credit\b`)
	reRequires      = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+requires\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reSupports      = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+supports\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reUses          = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+uses\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reVersion       = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+version\s+(?:is\s+)?(v?\d[\w\.\-]{0,20})\b`)
	reSLA           = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+SLA\s+(?:is\s+)?(\d{1,3}%|\d+\s*nines?)\b`)
	// General product / org graph (not ERB-only): dependency, ownership, integration.
	reDependsOn      = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+depends\s+on\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reProvides       = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+provides\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reIntegratesWith = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+integrates\s+with\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reOwnedBy        = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:is\s+)?owned\s+by\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reReportsTo      = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+reports\s+to\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	rePoweredBy      = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:is\s+)?powered\s+by\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reManages        = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+manages\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
	reResponsibleFor = regexp.MustCompile(`(?i)\b([A-Za-z][\w\s\-]{1,40}?)\s+(?:is\s+)?responsible\s+for\s+([A-Za-z0-9][\w\s\.\-%$]{1,50}?)(?:[.!?;,\n]|$)`)
)

// isAttributeSubjectSuffixes: if reIs subject ends with these, it is likely
// "X price is $N" / "X colour is red" already handled by specialized patterns —
// do not emit a bogus (subject="X price", pred="is") claim.
var isAttributeSubjectSuffixes = []string{
	" price", " cost", " costs", " colour", " color", " role",
}

// stopSubjects filters low-precision "X is Y" subjects.
var stopSubjects = map[string]struct{}{
	"it": {}, "this": {}, "that": {}, "there": {}, "here": {}, "what": {},
	"which": {}, "who": {}, "they": {}, "he": {}, "she": {}, "we": {},
	"one": {}, "some": {}, "any": {}, "all": {}, "each": {}, "every": {},
	"the": {}, "a": {}, "an": {}, "these": {}, "those": {},
}

// stopObjects filters vacuous objects.
var stopObjects = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "it": {}, "this": {}, "that": {},
	"true": {}, "false": {}, "important": {}, "possible": {}, "available": {},
}

// ExtractClaimsFromText runs high-precision deterministic patterns over doc text.
// Provenance is det_extract; DocumentIDs=[docID]. Does not mutate the store.
func ExtractClaimsFromText(docID, text string) []Claim {
	text = strings.TrimSpace(text)
	if text == "" || docID == "" {
		return nil
	}
	now := time.Now().UTC()
	var out []Claim
	seen := map[string]struct{}{}

	add := func(sub, pred, obj, match string, start, end int) {
		sub = normalizeClaimPart(sub)
		pred = normalizeClaimPart(pred)
		obj = normalizeClaimPart(obj)
		if sub == "" || pred == "" || obj == "" {
			return
		}
		if _, bad := stopSubjects[strings.ToLower(sub)]; bad {
			return
		}
		if _, bad := stopObjects[strings.ToLower(obj)]; bad {
			return
		}
		// Subject must look like a proper phrase (starts with letter, ≥2 chars).
		if !looksLikeEntity(sub) || len(obj) < 1 {
			return
		}
		key := strings.ToLower(sub) + "|" + strings.ToLower(pred) + "|" + strings.ToLower(obj)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		cl := Claim{
			Subject:     sub,
			Predicate:   pred,
			Object:      obj,
			DocumentIDs: []string{docID},
			ValidFrom:   now,
			ObservedAt:  now,
			Status:      ClaimActive,
			Provenance:  "det_extract",
			SpanDocID:   docID,
			SpanStart:   start,
			SpanEnd:     end,
			SpanText:    strings.TrimSpace(match),
		}
		out = append(out, cl)
	}
	// Two-group patterns: sub=1, obj=2, fixed predicate.
	run2 := func(re *regexp.Regexp, pred string) {
		for _, idx := range re.FindAllStringSubmatchIndex(text, -1) {
			if len(idx) < 6 {
				continue
			}
			full, sub, obj := text[idx[0]:idx[1]], text[idx[2]:idx[3]], text[idx[4]:idx[5]]
			add(sub, pred, obj, full, idx[0], idx[1])
		}
	}
	run2(rePriceOf, "price")
	run2(reXPriceIs, "price")
	run2(reCosts, "costs")
	run2(reHasRole, "role")
	run2(reRoleOf, "role")
	run2(rePainted, "color")
	run2(reColorIs, "color")
	run2(reEquals, "equals")
	run2(reCEOOf, "ceo")
	// "Ada is CEO of Acme" → subject=Acme pred=ceo obj=Ada (company→person).
	for _, idx := range reIsCEOOf.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 6 {
			continue
		}
		person, company := text[idx[2]:idx[3]], text[idx[4]:idx[5]]
		add(company, "ceo", person, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	run2(reRevenue, "revenue")
	run2(reTarget, "target")
	run2(reLocatedIn, "located_in")
	run2(reOwns, "owns")
	run2(reFounded, "founded")
	run2(reWorksAt, "works_at")
	for _, idx := range reReported.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 6 {
			continue
		}
		obj := text[idx[4]:idx[5]]
		if wordCount(obj) > 6 {
			continue
		}
		add(text[idx[2]:idx[3]], "reported", obj, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	// Three-group: "CEO of Acme is Bob" → sub=Acme pred=ceo obj=Bob
	for _, idx := range reOfIs.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 8 {
			continue
		}
		pred := strings.ToLower(text[idx[2]:idx[3]])
		sub := text[idx[4]:idx[5]]
		obj := text[idx[6]:idx[7]]
		add(sub, pred, obj, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	for _, idx := range reAnnounced.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 6 {
			continue
		}
		obj := text[idx[4]:idx[5]]
		if wordCount(obj) > 8 {
			continue
		}
		add(text[idx[2]:idx[3]], "announced", obj, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	// Entity-scoped RPO/RTO first (prefer MedThink--rpo_minutes-->15 over bare RPO).
	run2(reEntityRPO, "rpo_minutes")
	run2(reEntityRTO, "rto_minutes")
	// Bare recovery objectives — subject is fixed product term.
	for _, idx := range reRPO.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 4 {
			continue
		}
		add("RPO", "is", text[idx[2]:idx[3]], text[idx[0]:idx[1]], idx[0], idx[1])
	}
	for _, idx := range reRTO.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 4 {
			continue
		}
		add("RTO", "is", text[idx[2]:idx[3]], text[idx[0]:idx[1]], idx[0], idx[1])
	}
	run2(rePercentCredit, "credit_percent")
	run2(reRequires, "requires")
	run2(reSupports, "supports")
	run2(reUses, "uses")
	run2(reVersion, "version")
	run2(reSLA, "sla")
	// General product graph predicates (company docs, not just SLA/RPO).
	run2(reDependsOn, "depends_on")
	run2(reProvides, "provides")
	run2(reIntegratesWith, "integrates_with")
	run2(reOwnedBy, "owned_by")
	run2(reReportsTo, "reports_to")
	run2(rePoweredBy, "powered_by")
	run2(reManages, "manages")
	run2(reResponsibleFor, "responsible_for")
	for _, idx := range reBetween.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 8 {
			continue
		}
		obj := text[idx[4]:idx[5]] + " and " + text[idx[6]:idx[7]]
		add(text[idx[2]:idx[3]], "between", obj, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	for _, idx := range reEqSign.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 6 {
			continue
		}
		obj := text[idx[4]:idx[5]]
		if wordCount(strings.TrimSpace(obj)) > 6 {
			continue
		}
		add(text[idx[2]:idx[3]], "equals", obj, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	for _, idx := range reHasY.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 6 {
			continue
		}
		obj := strings.TrimSpace(text[idx[4]:idx[5]])
		if strings.HasPrefix(strings.ToLower(obj), "role ") || wordCount(obj) > 6 {
			continue
		}
		add(text[idx[2]:idx[3]], "has", obj, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	// "X: Y" lines
	off := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		lineStart := off
		off += len(line) + 1
		if trimmed == "" || len(trimmed) > 100 {
			continue
		}
		if m := reColonKV.FindStringSubmatch(trimmed); len(m) == 3 {
			sub, obj := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
			if wordCount(sub) <= 5 && wordCount(obj) <= 8 {
				add(sub, "is", obj, trimmed, lineStart, lineStart+len(line))
			}
		}
	}
	// Generic "X is Y" last.
	for _, idx := range reIs.FindAllStringSubmatchIndex(text, -1) {
		if len(idx) < 6 {
			continue
		}
		sub := strings.TrimSpace(text[idx[2]:idx[3]])
		obj := strings.TrimSpace(text[idx[4]:idx[5]])
		if wordCount(obj) > 6 || strings.Contains(strings.ToLower(obj), " is ") {
			continue
		}
		if isAttributeSubject(sub) {
			continue
		}
		add(sub, "is", obj, text[idx[0]:idx[1]], idx[0], idx[1])
	}
	return out
}

func isAttributeSubject(sub string) bool {
	low := strings.ToLower(strings.TrimSpace(sub))
	for _, suf := range isAttributeSubjectSuffixes {
		if strings.HasSuffix(low, suf) {
			return true
		}
	}
	return false
}

func normalizeClaimPart(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, " \t\"'`.,;:!?()[]{}")
	// Collapse internal whitespace.
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func looksLikeEntity(s string) bool {
	if len(s) < 2 {
		return false
	}
	r := []rune(s)
	if !unicode.IsLetter(r[0]) {
		return false
	}
	// Reject pure stopword phrases.
	low := strings.ToLower(s)
	if _, ok := stopSubjects[low]; ok {
		return false
	}
	return true
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}

package hosted

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/textbound"
)

// Source authority weights for conflict-aware soft ranking (ported from
// Python live/authority.py). Higher = more authoritative for policy/conflict.
var sourceAuthority = map[string]float64{
	"confluence":   1.0,
	"notion":       0.95,
	"google_drive": 0.9,
	"gdrive":       0.9,
	"github":       0.85,
	"linear":       0.8,
	"jira":         0.8,
	"slack":        0.55,
	"gmail":        0.5,
	"email":        0.5,
	"fireflies":    0.45,
	"video":        0.45,
	"meeting":      0.45,
}

var (
	dateYearRE     = regexp.MustCompile(`\b(20[12]\d)\b`)
	isoDateLooseRE = regexp.MustCompile(`\b(20[12]\d)-(\d{2})-(\d{2})\b`)
	currencyPosRE  = regexp.MustCompile(`(?i)\b(supersed(?:es|ed|ing)?|current\s+policy|updated\s+policy|goes\s+live|effective\s+20|the\s+newest\s+version|revised\s+policy|now\s+provides|company-?wide\s+policy)\b`)
	currencyNegRE  = regexp.MustCompile(`(?i)\b(draft\s+note|earlier\s+note|an\s+older\s+version|old\s+policy|superseded\s+by|obsolete|mistaken|wrongly|pilot\s+only|suggested)\b`)
)

// authorityForPassage returns heuristic authority in [0, 1].
func authorityForPassage(p Passage, sourceTypes []string) float64 {
	blob := strings.ToLower(p.DocumentID + " " + p.SourceURI + " " + p.Channel + " " + truncate(p.Text, 500))
	best := 0.5
	for key, weight := range sourceAuthority {
		if strings.Contains(blob, key) {
			if weight > best {
				best = weight
			}
		}
		for _, st := range sourceTypes {
			if strings.Contains(strings.ToLower(st), key) {
				if weight > best {
					best = weight
				}
			}
		}
	}
	// Durable policy language beats chat-only even when channel is ambiguous.
	if strings.Contains(blob, "policy") || strings.Contains(blob, "handbook") ||
		strings.Contains(blob, "effective ") || strings.Contains(blob, "confluence") {
		if best < 0.85 {
			best = 0.85
		}
	}
	return best
}

// passageTime extracts the most specific document date as time.Time.
// Prefers passageDocDate (effective/updated ISO) then any ISO in the head.
// Zero time means unknown — never invent a date.
func passageTime(p Passage) (time.Time, bool) {
	if ds := passageDocDate(p); ds != "" {
		if t, err := time.Parse("2006-01-02", ds); err == nil {
			return t, true
		}
	}
	scan := p.Text
	scan = textbound.Bytes(scan, 4000)
	if m := isoDateLooseRE.FindAllStringSubmatch(scan, -1); len(m) > 0 {
		var best time.Time
		found := false
		for _, g := range m {
			if len(g) < 4 {
				continue
			}
			y, _ := strconv.Atoi(g[1])
			mo, _ := strconv.Atoi(g[2])
			d, _ := strconv.Atoi(g[3])
			if y < 1990 || y > 2100 || mo < 1 || mo > 12 || d < 1 || d > 31 {
				continue
			}
			t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
			if !found || t.After(best) {
				best = t
				found = true
			}
		}
		if found {
			return best, true
		}
	}
	// Year-only is last resort (Jan 1 of that year) — still an honest calendar
	// anchor, not a decade squash.
	years := dateYearRE.FindAllString(scan, -1)
	maxY := 0
	for _, y := range years {
		n, err := strconv.Atoi(y)
		if err != nil {
			continue
		}
		if n > maxY {
			maxY = n
		}
	}
	if maxY >= 1990 && maxY <= 2100 {
		return time.Date(maxY, 1, 1, 0, 0, 0, 0, time.UTC), true
	}
	return time.Time{}, false
}

// parseRecencyScore is kept for diagnostics / undated soft prior only.
// Prefer passageTime + multi-key sort for actual ranking — do not squash
// ISO dates onto a multi-year [0,1] axis (May vs June of the same year
// collapsed to ~noise on a 2018–2027 horizon).
func parseRecencyScore(text string) float64 {
	p := Passage{Text: text}
	t, ok := passageTime(p)
	if !ok {
		return 0.5
	}
	// Day-resolution prior relative to "now" for undated-mix diagnostics only.
	// Range ~±10y still leaves one day ≈ 1.4e-4 of the unit interval.
	now := time.Now().UTC()
	days := now.Sub(t).Hours() / 24
	// Newer (negative age) → higher score. Map age -3650..+3650 → ~1..0.
	v := 0.5 - days/7300.0
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// currencyScore: soft boost for superseding/current policy language vs draft/older.
func currencyScore(text string) float64 {
	if text == "" {
		return 0.5
	}
	scan := text
	scan = textbound.Bytes(scan, 2500)
	pos := len(currencyPosRE.FindAllString(scan, -1))
	neg := len(currencyNegRE.FindAllString(scan, -1))
	// Map to [0,1] around 0.5.
	v := 0.5 + 0.12*float64(pos) - 0.15*float64(neg)
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// policyConflictSurface: question asks for current policy facts that often have
// superseding revisions (leave days, retention, RPO, "current", …).
func policyConflictSurface(question, questionType string) bool {
	if strings.EqualFold(strings.TrimSpace(questionType), "conflicting_info") {
		return true
	}
	q := strings.ToLower(question)
	// Soft recency reorder surface — NOT the same as inventing SUPERSEDING labels.
	// Keep leave/policy so applyAuthorityRecency can prefer dated docs; supersession
	// mark path uses wantsGlobalNewestMark (narrower).
	for _, c := range []string{
		"current policy", "current leave", "supersed", "which is correct",
		"days of leave", "days paid", "paid leave",
		"bereavement", "parental leave", "retention period", "retention window",
		"latest policy", "updated policy", "most recent policy",
	} {
		if strings.Contains(q, c) {
			return true
		}
	}
	return false
}

// applyAuthorityRecency soft-reorders the window for ALL question types.
// Does not drop passages — only reorders.
func applyAuthorityRecency(passages []Passage, questionType string, sourceTypes []string) ([]Passage, map[string]any) {
	return applyAuthorityRecencyQ(passages, "", questionType, sourceTypes)
}

// applyAuthorityRecencyQ reorders by accurate calendar dates first, then soft
// signals. Dates are never flattened onto a multi-year [0,1] scale for the
// primary comparison — two ISO dates differ by exact day order.
//
// Order of keys (conflict / policy surface):
//  1. session turns stay front
//  2. dated passages: newer calendar date first (exact time.Time)
//  3. dated beats undated when the question is conflict/policy-shaped
//  4. currency language, source authority, CE score as ties
//
// Soft blend only for non-conflict types or undated packs.
func applyAuthorityRecencyQ(passages []Passage, question, questionType string, sourceTypes []string) ([]Passage, map[string]any) {
	if len(passages) < 2 {
		return passages, map[string]any{"authority_rank": "skipped_small"}
	}
	qtype := strings.ToLower(strings.TrimSpace(questionType))
	conflictish := qtype == "conflicting_info" || policyConflictSurface(question, qtype)
	medium := qtype == "constrained" || qtype == "project_related" || qtype == "completeness"
	validityAnchor, hasValidityAnchor := questionDate(question)

	type scored struct {
		turn     bool
		hasDate  bool
		date     time.Time
		auth     float64
		cur      float64
		baseN    float64
		soft     float64 // secondary blend only
		validity int
		orig     int
		p        Passage
		dateISO  string
	}
	rows := make([]scored, 0, len(passages))
	datedN := 0
	for i, p := range passages {
		auth := authorityForPassage(p, sourceTypes)
		cur := currencyScore(p.Text)
		base := p.Score
		if base <= 0 {
			base = 1.0 / float64(i+1)
		}
		baseN := base
		if baseN > 1 {
			baseN = 1.0 / float64(i+1)
		}
		t, ok := passageTime(p)
		iso := ""
		if ok {
			iso = t.Format("2006-01-02")
			datedN++
		}
		// Soft blend is secondary (undated / non-conflict). Never the only
		// comparator when real dates exist.
		var soft float64
		switch {
		case conflictish:
			soft = 0.50*cur + 0.35*auth + 0.15*baseN
		case medium:
			soft = 0.30*cur + 0.30*auth + 0.40*baseN
		default:
			soft = 0.30*cur + 0.25*auth + 0.45*baseN
		}
		validity := 0
		if hasValidityAnchor {
			validity = passageValidityAt(p, validityAnchor)
		}
		rows = append(rows, scored{
			turn:     strings.HasPrefix(p.DocumentID, "turn:") || p.Channel == "turn_grep",
			hasDate:  ok,
			date:     t,
			auth:     auth,
			cur:      cur,
			baseN:    baseN,
			soft:     soft,
			validity: validity,
			orig:     i,
			p:        p,
			dateISO:  iso,
		})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		// Session turns always front.
		if a.turn != b.turn {
			return a.turn
		}
		if a.turn && b.turn {
			return a.orig < b.orig
		}
		// An explicit question date makes passage validity primary. Unknown stays
		// between applicable and explicitly out-of-window evidence.
		if hasValidityAnchor && a.validity != b.validity {
			return a.validity > b.validity
		}
		// Conflict/policy: accurate date is primary key (day resolution).
		if conflictish {
			if a.hasDate && b.hasDate {
				if !a.date.Equal(b.date) {
					return a.date.After(b.date) // newer first
				}
				// Same day: currency language, then authority, then CE.
				if a.cur != b.cur {
					return a.cur > b.cur
				}
				if a.auth != b.auth {
					return a.auth > b.auth
				}
				if a.baseN != b.baseN {
					return a.baseN > b.baseN
				}
				return a.orig < b.orig
			}
			// Dated policy beats undated chat when dates disagree on presence.
			if a.hasDate != b.hasDate {
				return a.hasDate
			}
			// Both undated: soft signals.
			if a.soft != b.soft {
				return a.soft > b.soft
			}
			return a.orig < b.orig
		}
		// Medium / default: date still wins when both dated and differ by ≥1 day;
		// soft blend for undated or same-day ties (preserve CE more).
		if a.hasDate && b.hasDate && !a.date.Equal(b.date) {
			// Soft weight on date: newer wins unless soft gap is huge.
			// Honest calendar order first — user asked not to flatten.
			return a.date.After(b.date)
		}
		if a.hasDate != b.hasDate && medium {
			return a.hasDate
		}
		if a.soft != b.soft {
			return a.soft > b.soft
		}
		return a.orig < b.orig
	})

	out := make([]Passage, len(rows))
	for i, r := range rows {
		out[i] = r.p
	}
	diag := map[string]any{
		"authority_rank": "ok",
		"question_type":  qtype,
		"always_on":      true,
		"dated_n":        datedN,
		"date_primary":   conflictish || medium,
	}
	if conflictish {
		diag["authority_mode"] = "date_primary_conflict"
	} else if medium {
		diag["authority_mode"] = "date_primary_medium"
	} else {
		diag["authority_mode"] = "date_then_soft"
	}
	if hasValidityAnchor {
		diag["validity_anchor"] = validityAnchor.Format("2006-01-02")
	}
	if len(rows) > 0 {
		top := rows[0]
		diag["top_authority"] = top.auth
		if hasValidityAnchor {
			diag["top_validity"] = top.validity
		}
		diag["top_currency"] = top.cur
		diag["top_doc"] = top.p.DocumentID
		if top.hasDate {
			diag["top_date"] = top.dateISO
			diag["top_recency"] = parseRecencyScore(top.p.Text) // diagnostic only
		}
	}
	return out, diag
}

// factSlotChecklist returns heuristic slots multi-fact answers should cover.
// Port of live/authority.fact_slot_checklist.
func factSlotChecklist(question, questionType string) []string {
	qtype := strings.ToLower(strings.TrimSpace(questionType))
	q := strings.ToLower(question)
	var slots []string
	if qtype != "completeness" && qtype != "project_related" && qtype != "conflicting_info" &&
		qtype != "semantic" {
		return nil
	}
	for _, pair := range []struct{ kw, slot string }{
		{"rpo", "RPO value/units"},
		{"rto", "RTO value/units"},
		{"sla", "SLA targets"},
		{"owner", "owner / responsible party"},
		{"deadline", "deadline / date"},
		{"budget", "budget / cost"},
		{"risk", "risk / mitigation"},
		{"rollback", "rollback steps"},
		{"encrypt", "encryption requirements"},
		{"retention", "retention period per customer"},
		{"customer", "each customer name + exception"},
		{"which", "full set of matching entities"},
		{"timeout", "timeout conditions / root cause"},
		{"latency", "latency targets (p50/p95)"},
		{"token", "token counts / rates"},
		{"who", "named people/roles"},
		{"when", "timeline / dates"},
		{"where", "systems / environments"},
	} {
		if strings.Contains(q, pair.kw) {
			slots = append(slots, pair.slot)
		}
	}
	if qtype == "completeness" && len(slots) == 0 {
		return []string{
			"every matching entity/name in the pack",
			"numeric thresholds / retention days",
			"owners/roles if mentioned",
			"all channels or categories asked",
		}
	}
	if qtype == "project_related" && len(slots) == 0 {
		return []string{
			"project/initiative name",
			"status or timeline",
			"owners and related systems",
			"cross-document links (tickets, docs)",
		}
	}
	return slots
}

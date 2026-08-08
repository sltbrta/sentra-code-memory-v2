package hosted

import (
	"sort"
	"strings"
	"time"
)

// adjudicateSupersession makes version conflicts explicit in the pack for
// product path (works without Mem cortex — ERB path2).
//
// For near-duplicate groups with distinct document dates:
//   - the version applicable at an explicit question date leads; otherwise newest
//     (and/or highest currency) is marked SUPERSEDING
//   - older or out-of-window copies remain as the conflicting side
//   - window is reordered so the selected version leads (before bestLast)
//
// Does not drop passages — both sides stay for dual-state honesty on
// conflicting_info; synth is instructed to lead with current.
func adjudicateSupersession(passages []Passage, question string) ([]Passage, map[string]any) {
	diag := map[string]any{"supersession_adjudicate": false}
	if len(passages) < 2 {
		return passages, diag
	}
	validityAnchor, hasValidityAnchor := questionDate(question)
	groups := nearDupGroups(passages)
	if len(groups) == 0 {
		// No near-dups: only mark global newest on *true* conflict surfaces.
		// Never fire on bare "how many days" / quantity asks (audit #4).
		if wantsGlobalNewestMark(question, "") {
			return markGlobalNewest(passages, question, diag)
		}
		return passages, diag
	}

	type mark struct {
		role string // SUPERSEDING | SUPERSEDED
		date string
	}
	marks := map[int]mark{}
	supersedingN, supersededN := 0, 0

	for _, g := range groups {
		if len(g) < 2 {
			continue
		}
		type dated struct {
			idx      int
			t        time.Time
			ok       bool
			cur      float64
			validity int
		}
		rows := make([]dated, 0, len(g))
		for _, i := range g {
			t, ok := passageTime(passages[i])
			validity := 0
			if hasValidityAnchor {
				validity = passageValidityAt(passages[i], validityAnchor)
			}
			rows = append(rows, dated{
				idx: i, t: t, ok: ok, cur: currencyScore(passages[i].Text), validity: validity,
			})
		}
		// Prefer dated newest; undated sink unless they have strong currency alone.
		sort.SliceStable(rows, func(a, b int) bool {
			ra, rb := rows[a], rows[b]
			if hasValidityAnchor && ra.validity != rb.validity {
				return ra.validity > rb.validity
			}
			if ra.ok && rb.ok && !ra.t.Equal(rb.t) {
				return ra.t.After(rb.t)
			}
			if ra.ok != rb.ok {
				return ra.ok
			}
			if ra.cur != rb.cur {
				return ra.cur > rb.cur
			}
			return ra.idx < rb.idx
		})
		// Need a real date split or currency gap to declare supersession.
		winner := rows[0]
		hasSplit := false
		if winner.ok {
			for _, r := range rows[1:] {
				if r.ok && !r.t.Equal(winner.t) {
					hasSplit = true
					break
				}
			}
		}
		if !hasSplit && winner.cur-rows[len(rows)-1].cur < 0.15 {
			continue
		}
		wDate := ""
		if winner.ok {
			wDate = winner.t.Format("2006-01-02")
		}
		marks[winner.idx] = mark{role: "SUPERSEDING", date: wDate}
		supersedingN++
		for _, r := range rows[1:] {
			d := ""
			if r.ok {
				d = r.t.Format("2006-01-02")
			}
			marks[r.idx] = mark{role: "SUPERSEDED", date: d}
			supersededN++
		}
	}

	if len(marks) == 0 {
		if wantsGlobalNewestMark(question, "") {
			return markGlobalNewest(passages, question, diag)
		}
		return passages, diag
	}

	out := make([]Passage, len(passages))
	copy(out, passages)
	for i, m := range marks {
		body := stripSupersessionHeaders(stripRecencyHeaders(out[i].Text))
		var tag string
		switch m.role {
		case "SUPERSEDING":
			if hasValidityAnchor {
				tag = "[APPLICABLE at requested date " + validityAnchor.Format("2006-01-02") + " — prefer for this question"
				if m.date != "" {
					tag += "; document date: " + m.date
				}
				tag += "]"
			} else if m.date != "" {
				tag = "[SUPERSEDING version — prefer this as current; document date: " + m.date + "]"
			} else {
				tag = "[SUPERSEDING version — prefer this as current]"
			}
		case "SUPERSEDED":
			if hasValidityAnchor {
				tag = "[ALTERNATE version at requested date " + validityAnchor.Format("2006-01-02")
				if m.date != "" {
					tag += "; document date: " + m.date
				}
				tag += "]"
			} else if m.date != "" {
				tag = "[SUPERSEDED version — older; document date: " + m.date + "]"
			} else {
				tag = "[SUPERSEDED version — older / draft]"
			}
		}
		if tag != "" {
			out[i].Text = tag + "\n" + body
		}
	}

	// Reorder: SUPERSEDING first (among non-turn), then unmarked, then SUPERSEDED.
	// Turns stay front.
	type row struct {
		rank int
		orig int
		p    Passage
	}
	rows := make([]row, len(out))
	for i, p := range out {
		rank := 1
		if strings.HasPrefix(p.DocumentID, "turn:") || p.Channel == "turn_grep" {
			rank = -100
		} else if m, ok := marks[i]; ok {
			if m.role == "SUPERSEDING" {
				rank = 0
			} else if m.role == "SUPERSEDED" {
				rank = 2
			}
		}
		rows[i] = row{rank: rank, orig: i, p: p}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].rank != rows[j].rank {
			return rows[i].rank < rows[j].rank
		}
		return rows[i].orig < rows[j].orig
	})
	ordered := make([]Passage, len(rows))
	for i, r := range rows {
		ordered[i] = r.p
	}
	diag["supersession_adjudicate"] = true
	diag["superseding_n"] = supersedingN
	diag["superseded_n"] = supersededN
	diag["supersession_groups"] = len(groups)
	if hasValidityAnchor {
		diag["validity_anchor"] = validityAnchor.Format("2006-01-02")
	}
	return ordered, diag
}

// wantsGlobalNewestMark is the narrow gate for inventing a SUPERSEDING label
// without a near-dup group. Must NOT match ordinary quantity / recency questions.
func wantsGlobalNewestMark(question, questionType string) bool {
	if strings.EqualFold(strings.TrimSpace(questionType), "conflicting_info") {
		return true
	}
	q := strings.ToLower(question)
	// Explicit conflict / correction language only.
	for _, c := range []string{
		"supersed", "which is correct", "which is right", "or is it",
		"rather than", "instead of", "corrected", "was corrected",
		"earlier note", "after deeper", "telemetry review",
		"conflicting", "contradict", "disagrees with",
	} {
		if strings.Contains(q, c) {
			return true
		}
	}
	return false
}

func markGlobalNewest(passages []Passage, question string, diag map[string]any) ([]Passage, map[string]any) {
	bestI := -1
	var bestT time.Time
	anchor, hasAnchor := questionDate(question)
	bestValidity := -2
	for i, p := range passages {
		if strings.HasPrefix(p.DocumentID, "turn:") {
			continue
		}
		t, ok := passageTime(p)
		if !ok {
			continue
		}
		validity := 0
		if hasAnchor {
			validity = passageValidityAt(p, anchor)
		}
		if bestI < 0 || validity > bestValidity || validity == bestValidity && t.After(bestT) {
			bestI = i
			bestT = t
			bestValidity = validity
		}
	}
	if bestI < 0 {
		return passages, diag
	}
	out := make([]Passage, len(passages))
	copy(out, passages)
	body := stripSupersessionHeaders(stripRecencyHeaders(out[bestI].Text))
	if hasAnchor {
		out[bestI].Text = "[APPLICABLE among dated pack at requested date " + anchor.Format("2006-01-02") +
			" — document date: " + bestT.Format("2006-01-02") + "]\n" + body
		diag["validity_anchor"] = anchor.Format("2006-01-02")
		diag["top_validity"] = bestValidity
	} else {
		out[bestI].Text = "[SUPERSEDING among dated pack — document date: " +
			bestT.Format("2006-01-02") + "]\n" + body
	}
	// Move newest to front of non-turn region.
	if bestI > 0 && !strings.HasPrefix(out[0].DocumentID, "turn:") {
		neu := out[bestI]
		copy(out[1:bestI+1], out[0:bestI])
		out[0] = neu
	}
	diag["supersession_adjudicate"] = true
	diag["superseding_n"] = 1
	diag["superseded_n"] = 0
	diag["global_newest"] = true
	diag["top_date"] = bestT.Format("2006-01-02")
	return out, diag
}

func stripSupersessionHeaders(text string) string {
	lines := strings.Split(text, "\n")
	var keep []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[SUPERSEDING") || strings.HasPrefix(t, "[SUPERSEDED") ||
			strings.HasPrefix(t, "[APPLICABLE") || strings.HasPrefix(t, "[ALTERNATE") {
			continue
		}
		keep = append(keep, ln)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// preferSupersedingCites moves SUPERSEDING-marked docs to the front of cite list.
func preferSupersedingCites(cited []string, passages []Passage) []string {
	if len(cited) == 0 || len(passages) == 0 {
		return cited
	}
	super := map[string]struct{}{}
	for _, p := range passages {
		if strings.Contains(p.Text, "[SUPERSEDING") || strings.Contains(p.Text, "[APPLICABLE") {
			super[p.DocumentID] = struct{}{}
		}
	}
	if len(super) == 0 {
		return cited
	}
	var front, rest []string
	seen := map[string]struct{}{}
	for _, id := range cited {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := super[id]; ok {
			front = append(front, id)
		} else {
			rest = append(rest, id)
		}
	}
	return append(front, rest...)
}

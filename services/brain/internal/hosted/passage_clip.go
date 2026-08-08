package hosted

import "strings"

// storagePassageChars is the in-memory/hydrate budget for passage bodies.
// Larger than the LLM prompt clip so ground/temporal rebind still sees
// timeline and correction tails that the prompt may soft-clip.
//
// Hard prompt rates are still required (token cost + latency). Hard *storage*
// rates at the prompt size are not — that was dropping facts before rebind.
func storagePassageChars(promptMax int) int {
	if promptMax <= 0 {
		promptMax = 2400
	}
	s := promptMax * 2
	if s < 4000 {
		s = 4000
	}
	if s > 8000 {
		s = 8000
	}
	return s
}

// clipPassageText trims evidence for LLM prompt budgets without discarding
// the fact-bearing tail when possible.
//
// Hard cut at MaxPassageChars was dropping mid/late timeline lines and
// correction paragraphs (dates, "$…", "N months") that live past the first
// 1600 chars on longer ERB chunks. Prefer:
//  1. full text when under budget
//  2. head + fact windows (ISO dates, money, duration atoms) when over budget
//  3. plain head clip as last resort
func clipPassageText(text string, maxChars int) string {
	if maxChars <= 0 || len(text) <= maxChars {
		return text
	}
	// Reserve space for ellipsis + fact windows.
	headN := maxChars * 70 / 100
	if headN < 400 {
		headN = maxChars
		if headN > len(text) {
			return text
		}
		return text[:headN]
	}
	head := text[:headN]
	restBudget := maxChars - headN - 32 // "\n…\n" glue
	if restBudget < 120 {
		return text[:maxChars]
	}
	// Collect fact-bearing windows from the truncated tail.
	tail := text[headN:]
	var windows []string
	seen := map[string]struct{}{}
	addWin := func(at int, atomLen int) {
		if at < 0 {
			return
		}
		// Absolute position in full text.
		abs := headN + at
		pad := 90
		start := abs - pad
		if start < headN {
			start = headN
		}
		if start < 0 {
			start = 0
		}
		end := abs + atomLen + pad
		if end > len(text) {
			end = len(text)
		}
		w := strings.TrimSpace(text[start:end])
		if w == "" {
			return
		}
		key := w
		if len(key) > 40 {
			key = key[:40]
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		windows = append(windows, w)
	}
	// ISO dates in tail
	for _, m := range isoDateRE.FindAllStringIndex(tail, -1) {
		addWin(m[0], m[1]-m[0])
	}
	// Money
	for _, m := range moneyRE.FindAllStringIndex(tail, -1) {
		addWin(m[0], m[1]-m[0])
	}
	// Durations (months/days) — same atoms used by quantity rebind
	for _, m := range durationAtomRE.FindAllStringIndex(tail, -1) {
		addWin(m[0], m[1]-m[0])
	}
	// Correction cues without a nearby atom still matter for conflict Qs.
	lowTail := strings.ToLower(tail)
	for _, cue := range []string{"corrected", "correction", "telemetry review", "supersed", "no sustained"} {
		if i := strings.Index(lowTail, cue); i >= 0 {
			addWin(i, len(cue))
		}
	}
	if len(windows) == 0 {
		// No facts in tail — plain clip.
		if maxChars > len(text) {
			return text
		}
		return text[:maxChars]
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n…\n")
	used := headN + 3
	for _, w := range windows {
		if used+len(w)+1 > maxChars {
			// Fit a truncated window if room.
			room := maxChars - used - 1
			if room >= 40 {
				b.WriteString(w[:room])
			}
			break
		}
		b.WriteString(w)
		b.WriteByte('\n')
		used += len(w) + 1
	}
	out := b.String()
	if len(out) > maxChars {
		return out[:maxChars]
	}
	return out
}

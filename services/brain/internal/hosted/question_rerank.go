package hosted

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const questionAwareRerankLimit = 64

var (
	qualifierBeforeLabelRE = regexp.MustCompile(`(?i)\b([a-z][a-z0-9._/-]{1,31})\s+(tier|region|product|tenant|environment)\b`)
	qualifierAfterLabelRE  = regexp.MustCompile(`(?i)\b(tier|region|product|tenant|environment)\s*(?:[:=]\s*|\bis\s+)([a-z][a-z0-9._/-]{1,31})\b`)
	questionISODateRE      = regexp.MustCompile(`\b20[12]\d-\d{2}-\d{2}\b`)
	validFromRE            = regexp.MustCompile(`(?i)(?:valid[ _-]+from|effective(?:[ _-]+from)?|goes[ _-]+live|starts)[^\d]{0,24}(20[12]\d-\d{2}-\d{2})`)
	validUntilRE           = regexp.MustCompile(`(?i)(?:valid[ _-]+until|valid[ _-]+through|through|expires|ends)[^\d]{0,24}(20[12]\d-\d{2}-\d{2})`)
)

type passageQualifier struct {
	key   string
	value string
}

// questionAwareRerank is a bounded, deterministic pass over the existing
// authorized candidate pool. It never retrieves, drops, or manufactures a
// passage: generic lexical/dense/CE order remains the tie-break and the tail
// beyond questionAwareRerankLimit is untouched.
func questionAwareRerank(passages []Passage, question, questionType string, sourceTypes []string) ([]Passage, map[string]any) {
	diag := map[string]any{
		"question_aware_rerank": "skipped",
		"question_aware_type":   strings.ToLower(strings.TrimSpace(questionType)),
		"question_aware_limit":  questionAwareRerankLimit,
	}
	if len(passages) < 2 {
		diag["question_aware_reason"] = "small_pool"
		return passages, diag
	}

	out := append([]Passage(nil), passages...)
	n := min(len(out), questionAwareRerankLimit)
	diag["question_aware_n"] = n
	var applied bool
	switch diag["question_aware_type"] {
	case "constrained":
		applied = rerankConstrained(out[:n], question, diag)
	case "conflicting_info":
		applied = rerankConflicting(out[:n], question, sourceTypes, diag)
	case "intra_document", "intra_document_reasoning":
		applied = rerankIntraDocument(out[:n], question, diag)
	default:
		diag["question_aware_reason"] = "question_type"
		return passages, diag
	}
	if !applied {
		return passages, diag
	}
	if !samePassageCandidates(passages, out) {
		diag["question_aware_rerank"] = "fallback"
		diag["question_aware_reason"] = "candidate_set_changed"
		return passages, diag
	}
	diag["question_aware_rerank"] = "ok"
	diag["question_aware_preserved_candidates"] = true
	return out, diag
}

func rerankConstrained(passages []Passage, question string, diag map[string]any) bool {
	want := extractPassageQualifiers(question)
	if len(want) == 0 {
		diag["question_aware_reason"] = "no_qualifiers"
		return false
	}
	anchor, hasAnchor := questionDate(question)
	type row struct {
		p         Passage
		matches   int
		conflicts int
		validity  int
		orig      int
	}
	rows := make([]row, len(passages))
	for i, p := range passages {
		blob := p.DocumentID + " " + p.SourceURI + " " + p.Channel + " " + p.Text
		have := extractPassageQualifiers(blob)
		byKey := map[string][]string{}
		for _, q := range have {
			byKey[q.key] = append(byKey[q.key], q.value)
		}
		r := row{p: p, orig: i}
		if hasAnchor {
			r.validity = passageValidityAt(p, anchor)
		}
		for _, q := range want {
			if containsQualifierValue(blob, q.value) {
				r.matches++
				continue
			}
			if len(byKey[q.key]) > 0 {
				r.conflicts++
			}
		}
		rows[i] = r
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].matches != rows[j].matches {
			return rows[i].matches > rows[j].matches
		}
		if rows[i].conflicts != rows[j].conflicts {
			return rows[i].conflicts < rows[j].conflicts
		}
		if rows[i].validity != rows[j].validity {
			return rows[i].validity > rows[j].validity
		}
		return rows[i].orig < rows[j].orig
	})
	for i := range rows {
		passages[i] = rows[i].p
	}
	if rows[0].matches > 0 {
		markQuestionAwareFloor(&passages[0])
		diag["question_aware_promoted"] = 1
	}
	diag["qualifier_count"] = len(want)
	diag["qualifier_top_matches"] = rows[0].matches
	diag["qualifier_top_conflicts"] = rows[0].conflicts
	if hasAnchor {
		diag["validity_anchor"] = anchor.Format("2006-01-02")
		diag["validity_top"] = rows[0].validity
	}
	return true
}

func extractPassageQualifiers(text string) []passageQualifier {
	seen := map[string]struct{}{}
	var out []passageQualifier
	add := func(key, value string) {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "?.,;:/"))
		if key == "" || value == "" || qualifierNoise(value) {
			return
		}
		id := key + "=" + value
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, passageQualifier{key: key, value: value})
	}
	for _, m := range qualifierBeforeLabelRE.FindAllStringSubmatch(text, -1) {
		add(m[2], m[1])
	}
	for _, m := range qualifierAfterLabelRE.FindAllStringSubmatch(text, -1) {
		add(m[1], m[2])
	}
	return out
}

func qualifierNoise(value string) bool {
	switch value {
	case "the", "this", "that", "which", "what", "whose", "each", "any", "all", "our", "your", "their", "in", "for", "of", "is":
		return true
	default:
		return false
	}
}

func containsQualifierValue(text, value string) bool {
	if value == "" {
		return false
	}
	return regexp.MustCompile(`(?i)(^|[^a-z0-9])`+regexp.QuoteMeta(value)+`([^a-z0-9]|$)`).FindStringIndex(text) != nil
}

func rerankConflicting(passages []Passage, question string, sourceTypes []string, diag map[string]any) bool {
	anchor, hasAnchor := questionDate(question)
	type row struct {
		p         Passage
		pairOrder int // current/older pairs are interleaved before unrelated rows
		date      time.Time
		hasDate   bool
		cur       float64
		auth      float64
		validity  int
		orig      int
	}
	rows := make([]row, len(passages))
	for i, p := range passages {
		d, ok := passageTime(p)
		rows[i] = row{
			p: p, pairOrder: len(passages) * 2, date: d, hasDate: ok,
			cur: currencyScore(p.Text), auth: authorityForPassage(p, sourceTypes), orig: i,
		}
		if hasAnchor {
			rows[i].validity = passageValidityAt(p, anchor)
		}
	}

	groups := nearDupGroups(passages)
	dualGroups := 0
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		ordered := append([]int(nil), group...)
		sort.SliceStable(ordered, func(i, j int) bool {
			a, b := rows[ordered[i]], rows[ordered[j]]
			if a.validity != b.validity {
				return a.validity > b.validity
			}
			if a.hasDate && b.hasDate && !a.date.Equal(b.date) {
				return a.date.After(b.date)
			}
			if a.hasDate != b.hasDate {
				return a.hasDate
			}
			if a.cur != b.cur {
				return a.cur > b.cur
			}
			if a.auth != b.auth {
				return a.auth > b.auth
			}
			return a.orig < b.orig
		})
		winner, counterpart := ordered[0], ordered[1]
		if rows[winner].hasDate && rows[counterpart].hasDate && rows[winner].date.Equal(rows[counterpart].date) &&
			rows[winner].cur-rows[counterpart].cur < 0.15 {
			continue
		}
		rows[winner].pairOrder = dualGroups * 2
		rows[counterpart].pairOrder = dualGroups*2 + 1
		dualGroups++
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].pairOrder != rows[j].pairOrder {
			return rows[i].pairOrder < rows[j].pairOrder
		}
		if rows[i].validity != rows[j].validity {
			return rows[i].validity > rows[j].validity
		}
		if rows[i].hasDate && rows[j].hasDate && !rows[i].date.Equal(rows[j].date) {
			return rows[i].date.After(rows[j].date)
		}
		if rows[i].hasDate != rows[j].hasDate {
			return rows[i].hasDate
		}
		if rows[i].cur != rows[j].cur {
			return rows[i].cur > rows[j].cur
		}
		if rows[i].auth != rows[j].auth {
			return rows[i].auth > rows[j].auth
		}
		return rows[i].orig < rows[j].orig
	})
	for i := range rows {
		passages[i] = rows[i].p
	}
	promoted := 1
	if dualGroups > 0 {
		promoted = min(2, len(passages))
	}
	for i := 0; i < promoted; i++ {
		markQuestionAwareFloor(&passages[i])
	}
	diag["question_aware_promoted"] = promoted
	diag["conflict_dual_side_groups"] = dualGroups
	diag["conflict_supersession_reasoning"] = dualGroups > 0 || wantsGlobalNewestMark(question, "conflicting_info")
	if hasAnchor {
		diag["validity_anchor"] = anchor.Format("2006-01-02")
		diag["validity_top"] = rows[0].validity
	}
	return true
}

func questionDate(question string) (time.Time, bool) {
	value := questionISODateRE.FindString(question)
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", value)
	return t, err == nil
}

// passageValidityAt reads validity windows from existing passage text/metadata.
// Return values sort naturally: valid (1), unknown (0), explicitly invalid (-1).
func passageValidityAt(p Passage, anchor time.Time) int {
	blob := p.SourceURI + " " + p.Text
	var from, until time.Time
	if m := validFromRE.FindStringSubmatch(blob); len(m) > 1 {
		from, _ = time.Parse("2006-01-02", m[1])
	}
	if m := validUntilRE.FindStringSubmatch(blob); len(m) > 1 {
		until, _ = time.Parse("2006-01-02", m[1])
	}
	if from.IsZero() && until.IsZero() {
		return 0
	}
	if !from.IsZero() && anchor.Before(from) || !until.IsZero() && anchor.After(until) {
		return -1
	}
	return 1
}

func rerankIntraDocument(passages []Passage, question string, diag map[string]any) bool {
	anchor, hasAnchor := questionDate(question)
	anchorText := ""
	relation := "document_sequence"
	if hasAnchor {
		anchorText = anchor.Format("2006-01-02")
		relation = "as_of"
		lowQ := strings.ToLower(question)
		switch {
		case strings.Contains(lowQ, "after") || strings.Contains(lowQ, "since"):
			relation = "after"
		case strings.Contains(lowQ, "before") || strings.Contains(lowQ, "prior to"):
			relation = "before"
		}
	}
	ids := extractIdentifiers(question)
	targetDoc := passages[0].DocumentID
	bestHits := passageIdentifierHits(passages[0].DocumentID+" "+passages[0].SourceURI+" "+passages[0].Text, ids)
	for _, p := range passages[1:] {
		hits := passageIdentifierHits(p.DocumentID+" "+p.SourceURI+" "+p.Text, ids)
		if hits > bestHits {
			targetDoc, bestHits = p.DocumentID, hits
		}
	}
	type row struct {
		p          Passage
		idHits     int
		relates    bool
		distance   time.Duration
		structural bool
		orig       int
	}
	rows := make([]row, len(passages))
	for i, p := range passages {
		d, ok := passageTime(p)
		relates := p.DocumentID == targetDoc
		distance := time.Duration(1<<63 - 1)
		if hasAnchor {
			relates = false
			if ok {
				switch relation {
				case "after":
					relates = d.After(anchor)
				case "before":
					relates = d.Before(anchor)
				default:
					relates = !d.After(anchor)
				}
				if relates {
					distance = d.Sub(anchor)
					if distance < 0 {
						distance = -distance
					}
				}
			}
		}
		blob := p.DocumentID + " " + p.SourceURI + " " + p.Text
		rows[i] = row{
			p: p, idHits: passageIdentifierHits(blob, ids), relates: relates, distance: distance,
			structural: strings.Contains(strings.ToLower(p.Channel), "temporal"), orig: i,
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].idHits != rows[j].idHits {
			return rows[i].idHits > rows[j].idHits
		}
		if rows[i].relates != rows[j].relates {
			return rows[i].relates
		}
		if rows[i].relates && rows[i].distance != rows[j].distance {
			return rows[i].distance < rows[j].distance
		}
		if rows[i].structural != rows[j].structural {
			return rows[i].structural
		}
		return rows[i].orig < rows[j].orig
	})
	for i := range rows {
		passages[i] = rows[i].p
	}
	promoted := 0
	for i := 0; i < len(passages) && i < 2; i++ {
		if !rows[i].relates {
			break
		}
		markQuestionAwareFloor(&passages[i])
		promoted++
	}
	if promoted > 0 {
		diag["question_aware_promoted"] = promoted
	}
	diag["temporal_relation"] = relation
	if hasAnchor {
		diag["temporal_anchor"] = anchorText
	}
	return true
}

func markQuestionAwareFloor(p *Passage) {
	if p == nil || strings.Contains(p.Channel, "question_aware_floor") {
		return
	}
	p.Channel += "+question_aware_floor"
}

func samePassageCandidates(a, b []Passage) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, p := range a {
		counts[passageCandidateKey(p)]++
	}
	for _, p := range b {
		key := passageCandidateKey(p)
		counts[key]--
		if counts[key] < 0 {
			return false
		}
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

func passageCandidateKey(p Passage) string {
	// Score and Channel are ranker annotations, not candidate identity.
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", p.DocumentID, p.ChunkID, p.SourceURI, p.Text)
}

package hosted

import (
	"strings"
)

// pruneCitesToAnswerAtoms drops citations whose pack text does not share
// meaningful content with the answer (stricter generate2 cited-only).
//
// Rules:
//   - info_not_found / empty answer: leave cites as-is (caller handles empty).
//   - Keep a cite if passage overlaps answer tokens (≥2 content tokens ≥4 chars)
//     OR shares an identifier (INC-…, dsid fragment, money/duration atom).
//   - Never empty the list for a non-abstain answer: keep best-overlap cite.
//   - Gold docs in window are not special here (eval ensureGoldCites runs after).
func pruneCitesToAnswerAtoms(cited []string, answer string, passages []Passage, questionType string) []string {
	if len(cited) == 0 || len(passages) == 0 {
		return cited
	}
	qt := strings.ToLower(strings.TrimSpace(questionType))
	if qt == "info_not_found" {
		return cited
	}
	ans := strings.TrimSpace(answer)
	if ans == "" || looksLikeAbstention(ans) || shouldClearCitesOnAbstain(ans) {
		return cited
	}
	byDoc := map[string][]Passage{}
	for _, p := range passages {
		if p.DocumentID == "" {
			continue
		}
		byDoc[p.DocumentID] = append(byDoc[p.DocumentID], p)
	}
	ansToks := contentTokens(ans)
	ansIDs := extractIdentifiers(ans)
	// Also duration/money atoms from answer.
	ansAtoms := append([]string{}, ansIDs...)
	for _, m := range durationAtomRE.FindAllString(ans, -1) {
		ansAtoms = append(ansAtoms, strings.ToLower(m))
	}
	for _, m := range moneyAtomRE.FindAllString(ans, -1) {
		ansAtoms = append(ansAtoms, strings.ToLower(m))
	}

	type scored struct {
		id    string
		score int
	}
	var kept []scored
	for _, id := range cited {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ps := byDoc[id]
		if len(ps) == 0 {
			// Cite not in pack — drop (cannot verify).
			continue
		}
		sc := citeAnswerOverlapScore(ps, ansToks, ansAtoms)
		if sc >= 2 {
			kept = append(kept, scored{id: id, score: sc})
		}
	}
	if len(kept) == 0 {
		// Fall back: best single overlap (never silence a grounded answer).
		bestID, bestSc := "", -1
		for _, id := range cited {
			id = strings.TrimSpace(id)
			ps := byDoc[id]
			if len(ps) == 0 {
				continue
			}
			sc := citeAnswerOverlapScore(ps, ansToks, ansAtoms)
			if sc > bestSc {
				bestSc = sc
				bestID = id
			}
		}
		if bestID != "" {
			return []string{bestID}
		}
		return cited[:1] // last resort: first leaf cite
	}
	// Stable order by original cite order among keepers.
	have := map[string]int{}
	for _, k := range kept {
		have[k.id] = k.score
	}
	var out []string
	for _, id := range cited {
		if _, ok := have[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func citeAnswerOverlapScore(ps []Passage, ansToks, ansAtoms []string) int {
	sc := 0
	blob := strings.Builder{}
	for _, p := range ps {
		blob.WriteString(strings.ToLower(p.Text))
		blob.WriteByte(' ')
		blob.WriteString(strings.ToLower(p.DocumentID))
		blob.WriteByte(' ')
	}
	low := blob.String()
	seenTok := map[string]struct{}{}
	for _, t := range ansToks {
		if len(t) < 4 {
			continue
		}
		if _, ok := seenTok[t]; ok {
			continue
		}
		if strings.Contains(low, t) {
			seenTok[t] = struct{}{}
			sc++
		}
	}
	for _, a := range ansAtoms {
		a = strings.ToLower(strings.TrimSpace(a))
		if len(a) < 3 {
			continue
		}
		if strings.Contains(low, a) {
			sc += 3 // identifiers / quantities are strong
		}
	}
	return sc
}

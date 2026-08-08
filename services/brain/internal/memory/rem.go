package memory

import (
	"os"
	"sort"
	"strings"
)

// REMResult is the outcome of a deterministic REM re-extract pass.
type REMResult struct {
	// DocsScanned is high-utility docs re-extracted.
	DocsScanned []string `json:"docs_scanned,omitempty"`
	// ClaimsAdmitted is count of claims admitted this pass (incl. agreement skips via Admit).
	ClaimsAdmitted int `json:"claims_admitted"`
	// RelationsAdmitted is new TemporalRelations seeded from claims this pass.
	RelationsAdmitted int `json:"relations_admitted"`
	// Quarantined is count of low-confidence / failed extracts sent to quarantine.
	Quarantined int `json:"quarantined"`
	// Enabled reports whether REM actually ran.
	Enabled bool `json:"enabled"`
	// LLMExtension notes whether the LLM re-encode extension point was requested.
	LLMExtension bool `json:"llm_extension,omitempty"`
}

// RunREM is the deterministic REM scaffold (opt-in; no LLM required).
// Re-runs ExtractClaimsFromText on high-utility docs (utility >= promoteThr)
// and admits results. Quarantined / low-utility docs are skipped.
//
// Failed/empty extracts on high-utility docs go to quarantine (reason rem_low_confidence).
//
// Extension point: when OUROBOROS_BRAIN_REM_LLM=1, LLMExtension is flagged true
// but no network call is made (leave hook for budgeted re-encode later).
// Default remains fully deterministic.
func (s *Store) RunREM(docs map[string]string, promoteThr float64) REMResult {
	res := REMResult{Enabled: true}
	if s == nil || len(docs) == 0 {
		return res
	}
	if promoteThr <= 0 {
		promoteThr = 1.5
	}
	if os.Getenv("OUROBOROS_BRAIN_REM_LLM") == "1" {
		// Extension only — no external API calls from this path.
		res.LLMExtension = true
	}
	ids := make([]string, 0, len(docs))
	for id := range docs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	s.EnsureUtility(ids)
	for _, id := range ids {
		if s.IsQuarantined(id) {
			continue
		}
		sc := s.GetUtility(id)
		if sc < promoteThr {
			continue
		}
		text := strings.TrimSpace(docs[id])
		if text == "" {
			continue
		}
		res.DocsScanned = append(res.DocsScanned, id)
		extracted := ExtractClaimsFromText(id, text)
		if len(extracted) == 0 {
			// Low-confidence: high-utility doc yielded no patterns → quarantine.
			_ = s.AddQuarantine(id, "rem_low_confidence", sc, "")
			res.Quarantined++
			continue
		}
		admitted := 0
		for _, cl := range extracted {
			if _, _, err := s.AdmitClaim(cl); err == nil {
				admitted++
				res.ClaimsAdmitted++
			}
		}
		if admitted == 0 {
			_ = s.AddQuarantine(id, "rem_admit_failed", sc, "")
			res.Quarantined++
		}
	}
	// Project new claims onto TemporalRelations (same left-shift as cortex wave).
	res.RelationsAdmitted = s.SeedRelationsFromClaims()
	return res
}

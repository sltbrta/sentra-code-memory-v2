package memory

import (
	"math"
	"sort"
	"time"
)

// UtilityRecord tracks document utility for ranking (Lattice E4+C3 closed loop).
type UtilityRecord struct {
	DocumentID string    `json:"document_id"`
	Score      float64   `json:"score"`
	LastDecay  time.Time `json:"last_decay,omitempty"`
	LastCited  time.Time `json:"last_cited,omitempty"`
	CiteCount  int       `json:"cite_count"`
}

// GetUtility returns score (default 1.0 if unknown).
func (s *Store) GetUtility(docID string) float64 {
	if s == nil {
		return 1
	}
	if u, ok := s.data.Utility[docID]; ok {
		return u.Score
	}
	return 1
}

// SetUtility sets absolute utility.
func (s *Store) SetUtility(docID string, score float64) error {
	if s == nil {
		return nil
	}
	if s.data.Utility == nil {
		s.data.Utility = map[string]UtilityRecord{}
	}
	rec := s.data.Utility[docID]
	rec.DocumentID = docID
	rec.Score = score
	s.data.Utility[docID] = rec
	return s.persist()
}

// DecayUtility applies a fixed multiplicative factor (default 0.95) to all docs.
// Prefer DecayUtilityHalfLife for Lattice E4 time-based decay.
// Returns updated scores. This is the write half of the closed loop.
func (s *Store) DecayUtility(factor float64) map[string]float64 {
	if s == nil {
		return nil
	}
	if factor <= 0 || factor > 1 {
		factor = 0.95
	}
	if s.data.Utility == nil {
		s.data.Utility = map[string]UtilityRecord{}
	}
	now := time.Now().UTC()
	out := map[string]float64{}
	for id, rec := range s.data.Utility {
		rec.Score = rec.Score * factor
		if rec.Score < 0.01 {
			rec.Score = 0.01
		}
		rec.LastDecay = now
		s.data.Utility[id] = rec
		out[id] = rec.Score
	}
	_ = s.persist()
	return out
}

// DecayUtilityHalfLife applies Lattice E4 time-based half-life decay:
//
//	score *= 0.5^(Δt / halfLife)
//	halfLife = 7d if score < 1.0 else 365d; floor 0.01
//
// If LastDecay is zero, sets LastDecay=now and skips decay on first call
// (establishes the decay clock). Returns updated scores.
func (s *Store) DecayUtilityHalfLife(now time.Time) map[string]float64 {
	if s == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if s.data.Utility == nil {
		s.data.Utility = map[string]UtilityRecord{}
	}
	const day = 24 * time.Hour
	out := map[string]float64{}
	for id, rec := range s.data.Utility {
		if rec.LastDecay.IsZero() {
			// Establish clock; no decay this pass.
			rec.LastDecay = now
			if rec.Score <= 0 {
				rec.Score = 1
			}
			s.data.Utility[id] = rec
			out[id] = rec.Score
			continue
		}
		dt := now.Sub(rec.LastDecay)
		if dt < 0 {
			dt = 0
		}
		halfLife := 365 * day
		if rec.Score < 1.0 {
			halfLife = 7 * day
		}
		// score *= 0.5^(dt/halfLife)
		if halfLife > 0 && dt > 0 {
			exponent := float64(dt) / float64(halfLife)
			rec.Score = rec.Score * math.Pow(0.5, exponent)
		}
		if rec.Score < 0.01 {
			rec.Score = 0.01
		}
		rec.LastDecay = now
		s.data.Utility[id] = rec
		out[id] = rec.Score
	}
	_ = s.persist()
	return out
}

// ReinforceUtility boosts cited documents (retrieval reinforcement C3).
func (s *Store) ReinforceUtility(docIDs []string, boost float64) {
	if s == nil || len(docIDs) == 0 {
		return
	}
	if boost <= 0 {
		boost = 0.1
	}
	if s.data.Utility == nil {
		s.data.Utility = map[string]UtilityRecord{}
	}
	now := time.Now().UTC()
	for _, id := range docIDs {
		if id == "" {
			continue
		}
		rec := s.data.Utility[id]
		if rec.DocumentID == "" {
			rec.DocumentID = id
			rec.Score = 1
		}
		rec.Score = math.Min(rec.Score+boost, 5)
		rec.LastCited = now
		rec.CiteCount++
		s.data.Utility[id] = rec
	}
	_ = s.persist()
}

// ApplyUtilityToScores multiplies base scores by utility weights (closed-loop ranking).
// Missing utility defaults to 1.0. Returns new map.
func (s *Store) ApplyUtilityToScores(base map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(base))
	for id, sc := range base {
		u := s.GetUtility(id)
		out[id] = sc * u
	}
	return out
}

// RankDocumentsByUtility sorts doc IDs by utility descending.
func (s *Store) RankDocumentsByUtility(docIDs []string) []string {
	type pair struct {
		id string
		sc float64
	}
	var ps []pair
	for _, id := range docIDs {
		ps = append(ps, pair{id, s.GetUtility(id)})
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].sc == ps[j].sc {
			return ps[i].id < ps[j].id
		}
		return ps[i].sc > ps[j].sc
	})
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.id
	}
	return out
}

// EnsureUtility seeds score 1.0 for docs missing utility.
func (s *Store) EnsureUtility(docIDs []string) {
	if s == nil {
		return
	}
	if s.data.Utility == nil {
		s.data.Utility = map[string]UtilityRecord{}
	}
	for _, id := range docIDs {
		if id == "" {
			continue
		}
		if _, ok := s.data.Utility[id]; !ok {
			s.data.Utility[id] = UtilityRecord{DocumentID: id, Score: 1}
		}
	}
	_ = s.persist()
}

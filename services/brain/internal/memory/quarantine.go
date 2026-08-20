package memory

import (
	"fmt"
	"sort"
	"time"
)

// QuarantineEntry marks a low-utility document for GC consideration.
// Does NOT delete primary evidence chunks — projection only.
type QuarantineEntry struct {
	DocumentID string    `json:"document_id"`
	Reason     string    `json:"reason,omitempty"`
	Utility    float64   `json:"utility"`
	At         time.Time `json:"at"`
	Generation string    `json:"generation_id,omitempty"`
}

// ListQuarantine returns quarantine entries sorted by time.
func (s *Store) ListQuarantine() []QuarantineEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]QuarantineEntry(nil), s.data.Quarantine...)
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// AddQuarantine records a document as quarantined (idempotent per docID).
func (s *Store) AddQuarantine(docID, reason string, utility float64, generation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil || docID == "" {
		return fmt.Errorf("memory: quarantine requires store and document_id")
	}
	now := time.Now().UTC()
	for i := range s.data.Quarantine {
		if s.data.Quarantine[i].DocumentID == docID {
			s.data.Quarantine[i].Reason = reason
			s.data.Quarantine[i].Utility = utility
			s.data.Quarantine[i].At = now
			if generation != "" {
				s.data.Quarantine[i].Generation = generation
			}
			return s.persistLocked()
		}
	}
	s.data.Quarantine = append(s.data.Quarantine, QuarantineEntry{
		DocumentID: docID,
		Reason:     reason,
		Utility:    utility,
		At:         now,
		Generation: generation,
	})
	return s.persistLocked()
}

// IsQuarantined reports whether docID is in quarantine.
func (s *Store) IsQuarantined(docID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.data.Quarantine {
		if q.DocumentID == docID {
			return true
		}
	}
	return false
}

// NREMResult is the outcome of a product-side NREM consolidation pass.
type NREMResult struct {
	Promoted    []string `json:"promoted,omitempty"`
	Quarantined []string `json:"quarantined,omitempty"`
}

// RunNREM closes the NREM loop on the memory cortex:
//   - utility >= promoteThr (default 1.5) → slight boost / ensure utility
//   - utility < floor (default 0.2) → quarantine (no chunk deletion)
//
// docs maps documentID → text (text unused for scoring; utility drives decisions).
func (s *Store) RunNREM(docs map[string]string, utilityFloor, promoteThr float64) NREMResult {
	res := NREMResult{}
	if s == nil || len(docs) == 0 {
		return res
	}
	if utilityFloor <= 0 {
		utilityFloor = 0.2
	}
	if promoteThr <= 0 {
		promoteThr = 1.5
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
		sc := s.GetUtility(id)
		if sc >= promoteThr {
			// Slight promote boost (capped by ReinforceUtility at 5).
			s.ReinforceUtility([]string{id}, 0.05)
			res.Promoted = append(res.Promoted, id)
			continue
		}
		if sc < utilityFloor {
			_ = s.AddQuarantine(id, "nrem_low_utility", sc, "")
			res.Quarantined = append(res.Quarantined, id)
		}
	}
	return res
}

package memory

import (
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

// ResolutionOutcome is the result of ordered conflict resolution.
type ResolutionOutcome string

const (
	ResolutionWinner    ResolutionOutcome = "winner"
	ResolutionContested ResolutionOutcome = "contested"
	ResolutionNone      ResolutionOutcome = "none"
)

// Resolution is the ordered-ladder result for a contested claim group.
// Tie always remains contested — never a silent UUID winner (SFS DeterministicResolver).
type Resolution struct {
	Outcome   ResolutionOutcome `json:"outcome"`
	WinnerID  string            `json:"winner_id,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	ClaimIDs  []string          `json:"claim_ids,omitempty"`
	Contested bool              `json:"contested"`
}

// ResolveGroup applies the deterministic ladder to claims that share a ClaimKey:
//  1. multi-valued predicates → no contest (all remain active-capable)
//  2. explicit supersession relation (Supersedes / SupersededBy)
//  3. higher EvidenceQuality
//  4. more DocumentIDs
//  5. tighter valid window (shorter or closed ValidTo preferred)
//  6. tie → contested (never silent UUID pick)
func ResolveGroup(claims []Claim) Resolution {
	if len(claims) == 0 {
		return Resolution{Outcome: ResolutionNone, Reason: "empty"}
	}
	ids := make([]string, 0, len(claims))
	for _, c := range claims {
		ids = append(ids, c.ID)
	}
	if len(claims) == 1 {
		return Resolution{
			Outcome:  ResolutionWinner,
			WinnerID: claims[0].ID,
			Reason:   "single",
			ClaimIDs: ids,
		}
	}
	// Multi-valued: different objects do not contest (policy pack).
	if pred := strings.TrimSpace(claims[0].Predicate); pred != "" && ontology.IsMultiValuedPredicate(pred) {
		return Resolution{
			Outcome:  ResolutionNone,
			Reason:   "multi_valued_predicate",
			ClaimIDs: ids,
		}
	}

	// 1) Explicit supersession: if A.Supersedes == B.ID or B.SupersededBy == A.ID, prefer A.
	// Prefer the claim that supersedes another in the group and is not itself superseded in-group.
	superWinner := ""
	for _, c := range claims {
		if c.Supersedes == "" && c.SupersededBy == "" {
			continue
		}
		// If this claim supersedes another in the group, it wins the ladder step.
		for _, other := range claims {
			if other.ID == c.ID {
				continue
			}
			if c.Supersedes == other.ID || other.SupersededBy == c.ID {
				// Ensure c is not superseded by someone else in group.
				if c.SupersededBy != "" {
					supersededInGroup := false
					for _, x := range claims {
						if x.ID == c.SupersededBy {
							supersededInGroup = true
							break
						}
					}
					if supersededInGroup {
						continue
					}
				}
				if superWinner != "" && superWinner != c.ID {
					// Multiple superseders → contested
					return Resolution{
						Outcome:   ResolutionContested,
						Reason:    "multiple_supersession",
						ClaimIDs:  ids,
						Contested: true,
					}
				}
				superWinner = c.ID
			}
		}
	}
	if superWinner != "" {
		return Resolution{
			Outcome:  ResolutionWinner,
			WinnerID: superWinner,
			Reason:   "supersession",
			ClaimIDs: ids,
		}
	}

	// 2–4) Score ladder: quality, then doc count, then tighter window.
	type scored struct {
		id    string
		q     uint16
		docs  int
		tight int64 // smaller = tighter (duration ns; open-ended = large)
	}
	var ss []scored
	for _, c := range claims {
		tight := int64(1 << 62)
		if c.ValidTo != nil && !c.ValidFrom.IsZero() {
			d := c.ValidTo.Sub(c.ValidFrom)
			if d > 0 {
				tight = int64(d)
			}
		}
		ss = append(ss, scored{
			id:    c.ID,
			q:     c.EvidenceQuality,
			docs:  len(c.DocumentIDs),
			tight: tight,
		})
	}

	// Find max quality.
	var maxQ uint16
	for _, s := range ss {
		if s.q > maxQ {
			maxQ = s.q
		}
	}
	var byQ []scored
	for _, s := range ss {
		if s.q == maxQ {
			byQ = append(byQ, s)
		}
	}
	if len(byQ) == 1 {
		return Resolution{
			Outcome:  ResolutionWinner,
			WinnerID: byQ[0].id,
			Reason:   "evidence_quality",
			ClaimIDs: ids,
		}
	}

	// More DocumentIDs among remaining.
	maxDocs := -1
	for _, s := range byQ {
		if s.docs > maxDocs {
			maxDocs = s.docs
		}
	}
	var byDocs []scored
	for _, s := range byQ {
		if s.docs == maxDocs {
			byDocs = append(byDocs, s)
		}
	}
	if len(byDocs) == 1 {
		return Resolution{
			Outcome:  ResolutionWinner,
			WinnerID: byDocs[0].id,
			Reason:   "document_count",
			ClaimIDs: ids,
		}
	}

	// Tighter valid window among remaining.
	minTight := byDocs[0].tight
	for _, s := range byDocs {
		if s.tight < minTight {
			minTight = s.tight
		}
	}
	var byTight []scored
	for _, s := range byDocs {
		if s.tight == minTight {
			byTight = append(byTight, s)
		}
	}
	if len(byTight) == 1 && minTight < int64(1<<62) {
		// Only declare tighter-window winner when at least one has a closed window.
		return Resolution{
			Outcome:  ResolutionWinner,
			WinnerID: byTight[0].id,
			Reason:   "tighter_valid_window",
			ClaimIDs: ids,
		}
	}

	// 5) True tie → contested; never pick by UUID/ID order.
	return Resolution{
		Outcome:   ResolutionContested,
		Reason:    "tie",
		ClaimIDs:  ids,
		Contested: true,
	}
}

// ApplyResolution updates store claim statuses for a group resolution.
// Winner → Active; losers → Superseded (if winner) or remain Contested (if tie).
// Locks for the full mutate+persist critical section and invalidates the
// contested-group index so answer-path reads see the new statuses.
func (s *Store) ApplyResolution(res Resolution) error {
	if s == nil || res.Outcome == ResolutionNone {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyResolutionLocked(res)
}

// applyResolutionLocked implements ApplyResolution. Caller must hold s.mu.
func (s *Store) applyResolutionLocked(res Resolution) error {
	s.invalidateContestedLocked()
	if res.Outcome == ResolutionContested {
		// Ensure all listed claims stay contested.
		for i := range s.data.Claims {
			c := &s.data.Claims[i]
			for _, id := range res.ClaimIDs {
				if c.ID == id && c.Status != ClaimTombstoned && c.Status != ClaimSuperseded {
					c.Status = ClaimContested
				}
			}
		}
		return s.persistLocked()
	}
	if res.Outcome == ResolutionWinner && res.WinnerID != "" {
		now := time.Now().UTC()
		for i := range s.data.Claims {
			c := &s.data.Claims[i]
			inGroup := false
			for _, id := range res.ClaimIDs {
				if c.ID == id {
					inGroup = true
					break
				}
			}
			if !inGroup {
				continue
			}
			if c.ID == res.WinnerID {
				c.Status = ClaimActive
				c.ConflictsWith = nil
				continue
			}
			// Loser: supersede if same key group.
			c.Status = ClaimSuperseded
			end := now
			c.ValidTo = &end
			c.SupersededBy = res.WinnerID
		}
		// Link winner supersedes first loser if empty.
		for i := range s.data.Claims {
			if s.data.Claims[i].ID == res.WinnerID && s.data.Claims[i].Supersedes == "" {
				for _, id := range res.ClaimIDs {
					if id != res.WinnerID {
						s.data.Claims[i].Supersedes = id
						break
					}
				}
			}
		}
		return s.persistLocked()
	}
	return nil
}

// ResolveContestedGroups runs ResolveGroup on each contested group and applies winners.
// Ties remain contested. Returns number of groups resolved to a winner.
func (s *Store) ResolveContestedGroups() int {
	if s == nil {
		return 0
	}
	groups := s.ContestedGroups()
	resolved := 0
	for _, claims := range groups {
		res := ResolveGroup(claims)
		if res.Outcome == ResolutionWinner {
			_ = s.ApplyResolution(res)
			resolved++
		}
	}
	return resolved
}

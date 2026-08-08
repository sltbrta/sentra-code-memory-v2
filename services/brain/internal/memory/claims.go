package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

// ClaimStatus is the exposure state of a claim.
type ClaimStatus string

const (
	ClaimActive     ClaimStatus = "active"
	ClaimSuperseded ClaimStatus = "superseded"
	ClaimContested  ClaimStatus = "contested"
	ClaimTombstoned ClaimStatus = "tombstoned"
)

// Claim is a bi-temporal, evidence-backed assertion (Graphiti/SFS-inspired).
//
// World time (valid time T):   ValidFrom / ValidTo — when the fact was true in the world.
// Transaction time (T′):       ObservedAt / ExpiredAt — when the system knew / expired it.
//
// Graphiti maps: valid_at↔ValidFrom, invalid_at↔ValidTo, created_at↔ObservedAt,
// expired_at↔ExpiredAt. Supersedes links non-lossy history (never delete).
type Claim struct {
	ID              string      `json:"id"`
	Subject         string      `json:"subject"`
	Predicate       string      `json:"predicate"`
	Object          string      `json:"object"`
	DocumentIDs     []string    `json:"document_ids,omitempty"`
	ValidFrom       time.Time   `json:"valid_from"`
	ValidTo         *time.Time  `json:"valid_to,omitempty"` // nil = open-ended world validity
	ObservedAt      time.Time   `json:"observed_at"`
	ExpiredAt       *time.Time  `json:"expired_at,omitempty"` // transaction-time close (Graphiti expired_at)
	Status          ClaimStatus `json:"status"`
	Supersedes      string      `json:"supersedes,omitempty"`
	SupersededBy    string      `json:"superseded_by,omitempty"`
	ConflictsWith   []string    `json:"conflicts_with,omitempty"`
	Provenance      string      `json:"provenance,omitempty"`
	Authority       string      `json:"authority,omitempty"`
	EvidenceQuality uint16      `json:"evidence_quality,omitempty"`
	// Evidence span (byte offsets into source document text) — GAP-MEM-EVIDENCE-SPANS.
	SpanDocID string `json:"span_doc_id,omitempty"`
	SpanStart int    `json:"span_start,omitempty"`
	SpanEnd   int    `json:"span_end,omitempty"`
	SpanText  string `json:"span_text,omitempty"`
}

// ClaimKey groups claims that may conflict (same subject+predicate).
func (c Claim) ClaimKey() string {
	return strings.ToLower(strings.TrimSpace(c.Subject)) + "|" + strings.ToLower(strings.TrimSpace(c.Predicate))
}

// ValidAt reports whether world-time t is inside the claim's validity window.
func (c Claim) ValidAt(t time.Time) bool {
	if c.Status == ClaimTombstoned {
		return false
	}
	if t.Before(c.ValidFrom) {
		return false
	}
	if c.ValidTo != nil && !t.Before(*c.ValidTo) && !t.Equal(*c.ValidTo) {
		// valid_to exclusive end
		if t.After(*c.ValidTo) {
			return false
		}
	}
	return true
}

// KnownAt reports whether the claim was in the system's knowledge at transaction time t′.
// ObservedAt <= t′ and (ExpiredAt is nil or t′ < ExpiredAt).
func (c Claim) KnownAt(t time.Time) bool {
	if c.Status == ClaimTombstoned {
		return false
	}
	if t.IsZero() {
		return c.ExpiredAt == nil
	}
	if c.ObservedAt.After(t) {
		return false
	}
	if c.ExpiredAt != nil && !t.Before(*c.ExpiredAt) {
		return false
	}
	return true
}

// ExpireClaim marks transaction-time close without erasing history (Graphiti expired_at).
// Locks for the full mutate+persist critical section so concurrent readers and
// the contested-group index never observe a torn claim list.
func (s *Store) ExpireClaim(id string, at time.Time) error {
	if s == nil {
		return fmt.Errorf("memory: nil store")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Claims {
		if s.data.Claims[i].ID != id {
			continue
		}
		exp := at
		s.data.Claims[i].ExpiredAt = &exp
		if s.data.Claims[i].Status == ClaimActive {
			s.data.Claims[i].Status = ClaimTombstoned
		}
		s.invalidateContestedLocked()
		return s.persistLocked()
	}
	return fmt.Errorf("memory: claim %s not found", id)
}

// OverlapsValidTime reports whether two validity intervals overlap.
func OverlapsValidTime(a, b Claim) bool {
	// Treat open ValidTo as +inf
	aEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	bEnd := aEnd
	if a.ValidTo != nil {
		aEnd = *a.ValidTo
	}
	if b.ValidTo != nil {
		bEnd = *b.ValidTo
	}
	// [from, to) intervals overlap if from < other.to && other.from < to
	return a.ValidFrom.Before(bEnd) && b.ValidFrom.Before(aEnd)
}

// AdmitClaim appends a claim; detects conflicts with overlapping active claims.
// Locks for the full mutate+persist (+optional re-resolve) critical section so
// concurrent readers and the contested-group index never observe a torn claim list.
func (s *Store) AdmitClaim(c Claim) (Claim, []Claim, error) {
	if s == nil {
		return Claim{}, nil, fmt.Errorf("memory: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admitClaimLocked(c)
}

// admitClaimLocked implements AdmitClaim. Caller must hold s.mu.
func (s *Store) admitClaimLocked(c Claim) (Claim, []Claim, error) {
	c.Subject = strings.TrimSpace(c.Subject)
	c.Predicate = strings.TrimSpace(c.Predicate)
	c.Object = strings.TrimSpace(c.Object)
	if c.Subject == "" || c.Predicate == "" || c.Object == "" {
		return Claim{}, nil, fmt.Errorf("memory: claim requires subject, predicate, object")
	}
	if c.ID == "" {
		c.ID = fmt.Sprintf("cl-%d", s.seq.Add(1))
	}
	if c.ObservedAt.IsZero() {
		c.ObservedAt = time.Now().UTC()
	}
	if c.ValidFrom.IsZero() {
		c.ValidFrom = c.ObservedAt
	}
	if c.Status == "" {
		c.Status = ClaimActive
	}
	if c.Provenance == "" {
		c.Provenance = "product_brain"
	}

	var contested []Claim
	key := c.ClaimKey()
	for i := range s.data.Claims {
		ex := &s.data.Claims[i]
		if ex.ClaimKey() != key || ex.Status != ClaimActive {
			continue
		}
		if !OverlapsValidTime(c, *ex) {
			continue
		}
		if strings.EqualFold(ex.Object, c.Object) {
			continue // agreement
		}
		// Multi-valued predicates (tags/aliases/…) may hold many objects — no contest.
		if ontology.IsMultiValuedPredicate(c.Predicate) {
			continue
		}
		// Conflict: mark both contested (preserve evidence; no silent overwrite).
		ex.Status = ClaimContested
		ex.ConflictsWith = appendUnique(ex.ConflictsWith, c.ID)
		c.Status = ClaimContested
		c.ConflictsWith = appendUnique(c.ConflictsWith, ex.ID)
		contested = append(contested, *ex)
	}
	s.invalidateContestedLocked()
	s.data.Claims = append(s.data.Claims, c)
	if err := s.persistLocked(); err != nil {
		return Claim{}, nil, err
	}
	// Optionally re-resolve contested groups for this key (quality ladder).
	if c.Status == ClaimContested {
		key := c.ClaimKey()
		var group []Claim
		for _, ex := range s.data.Claims {
			if ex.ClaimKey() == key && ex.Status == ClaimContested {
				group = append(group, ex)
			}
		}
		if res := ResolveGroup(group); res.Outcome == ResolutionWinner {
			_ = s.applyResolutionLocked(res)
			// Refresh admitted claim status from store.
			for _, ex := range s.data.Claims {
				if ex.ID == c.ID {
					c = ex
					break
				}
			}
			// Contested list only if still contested peers remain.
			var still []Claim
			for _, ex := range s.data.Claims {
				if ex.ClaimKey() == key && ex.Status == ClaimContested {
					still = append(still, ex)
				}
			}
			contested = still
		}
	}
	return c, contested, nil
}

// SupersedeClaim ends the validity of oldID and admits newClaim as successor.
// Locks for the full mutate+persist critical section.
func (s *Store) SupersedeClaim(oldID string, newClaim Claim, at time.Time) (Claim, error) {
	if s == nil {
		return Claim{}, fmt.Errorf("memory: nil store")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.data.Claims {
		ex := &s.data.Claims[i]
		if ex.ID != oldID {
			continue
		}
		found = true
		ex.Status = ClaimSuperseded
		end := at
		ex.ValidTo = &end
		newClaim.Supersedes = oldID
		s.invalidateContestedLocked()
		break
	}
	if !found {
		return Claim{}, fmt.Errorf("memory: claim %s not found", oldID)
	}
	newClaim.Status = ClaimActive
	if newClaim.ValidFrom.IsZero() {
		newClaim.ValidFrom = at
	}
	admitted, _, err := s.admitClaimLocked(newClaim)
	if err != nil {
		return Claim{}, err
	}
	// Link superseded_by
	for i := range s.data.Claims {
		if s.data.Claims[i].ID == oldID {
			s.data.Claims[i].SupersededBy = admitted.ID
			break
		}
	}
	_ = s.persistLocked()
	return admitted, nil
}

// CurrentClaims returns active (non-contested optional) claims valid at t.
// Equivalent to CurrentClaimsAsOf(t, zero, includeContested) — no knownAt filter.
func (s *Store) CurrentClaims(t time.Time, includeContested bool) []Claim {
	return s.CurrentClaimsAsOf(t, time.Time{}, includeContested)
}

// CurrentClaimsAsOf is the dual-axis bi-temporal query:
//   - validAt: world time (ValidFrom/ValidTo window via ValidAt)
//   - knownAt: system knowledge time via KnownAt (ObservedAt/ExpiredAt)
//     (zero knownAt = no transaction-time filter, backward compatible)
//
// Contested claims are included only when includeContested is true.
// Superseded and tombstoned claims never surface as current.
// Locks so concurrent claim mutators cannot tear the scan (answer-path hot read).
func (s *Store) CurrentClaimsAsOf(validAt, knownAt time.Time, includeContested bool) []Claim {
	if s == nil {
		return nil
	}
	if validAt.IsZero() {
		validAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Claim
	for _, c := range s.data.Claims {
		if c.Status == ClaimTombstoned || c.Status == ClaimSuperseded {
			continue
		}
		if c.Status == ClaimContested && !includeContested {
			continue
		}
		if !c.ValidAt(validAt) {
			continue
		}
		if !knownAt.IsZero() && !c.KnownAt(knownAt) {
			continue
		}
		// Even with zero knownAt, hide transaction-expired claims (expired_at set).
		if knownAt.IsZero() && c.ExpiredAt != nil {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ContestedGroups returns contested claims grouped by ClaimKey.
//
// The result is served from a lazily built index guarded by s.mu: every
// claim/status mutator invalidates it, so repeated answer-path reads do not
// rescan an unchanged claim list, while a rebuild always reproduces exactly
// what a fresh scan of the claim list would return (same keys, same order).
// The returned map and slices are copies; the Claim values share DocumentIDs/
// ConflictsWith backing arrays exactly as the prior scan-based result did.
func (s *Store) ContestedGroups() map[string][]Claim {
	out := map[string][]Claim{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.contestedValid {
		cache := map[string][]Claim{}
		for _, c := range s.data.Claims {
			if c.Status != ClaimContested {
				continue
			}
			k := c.ClaimKey()
			cache[k] = append(cache[k], c)
		}
		s.contestedCache = cache
		s.contestedValid = true
	}
	for k, v := range s.contestedCache {
		out[k] = append([]Claim(nil), v...)
	}
	return out
}

// ClaimsForDocuments returns claims citing any of the document IDs.
// Locks so concurrent claim mutators cannot tear the scan.
func (s *Store) ClaimsForDocuments(docIDs []string) []Claim {
	if s == nil || len(docIDs) == 0 {
		return nil
	}
	want := map[string]struct{}{}
	for _, d := range docIDs {
		want[d] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Claim
	for _, c := range s.data.Claims {
		for _, d := range c.DocumentIDs {
			if _, ok := want[d]; ok {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

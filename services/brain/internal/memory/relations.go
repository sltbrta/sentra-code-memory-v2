package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/ontology"
)

// RelationStatus mirrors claim exposure for graph edges.
type RelationStatus string

const (
	RelationActive     RelationStatus = "active"
	RelationSuperseded RelationStatus = "superseded"
	RelationContested  RelationStatus = "contested"
	RelationExpired    RelationStatus = "expired"
)

// TemporalRelation is a Graphiti-class bi-temporal edge between entities.
//
// Unlike doc co-occur adjacency (Edges map, PPR prior only), these edges carry
// fact text, dual timelines, and supersession — the multi-hop intelligence layer.
//
//	World time:       ValidFrom / ValidTo
//	Transaction time: ObservedAt / ExpiredAt
type TemporalRelation struct {
	ID              string         `json:"id"`
	Src             string         `json:"src"`      // entity / subject node
	Dst             string         `json:"dst"`      // entity / object node
	Relation        string         `json:"relation"` // edge type / predicate
	FactText        string         `json:"fact_text,omitempty"`
	DocumentIDs     []string       `json:"document_ids,omitempty"`
	ClaimID         string         `json:"claim_id,omitempty"` // optional link to Claim
	ValidFrom       time.Time      `json:"valid_from"`
	ValidTo         *time.Time     `json:"valid_to,omitempty"`
	ObservedAt      time.Time      `json:"observed_at"`
	ExpiredAt       *time.Time     `json:"expired_at,omitempty"`
	Status          RelationStatus `json:"status"`
	Supersedes      string         `json:"supersedes,omitempty"`
	SupersededBy    string         `json:"superseded_by,omitempty"`
	ConflictsWith   []string       `json:"conflicts_with,omitempty"`
	EvidenceQuality uint16         `json:"evidence_quality,omitempty"`
	Weight          float64        `json:"weight,omitempty"` // ranking prior; default 1
}

// RelationKey groups edges that may conflict (same src+relation+dst for multi-valued skip).
func (r TemporalRelation) RelationKey() string {
	return strings.ToLower(strings.TrimSpace(r.Src)) + "|" +
		strings.ToLower(strings.TrimSpace(r.Relation)) + "|" +
		strings.ToLower(strings.TrimSpace(r.Dst))
}

// ConflictKey groups edges that contest for the same src+relation (different dst).
func (r TemporalRelation) ConflictKey() string {
	return strings.ToLower(strings.TrimSpace(r.Src)) + "|" +
		strings.ToLower(strings.TrimSpace(r.Relation))
}

// ValidAt is world-time validity (same semantics as Claim.ValidAt).
func (r TemporalRelation) ValidAt(t time.Time) bool {
	if r.Status == RelationExpired {
		return false
	}
	if t.Before(r.ValidFrom) {
		return false
	}
	if r.ValidTo != nil && t.After(*r.ValidTo) {
		return false
	}
	return true
}

// KnownAt is transaction-time knowledge filter.
func (r TemporalRelation) KnownAt(t time.Time) bool {
	if r.Status == RelationExpired {
		return false
	}
	if t.IsZero() {
		return r.ExpiredAt == nil
	}
	if r.ObservedAt.After(t) {
		return false
	}
	if r.ExpiredAt != nil && !t.Before(*r.ExpiredAt) {
		return false
	}
	return true
}

// AdmitRelation appends a bi-temporal edge; marks conflicts on same ConflictKey + overlapping valid time.
// AdmitRelation takes the store lock and delegates. The admitRelationLocked form exists
// because composed maintenance operations call it while already holding
// the lock, and sync.Mutex is not reentrant -- taking it twice deadlocks.
func (s *Store) AdmitRelation(rel TemporalRelation) (TemporalRelation, []TemporalRelation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admitRelationLocked(rel)
}

// admitRelationLocked assumes the caller holds s.mu.
func (s *Store) admitRelationLocked(rel TemporalRelation) (TemporalRelation, []TemporalRelation, error) {
	if s == nil {
		return TemporalRelation{}, nil, fmt.Errorf("memory: nil store")
	}
	rel.Src = strings.TrimSpace(rel.Src)
	rel.Dst = strings.TrimSpace(rel.Dst)
	rel.Relation = strings.TrimSpace(rel.Relation)
	if rel.Src == "" || rel.Dst == "" || rel.Relation == "" {
		return TemporalRelation{}, nil, fmt.Errorf("memory: relation requires src, relation, dst")
	}
	if rel.ID == "" {
		rel.ID = fmt.Sprintf("rel-%d", s.seq.Add(1))
	}
	if rel.ObservedAt.IsZero() {
		rel.ObservedAt = time.Now().UTC()
	}
	if rel.ValidFrom.IsZero() {
		rel.ValidFrom = rel.ObservedAt
	}
	if rel.Status == "" {
		rel.Status = RelationActive
	}
	if rel.Weight <= 0 {
		rel.Weight = 1
	}
	// Multi-valued predicates: different dst do not contest.
	if ontology.IsMultiValuedPredicate(rel.Relation) {
		s.data.Relations = append(s.data.Relations, rel)
		if err := s.persistLocked(); err != nil {
			return TemporalRelation{}, nil, err
		}
		return rel, nil, nil
	}

	var contested []TemporalRelation
	ck := rel.ConflictKey()
	for i := range s.data.Relations {
		ex := &s.data.Relations[i]
		if ex.Status != RelationActive && ex.Status != RelationContested {
			continue
		}
		if ex.ConflictKey() != ck {
			continue
		}
		// Same exact triple: ignore duplicate
		if ex.RelationKey() == rel.RelationKey() && ex.FactText == rel.FactText {
			continue
		}
		// Different object / fact with overlapping world time → contest
		if !OverlapsValidTime(
			Claim{ValidFrom: ex.ValidFrom, ValidTo: ex.ValidTo},
			Claim{ValidFrom: rel.ValidFrom, ValidTo: rel.ValidTo},
		) {
			continue
		}
		if strings.EqualFold(ex.Dst, rel.Dst) && ex.FactText == rel.FactText {
			continue
		}
		ex.Status = RelationContested
		ex.ConflictsWith = appendUnique(ex.ConflictsWith, rel.ID)
		rel.Status = RelationContested
		rel.ConflictsWith = appendUnique(rel.ConflictsWith, ex.ID)
		contested = append(contested, *ex)
	}
	s.data.Relations = append(s.data.Relations, rel)
	if err := s.persistLocked(); err != nil {
		return TemporalRelation{}, nil, err
	}
	return rel, contested, nil
}

// SupersedeRelation marks oldID superseded by a new relation (non-lossy).
func (s *Store) SupersedeRelation(oldID string, neu TemporalRelation) (TemporalRelation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil {
		return TemporalRelation{}, fmt.Errorf("memory: nil store")
	}
	found := false
	for i := range s.data.Relations {
		if s.data.Relations[i].ID == oldID {
			found = true
			s.data.Relations[i].Status = RelationSuperseded
			break
		}
	}
	if !found {
		return TemporalRelation{}, fmt.Errorf("memory: relation %s not found", oldID)
	}
	neu.Supersedes = oldID
	if neu.Status == "" {
		neu.Status = RelationActive
	}
	admitted, _, err := s.admitRelationLocked(neu)
	if err != nil {
		return TemporalRelation{}, err
	}
	for i := range s.data.Relations {
		if s.data.Relations[i].ID == oldID {
			s.data.Relations[i].SupersededBy = admitted.ID
			break
		}
	}
	_ = s.persistLocked()
	return admitted, nil
}

// CurrentRelationsAsOf dual-axis filter for temporal edges.
func (s *Store) CurrentRelationsAsOf(validAt, knownAt time.Time, includeContested bool) []TemporalRelation {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if validAt.IsZero() {
		validAt = time.Now().UTC()
	}
	var out []TemporalRelation
	for _, r := range s.data.Relations {
		if r.Status == RelationExpired || r.Status == RelationSuperseded {
			continue
		}
		if r.Status == RelationContested && !includeContested {
			continue
		}
		if !r.ValidAt(validAt) {
			continue
		}
		if !knownAt.IsZero() && !r.KnownAt(knownAt) {
			continue
		}
		if knownAt.IsZero() && r.ExpiredAt != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ExpandRelations returns neighbor entity IDs from current relations (multi-hop seed).
// Includes contested edges (still load-bearing for graph recall; conflict is for answer policy).
// Note: entity IDs are not document IDs — use ExpandRelationDocuments for serve hydrate.
func (s *Store) ExpandRelations(seeds []string, validAt, knownAt time.Time, maxN int) []string {
	if maxN <= 0 {
		maxN = 16
	}
	rels := s.CurrentRelationsAsOf(validAt, knownAt, true)
	seedSet := map[string]struct{}{}
	for _, s0 := range seeds {
		seedSet[strings.ToLower(strings.TrimSpace(s0))] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, r := range rels {
		src := strings.ToLower(r.Src)
		dst := strings.ToLower(r.Dst)
		_, srcSeed := seedSet[src]
		_, dstSeed := seedSet[dst]
		if srcSeed && !dstSeed {
			if _, ok := seen[r.Dst]; !ok {
				seen[r.Dst] = struct{}{}
				out = append(out, r.Dst)
			}
		}
		if dstSeed && !srcSeed {
			if _, ok := seen[r.Src]; !ok {
				seen[r.Src] = struct{}{}
				out = append(out, r.Src)
			}
		}
		if len(out) >= maxN {
			break
		}
	}
	return out
}

// relationMatchesSeed is true when seed equals src/dst (case-insensitive) or
// appears as a token substring in fact text (entity names in prose).
func relationMatchesSeed(r TemporalRelation, seed string) bool {
	if seed == "" {
		return false
	}
	if strings.EqualFold(r.Src, seed) || strings.EqualFold(r.Dst, seed) {
		return true
	}
	fact := strings.ToLower(r.FactText)
	if fact == "" {
		return false
	}
	s := strings.ToLower(seed)
	if len(s) < 3 {
		return false
	}
	return strings.Contains(fact, s)
}

// ExpandRelationDocuments returns document IDs cited by temporal edges matching
// any seed. This is the serve-path hydrate surface — entity neighbor names alone
// do not key DocTexts. Contested edges included (same as ExpandRelations).
func (s *Store) ExpandRelationDocuments(seeds []string, validAt, knownAt time.Time, maxN int) []string {
	if s == nil || len(seeds) == 0 {
		return nil
	}
	if maxN <= 0 {
		maxN = 16
	}
	rels := s.CurrentRelationsAsOf(validAt, knownAt, true)
	seen := map[string]struct{}{}
	var out []string
	for _, r := range rels {
		hit := false
		for _, seed := range seeds {
			if relationMatchesSeed(r, strings.TrimSpace(seed)) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		for _, d := range r.DocumentIDs {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			out = append(out, d)
			if len(out) >= maxN {
				return out
			}
		}
	}
	return out
}

// RelationFactForDoc returns the first non-empty FactText for a document among
// current relations (serve fallback when DocTexts lacks the body).
func (s *Store) RelationFactForDoc(docID string) string {
	if s == nil || strings.TrimSpace(docID) == "" {
		return ""
	}
	for _, r := range s.RelationsForDocuments([]string{docID}) {
		if t := strings.TrimSpace(r.FactText); t != "" {
			return t
		}
	}
	return ""
}

// RelationsForDocuments returns temporal edges citing any document id.
func (s *Store) RelationsForDocuments(docIDs []string) []TemporalRelation {
	if s == nil || len(docIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[string]struct{}{}
	for _, d := range docIDs {
		want[d] = struct{}{}
	}
	var out []TemporalRelation
	for _, r := range s.data.Relations {
		for _, d := range r.DocumentIDs {
			if _, ok := want[d]; ok {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// RelationFromClaim projects a claim triple onto a Graphiti-class TemporalRelation
// (Subject --predicate--> Object). Dual timelines and document evidence are inherited.
// Status is always RelationActive here — AdmitRelation re-marks contest with peers.
func RelationFromClaim(c Claim) TemporalRelation {
	fact := strings.TrimSpace(c.SpanText)
	if fact == "" {
		fact = strings.TrimSpace(c.Subject + " " + c.Predicate + " " + c.Object)
	}
	return TemporalRelation{
		Src:             c.Subject,
		Dst:             c.Object,
		Relation:        c.Predicate,
		FactText:        fact,
		DocumentIDs:     append([]string(nil), c.DocumentIDs...),
		ClaimID:         c.ID,
		ValidFrom:       c.ValidFrom,
		ValidTo:         c.ValidTo,
		ObservedAt:      c.ObservedAt,
		ExpiredAt:       c.ExpiredAt,
		Status:          RelationActive,
		EvidenceQuality: c.EvidenceQuality,
		Weight:          1,
	}
}

// SeedRelationsFromClaims left-shifts gardener extract → entity graph: every
// active/contested claim is admitted as a TemporalRelation so ExpandRelations
// and the lean retrieve structure arm walk precomputed multi-hop edges (no
// extract at query time). Idempotent on ClaimID and exact RelationKey+FactText.
// Returns newly admitted count.
func (s *Store) SeedRelationsFromClaims() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	haveClaim := map[string]struct{}{}
	haveTriple := map[string]struct{}{}
	for _, r := range s.data.Relations {
		if r.ClaimID != "" {
			haveClaim[r.ClaimID] = struct{}{}
		}
		haveTriple[r.RelationKey()+"|"+strings.ToLower(strings.TrimSpace(r.FactText))] = struct{}{}
	}
	n := 0
	for _, c := range s.data.Claims {
		if c.Status != ClaimActive && c.Status != ClaimContested {
			continue
		}
		if strings.TrimSpace(c.Subject) == "" || strings.TrimSpace(c.Object) == "" ||
			strings.TrimSpace(c.Predicate) == "" {
			continue
		}
		if c.ID != "" {
			if _, ok := haveClaim[c.ID]; ok {
				continue
			}
		}
		rel := RelationFromClaim(c)
		tk := rel.RelationKey() + "|" + strings.ToLower(strings.TrimSpace(rel.FactText))
		if _, ok := haveTriple[tk]; ok {
			continue
		}
		admitted, _, err := s.admitRelationLocked(rel)
		if err != nil {
			continue
		}
		if admitted.ClaimID != "" {
			haveClaim[admitted.ClaimID] = struct{}{}
		}
		haveTriple[admitted.RelationKey()+"|"+strings.ToLower(strings.TrimSpace(admitted.FactText))] = struct{}{}
		n++
	}
	return n
}

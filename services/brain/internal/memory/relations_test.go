package memory_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

func TestTemporalRelationAdmitAndSupersede(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	old, contested, err := s.AdmitRelation(memory.TemporalRelation{
		Src: "MedThink", Relation: "rpo_minutes", Dst: "15",
		FactText: "MedThink RPO is 15 minutes", DocumentIDs: []string{"d1"},
		ValidFrom: t0, ObservedAt: t0, EvidenceQuality: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(contested) != 0 {
		t.Fatalf("first admit should not contest: %+v", contested)
	}
	if old.Status != memory.RelationActive {
		t.Fatalf("status=%s", old.Status)
	}

	// Conflicting dst for same src+relation overlapping validity.
	_, contested, err = s.AdmitRelation(memory.TemporalRelation{
		Src: "MedThink", Relation: "rpo_minutes", Dst: "30",
		FactText: "MedThink RPO is 30 minutes", DocumentIDs: []string{"d2"},
		ValidFrom: t0, ObservedAt: t1, EvidenceQuality: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(contested) == 0 {
		t.Fatal("expected contest on conflicting RPO")
	}

	// Supersede old with explicit new relation.
	neu, err := s.SupersedeRelation(old.ID, memory.TemporalRelation{
		Src: "MedThink", Relation: "rpo_minutes", Dst: "15",
		FactText:    "MedThink RPO confirmed 15 minutes (policy v2)",
		DocumentIDs: []string{"d3"}, ValidFrom: t1, ObservedAt: t1, EvidenceQuality: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if neu.Supersedes != old.ID {
		t.Fatalf("supersedes link: %+v", neu)
	}

	// Dual-axis: at t0 world time, known mid-year.
	cur := s.CurrentRelationsAsOf(t0, t1, true)
	// Contested may appear; superseded old must not as active-only.
	active := s.CurrentRelationsAsOf(t0, t1, false)
	for _, r := range active {
		if r.ID == old.ID {
			t.Fatalf("superseded edge must not surface: %+v", r)
		}
	}
	_ = cur

	// Expand from entity seed.
	nbrs := s.ExpandRelations([]string{"MedThink"}, t0, t1, 8)
	if len(nbrs) == 0 {
		t.Fatal("expand from MedThink should yield neighbors")
	}
}

func TestClaimExpireTransactionTime(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tObs := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tExp := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	c, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Policy", Predicate: "version", Object: "v1",
		DocumentIDs: []string{"d1"}, ValidFrom: tObs, ObservedAt: tObs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireClaim(c.ID, tExp); err != nil {
		t.Fatal(err)
	}
	// After expire, not current even with open knownAt.
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	cur := s.CurrentClaimsAsOf(now, time.Time{}, true)
	for _, x := range cur {
		if x.ID == c.ID {
			t.Fatalf("expired claim still current: %+v", x)
		}
	}
	// knownAt before expire still sees it via KnownAt if we included expired status —
	// CurrentClaimsAsOf skips tombstoned; ExpireClaim sets tombstoned.
	if c2, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Policy", Predicate: "version", Object: "v2",
		DocumentIDs: []string{"d2"}, ValidFrom: tExp, ObservedAt: tExp,
	}); err != nil || c2.Object != "v2" {
		t.Fatalf("admit v2: %+v %v", c2, err)
	}
	cur2 := s.CurrentClaimsAsOf(now, time.Time{}, false)
	if len(cur2) != 1 || cur2[0].Object != "v2" {
		t.Fatalf("want only v2: %+v", cur2)
	}
}

func TestMultiValuedRelationNoContest(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// tags is multi-valued in default ontology pack
	_, cont, err := s.AdmitRelation(memory.TemporalRelation{
		Src: "DocA", Relation: "tags", Dst: "security",
		ValidFrom: time.Now().UTC(), ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cont) != 0 {
		t.Fatalf("multi-valued first: %+v", cont)
	}
	_, cont, err = s.AdmitRelation(memory.TemporalRelation{
		Src: "DocA", Relation: "tags", Dst: "compliance",
		ValidFrom: time.Now().UTC(), ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cont) != 0 {
		t.Fatalf("multi-valued tags must not contest: %+v", cont)
	}
}

func TestSeedRelationsFromClaimsLeftShift(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	c, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Acme", Predicate: "ceo", Object: "Ada",
		SpanText: "Acme CEO is Ada", DocumentIDs: []string{"d1"},
		ValidFrom: t0, ObservedAt: t0, EvidenceQuality: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	n := s.SeedRelationsFromClaims()
	if n != 1 {
		t.Fatalf("first seed want 1 got %d", n)
	}
	if n2 := s.SeedRelationsFromClaims(); n2 != 0 {
		t.Fatalf("idempotent seed want 0 got %d", n2)
	}
	nbrs := s.ExpandRelations([]string{"Acme"}, t0, t0, 8)
	if len(nbrs) == 0 {
		t.Fatal("ExpandRelations should walk seeded claim edge")
	}
	found := false
	for _, id := range nbrs {
		if id == "Ada" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("want neighbor Ada from claim %s: %+v", c.ID, nbrs)
	}
	// Direct relation projection retains ClaimID for evidence.
	rels := s.RelationsForDocuments([]string{"d1"})
	if len(rels) == 0 || rels[0].ClaimID != c.ID {
		t.Fatalf("want relation ClaimID=%s: %+v", c.ID, rels)
	}
	// Serve path: ExpandRelationDocuments promotes evidence docs, not just entity names.
	docs := s.ExpandRelationDocuments([]string{"Acme"}, t0, t0, 8)
	if len(docs) != 1 || docs[0] != "d1" {
		t.Fatalf("want doc d1 from Acme relation: %+v", docs)
	}
	if fact := s.RelationFactForDoc("d1"); !strings.Contains(fact, "Ada") {
		t.Fatalf("RelationFactForDoc: %q", fact)
	}
}

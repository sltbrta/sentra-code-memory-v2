package memory_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// groupIDs reduces ContestedGroups output to key → claim IDs (in group order)
// so tests can assert exact map contents.
func groupIDs(groups map[string][]memory.Claim) map[string][]string {
	out := map[string][]string{}
	for k, claims := range groups {
		for _, c := range claims {
			out[k] = append(out[k], c.ID)
		}
	}
	return out
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A new conflict admitted after a cached read must invalidate the index:
// the next read has to contain the newly contested group, not the stale
// first-read snapshot. (Note: AdmitClaim only contests *active* peers, so a
// new same-key claim never joins an already-contested group — the invalidation
// proof uses a fresh conflicting key.)
func TestContestedGroupsRefreshesOnNewConflict(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	c1, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "price", Object: "$10",
		DocumentIDs: []string{"d1"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "price", Object: "$12",
		DocumentIDs: []string{"d2"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// First read builds the cache.
	g1 := s.ContestedGroups()
	ids1 := groupIDs(g1)
	if !sameIDs(ids1["widget|price"], []string{c1.ID, c2.ID}) {
		t.Fatalf("initial contested group = %v, want [%s %s]", ids1, c1.ID, c2.ID)
	}
	// Repeat read: identical contents served from cache.
	if ids2 := groupIDs(s.ContestedGroups()); !sameIDs(ids2["widget|price"], []string{c1.ID, c2.ID}) {
		t.Fatalf("cached contested group = %v, want [%s %s]", ids2, c1.ID, c2.ID)
	}

	// New conflicting key admitted afterwards. A stale cache would keep
	// serving the widget-only snapshot; the fresh read must show both groups.
	g1st, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "color", Object: "blue",
		DocumentIDs: []string{"d3"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	g2nd, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "color", Object: "red",
		DocumentIDs: []string{"d4"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	g2 := s.ContestedGroups()
	if len(g2) != 2 {
		t.Fatalf("contested groups after new conflict = %v, want widget + gadget groups", groupIDs(g2))
	}
	if got := groupIDs(g2)["widget|price"]; !sameIDs(got, []string{c1.ID, c2.ID}) {
		t.Fatalf("widget group changed unexpectedly: %v", got)
	}
	if got := groupIDs(g2)["gadget|color"]; !sameIDs(got, []string{g1st.ID, g2nd.ID}) {
		t.Fatalf("new contested group missing after admit: %v", got)
	}
	// Exact contents: every claim in every group is contested with full fields.
	for key, claims := range g2 {
		for _, c := range claims {
			if c.Status != memory.ClaimContested {
				t.Fatalf("group %s claim not contested: %+v", key, c)
			}
			if c.Object == "" || len(c.DocumentIDs) == 0 {
				t.Fatalf("group %s claim lost fields: %+v", key, c)
			}
		}
	}
}

// ApplyResolution (winner and tie outcomes) must invalidate the index so the
// next read reflects the new statuses, and the cache must re-arm afterwards.
func TestContestedGroupsRefreshesAfterResolution(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	a, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Leave", Predicate: "days", Object: "5 days",
		DocumentIDs: []string{"dl1"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Leave", Predicate: "days", Object: "10 days",
		DocumentIDs: []string{"dl2"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ContestedGroups()) != 1 {
		t.Fatalf("want one contested group before resolution: %v", s.ContestedGroups())
	}

	// Winner resolution (as computed by ResolveGroup for stronger evidence).
	res := memory.Resolution{
		Outcome:  memory.ResolutionWinner,
		WinnerID: b.ID,
		Reason:   "evidence_quality",
		ClaimIDs: []string{a.ID, b.ID},
	}
	if err := s.ApplyResolution(res); err != nil {
		t.Fatal(err)
	}
	if groups := s.ContestedGroups(); len(groups) != 0 {
		t.Fatalf("contested groups after winner resolution = %v, want empty", groups)
	}
	// Winner is now active and surfaces as a current claim.
	found := false
	for _, c := range s.CurrentClaims(t0.Add(24*time.Hour), false) {
		if c.ID == b.ID && c.Status == memory.ClaimActive {
			found = true
		}
	}
	if !found {
		t.Fatalf("winner %s not active/current after resolution", b.ID)
	}

	// The index must re-arm: a fresh conflict after an empty cache read has
	// to appear on the next read.
	x, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "weight", Object: "2 kg",
		DocumentIDs: []string{"dg1"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	y, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "weight", Object: "3 kg",
		DocumentIDs: []string{"dg2"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := groupIDs(s.ContestedGroups())["gadget|weight"]; !sameIDs(got, []string{x.ID, y.ID}) {
		t.Fatalf("new contested group after re-arm = %v, want [%s %s]", got, x.ID, y.ID)
	}

	// Tie resolution keeps both contested — invalidation must still run so
	// the (unchanged) group is served from a fresh, valid cache.
	tie := memory.Resolution{
		Outcome:   memory.ResolutionContested,
		Reason:    "tie",
		ClaimIDs:  []string{x.ID, y.ID},
		Contested: true,
	}
	if err := s.ApplyResolution(tie); err != nil {
		t.Fatal(err)
	}
	if got := groupIDs(s.ContestedGroups())["gadget|weight"]; !sameIDs(got, []string{x.ID, y.ID}) {
		t.Fatalf("contested group after tie resolution = %v, want [%s %s]", got, x.ID, y.ID)
	}
}

// SupersedeClaim and ExpireClaim must invalidate the index; expiry of a
// contested claim keeps it contested but the served claim must carry the new
// transaction-time close (exact contents, not stale cache).
func TestContestedGroupsRefreshesAfterSupersedeAndExpire(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	c1, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "price", Object: "$10",
		DocumentIDs: []string{"d1"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, _, err := s.AdmitClaim(memory.Claim{
		Subject: "Widget", Predicate: "price", Object: "$12",
		DocumentIDs: []string{"d2"}, ValidFrom: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ContestedGroups()) != 1 {
		t.Fatalf("want one contested group: %v", s.ContestedGroups())
	}

	// Supersede the first contested claim; the group must shrink to c2.
	if _, err := s.SupersedeClaim(c1.ID, memory.Claim{
		Subject: "Widget", Predicate: "price", Object: "$99",
		DocumentIDs: []string{"d3"},
	}, t0.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := groupIDs(s.ContestedGroups())["widget|price"]; !sameIDs(got, []string{c2.ID}) {
		t.Fatalf("contested group after supersede = %v, want [%s]", got, c2.ID)
	}

	// Expire the remaining contested claim: status stays contested, but the
	// served claim must show the fresh ExpiredAt (a stale cache would not).
	tExp := t0.Add(72 * time.Hour)
	if err := s.ExpireClaim(c2.ID, tExp); err != nil {
		t.Fatal(err)
	}
	groups := s.ContestedGroups()
	got := groups["widget|price"]
	if len(got) != 1 || got[0].ID != c2.ID {
		t.Fatalf("contested group after expire = %v, want [%s]", groupIDs(groups), c2.ID)
	}
	if got[0].ExpiredAt == nil || !got[0].ExpiredAt.Equal(tExp) {
		t.Fatalf("served contested claim missing fresh ExpiredAt: %+v", got[0])
	}
}

// Readers hammering the answer-path read while claim mutations are in flight
// must never observe torn state (run with -race).
func TestContestedGroupsConcurrentMutationAndRead(t *testing.T) {
	s, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: contested index serves only contested claims; CurrentClaims
	// stays usable while mutations are in flight.
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, claims := range s.ContestedGroups() {
					for _, c := range claims {
						if c.Status != memory.ClaimContested {
							t.Errorf("non-contested claim served from contested index: %+v", c)
							return
						}
					}
				}
				_ = s.CurrentClaims(t0.Add(24*time.Hour), true)
			}
		}()
	}

	// Writer: conflicting admits across a few keys plus expiry churn.
	const iters = 150
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		var ids []string
		for i := 0; i < iters; i++ {
			c, _, err := s.AdmitClaim(memory.Claim{
				Subject: fmt.Sprintf("item-%d", i%3), Predicate: "price",
				Object:      fmt.Sprintf("$%d", i),
				DocumentIDs: []string{fmt.Sprintf("d%d", i)},
				ValidFrom:   t0,
			})
			if err != nil {
				t.Errorf("admit: %v", err)
				return
			}
			ids = append(ids, c.ID)
			if len(ids) > 8 {
				if err := s.ExpireClaim(ids[0], t0.Add(48*time.Hour)); err != nil {
					t.Errorf("expire: %v", err)
					return
				}
				ids = ids[1:]
			}
		}
	}()
	<-done
	close(stop)
	wg.Wait()

	// Resolution over the cached index while readers are done: ties stay
	// contested, and the index stays consistent with the outcome.
	s.ResolveContestedGroups()
	for _, claims := range s.ContestedGroups() {
		for _, c := range claims {
			if c.Status != memory.ClaimContested {
				t.Fatalf("post-resolution index claim not contested: %+v", c)
			}
		}
	}
}

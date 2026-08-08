package hosted

import (
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// The answer path (applyClaimConflictPolicy) reads ContestedGroups through a
// cached index. The index must be invalidated by claim mutations so policy
// decisions never come from a stale conflict snapshot: after a resolution
// empties the groups the policy must no-op, and a freshly admitted conflict
// must be visible on the very next answer.
func TestClaimConflictPolicySeesFreshContestedGroups(t *testing.T) {
	c, err := CreateLocal(t.TempDir(), "brain")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mem := c.MemoryStore()
	if mem == nil {
		t.Fatal("memory store missing")
	}

	// Tied contested pair (equal quality/docs, open windows) → abstain policy.
	a, _, err := mem.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "weight", Object: "2 kg",
		DocumentIDs: []string{"dg1"}, EvidenceQuality: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := mem.AdmitClaim(memory.Claim{
		Subject: "Gadget", Predicate: "weight", Object: "3 kg",
		DocumentIDs: []string{"dg2"}, EvidenceQuality: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if groups := mem.ContestedGroups(); len(groups) != 1 {
		t.Fatalf("want one contested group before policy: %v", groups)
	}

	g := &Grounded{Answer: "2 kg", CitedDocumentIDs: []string{"dg1"}, Diagnostics: map[string]any{}}
	diag := map[string]any{}
	c.applyClaimConflictPolicy("What is Gadget weight?", g, diag)
	if diag["conflict_policy"] != "dual_cite_and_abstain" {
		t.Fatalf("tie policy = %v, want dual_cite_and_abstain (diag=%v)", diag["conflict_policy"], diag)
	}

	// Resolve the group; the answer path must immediately see zero groups.
	res := memory.Resolution{
		Outcome:  memory.ResolutionWinner,
		WinnerID: b.ID,
		Reason:   "evidence_quality",
		ClaimIDs: []string{a.ID, b.ID},
	}
	if err := mem.ApplyResolution(res); err != nil {
		t.Fatal(err)
	}
	g2 := &Grounded{Answer: "3 kg", CitedDocumentIDs: []string{"dg2"}, Diagnostics: map[string]any{}}
	diag2 := map[string]any{}
	c.applyClaimConflictPolicy("What is Gadget weight?", g2, diag2)
	if _, ok := diag2["claim_conflict"]; ok {
		t.Fatalf("policy still sees contested groups after resolution: diag=%v", diag2)
	}
	if g2.Answer != "3 kg" {
		t.Fatalf("answer rewritten despite no contested groups: %q", g2.Answer)
	}

	// A new conflict admitted afterwards must be visible to the very next
	// answer (cache re-armed, not pinned to the empty snapshot).
	if _, _, err := mem.AdmitClaim(memory.Claim{
		Subject: "Gizmo", Predicate: "size", Object: "small",
		DocumentIDs: []string{"dg3"}, EvidenceQuality: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mem.AdmitClaim(memory.Claim{
		Subject: "Gizmo", Predicate: "size", Object: "large",
		DocumentIDs: []string{"dg4"}, EvidenceQuality: 4,
	}); err != nil {
		t.Fatal(err)
	}
	g3 := &Grounded{Answer: "small", CitedDocumentIDs: []string{"dg3"}, Diagnostics: map[string]any{}}
	diag3 := map[string]any{}
	c.applyClaimConflictPolicy("What size is Gizmo?", g3, diag3)
	if diag3["conflict_policy"] != "dual_cite_and_abstain" {
		t.Fatalf("new conflict not visible to policy: diag=%v", diag3)
	}
	if diag3["contested_claim_groups"] != 1 {
		t.Fatalf("policy saw %v contested groups, want 1", diag3["contested_claim_groups"])
	}
}

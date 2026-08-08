package query

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// packEntry builds a digest-free evidence entry for packing tests. Packing
// measures bytes and permutes references only; it never inspects canonical
// digests, so the path/line markers are enough to prove ordering, truncation,
// offset preservation, and citation safety.
func packEntry(path string, blockStart uint32, lines ...string) EvidenceEntry {
	if len(lines) == 0 {
		lines = []string{"x"}
	}
	return EvidenceEntry{
		Path:           path,
		Language:       "go",
		RevisionID:     path + "@" + fmt.Sprint(blockStart),
		BlobOID:        strings.Repeat("b", 40),
		ContentDigest:  strings.Repeat("c", 64),
		BlockStartLine: blockStart,
		Lines:          lines,
		DefinitionText: path,
	}
}

func intsEqual(a, b []int) bool {
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

// TestPackEvidenceAppliesFrozenBoundsInRetrievalOrder pins the deterministic
// fallback: greedy fill in retrieval-rank order up to the frozen entry cap,
// oversized entries dropped with packing continuing, and the dropped slice as
// the input-order remainder of the wide retrieval pool.
func TestPackEvidenceAppliesFrozenBoundsInRetrievalOrder(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxEvidenceEntries = 3
	limits.MaxEvidencePackBytes = 10 * 1024

	entries := []EvidenceEntry{
		packEntry("a.go", 1), packEntry("b.go", 1), packEntry("c.go", 1), packEntry("d.go", 1),
	}
	packed, dropped := packEvidence(entries, limits)
	if len(packed) != 3 || len(dropped) != 1 {
		t.Fatalf("packed=%d dropped=%d", len(packed), len(dropped))
	}
	if packed[0].Path != "a.go" || packed[2].Path != "c.go" {
		t.Fatalf("retrieval order not preserved: %#v", packed)
	}
	if dropped[0].Path != "d.go" {
		t.Fatalf("dropped must be input-order remainder: %#v", dropped)
	}

	// An oversized entry is dropped and packing continues with the rest.
	big := packEntry("z.go", 1, strings.Repeat("x", limits.MaxEvidenceEntryBytes+1))
	packedBig, droppedBig := packEvidence(append([]EvidenceEntry{big}, entries...), limits)
	if len(packedBig) != 3 {
		t.Fatalf("oversized entry must drop and packing continue: packed=%d", len(packedBig))
	}
	if droppedBig[0].Path != "z.go" {
		t.Fatalf("oversized entry must lead dropped: %#v", droppedBig)
	}
}

// TestOrderEvidenceStrategies proves each ordering strategy permutes retrieval
// rank deterministically, never mutates the wide pool, and falls back to
// retrieval order for an unknown strategy.
func TestOrderEvidenceStrategies(t *testing.T) {
	// Retrieval-rank input: index 0 strongest ... 4 weakest. Paths/lines are
	// deliberately out of document order so original-order differs from rank.
	entries := []EvidenceEntry{
		packEntry("c.go", 10), // rank 0
		packEntry("a.go", 30), // rank 1
		packEntry("e.go", 5),  // rank 2
		packEntry("b.go", 20), // rank 3
		packEntry("d.go", 15), // rank 4
	}
	cases := []struct {
		order PackOrder
		want  []int
	}{
		{PackOrderRetrieval, []int{0, 1, 2, 3, 4}},
		{PackOrderOriginal, []int{1, 3, 0, 4, 2}}, // sorted by (path, block line)
		{PackOrderTailFirst, []int{4, 3, 2, 1, 0}},
		{PackOrderHeadTail, []int{0, 2, 4, 3, 1}}, // strongest at both edges, weakest in middle
	}
	for _, tc := range cases {
		got := orderEvidence(entries, tc.order)
		if !intsEqual(got, tc.want) {
			t.Errorf("order=%s got %v want %v", tc.order, got, tc.want)
		}
	}

	// Ordering is a pure permutation: source offsets, lines, and identity are
	// never mutated.
	before := make([]EvidenceEntry, len(entries))
	for i := range entries {
		before[i] = entries[i]
	}
	for _, order := range []PackOrder{PackOrderRetrieval, PackOrderOriginal, PackOrderTailFirst, PackOrderHeadTail} {
		_ = orderEvidence(entries, order)
	}
	for i := range entries {
		if fmt.Sprintf("%#v", entries[i]) != fmt.Sprintf("%#v", before[i]) {
			t.Fatalf("orderEvidence mutated wide-pool entry[%d]: %#v != %#v", i, entries[i], before[i])
		}
	}

	// An unknown order falls back to retrieval rather than dropping evidence.
	if got := orderEvidence(entries, PackOrder("bogus")); !intsEqual(got, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("unknown order must fall back to retrieval: %v", got)
	}
}

// TestPackBudgetNarrowsWithoutWidening proves the adaptive budget only narrows
// the grounding pack below the frozen ceilings: a narrower entry/byte target
// drops the low-ranked remainder, a zero budget equals the frozen bounds, and
// an over-max target never widens beyond the contract cap.
func TestPackBudgetNarrowsWithoutWidening(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxEvidenceEntries = 4
	limits.MaxEvidencePackBytes = 100 * 1024
	entries := []EvidenceEntry{
		packEntry("a.go", 1), packEntry("b.go", 1), packEntry("c.go", 1), packEntry("d.go", 1),
	}

	// Target entries narrows below the frozen cap, keeping the strongest first.
	packed, dropped := packEvidenceWithPolicy(entries, PackPolicy{
		Order: PackOrderRetrieval, Budget: PackBudget{TargetEntries: 2},
	}, limits)
	if len(packed) != 2 || len(dropped) != 2 {
		t.Fatalf("target entries: packed=%d dropped=%d", len(packed), len(dropped))
	}
	if packed[0].Path != "a.go" || packed[1].Path != "b.go" {
		t.Fatalf("narrowed pack must keep strongest-first: %#v", packed)
	}
	if dropped[0].Path != "c.go" || dropped[1].Path != "d.go" {
		t.Fatalf("dropped must be input-order remainder: %#v", dropped)
	}

	// Target bytes narrows below the frozen cap. Each entry is "x\n" = 2 bytes,
	// so a 5-byte budget admits two entries and drops the third at the boundary.
	packedBytes, _ := packEvidenceWithPolicy(entries, PackPolicy{
		Budget: PackBudget{TargetBytes: 5},
	}, limits)
	if len(packedBytes) != 2 {
		t.Fatalf("target bytes must narrow pack: packed=%d want 2", len(packedBytes))
	}

	// Zero budget equals the frozen bounds: no narrowing, no widening.
	packedZero, droppedZero := packEvidenceWithPolicy(entries, PackPolicy{}, limits)
	if len(packedZero) != 4 || len(droppedZero) != 0 {
		t.Fatalf("zero budget must equal frozen bounds: packed=%d dropped=%d", len(packedZero), len(droppedZero))
	}

	// An over-max target is clamped by the packer and never widens the pack.
	packedOver, _ := packEvidenceWithPolicy(entries, PackPolicy{
		Budget: PackBudget{TargetEntries: limits.MaxEvidenceEntries + 10},
	}, limits)
	if len(packedOver) != 4 {
		t.Fatalf("over-max target must not widen beyond frozen cap: packed=%d", len(packedOver))
	}
}

// TestPackingPreservesSourceOffsets proves ordering and truncation leave every
// survivor byte-identical to its wide-pool origin: absolute block offsets,
// revision identity, and leaf lines are unchanged, so citation anchor math is
// unaffected by any pack permutation.
func TestPackingPreservesSourceOffsets(t *testing.T) {
	entries := []EvidenceEntry{
		packEntry("a.go", 100, "alpha", "return \"one\""),
		packEntry("b.go", 50, "beta", "return \"two\""),
		packEntry("c.go", 200, "gamma", "return \"three\""),
	}
	before := make([]EvidenceEntry, len(entries))
	for i := range entries {
		before[i] = entries[i]
	}
	policy := PackPolicy{Order: PackOrderHeadTail, Budget: PackBudget{TargetEntries: 2}}
	packed, _ := packEvidenceWithPolicy(entries, policy, DefaultLimits())

	survivor := map[string]EvidenceEntry{}
	for _, entry := range packed {
		survivor[entry.Path] = entry
	}
	for _, original := range entries {
		got, ok := survivor[original.Path]
		if !ok {
			continue // truncated away; offset safety for survivors only
		}
		if got.BlockStartLine != original.BlockStartLine ||
			got.RevisionID != original.RevisionID ||
			got.Path != original.Path ||
			fmt.Sprintf("%#v", got.Lines) != fmt.Sprintf("%#v", original.Lines) {
			t.Fatalf("packing mutated offsets/lines for %s: %#v vs %#v", original.Path, got, original)
		}
	}
	// The wide retrieval pool is never mutated by packing.
	for i := range entries {
		if fmt.Sprintf("%#v", entries[i]) != fmt.Sprintf("%#v", before[i]) {
			t.Fatalf("packing mutated wide-pool entry[%d]", i)
		}
	}
}

// TestCitationsVerifyAfterReorderAndTruncate is the citation-offset safety
// proof: after head/tail reordering and budget truncation, a citation whose
// entry survived still verifies against the packed slice with the identical
// supporting-text digest, because offsets are absolute and per-entry. A
// citation into a truncated-away slot fails verification rather than binding
// stale bytes.
func TestCitationsVerifyAfterReorderAndTruncate(t *testing.T) {
	strong := packEntry("strong.go", 10, "def Strong() string {", "    return \"first\"")
	middle := packEntry("middle.go", 50, "def Middle() string {", "    return \"mid\"")
	weak := packEntry("weak.go", 200, "def Weak() string {", "    return \"second\"")
	wide := []EvidenceEntry{strong, middle, weak} // ranks 0,1,2

	// Absolute citation onto strong's return line (block starts at line 10,
	// return is line 11). The line `    return "first"` is 18 bytes, so the
	// half-open span ends at column 19.
	proposed := ProposedCitation{EvidenceIndex: 0, StartLine: 11, StartColumn: 5, EndLine: 11, EndColumn: 19}
	_, wantDigest, err := resolveSupportingText(strong, proposed)
	if err != nil {
		t.Fatalf("resolve original supporting text: %v", err)
	}

	// head/tail on three ranks reorders to [strong, weak, middle]; a budget of
	// two truncates `middle` away.
	policy := PackPolicy{Order: PackOrderHeadTail, Budget: PackBudget{TargetEntries: 2}}
	packed, dropped := packEvidenceWithPolicy(wide, policy, DefaultLimits())
	if len(packed) != 2 || len(dropped) != 1 || dropped[0].Path != "middle.go" {
		t.Fatalf("pack: packed=%#v dropped=%#v", packed, dropped)
	}
	if packed[0].Path != "strong.go" || packed[1].Path != "weak.go" {
		t.Fatalf("head/tail reorder+truncate order wrong: %#v", packed)
	}

	// Strong survived at pack index 0. The same ABSOLUTE offsets with the NEW
	// pack index still verify and yield the identical digest.
	shifted := proposed // EvidenceIndex already 0; rebind explicitly to prove index-independence
	shifted.EvidenceIndex = 0
	cited, _, err := verifyCitation(shifted, packed, Snapshot{})
	if err != nil {
		t.Fatalf("verifyCitation after reorder/truncate: %v", err)
	}
	if cited.SupportingTextDigest != wantDigest {
		t.Fatalf("supporting-text digest changed under reorder: got %s want %s", cited.SupportingTextDigest, wantDigest)
	}
	if cited.Path != "strong.go" || cited.StartLine != 11 || cited.EndLine != 11 {
		t.Fatalf("citation bound wrong entry/range: %#v", cited)
	}

	// A citation into the truncated-away slot (index beyond the packed range)
	// fails verification rather than resolving to stale bytes: pruning cannot
	// leave a dangling citation that verifies.
	droppedProposal := ProposedCitation{EvidenceIndex: len(packed), StartLine: 51, StartColumn: 5, EndLine: 51, EndColumn: 17}
	if _, _, err := verifyCitation(droppedProposal, packed, Snapshot{}); err == nil {
		t.Fatal("citation into a truncated slot must fail verification")
	}
}

// TestPackPolicyDeterministicFallback proves the zero policy, an explicit
// retrieval policy, and the legacy packEvidence entry point agree byte-for-
// byte, and that any policy is reproducible across calls.
func TestPackPolicyDeterministicFallback(t *testing.T) {
	limits := DefaultLimits()
	entries := []EvidenceEntry{
		packEntry("a.go", 1), packEntry("b.go", 1), packEntry("c.go", 1),
	}
	zeroPacked, zeroDropped := packEvidenceWithPolicy(entries, PackPolicy{}, limits)
	explicitPacked, explicitDropped := packEvidenceWithPolicy(entries, PackPolicy{Order: PackOrderRetrieval}, limits)
	legacyPacked, legacyDropped := packEvidence(entries, limits)

	if fmt.Sprintf("%#v", zeroPacked) != fmt.Sprintf("%#v", explicitPacked) ||
		fmt.Sprintf("%#v", zeroPacked) != fmt.Sprintf("%#v", legacyPacked) {
		t.Fatalf("zero, explicit-retrieval, and legacy pack must agree:\n%#v\n%#v\n%#v", zeroPacked, explicitPacked, legacyPacked)
	}
	if fmt.Sprintf("%#v", zeroDropped) != fmt.Sprintf("%#v", explicitDropped) ||
		fmt.Sprintf("%#v", zeroDropped) != fmt.Sprintf("%#v", legacyDropped) {
		t.Fatalf("dropped must agree across fallback shapes")
	}

	// Any policy is byte-for-byte reproducible.
	policy := PackPolicy{Order: PackOrderHeadTail, Budget: PackBudget{TargetEntries: 2}}
	first, _ := packEvidenceWithPolicy(entries, policy, limits)
	second, _ := packEvidenceWithPolicy(entries, policy, limits)
	if fmt.Sprintf("%#v", first) != fmt.Sprintf("%#v", second) {
		t.Fatalf("non-deterministic pack under fixed policy")
	}
}

// TestPackPolicyValidation proves a policy is admitted only when its order is
// known and its budget stays within the frozen ceilings.
func TestPackPolicyValidation(t *testing.T) {
	limits := DefaultLimits()
	if err := (PackPolicy{}).validate(limits); err != nil {
		t.Fatalf("zero policy must validate: %v", err)
	}
	if err := (PackPolicy{Order: PackOrderHeadTail, Budget: PackBudget{TargetEntries: 2, TargetBytes: 1024}}).validate(limits); err != nil {
		t.Fatalf("in-range policy must validate: %v", err)
	}
	for i, bad := range []PackPolicy{
		{Order: PackOrder("bogus")},
		{Budget: PackBudget{TargetEntries: -1}},
		{Budget: PackBudget{TargetBytes: -1}},
		{Budget: PackBudget{TargetEntries: limits.MaxEvidenceEntries + 1}},
		{Budget: PackBudget{TargetBytes: limits.MaxEvidencePackBytes + 1}},
	} {
		if err := bad.validate(limits); err == nil {
			t.Fatalf("bad policy %d must be rejected: %#v", i, bad)
		}
	}
}

// TestEngineValidatesAndAppliesPackingPolicy proves the engine admits an
// optional packing policy at construction, rejects misshapen policies there,
// and grounds safely (verified citation, no citation_verification_failed)
// under a non-default ordering and narrowed budget.
func TestEngineValidatesAndAppliesPackingPolicy(t *testing.T) {
	corpus := buildFixtureCorpus(t)
	_, currentID := generationIDs(t, corpus)

	base := Config{
		Corpus: corpus, Authorizer: &stubAuthorizer{epoch: 7},
		Synthesizer: NewDeterministicSynthesizer(), Clock: stubClock{now: testNow},
		Limits: DefaultLimits(), AllowLegacyUnadmittedEvidence: true,
	}

	// Over-max budget is rejected at construction, never at request time.
	over := base
	over.Packing = PackPolicy{Budget: PackBudget{TargetEntries: base.Limits.MaxEvidenceEntries + 1}}
	if _, err := NewEngine(over); err == nil {
		t.Fatal("over-max pack budget must be rejected at construction")
	}
	// Unknown order is rejected at construction.
	badOrder := base
	badOrder.Packing = PackPolicy{Order: PackOrder("nope")}
	if _, err := NewEngine(badOrder); err == nil {
		t.Fatal("unknown pack order must be rejected at construction")
	}

	// A non-default policy still grounds and verifies citations end to end.
	withPolicy := base
	withPolicy.Packing = PackPolicy{Order: PackOrderHeadTail, Budget: PackBudget{TargetEntries: 1}}
	engine, err := NewEngine(withPolicy)
	if err != nil {
		t.Fatalf("NewEngine with policy: %v", err)
	}

	manifest := loadGroundingCases(t)
	var answered *groundingCase
	for i := range manifest.Cases {
		if manifest.Cases[i].CaseID == "answered-go-anchor" {
			answered = &manifest.Cases[i]
			break
		}
	}
	if answered == nil {
		t.Fatal("answered-go-anchor fixture case missing")
	}
	result, err := engine.Answer(context.Background(), fixtureQuery(
		answered.CaseID, currentID, answered.Query, answered.Freshness,
	))
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if result.Answer.Status != StatusAnswered {
		t.Fatalf("status = %s, want answered", result.Answer.Status)
	}
	if len(result.Answer.Claims) == 0 || len(result.Answer.Claims[0].Citations) == 0 {
		t.Fatalf("policy must not strip the verified citation: %#v", result.Answer)
	}
	for _, reason := range result.Answer.DegradedReasons {
		if reason == ReasonCitationVerificationFailed {
			t.Fatalf("citation verification must not fail under a packing policy: %#v", result.Answer)
		}
	}
}

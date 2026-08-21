package memory

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// N-004. Two producers built phrase node ids independently and disagreed:
// SeedPhrasePassageEdgesFromClaims replaced spaces with underscores and cut at
// 48 bytes, while BuildBipartitePhraseEdges used the raw phrase. The same
// phrase therefore became two disconnected nodes, so every seeded edge pointed
// at an id the query path never constructs -- the seeding contributed nothing
// to the graph, and a comment claimed the prefixes matched.
//
// The fix unified them in PhraseNodeID. Nothing asserted that the two agree,
// so reverting either one left the suite green.

const seededPhrase = "revenue recognition"

// longPhrase is 42 characters in four words: past PhraseNodeID's 40-rune cut
// but inside the seeder's old 48-byte one, so the two schemes disagree here
// and nowhere shorter. Without it a revert of the seeder alone is invisible,
// because both schemes turn spaces into underscores.
const longPhrase = "consolidated quarterly revenue recognition"

func phraseFixture(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, _, err := store.AdmitClaim(Claim{
		Subject: seededPhrase, Predicate: "covers", Object: "deferred balances",
		DocumentIDs: []string{"doc-1"},
		ValidFrom:   now, ObservedAt: now, Status: ClaimActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdmitClaim(Claim{
		Subject: longPhrase, Predicate: "governs", Object: "segment reporting",
		DocumentIDs: []string{"doc-2"},
		ValidFrom:   now, ObservedAt: now, Status: ClaimActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDocTexts(map[string]string{
		"doc-1": "the " + seededPhrase + " policy applies to deferred balances",
		"doc-2": "the " + longPhrase + " governs segment reporting",
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// phraseNodes returns every "phrase:" node appearing in an adjacency map.
func phraseNodes(adjacency map[string][]string) map[string]bool {
	out := map[string]bool{}
	for node, neighbours := range adjacency {
		if strings.HasPrefix(node, "phrase:") {
			out[node] = true
		}
		for _, n := range neighbours {
			if strings.HasPrefix(n, "phrase:") {
				out[n] = true
			}
		}
	}
	return out
}

// TestBothPhraseProducersBuildTheSameNodeID is the whole finding: the ids the
// seeder writes must be the ids the query path builds, or the seeded edges
// join nothing to nothing.
//
// The two producers are run against separate stores holding identical content.
// Running them against one store hides the bug: BuildBipartitePhraseEdges
// starts from the stored doc adjacency, so the seeder's own nodes come back
// through the base and the comparison passes whatever ids the builder itself
// produces.
func TestBothPhraseProducersBuildTheSameNodeID(t *testing.T) {
	seederStore := phraseFixture(t)
	builderStore := phraseFixture(t)

	if added := seederStore.SeedPhrasePassageEdgesFromClaims(); added == 0 {
		t.Fatal("the seeder added no edges, so this guard checked nothing")
	}
	seeded := map[string]bool{}
	for key := range seederStore.WeightedEdges() {
		a, b, ok := parseEdgeKey(key)
		if !ok {
			continue
		}
		for _, side := range []string{a, b} {
			if strings.HasPrefix(side, "phrase:") {
				seeded[side] = true
			}
		}
	}
	built := phraseNodes(builderStore.BuildBipartitePhraseEdges(256))
	if len(seeded) == 0 || len(built) == 0 {
		t.Fatalf("no phrase nodes on one side: seeded=%v built=%v",
			sortedKeys(seeded), sortedKeys(built))
	}

	// Every phrase node the seeder emits is derived from a claim, and the
	// builder derives nodes from those same claims -- so each one has to be a
	// node the query path constructs. A single mismatch is the finding.
	for node := range seeded {
		if !built[node] {
			t.Fatalf("the seeder emitted %q, which the query path never builds.\n"+
				"seeded: %v\nbuilt:  %v\nSeeded edges point at disconnected nodes "+
				"and contribute nothing to the graph.",
				node, sortedKeys(seeded), sortedKeys(built))
		}
	}

	for _, phrase := range []string{seededPhrase, longPhrase} {
		want := PhraseNodeID(phrase)
		if !seeded[want] || !built[want] {
			t.Fatalf("expected both producers to build %q; seeded=%v built=%v",
				want, seeded[want], built[want])
		}
	}
}

// TestPhraseNodeIDTruncatesByRuneNotByte covers the other half of the fix:
// cutting a multi-byte phrase at a byte offset produces ids that are not valid
// UTF-8.
func TestPhraseNodeIDTruncatesByRuneNotByte(t *testing.T) {
	id := PhraseNodeID(strings.Repeat("é", 80))
	if !utf8.ValidString(id) {
		t.Fatalf("node id is not valid UTF-8: %q", id)
	}
	if n := utf8.RuneCountInString(strings.TrimPrefix(id, "phrase:")); n != 40 {
		t.Fatalf("truncated to %d runes, want 40", n)
	}
	if PhraseNodeID("  spaced phrase  ") != PhraseNodeID("spaced phrase") {
		t.Fatal("surrounding whitespace changed the node id")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

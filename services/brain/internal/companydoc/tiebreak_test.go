package companydoc

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// N-005. companydoc collected documents from a map and sorted on score alone.
// tokenOverlap produces coarse values -- 0.5 and 0.33 are common -- so ties
// across documents are the normal case, not an edge case, and a tie kept
// whatever order Go's randomised map iteration happened to produce. The same
// corpus and the same question therefore cited different documents in
// different processes.
//
// The fix adds the document id as a total order. Nothing compared two runs, so
// reverting it left the suite green.

// tiedCorpus builds documents that all score identically against the question
// below: same tokens, same length, different ids. Nothing but the tiebreak can
// order them.
func tiedCorpus(t *testing.T) *LiveCorpus {
	t.Helper()
	docs := make([]Document, 0, 24)
	for i := 0; i < 24; i++ {
		docs = append(docs, Document{
			ID:    fmt.Sprintf("doc-%02d", i),
			Title: fmt.Sprintf("Policy %02d", i),
			Text:  "quarterly revenue recognition policy for the segment",
		})
	}
	corpus, err := OpenLive(context.Background(), Batch{
		SourceID: "src-1", GenerationID: "gen-1", Kind: SourceInline,
		Documents: docs, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return corpus
}

func TestTiedLexicalScoresRankInAStableOrder(t *testing.T) {
	const question = "quarterly revenue recognition"
	first := diagnosticList(t, tiedCorpus(t), question, "lexical")
	if len(first) < 2 {
		t.Fatalf("want several tied documents, got %v", first)
	}
	for round := 0; round < 12; round++ {
		// A fresh corpus each round: the map is rebuilt, so its iteration
		// order is drawn again.
		got := diagnosticList(t, tiedCorpus(t), question, "lexical")
		if !sameOrder(first, got) {
			t.Fatalf("round %d ranked tied documents differently:\nfirst: %v\ngot:   %v\n"+
				"the same corpus and question cite different documents per process",
				round, first, got)
		}
	}
}

func TestFusedRankingIsStableAcrossRuns(t *testing.T) {
	const question = "quarterly revenue recognition"
	first := answerCitations(t, tiedCorpus(t), question)
	if len(first) == 0 {
		t.Fatal("no citations, so this guard checked nothing")
	}
	for round := 0; round < 12; round++ {
		got := answerCitations(t, tiedCorpus(t), question)
		if !sameOrder(first, got) {
			t.Fatalf("round %d cited a different set or order:\nfirst: %v\ngot:   %v",
				round, first, got)
		}
	}
}

func sameOrder(a, b []string) bool {
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

func diagnosticList(t *testing.T, corpus *LiveCorpus, question, key string) []string {
	t.Helper()
	_, diag := corpus.Retrieve(context.Background(), question, 8)
	got, ok := diag[key].([]string)
	if !ok {
		t.Fatalf("diagnostic %q is %T, not []string", key, diag[key])
	}
	return got
}

func answerCitations(t *testing.T, corpus *LiveCorpus, question string) []string {
	t.Helper()
	ids, _ := corpus.Retrieve(context.Background(), question, 8)
	return ids
}

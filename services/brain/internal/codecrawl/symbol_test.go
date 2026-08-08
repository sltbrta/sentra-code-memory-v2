package codecrawl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSymbolHopPromotesLinkedFile(t *testing.T) {
	root := t.TempDir()
	// a.go defines UniqueHopMarker; b.go only references it (no shared FTS-heavy tokens with query).
	a := `package a
func UniqueHopMarker() int { return 42 }
`
	b := `package b
func other() { _ = UniqueHopMarker() }
`
	noise := `package noise
func UnrelatedPicnic() {}
`
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "noise.go"), []byte(noise), 0o644); err != nil {
		t.Fatal(err)
	}

	idx, st, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.FilesIndexed != 3 {
		t.Fatalf("files %d", st.FilesIndexed)
	}
	defs, refs := idx.SymbolStats()
	if defs == 0 {
		t.Fatal("expected symbol defs extracted")
	}
	_ = refs

	// Lexical search for the function name hits a.go (definition body).
	hits := idx.Search("UniqueHopMarker", 5)
	if len(hits) == 0 {
		t.Fatal("expected lexical hit")
	}
	// With hop, b.go should appear even if weaker TF.
	hopped := idx.SearchOpts("UniqueHopMarker", 5, true)
	foundB := false
	for _, h := range hopped {
		if h.Path == "b.go" || filepath.Base(h.Path) == "b.go" {
			foundB = true
		}
	}
	// SymbolHop from a.go seeds should list b.go
	seeds := []string{"a.go"}
	neigh := idx.SymbolHop(seeds, 8)
	for _, n := range neigh {
		if n == "b.go" {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("expected b.go via symbol hop; hits=%v hop=%v neigh=%v defs=%d", hopped, hits, neigh, defs)
	}
}

func TestExtractGoSymbolsDefs(t *testing.T) {
	defs, _, _ := extractGoSymbols("x.go", "package x\nfunc HelloWorld() {}\ntype FooBar struct{}\n")
	has := map[string]bool{}
	for _, d := range defs {
		has[d] = true
	}
	if !has["HelloWorld"] || !has["FooBar"] {
		t.Fatalf("defs=%v", defs)
	}
}

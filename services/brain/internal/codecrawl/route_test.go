package codecrawl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindRouteBridges(t *testing.T) {
	dir := t.TempDir()
	// a.go defines Foo; b.go refs Foo and Bar; c.go defines Bar
	mustWrite(t, filepath.Join(dir, "a.go"), "package p\nfunc Foo() {}\n")
	mustWrite(t, filepath.Join(dir, "b.go"), "package p\nfunc use() { Foo(); Bar() }\n")
	mustWrite(t, filepath.Join(dir, "c.go"), "package p\nfunc Bar() {}\n")
	idx, _, err := CrawlDir(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec := idx.FindRoute("Foo", "Bar", 8)
	if rec.Authority == "" {
		t.Fatal("empty authority")
	}
	// Expect at least via symbols or bridges.
	if len(rec.ViaSymbols) == 0 && len(rec.Bridges) == 0 {
		t.Fatalf("empty route: %+v", rec)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

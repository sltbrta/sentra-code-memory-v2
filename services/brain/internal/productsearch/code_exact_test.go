package productsearch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodeExactFindsDefinition(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pkg.go")
	if err := os.WriteFile(src, []byte("package pkg\n\nfunc OpenOrRefresh() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Search(context.Background(), Request{
		Profile: ProfileCodeExact, CodeRoot: dir, Question: "OpenOrRefresh",
		ExactKind: "definition", TopK: 10,
	})
	if res.Failure != "" {
		t.Fatal(res.Failure)
	}
	if len(res.Hits) < 1 {
		t.Fatalf("expected hits: %+v", res)
	}
	if res.SearchMode != "product_codeindex_exact" {
		t.Fatalf("mode %s", res.SearchMode)
	}
}

package productsearch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeExactScansLargeRepositoryWithoutSnapshotResultOverflow(t *testing.T) {
	root := t.TempDir()
	body := "package sample\n\nfunc Target() {}\n"
	body += strings.Repeat("func useTarget() { Target() }\n", 320)
	for i := 0; i < 200; i++ {
		path := filepath.Join(root, fmt.Sprintf("%03d.go", i))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result := searchCodeExact(context.Background(), Request{
		CodeRoot: root, Question: "Target", TopK: 1, ExactKind: "definition",
	})
	if result.Failure != "" {
		t.Fatalf("large exact search failed: %s", result.Failure)
	}
	if len(result.Hits) != 1 || result.Hits[0].ID != "000.go" {
		t.Fatalf("unexpected exact hits: %#v", result.Hits)
	}
	if result.RetrievalDiagnostics["files"] != 200 {
		t.Fatalf("expected all sources to be accounted for, diagnostics=%#v", result.RetrievalDiagnostics)
	}
}

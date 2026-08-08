package productsearch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeExactHonorsRepositoryIgnorePolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("generated/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"kept.go":              "package kept\nfunc Target() {}\n",
		"generated/ignored.go": "package ignored\nfunc Target() {}\n",
		".env.go":              "package secret\nfunc Target() {}\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result := searchCodeExact(context.Background(), Request{
		CodeRoot: root, Question: "Target", TopK: 10, ExactKind: "definition",
	})
	if result.Failure != "" {
		t.Fatalf("exact search failed: %s", result.Failure)
	}
	if len(result.Hits) != 1 || result.Hits[0].ID != "kept.go" {
		t.Fatalf("ignored exact sources leaked into results: %#v", result.Hits)
	}
}

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

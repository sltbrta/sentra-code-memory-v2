package repoignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadHonorsRepositoryIgnoreFilesAndDefaults(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/root-only/\nignored/\n*.generated.go\n!important.generated.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("docker-only/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("excluded.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	matcher, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path  string
		dir   bool
		want  bool
		label string
	}{
		{"root-only", true, true, "rooted gitignore directory"},
		{"nested/root-only", true, false, "rooted pattern stays at root"},
		{"ignored", true, true, "gitignore directory"},
		{"ignored", false, false, "directory pattern does not hide same-named file"},
		{"ignored/source.go", false, true, "gitignore child"},
		{"docker-only/source.go", false, true, "dockerignore child"},
		{"excluded.go", false, true, "git exclude"},
		{"config.generated.go", false, true, "generated pattern"},
		{"important.generated.go", false, false, "negated generated pattern"},
		{".env.local", false, true, "secret dotfile"},
		{".idea/workspace.xml", false, true, "editor metadata"},
		{".github/workflows/ci.go", false, false, "useful dot directory"},
		{"src/main.go", false, false, "source"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if got := matcher.Ignored(tc.path, tc.dir); got != tc.want {
				t.Fatalf("Ignored(%q, %t) = %t, want %t", tc.path, tc.dir, got, tc.want)
			}
		})
	}
}

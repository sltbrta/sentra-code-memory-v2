package repoignore_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
)

// FuzzLoadHandlesArbitraryIgnoreFiles is the amplification pass over the glob
// translator. A .gitignore is repository content: whatever a repository
// contains, Load must return a usable matcher rather than panic, because it
// runs at the top of every crawl, watch and ingest path and none of them
// recover.
//
// Seeded with the bracket expressions that produced the original panic.
func FuzzLoadHandlesArbitraryIgnoreFiles(f *testing.F) {
	for _, seed := range []string{
		"", "\n", "#comment\n",
		"*.log\n", "build/\n", "!keep.log\n",
		"[!]\n", "[z-a]\n", "[\n", "[]\n", "[a-]\n", "[[:bogus:]]\n",
		"**/vendor/**\n", "/rooted\n", "a\\[b\n",
		"\\\n", "!\n", "!!\n", "///\n", "..\n", "*/**/*\n",
		"[^a]\n", "[]]\n", "[!]a]\n", "\x00\n", "über/\n",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, contents string) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(contents), 0o644); err != nil {
			t.Skip("unwritable fixture")
		}
		matcher, _ := repoignore.Load(dir)
		if matcher == nil {
			t.Fatal("Load returned a nil matcher")
		}
		// Matching is where a lazily built expression would fail, so exercise it.
		for _, path := range []string{
			"a.go", "dir/b.go", "deep/nested/c.txt", "", ".", "..",
			"[!]", "über.go", "with space.go",
		} {
			matcher.Ignored(path, false)
			matcher.Ignored(path, true)
		}
	})
}

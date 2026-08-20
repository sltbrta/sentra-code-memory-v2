package repoignore_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
)

// Glob translation built a regular expression by splicing the contents of a
// bracket expression straight through, then compiled it with MustCompile. A
// `.gitignore` is repository content, so a line like `[!]` -- valid in a
// gitignore, invalid as a character class -- panicked the indexer. Load runs at
// the top of every crawl, refresh, watch and ingest path, and nothing on those
// paths recovers, so such a repository was un-indexable and took the process
// with it.
//
// git itself treats an unparseable bracket expression as literal text, so that
// is what the matcher does now.

func TestLoadDoesNotPanicOnAMalformedBracketExpression(t *testing.T) {
	patterns := []string{
		"[!]",   // negation marker with nothing to negate
		"[z-a]", // reversed range
		"[",     // unterminated
		"[]",    // empty class
		"[a-]",  // dangling dash
		"[[:bogus:]]",
		"**[",
		"a[b",
		"[^]",
		"[\\]",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".gitignore"),
				[]byte(pattern+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			matcher, _ := repoignore.Load(dir)
			if matcher == nil {
				t.Fatalf("pattern %q produced a nil matcher", pattern)
			}
			// Exercising it is the point: a lazily compiled pattern would still
			// panic here.
			matcher.Ignored("some/path.go", false)
			matcher.Ignored("another", true)
		})
	}
}

// TestMalformedPatternMatchesLiterally pins the chosen fallback: the pattern is
// treated as literal text rather than dropped, which is what git does and
// which keeps a deliberate ignore rule working instead of silently indexing the
// files it names.
func TestMalformedPatternMatchesLiterally(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("[!]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matcher, _ := repoignore.Load(dir)
	if !matcher.Ignored("[!]", false) {
		t.Fatal(`a malformed pattern should still match its own literal text`)
	}
	if matcher.Ignored("unrelated.go", false) {
		t.Fatal("a malformed pattern must not match unrelated paths")
	}
}

// TestWellFormedPatternsStillWork guards against a fallback that swallows
// everything.
func TestWellFormedPatternsStillWork(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"),
		[]byte("*.log\nbuild/\n[abc].txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	matcher, _ := repoignore.Load(dir)
	for _, test := range []struct {
		path string
		dir  bool
		want bool
	}{
		{"debug.log", false, true},
		{"build", true, true},
		{"a.txt", false, true},
		{"d.txt", false, false},
		{"main.go", false, false},
	} {
		if got := matcher.Ignored(test.path, test.dir); got != test.want {
			t.Fatalf("Ignored(%q, dir=%v) = %v, want %v", test.path, test.dir, got, test.want)
		}
	}
}

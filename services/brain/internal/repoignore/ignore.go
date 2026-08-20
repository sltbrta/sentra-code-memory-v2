// Package repoignore provides one conservative, repository-scoped ignore policy
// for local code indexing. It understands the common gitignore/dockerignore
// pattern subset without requiring an external VCS or container runtime.
package repoignore

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type rule struct {
	pattern *regexp.Regexp
	negated bool
	dirOnly bool
}

// Matcher applies built-in generated/secret exclusions followed by patterns
// from the repository's root .gitignore, .dockerignore, and .git/info/exclude.
// Rules are evaluated in source order; later negations can re-include files.
type Matcher struct {
	rules []rule
}

var defaultPatterns = []string{
	".git/", ".sentra/", ".ouroboros/", ".idea/", ".vscode/",
	"node_modules/", "vendor/", "target/", "dist/", "build/", "out/",
	"coverage/", "__pycache__/", ".cache/", ".pytest_cache/", ".mypy_cache/",
	".ruff_cache/", ".next/", ".nuxt/", ".turbo/", ".nx/", ".venv/", "venv/",
	"bazel-*/", ".DS_Store", ".env", ".env.*", "*.pyc", "*.pyo", "*.log",
	"*.map",
}

// Load reads the repository-local ignore files. Missing files are normal; an
// unreadable existing file is returned as an error so callers can report a
// deterministic indexing problem rather than silently broadening the corpus.
func Load(root string) (*Matcher, error) {
	matcher := &Matcher{}
	for _, pattern := range defaultPatterns {
		if err := matcher.add(pattern); err != nil {
			return nil, err
		}
	}
	files := []string{
		filepath.Join(root, ".gitignore"),
		filepath.Join(root, ".dockerignore"),
		filepath.Join(root, ".git", "info", "exclude"),
	}
	for _, path := range files {
		if err := matcher.addFile(path); err != nil {
			return nil, err
		}
	}
	return matcher, nil
}

func (m *Matcher) addFile(path string) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if err := m.add(scanner.Text()); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (m *Matcher) add(raw string) error {
	pattern := strings.TrimSpace(raw)
	if pattern == "" || strings.HasPrefix(pattern, "#") {
		return nil
	}
	negated := false
	if strings.HasPrefix(pattern, "!") {
		negated = true
		pattern = strings.TrimPrefix(pattern, "!")
	}
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	dirOnly := strings.HasSuffix(pattern, "/")
	rooted := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "./")
	pattern = strings.TrimPrefix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return nil
	}
	m.rules = append(m.rules, rule{pattern: compile(pattern, rooted), negated: negated, dirOnly: dirOnly})
	return nil
}

// Ignored reports whether repository-relative path should be excluded. Paths
// use slash separators regardless of host OS. Callers should skip ignored
// directories during traversal so negated descendants remain possible.
func (m *Matcher) Ignored(path string, isDir bool) bool {
	if m == nil {
		return false
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." || path == "" {
		return false
	}
	ignored := false
	for _, rule := range m.rules {
		if rule.dirOnly && !isDir && !strings.Contains(path, "/") {
			continue
		}
		if rule.pattern.MatchString(path) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func compile(pattern string, rooted bool) *regexp.Regexp {
	anchored := rooted || strings.Contains(pattern, "/")
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end < 0 {
				b.WriteString("\\[")
				continue
			}
			end += i + 1
			class := pattern[i+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteByte('[')
			b.WriteString(class)
			b.WriteByte(']')
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("(?:/.*)?$")
	// A .gitignore is repository content, so this expression is built partly
	// from untrusted input: a bracket expression's body is spliced through, and
	// `[!]` or `[z-a]` is a valid gitignore line but an invalid character class.
	// MustCompile turned such a repository into a panic on every crawl, watch
	// and ingest path, none of which recover.
	//
	// git treats an unparseable bracket expression as literal text, so fall
	// back to matching the pattern literally rather than dropping the rule --
	// dropping it would silently start indexing files the author excluded.
	if re, err := regexp.Compile(b.String()); err == nil {
		return re
	}
	var literal strings.Builder
	literal.WriteString("^")
	if !rooted {
		literal.WriteString("(?:.*/)?")
	}
	literal.WriteString(regexp.QuoteMeta(pattern))
	literal.WriteString("(?:/.*)?$")
	if re, err := regexp.Compile(literal.String()); err == nil {
		return re
	}
	// Unreachable in practice: QuoteMeta output always compiles. Returning a
	// never-matching expression keeps the contract (never nil) if it ever is.
	return regexp.MustCompile(`$never^`)
}

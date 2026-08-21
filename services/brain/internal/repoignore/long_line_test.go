package repoignore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One over-long line in a .gitignore stopped indexing the whole repository.
//
// The scanner ran on its 64 KiB default while every other reader in this
// repository raises the bound. A longer line ended the scan, Load returned a
// bare "bufio.Scanner: token too long", and every walk loads the ignore policy
// before it does anything -- so a single checked-in line failed the crawl with
// an error that named neither the file nor the cause.
//
// The same file already carried a P0 for a pattern that panicked the indexer.
// This is the adjacent way a checked-in file stops it.

func writeIgnore(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestALongIgnoreLineDoesNotFailTheWholeLoad(t *testing.T) {
	dir := t.TempDir()
	// Well past the scanner default, well inside the raised bound.
	long := strings.Repeat("a", 100*1024)
	writeIgnore(t, dir, "node_modules/\n"+long+"\nvendor/\n")

	matcher, err := Load(dir)
	if err != nil {
		t.Fatalf("a %d-byte ignore line failed the whole load: %v", len(long), err)
	}
	// The rules on both sides of the long line must still apply: a scan that
	// ended early would silently drop everything after it.
	if !matcher.Ignored("node_modules", true) {
		t.Error("a rule before the long line was lost")
	}
	if !matcher.Ignored("vendor", true) {
		t.Error("a rule after the long line was lost: the scan ended early and " +
			"the crawler will index what the policy excludes")
	}
}

// TestAnUnreadableIgnoreFileStillFailsClosed keeps the direction right past
// the raised bound. An ignore rule that cannot be read cannot be shown not to
// exclude something, and skipping it would widen what the crawler and
// code_read will serve -- the wrong way for a policy whose job is to withhold.
func TestAnUnreadableIgnoreFileStillFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeIgnore(t, dir, "node_modules/\n"+strings.Repeat("a", maxIgnoreLineBytes+1024)+"\n")

	if _, err := Load(dir); err == nil {
		t.Fatal("an ignore file that cannot be read was accepted; the crawler " +
			"would then index what the policy may have excluded")
	} else if !strings.Contains(err.Error(), ".gitignore") {
		t.Errorf("the failure does not name the file: %v", err)
	}
}

// TestOrdinaryIgnoreFilesAreUnaffected pins that the raised bound changed
// nothing for real files.
func TestOrdinaryIgnoreFilesAreUnaffected(t *testing.T) {
	dir := t.TempDir()
	writeIgnore(t, dir, "# comment\nnode_modules/\n*.log\n!keep.log\nbuild/\n")

	matcher, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"node_modules": true,
		"build":        true,
	} {
		if matcher.Ignored(name, true) != want {
			t.Errorf("%s: ignored=%v want %v", name, matcher.Ignored(name, true), want)
		}
	}
	if !matcher.Ignored("debug.log", false) {
		t.Error("*.log was not applied")
	}
	if matcher.Ignored("keep.log", false) {
		t.Error("the negation was not applied")
	}
}

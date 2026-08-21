package durablefile_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/durablefile"
)

// Two defects a fresh-eyes review found by reading, neither covered.
//
//  1. The temp-file cleanup ran only `if retErr != nil`, and retErr is nil
//     while a panic is unwinding. emit is caller-supplied -- a JSON encoder, a
//     gob stream -- so a panic inside it left the descriptor open and the temp
//     file on disk, once per call, in processes that run for weeks.
//  2. Renaming onto the target replaced a symlink with a regular file, which
//     dissolves a deliberate layout on the first write. Every caller here was
//     migrated from os.WriteFile, which writes through the link.

// tempFiles returns the leftover temp files durablefile creates, which are
// dot-prefixed and carry a ".tmp-" infix.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && strings.Contains(entry.Name(), ".tmp-") {
			out = append(out, entry.Name())
		}
	}
	return out
}

// openDescriptors counts this process's open file descriptors. /dev/fd is
// present on both darwin and linux; an absent one skips the count rather than
// failing the test.
func openDescriptors(t *testing.T) (int, bool) {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0, false
	}
	return len(entries), true
}

func TestAPanickingEmitLeavesNoTempFileOrDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.jsonl")
	if err := durablefile.Write(path, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	before, canCount := openDescriptors(t)

	const rounds = 64
	for i := 0; i < rounds; i++ {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("emit did not panic; this guard is checking nothing")
				}
			}()
			_ = durablefile.WriteFunc(path, 0o600, func(w io.Writer) error {
				_, _ = io.WriteString(w, "partial")
				panic(fmt.Sprintf("encoder defect on round %d", i))
			})
		}()
	}

	if leftovers := tempFiles(t, dir); len(leftovers) != 0 {
		t.Fatalf("%d temp files left behind by %d panicking writes: %v",
			len(leftovers), rounds, leftovers)
	}
	if canCount {
		after, _ := openDescriptors(t)
		// A little slack for unrelated runtime activity; the defect leaks one
		// descriptor per call, so 64 rounds is unmistakable.
		if after-before > 8 {
			t.Fatalf("open descriptors went from %d to %d across %d panicking "+
				"writes: the temp file's descriptor is never closed",
				before, after, rounds)
		}
	}

	// The original file must be untouched: a panic is a failed write.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original\n" {
		t.Fatalf("a panicking write changed the live file: %q", body)
	}
}

func TestWriteFollowsASymlinkInsteadOfReplacingIt(t *testing.T) {
	targetDir := t.TempDir()
	linkDir := t.TempDir()
	target := filepath.Join(targetDir, "real-corpus.jsonl")
	link := filepath.Join(linkDir, "corpus.jsonl")

	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := durablefile.Write(link, []byte("replacement\n"), 0o600); err != nil {
		t.Fatalf("Write through a symlink: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file: a deliberate " +
			"layout is dissolved by the first write")
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "replacement\n" {
		t.Fatalf("the link's target was not updated: %q", body)
	}
	// The temp file must have been created beside the target, not beside the
	// link: renaming across filesystems fails.
	if leftovers := tempFiles(t, linkDir); len(leftovers) != 0 {
		t.Fatalf("temp files left beside the link: %v", leftovers)
	}
}

// TestWriteReplacesADanglingSymlink covers the case where there is nothing to
// write through to.
func TestWriteReplacesADanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "corpus.jsonl")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Fatal(err)
	}
	if err := durablefile.Write(link, []byte("body\n"), 0o600); err != nil {
		t.Fatalf("Write onto a dangling symlink: %v", err)
	}
	body, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "body\n" {
		t.Fatalf("got %q", body)
	}
}

// TestWriteFuncStillCleansUpOnAnError keeps the ordinary failure path covered
// alongside the panic one.
func TestWriteFuncStillCleansUpOnAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.jsonl")
	if err := durablefile.Write(path, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := durablefile.WriteFunc(path, 0o600, func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		return fmt.Errorf("encoder failed")
	})
	if err == nil {
		t.Fatal("a failing emit reported success")
	}
	if leftovers := tempFiles(t, dir); len(leftovers) != 0 {
		t.Fatalf("temp file left behind: %v", leftovers)
	}
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "original\n" {
		t.Fatalf("a failed write changed the live file: %q", body)
	}
}

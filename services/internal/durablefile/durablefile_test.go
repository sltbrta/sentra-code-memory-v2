package durablefile_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/durablefile"
)

func TestWriteReplacesContentAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	if err := durablefile.Write(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := durablefile.Write(path, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "second\n" {
		t.Fatalf("content = %q, want %q", body, "second\n")
	}
}

// TestWriteLeavesTheOriginalIntactWhenEmitFails is the property the truncating
// writers lacked. os.Create destroys the old contents before the new ones
// exist, so an error midway through left a corpus that was neither.
func TestWriteLeavesTheOriginalIntactWhenEmitFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	original := "the only copy of this data\n"
	if err := durablefile.Write(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("disk full")
	err := durablefile.WriteFunc(path, 0o600, func(w io.Writer) error {
		if _, writeErr := w.Write([]byte("partial")); writeErr != nil {
			return writeErr
		}
		return wantErr
	})
	if err == nil {
		t.Fatal("a failing emit must return an error, not report success")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error should wrap the cause, got %v", err)
	}

	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != original {
		t.Fatalf("original destroyed by a failed write: %q", body)
	}
}

// TestWriteLeavesNoTemporaryFileBehind: a stale temp file in a cache directory
// is picked up by the crawler as an ordinary file.
func TestWriteLeavesNoTemporaryFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.jsonl")

	_ = durablefile.WriteFunc(path, 0o600, func(w io.Writer) error {
		return errors.New("nope")
	})
	if err := durablefile.Write(path, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly the target file, got %d entries", len(entries))
	}
}

func TestWriteAppliesTheRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	if err := durablefile.Write(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600: corpus files were world-readable at 0644", got)
	}
}

func TestWriteCreatesMissingParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "f.json")
	if err := durablefile.Write(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// TestWriteFailsWhenTheDirectoryIsUnwritable proves the error is reported
// rather than swallowed: this is the shape of a full or read-only disk.
func TestWriteFailsWhenTheDirectoryIsUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	readonly := filepath.Join(dir, "ro")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0o700) })

	if err := durablefile.Write(filepath.Join(readonly, "f.json"), []byte("{}"), 0o600); err == nil {
		t.Fatal("writing into a read-only directory must fail")
	}
}

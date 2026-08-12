package codecrawl

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeFixture returns a root with two indexable files.
func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc HardAlpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\nfunc HardBeta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestConcurrentSaveAndLoadNeverPartial(t *testing.T) {
	t.Parallel()
	root := writeFixture(t)
	gobPath := filepath.Join(t.TempDir(), "code-index.gob")
	idx, _, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	ensureHashes(idx, root)

	// Prime one complete state so readers always have a gob to load.
	if err := idx.Save(gobPath, root); err != nil {
		t.Fatal(err)
	}

	const writers = 4
	const readers = 4
	const rounds = 25
	var wg sync.WaitGroup
	errs := make(chan error, (writers+readers)*rounds)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := idx.Save(gobPath, root); err != nil {
					errs <- err
				}
			}
		}()
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				// Every successful load must be a complete decodable index —
				// never a torn/partial gob from a concurrent writer.
				got, meta, err := Load(gobPath)
				if err != nil {
					errs <- err
					continue
				}
				if meta.Root != root || got.FileCount() != 2 {
					errs <- errors.New("loaded mismatched root/index pair")
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent save/load: %v", err)
	}
}

func TestConcurrentOpenOrRefresh(t *testing.T) {
	t.Parallel()
	root := writeFixture(t)
	gobPath := filepath.Join(t.TempDir(), "code-index.gob")
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
			if err != nil {
				errs <- err
				return
			}
			if hits := idx.Search("HardAlpha", 3); len(hits) == 0 {
				errs <- errors.New("missing HardAlpha after concurrent open")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent open/refresh: %v", err)
	}
}

func TestStaleTmpDoesNotBlockRecovery(t *testing.T) {
	t.Parallel()
	root := writeFixture(t)
	gobPath := filepath.Join(t.TempDir(), "code-index.gob")
	idx, _, wrote, meta, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil || !wrote {
		t.Fatalf("initial index: wrote=%v err=%v", wrote, err)
	}

	// Crash simulation: an interrupted writer leaves a partial .tmp behind.
	// The live gob must still load the old complete state.
	if err := os.WriteFile(gobPath+".tmp", []byte("partial-write-garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, gotMeta, err := Load(gobPath)
	if err != nil {
		t.Fatalf("stale tmp must not affect the live gob: %v", err)
	}
	if gotMeta.Root != meta.Root || got.FileCount() != idx.FileCount() {
		t.Fatal("live gob changed under an interrupted write")
	}

	// The next Save discards the stale tmp and publishes complete-new state.
	if err := idx.Save(gobPath, root); err != nil {
		t.Fatalf("save after interrupted write: %v", err)
	}
	if _, _, err := Load(gobPath); err != nil {
		t.Fatalf("load after recovery save: %v", err)
	}
}

func TestCorruptGobFailsLoadAndRecoversByReindex(t *testing.T) {
	t.Parallel()
	root := writeFixture(t)
	gobPath := filepath.Join(t.TempDir(), "code-index.gob")
	if _, _, wrote, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil || !wrote {
		t.Fatalf("initial index: wrote=%v err=%v", wrote, err)
	}

	// Crash/tamper simulation: the gob itself is unrecognizable. Load must
	// fail closed rather than serve a degraded index.
	if err := os.WriteFile(gobPath, []byte("not-a-gob-at-all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(gobPath); err == nil {
		t.Fatal("corrupt gob must fail Load")
	}

	// Refresh recovers by reindexing and publishing complete-new state.
	idx, _, wrote, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatalf("refresh after corrupt gob: %v", err)
	}
	if !wrote {
		t.Fatal("expected rewrite after corrupt gob")
	}
	if hits := idx.Search("HardBeta", 3); len(hits) == 0 {
		t.Fatal("recovered index lost HardBeta")
	}
}

func TestValidateRootBinding(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	meta := DurableMeta{Root: root}
	if err := ValidateRoot(meta, root); err != nil {
		t.Fatalf("same root must validate: %v", err)
	}
	if err := ValidateRoot(meta, ""); err != nil {
		t.Fatalf("empty caller root means no expectation: %v", err)
	}
	if err := ValidateRoot(meta, filepath.Join(root, "sub")); !errors.Is(err, ErrRootMismatch) {
		t.Fatalf("different root must be ErrRootMismatch, got %v", err)
	}
	if err := ValidateRoot(DurableMeta{}, root); !errors.Is(err, ErrRootMismatch) {
		t.Fatalf("unbound index must fail closed, got %v", err)
	}
	// Symlink-spelled roots (macOS /var → /private/var) compare equal.
	real, err := filepath.EvalSymlinks(root)
	if err == nil && real != root {
		if err := ValidateRoot(DurableMeta{Root: real}, root); err != nil {
			t.Fatalf("symlink-spelled root must validate: %v", err)
		}
	}
}

func TestOpenOrRefreshReindexesOnRootMismatch(t *testing.T) {
	t.Parallel()
	rootA := writeFixture(t)
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootB, "b.go"), []byte("package b\nfunc HardGamma() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gobPath := filepath.Join(t.TempDir(), "code-index.gob")
	if _, _, _, _, err := OpenOrRefresh(rootA, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}
	// The same gob path opened for a different root must not serve rootA's
	// index; it reindexes and rebinds to rootB.
	idx, _, wrote, meta, err := OpenOrRefresh(rootB, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("root mismatch must reindex and rewrite")
	}
	if hits := idx.Search("HardAlpha", 3); len(hits) != 0 {
		t.Fatal("mismatched root served rootA's index")
	}
	if hits := idx.Search("HardGamma", 3); len(hits) == 0 {
		t.Fatal("reindex did not index rootB")
	}
	if err := ValidateRoot(meta, rootB); err != nil {
		t.Fatalf("rebound meta must validate for rootB: %v", err)
	}
}

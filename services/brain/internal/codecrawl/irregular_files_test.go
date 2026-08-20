//go:build unix

package codecrawl_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
)

// Only SourceFiles checked Mode().IsRegular(). CrawlDir, CrawlDeltaFrom,
// IngestPaths and ensureHashes did not, and os.Stat on a FIFO succeeds while
// os.ReadFile on one blocks until a writer appears. A single `mkfifo pipe.go`
// anywhere in a workspace therefore wedged a crawl worker forever; because
// nothing here takes a context, the hang was unkillable and wg.Wait() never
// returned.
//
// These tests carry their own deadline: a regression does not fail them, it
// hangs them, and an unbounded hang in CI is worse than a failure.

func withDeadline(t *testing.T, name string, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: an irregular file blocked a worker", name, d)
	}
}

func fifoWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.go"),
		[]byte("package a\n\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Named with a source extension so the extension filter admits it.
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.go"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	return root
}

func TestCrawlDirSkipsAFifoInsteadOfBlocking(t *testing.T) {
	root := fifoWorkspace(t)
	var idx *codecrawl.Index
	withDeadline(t, "CrawlDir", 15*time.Second, func() {
		var err error
		idx, _, err = codecrawl.CrawlDir(root, 2)
		if err != nil {
			t.Errorf("CrawlDir: %v", err)
		}
	})
	if idx == nil {
		t.Fatal("no index")
	}
	if !idx.HasFile("real.go") {
		t.Fatal("the regular file was not indexed")
	}
	if idx.HasFile("pipe.go") {
		t.Fatal("a FIFO was indexed as source")
	}
}

func TestOpenOrRefreshSkipsAFifoInsteadOfBlocking(t *testing.T) {
	root := fifoWorkspace(t)
	cache := filepath.Join(t.TempDir(), "code-index.gob")
	withDeadline(t, "OpenOrRefresh", 15*time.Second, func() {
		if _, _, _, _, err := codecrawl.OpenOrRefresh(root, cache, 2, false); err != nil {
			t.Errorf("OpenOrRefresh: %v", err)
		}
	})
	// A second pass exercises the delta path, which has its own walk.
	withDeadline(t, "OpenOrRefresh (warm)", 15*time.Second, func() {
		if _, _, _, _, err := codecrawl.OpenOrRefresh(root, cache, 2, false); err != nil {
			t.Errorf("OpenOrRefresh warm: %v", err)
		}
	})
}

func TestIngestPathsSkipsAFifoInsteadOfBlocking(t *testing.T) {
	root := fifoWorkspace(t)
	cache := filepath.Join(t.TempDir(), "code-index.gob")
	var idx *codecrawl.Index
	// This setup crawl was outside withDeadline, so a regression hung the
	// binary to the package timeout instead of failing -- and took the sibling
	// test with it, which never ran at all. Every call that touches the FIFO
	// carries the deadline.
	withDeadline(t, "CrawlDir (setup)", 15*time.Second, func() {
		var err error
		idx, _, err = codecrawl.CrawlDir(root, 2)
		if err != nil {
			t.Errorf("CrawlDir: %v", err)
		}
	})
	if idx == nil {
		t.Fatal("setup crawl produced no index")
	}
	if err := idx.Save(cache, root); err != nil {
		t.Fatal(err)
	}
	withDeadline(t, "IngestPaths", 15*time.Second, func() {
		if _, err := idx.IngestPaths(root, []string{"pipe.go", "real.go"}); err != nil {
			t.Errorf("IngestPaths: %v", err)
		}
	})
}

// TestCrawlDirSkipsADeviceSymlink covers the other shape of the same defect: a
// symlink named like source but pointing at a character device reads forever
// (or grows until the process is OOM-killed).
func TestCrawlDirSkipsADeviceSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// /dev/null rather than /dev/zero: both are character devices and exercise
	// the same check, but a regression on /dev/zero grows the buffer until the
	// runner OOMs -- measured at 9 GB RSS -- which loses the failure message.
	if err := os.Symlink(os.DevNull, filepath.Join(root, "zero.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	var idx *codecrawl.Index
	withDeadline(t, "CrawlDir", 15*time.Second, func() {
		var err error
		idx, _, err = codecrawl.CrawlDir(root, 2)
		if err != nil {
			t.Errorf("CrawlDir: %v", err)
		}
	})
	if idx != nil && idx.HasFile("zero.go") {
		t.Fatal("a character device was indexed as source")
	}
}

// TestIngestPathsAndCrawlAgreeOnSymlinks closes a fresh-eyes finding: the
// symlink exclusion only worked where filepath.Walk lstats. IngestPaths and
// ensureHashes call os.Stat, which resolves the link first, so ingesting a
// symlinked source file added it to the index and the next full crawl silently
// removed it -- answers for that file appeared and disappeared depending on
// which verb ran last.
func TestIngestPathsAndCrawlAgreeOnSymlinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.go"),
		[]byte("package outside\n\nfunc Secret() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.go"), filepath.Join(root, "linked.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	idx, _, err := codecrawl.CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	crawled := idx.HasFile("linked.go")

	if _, err := idx.IngestPaths(root, []string{"linked.go"}); err != nil {
		t.Fatalf("IngestPaths: %v", err)
	}
	ingested := idx.HasFile("linked.go")

	if crawled != ingested {
		t.Fatalf("CrawlDir indexed the symlink=%v but IngestPaths indexed it=%v: "+
			"the two paths disagree, so the file appears and disappears by verb", crawled, ingested)
	}
	if ingested {
		t.Fatal("a symlink resolving outside the root was indexed")
	}
}

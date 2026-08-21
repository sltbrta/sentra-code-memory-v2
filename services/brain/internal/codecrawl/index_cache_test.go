package codecrawl

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Every verb call gob-decoded the whole index. Measured on this repository
// (1,067 indexed files): 66ms per decode against 136ms for a served
// code_search, so about half the cost of answering a query was re-reading an
// index the process already had.
//
// A cache that serves a stale index is worse than the decode it replaces, so
// these cover invalidation as carefully as the hit.

func cacheRepo(t *testing.T) (root, gobPath string) {
	t.Helper()
	root = t.TempDir()
	write(t, root, "main.go", "package main\n\nfunc main() {}\n")
	write(t, root, "lib/util.go", "package lib\n\nfunc Helper() int { return 1 }\n")
	return root, filepath.Join(t.TempDir(), "index.gob")
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWarmOpensReuseOneDecodedIndex(t *testing.T) {
	root, gobPath := cacheRepo(t)
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}
	first, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	second, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("two warm opens decoded the index twice: the decode is roughly " +
			"half the cost of answering a query and it is being paid per call")
	}
}

// TestAnExternalRewriteIsPickedUp is the property that makes the cache safe.
// Another process, a forced reindex or a watch refresh replaces the gob, and
// the next read must see it.
func TestAnExternalRewriteIsPickedUp(t *testing.T) {
	root, gobPath := cacheRepo(t)
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}
	before, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if !before.HasFile("lib/util.go") {
		t.Fatal("fixture is wrong")
	}

	// Rebuild the index from a tree with an extra file, writing it as another
	// process would. The mtime must differ from the cached identity.
	write(t, root, "lib/extra.go", "package lib\n\nfunc Extra() {}\n")
	time.Sleep(10 * time.Millisecond)
	rebuilt, _, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	ensureHashes(rebuilt, root)
	if err := rebuilt.Save(gobPath, root); err != nil {
		t.Fatal(err)
	}

	after, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if !after.HasFile("lib/extra.go") {
		t.Fatal("a rewritten index was served from the cache: the process is " +
			"answering from an index that no longer exists on disk")
	}
}

// TestSaveDropsTheCachedIndex covers the in-process writer. The identity check
// would catch it on the next read, but only after a window in which this
// process serves what it has just replaced.
func TestSaveDropsTheCachedIndex(t *testing.T) {
	root, gobPath := cacheRepo(t)
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}

	indexCache.mu.RLock()
	_, cached := indexCache.entries[gobPath]
	indexCache.mu.RUnlock()
	if !cached {
		t.Fatal("nothing was cached, so this guard checks nothing")
	}

	rebuilt, _, err := CrawlDir(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	ensureHashes(rebuilt, root)
	if err := rebuilt.Save(gobPath, root); err != nil {
		t.Fatal(err)
	}

	indexCache.mu.RLock()
	_, stillCached := indexCache.entries[gobPath]
	indexCache.mu.RUnlock()
	if stillCached {
		t.Fatal("Save left the previous index in the cache")
	}
}

// TestConcurrentReadersShareTheIndexWithoutRacing is what the cache makes
// newly possible: one Index serving many goroutines. Graph()'s lazy
// assignment was an unsynchronised write on a read path, safe only while
// every caller decoded a private copy.
func TestConcurrentReadersShareTheIndexWithoutRacing(t *testing.T) {
	root, gobPath := cacheRepo(t)
	if _, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				idx, _, _, _, err := OpenOrRefresh(root, gobPath, 2, false)
				if err != nil {
					t.Error(err)
					return
				}
				_ = idx.SearchOpts("Helper", 5, true)
				_ = idx.HasGraph()
				_ = idx.Files()
				_ = idx.RepoMap("Helper", RepoMapOptions{})
			}
		}()
	}
	wg.Wait()
}

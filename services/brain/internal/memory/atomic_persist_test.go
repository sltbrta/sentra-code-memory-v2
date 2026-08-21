package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// D-004. The whole memory cortex -- every claim, temporal relation, episode,
// PageIndex tree, PageRank vector and agent-memory tier -- is one file, and
// persistLocked rewrote it with os.WriteFile. That truncates the live file in
// place, so from the moment the write opens until it completes there is no
// complete copy of the cortex anywhere: a crash, a SIGKILL or a full disk in
// that window leaves a memory.json that fails to parse at Open, and every
// mutator takes this path, thousands of times per gardener wave.
//
// The fix routes it through durablefile (temp file, fsync, rename), and
// nothing checked it -- reverting it left the suite green.
//
// The invariant is not "the write succeeds", which the old code also managed
// almost always. It is that the file on disk is never observed incomplete. A
// concurrent reader is what distinguishes them.

// largeCortex builds a cortex big enough that a single in-place rewrite is not
// instantaneous, so a reader can observe the window if one exists.
func largeCortex(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	texts := make(map[string]string, 512)
	body := strings.Repeat("cortex payload ", 512)
	for i := 0; i < 512; i++ {
		texts[string(rune('a'+i%26))+"-doc-"+strings.Repeat("n", i%17)] = body
	}
	if err := store.SetDocTexts(texts); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCortexIsNeverObservedPartiallyWritten(t *testing.T) {
	store := largeCortex(t)
	path := filepath.Join(store.Dir(), dataFile)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	full := info.Size()
	if full < 1<<20 {
		t.Fatalf("fixture cortex is only %d bytes; too small for this to mean anything", full)
	}

	// The content does not change between rounds, so every complete cortex on
	// disk is exactly `full` bytes. Any other size is a file caught mid-write.
	// Sampling with Stat rather than a full read is what makes this reliable:
	// reading a megabyte takes long enough that the window is usually missed,
	// and a guard that only sometimes goes red is not a guard.
	var stop atomic.Bool
	var wg sync.WaitGroup
	var samples, torn atomic.Int64
	var shortest atomic.Int64
	shortest.Store(full)

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				info, err := os.Stat(path)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					t.Error(err)
					return
				}
				samples.Add(1)
				if size := info.Size(); size != full {
					torn.Add(1)
					for {
						prev := shortest.Load()
						if size >= prev || shortest.CompareAndSwap(prev, size) {
							break
						}
					}
				}
			}
		}()
	}

	for i := 0; i < 40; i++ {
		if err := store.persist(); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}
	stop.Store(true)
	wg.Wait()

	if samples.Load() == 0 {
		t.Fatal("the readers never ran, so this guard checked nothing")
	}
	if n := torn.Load(); n > 0 {
		t.Fatalf("%d of %d samples caught the cortex mid-write (shortest %d of %d "+
			"bytes): the live file is truncated in place, so a crash in that "+
			"window loses every claim, episode and PageRank vector at once",
			n, samples.Load(), shortest.Load(), full)
	}

	// Corroboration: what is on disk at the end is still a whole cortex.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("the cortex no longer parses: %v", err)
	}
}

// TestAFailedCortexWriteLeavesThePreviousOneIntact is the crash case made
// deterministic: when the replacement cannot be written at all, the previous
// cortex must still be there and still parse.
func TestAFailedCortexWriteLeavesThePreviousOneIntact(t *testing.T) {
	store := largeCortex(t)
	dir := store.Dir()
	path := filepath.Join(dir, dataFile)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := store.persist(); err == nil {
		t.Fatal("a write into a read-only directory reported success")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the previous cortex is gone: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the previous cortex was truncated from %d to %d bytes by a "+
			"write that failed", len(before), len(after))
	}
	var probe map[string]any
	if err := json.Unmarshal(after, &probe); err != nil {
		t.Fatalf("the cortex left on disk no longer parses: %v", err)
	}
}

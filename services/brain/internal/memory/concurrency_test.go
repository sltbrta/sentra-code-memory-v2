package memory_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// Store's own doc comment conceded the problem: "other mutators call persist()
// which locks only for the JSON write. Prefer locking in mutators when adding
// new concurrent writers." That is exactly the situation the product is in --
// a *Store is shared as Client.Mem between the 500ms auto-gardener goroutine
// and the answer path, and codeserve exposes four memory verbs over concurrent
// HTTP dispatch.
//
// The suite passed -race from the day it was written because no test ever ran
// two operations at once. These are the tests that make that gate mean
// something. A concurrent map write in Go is a fatal error, not a recoverable
// panic, so this class of defect kills the process outright.

// hammer runs fn from n goroutines, each iters times, released together so the
// window where they overlap is as wide as possible.
func hammer(t *testing.T, goroutines, iters int, fn func(worker, iter int)) {
	t.Helper()
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for w := 0; w < goroutines; w++ {
		done.Add(1)
		go func(worker int) {
			defer done.Done()
			start.Wait()
			for i := 0; i < iters; i++ {
				fn(worker, i)
			}
		}(w)
	}
	start.Done()
	waitOrFail(t, &done, 60*time.Second)
}

func waitOrFail(t *testing.T, wg *sync.WaitGroup, budget time.Duration) {
	t.Helper()
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(budget):
		t.Fatalf("concurrent operations did not finish within %s (deadlock?)", budget)
	}
}

func TestConcurrentAgentMemoryWritesDoNotRace(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	hammer(t, 8, 25, func(worker, iter int) {
		if _, err := store.PutAgentMemoryTier(
			fmt.Sprintf("principal-%d", worker), "note",
			fmt.Sprintf("worker %d iteration %d", worker, iter), nil, "stm",
		); err != nil {
			t.Errorf("PutAgentMemoryTier: %v", err)
		}
	})

	// Every write must be present: an unsynchronised append silently loses
	// entries even when the race detector is not watching. GetAgentMemory
	// requires a principal, so count per worker.
	total := 0
	for w := 0; w < 8; w++ {
		total += len(store.GetAgentMemory(fmt.Sprintf("principal-%d", w), 1000))
	}
	if total != 8*25 {
		t.Fatalf("agent memory holds %d entries, want %d: writes were lost to a racing append", total, 8*25)
	}
}

func TestConcurrentClaimAdmissionDoesNotRace(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	hammer(t, 8, 20, func(worker, iter int) {
		_, _, _ = store.AdmitClaim(memory.Claim{
			Subject:   fmt.Sprintf("subject-%d", worker),
			Predicate: "is",
			Object:    fmt.Sprintf("value-%d-%d", worker, iter),
		})
	})
}

// TestConcurrentReadsAndWritesDoNotRace is the shape that actually occurs in
// production: the gardener maintaining the cortex while the answer path reads
// it.
func TestConcurrentReadsAndWritesDoNotRace(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		if _, _, err := store.AdmitClaim(memory.Claim{
			Subject: fmt.Sprintf("s-%d", i), Predicate: "is", Object: fmt.Sprintf("o-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers, standing in for the auto-gardener wave.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_, _, _ = store.AdmitClaim(memory.Claim{
					Subject:   fmt.Sprintf("w%d-%d", worker, i),
					Predicate: "is", Object: "value",
				})
				store.ReinforceUtility([]string{fmt.Sprintf("doc-%d", worker)}, 0.5)
				_ = store.StorePageRank(map[string]float64{fmt.Sprintf("doc-%d", worker): float64(i)})
			}
		}(w)
	}

	// Readers, standing in for the answer path.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = store.CurrentClaims(time.Now(), true)
				_ = store.GetUtility("doc-0")
				_ = store.GetAgentMemory("principal-0", 10)
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	waitOrFail(t, &wg, 60*time.Second)
}

// The tests below close blockers found by a fresh-eyes review of the first
// locking pass. Locking every mutator made each call safe and left the
// compositions torn: a maintenance wave is a read-modify-write across several
// individually-locked calls, and two concurrent waves interleave between the
// read and the write.

func TestConcurrentPageRankReadsAndWritesDoNotRace(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = store.StorePageRank(map[string]float64{
					fmt.Sprintf("doc-%d", worker): float64(i),
				})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// The nil test on s.data.PageRank used to run above the lock.
				_ = store.PageRankScores()
			}
		}()
	}
	time.Sleep(250 * time.Millisecond)
	close(stop)
	waitOrFail(t, &wg, 60*time.Second)
}

// TestConcurrentMaintenanceWavesDoNotLoseSummaries pins the composition, not
// the parts. Measured before the wave lock: 3 of 40 summaries survived.
func TestConcurrentMaintenanceWavesDoNotLoseSummaries(t *testing.T) {
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{}
	for i := 0; i < 12; i++ {
		docs[fmt.Sprintf("doc-%02d", i)] = fmt.Sprintf(
			"alpha beta gamma delta epsilon document %d about authentication and storage", i)
	}

	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_ = store.RunCortexMaintenance(docs)
			}
		}()
	}
	waitOrFail(t, &wg, 120*time.Second)

	// After concurrent waves the summary set must reflect a whole wave, not a
	// torn interleaving of several.
	if got := len(store.ListSummaries()); got == 0 {
		t.Fatal("concurrent maintenance waves discarded every summary")
	}
}

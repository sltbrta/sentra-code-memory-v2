package dense

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestHNSWUpsertSearchRoundTrip(t *testing.T) {
	h := NewHNSW(4, 8, 16)
	if err := h.Upsert("a", []float32{1, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := h.Upsert("b", []float32{0.9, 0.1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := h.Upsert("c", []float32{0, 1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	hits := h.Search([]float32{1, 0, 0, 0}, 2)
	if len(hits) < 1 || hits[0].DocumentID != "a" {
		t.Fatalf("hits=%+v", hits)
	}
	path := filepath.Join(t.TempDir(), "x.hnsw")
	if err := h.Save(path); err != nil {
		t.Fatal(err)
	}
	h2, err := LoadHNSW(path)
	if err != nil {
		t.Fatal(err)
	}
	hits2 := h2.Search([]float32{1, 0, 0, 0}, 2)
	if len(hits2) < 1 || hits2[0].DocumentID != "a" {
		t.Fatalf("reload hits=%+v", hits2)
	}
}

func TestHNSWIdentityPersistenceAndDeterministicTies(t *testing.T) {
	identity := IndexIdentity{Scope: "brain-a", Model: "embed:test-v1", Dimensions: 3}
	h := NewScopedHNSW(identity, 8, 16)
	for _, id := range []string{"z", "a"} {
		if err := h.Upsert(id, []float32{1, 0, 0}); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		hits, diag, err := h.SearchScoped([]float32{1, 0, 0}, 2, identity)
		if err != nil {
			t.Fatal(err)
		}
		if diag.Route != "exact_small" || len(hits) != 2 || hits[0].DocumentID != "a" || hits[1].DocumentID != "z" {
			t.Fatalf("attempt %d hits=%+v diag=%+v", attempt, hits, diag)
		}
	}
	path := filepath.Join(t.TempDir(), "identity.ann")
	if err := h.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadHNSW(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Identity(); got != identity {
		t.Fatalf("identity=%+v want %+v", got, identity)
	}
	for name, wrong := range map[string]IndexIdentity{
		"scope":      {Scope: "brain-b", Model: identity.Model, Dimensions: identity.Dimensions},
		"model":      {Scope: identity.Scope, Model: "embed:test-v2", Dimensions: identity.Dimensions},
		"dimensions": {Scope: identity.Scope, Model: identity.Model, Dimensions: 4},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := loaded.SearchScoped([]float32{1, 0, 0}, 1, wrong)
			var identityErr *IdentityError
			if !errors.As(err, &identityErr) || identityErr.Field != name {
				t.Fatalf("error=%v, want IdentityError field %s", err, name)
			}
		})
	}
}

func TestHNSWSearchScopedNilReceiverReportsMissing(t *testing.T) {
	var h *HNSW
	hits, diag, err := h.SearchScoped([]float32{1, 0}, 1, IndexIdentity{
		Scope: "brain-a", Model: "embed:v1", Dimensions: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || diag.Route != "ann" || diag.IndexState != "missing" {
		t.Fatalf("nil receiver hits=%+v diagnostics=%+v", hits, diag)
	}
}

func TestHNSWSaveAtomicVisibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.ann")
	makeIndex := func(id string, axis int) *HNSW {
		h := NewScopedHNSW(IndexIdentity{Scope: "atomic", Model: "test:v1", Dimensions: 2}, 8, 16)
		vec := []float32{0, 0}
		vec[axis] = 1
		if err := h.Upsert(id, vec); err != nil {
			t.Fatal(err)
		}
		return h
	}
	a, b := makeIndex("a", 0), makeIndex("b", 1)
	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			loaded, err := LoadHNSW(path)
			if err != nil {
				errCh <- err
				return
			}
			if loaded.Len() != 1 {
				errCh <- fmt.Errorf("observed partial index length %d", loaded.Len())
				return
			}
		}
	}()
	for i := 0; i < 20; i++ {
		index := a
		if i%2 == 0 {
			index = b
		}
		if err := index.Save(path); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

// Fixed-corpus gate compares ANN top-k to the exact top-k truth set. It emits
// the review metrics at multiple sizes: recall@k, p50/p95, allocations,
// estimated index memory, and durable file bytes.
func TestHNSWFixedCorpusExactVsANNMetrics(t *testing.T) {
	const dim, queries, topK = 32, 24, 10
	for _, vectors := range []int{256, 1024, 2048} {
		t.Run(fmt.Sprintf("vectors_%d", vectors), func(t *testing.T) {
			identity := IndexIdentity{Scope: fmt.Sprintf("fixed-%d", vectors), Model: "synthetic:v1", Dimensions: dim}
			h := NewScopedHNSW(identity, 16, 96)
			rng := rand.New(rand.NewSource(305 + int64(vectors)))
			corpus := make([][]float32, vectors)
			for i := range corpus {
				corpus[i] = make([]float32, dim)
				for j := range corpus[i] {
					corpus[i][j] = float32(rng.NormFloat64())
				}
				if err := h.Upsert(fmt.Sprintf("doc-%06d", i), corpus[i]); err != nil {
					t.Fatal(err)
				}
			}
			var latencies []time.Duration
			var overlap int
			for i := 0; i < queries; i++ {
				q := append([]float32(nil), corpus[(i*61)%vectors]...)
				for j := range q {
					q[j] += float32(rng.NormFloat64() * 0.01)
				}
				exact, _, err := h.SearchScopedMode(q, topK, identity, SearchModeExact)
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				ann, diag, err := h.SearchScopedMode(q, topK, identity, SearchModeANN)
				latencies = append(latencies, time.Since(start))
				if err != nil {
					t.Fatal(err)
				}
				if diag.Route != "ann_override" || diag.DistanceCalculations > diag.CandidateLimit {
					t.Fatalf("unbounded ANN diagnostics: %+v", diag)
				}
				overlap += topKOverlap(exact, ann)
			}
			recall := float64(overlap) / float64(queries*topK)
			if recall < 0.75 {
				t.Fatalf("true recall@%d=%.3f, want >=0.75", topK, recall)
			}
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			p50, p95 := percentileDuration(latencies, 0.50), percentileDuration(latencies, 0.95)
			if p95 > 500*time.Millisecond {
				t.Fatalf("ANN p95=%s, want <=500ms", p95)
			}
			allocs := testing.AllocsPerRun(20, func() {
				_, _, _ = h.SearchScopedMode(corpus[305%vectors], topK, identity, SearchModeANN)
			})
			if allocs > 1200 {
				t.Fatalf("allocations/query=%.1f, want <=1200", allocs)
			}
			path := filepath.Join(t.TempDir(), "fixed.ann")
			if err := h.Save(path); err != nil {
				t.Fatal(err)
			}
			stat, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if h.EstimatedMemoryBytes() <= 0 || h.EstimatedMemoryBytes() > int64(vectors*1024) ||
				stat.Size() <= 0 || stat.Size() > int64(vectors*1024) || p50 <= 0 || p95 < p50 {
				t.Fatalf("invalid metrics p50=%s p95=%s memory=%d disk=%d", p50, p95, h.EstimatedMemoryBytes(), stat.Size())
			}
			t.Logf("vectors=%d recall@%d=%.3f p50=%s p95=%s alloc/query=%.1f memory_bytes=%d disk_bytes=%d",
				vectors, topK, recall, p50, p95, allocs, h.EstimatedMemoryBytes(), stat.Size())
		})
	}
}

func topKOverlap(a, b []Hit) int {
	want := make(map[string]struct{}, len(a))
	for _, hit := range a {
		want[hit.DocumentID] = struct{}{}
	}
	n := 0
	for _, hit := range b {
		if _, ok := want[hit.DocumentID]; ok {
			n++
		}
	}
	return n
}

func percentileDuration(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	idx := int(float64(len(values)-1) * quantile)
	return values[idx]
}

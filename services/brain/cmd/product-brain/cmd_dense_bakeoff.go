// product-brain dense-bakeoff — deterministic exact-vs-ANN receipt harness.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
)

type denseBakeoffMetrics struct {
	Mode                   string  `json:"mode"`
	RecallAtK              float64 `json:"recall_at_k"`
	P50US                  int64   `json:"p50_us"`
	P95US                  int64   `json:"p95_us"`
	AllocationsPerQuery    float64 `json:"allocations_per_query"`
	AllocatedBytesPerQuery float64 `json:"allocated_bytes_per_query"`
	DistanceCalcsPerQuery  float64 `json:"distance_calculations_per_query"`
}

type denseBakeoffCorpus struct {
	Vectors          int                 `json:"vectors"`
	Dimensions       int                 `json:"dimensions"`
	Queries          int                 `json:"queries"`
	TopK             int                 `json:"top_k"`
	BuildMS          int64               `json:"build_ms"`
	IndexMemoryBytes int64               `json:"index_memory_bytes"`
	IndexDiskBytes   int64               `json:"index_disk_bytes"`
	Exact            denseBakeoffMetrics `json:"exact"`
	ANN              denseBakeoffMetrics `json:"ann"`
}

type denseBakeoffReceipt struct {
	Event     string               `json:"event"`
	Timestamp string               `json:"timestamp"`
	Seed      int64                `json:"seed"`
	Corpora   []denseBakeoffCorpus `json:"corpora"`
	Note      string               `json:"note"`
}

// runDenseBakeoff measures the actual local serving algorithm against its
// exact top-k truth set. It deliberately forces both routes, including below
// the automatic small-corpus exact threshold, so the comparison is not a
// substrate/configuration proxy.
func runDenseBakeoff(args []string) {
	fs := flag.NewFlagSet("dense-bakeoff", flag.ExitOnError)
	out := fs.String("out", "", "write receipt JSON to path (default stdout only)")
	sizesFlag := fs.String("sizes", "256,2048,8192", "comma-separated fixed corpus sizes")
	dim := fs.Int("dim", 32, "synthetic vector dimensions")
	queries := fs.Int("queries", 32, "deterministic queries per corpus")
	topK := fs.Int("top-k", 10, "recall cutoff")
	_ = fs.Parse(args)
	sizes, err := parseDenseBakeoffSizes(*sizesFlag)
	if err != nil || *dim <= 0 || *queries <= 0 || *topK <= 0 || (len(sizes) > 0 && *topK > sizes[0]) {
		fatal("dense-bakeoff: invalid fixed-corpus arguments")
	}
	receipt, err := measureDenseBakeoff(sizes, *dim, *queries, *topK, 305)
	if err != nil {
		fatal("dense-bakeoff: " + err.Error())
	}
	emitJSON(receipt)
	if *out != "" {
		raw, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			fatal("dense-bakeoff: marshal: " + err.Error())
		}
		if err := os.WriteFile(*out, append(raw, '\n'), 0o644); err != nil {
			fatal("dense-bakeoff: write out: " + err.Error())
		}
	}
}

func parseDenseBakeoffSizes(value string) ([]int, error) {
	seen := map[int]struct{}{}
	var sizes []int
	for _, raw := range strings.Split(value, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid corpus size %q", raw)
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		sizes = append(sizes, n)
	}
	if len(sizes) < 2 {
		return nil, fmt.Errorf("at least two corpus sizes are required")
	}
	sort.Ints(sizes)
	return sizes, nil
}

func measureDenseBakeoff(sizes []int, dim, queryCount, topK int, seed int64) (denseBakeoffReceipt, error) {
	receipt := denseBakeoffReceipt{
		Event: "dense_exact_ann_bakeoff", Timestamp: time.Now().UTC().Format(time.RFC3339), Seed: seed,
		Note: "deterministic synthetic corpus; recall@k is ANN/exact top-k overlap; memory excludes Go object overhead",
	}
	workDir, err := os.MkdirTemp("", "ouro-dense-bakeoff-*")
	if err != nil {
		return receipt, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	for _, vectors := range sizes {
		identity := dense.IndexIdentity{Scope: fmt.Sprintf("bakeoff-%d", vectors), Model: "synthetic:v1", Dimensions: dim}
		index := dense.NewScopedHNSW(identity, 16, 96)
		rng := rand.New(rand.NewSource(seed + int64(vectors)))
		corpus := make([][]float32, vectors)
		buildStart := time.Now()
		for i := range corpus {
			corpus[i] = make([]float32, dim)
			for j := range corpus[i] {
				corpus[i][j] = float32(rng.NormFloat64())
			}
			if err := index.Upsert(fmt.Sprintf("doc-%08d", i), corpus[i]); err != nil {
				return receipt, err
			}
		}
		buildMS := time.Since(buildStart).Milliseconds()
		queries := make([][]float32, queryCount)
		for i := range queries {
			queries[i] = append([]float32(nil), corpus[(i*61)%vectors]...)
			for j := range queries[i] {
				queries[i][j] += float32(rng.NormFloat64() * 0.01)
			}
		}
		path := filepath.Join(workDir, fmt.Sprintf("%d.ann", vectors))
		if err := index.Save(path); err != nil {
			return receipt, err
		}
		stat, err := os.Stat(path)
		if err != nil {
			return receipt, err
		}
		exact, truth, err := measureDenseMode(index, identity, queries, topK, dense.SearchModeExact, nil)
		if err != nil {
			return receipt, err
		}
		ann, _, err := measureDenseMode(index, identity, queries, topK, dense.SearchModeANN, truth)
		if err != nil {
			return receipt, err
		}
		receipt.Corpora = append(receipt.Corpora, denseBakeoffCorpus{
			Vectors: vectors, Dimensions: dim, Queries: queryCount, TopK: topK,
			BuildMS: buildMS, IndexMemoryBytes: index.EstimatedMemoryBytes(),
			IndexDiskBytes: stat.Size(), Exact: exact, ANN: ann,
		})
	}
	return receipt, nil
}

func measureDenseMode(index *dense.HNSW, identity dense.IndexIdentity, queries [][]float32, topK int, mode dense.SearchMode, truth [][]dense.Hit) (denseBakeoffMetrics, [][]dense.Hit, error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	latencies := make([]time.Duration, 0, len(queries))
	hitsByQuery := make([][]dense.Hit, 0, len(queries))
	overlap, distanceCalcs := 0, 0
	for i, query := range queries {
		start := time.Now()
		hits, diag, err := index.SearchScopedMode(query, topK, identity, mode)
		latencies = append(latencies, time.Since(start))
		if err != nil {
			return denseBakeoffMetrics{}, nil, err
		}
		hitsByQuery = append(hitsByQuery, hits)
		distanceCalcs += diag.DistanceCalculations
		if truth == nil {
			overlap += len(hits)
		} else {
			overlap += denseHitOverlap(truth[i], hits)
		}
	}
	runtime.ReadMemStats(&after)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	n := float64(len(queries))
	metrics := denseBakeoffMetrics{
		Mode: string(mode), RecallAtK: float64(overlap) / float64(len(queries)*topK),
		P50US: densePercentile(latencies, .50).Microseconds(), P95US: densePercentile(latencies, .95).Microseconds(),
		AllocationsPerQuery:    float64(after.Mallocs-before.Mallocs) / n,
		AllocatedBytesPerQuery: float64(after.TotalAlloc-before.TotalAlloc) / n,
		DistanceCalcsPerQuery:  float64(distanceCalcs) / n,
	}
	return metrics, hitsByQuery, nil
}

func denseHitOverlap(a, b []dense.Hit) int {
	set := make(map[string]struct{}, len(a))
	for _, hit := range a {
		set[hit.DocumentID] = struct{}{}
	}
	n := 0
	for _, hit := range b {
		if _, ok := set[hit.DocumentID]; ok {
			n++
		}
	}
	return n
}

func densePercentile(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	return values[int(float64(len(values)-1)*quantile)]
}

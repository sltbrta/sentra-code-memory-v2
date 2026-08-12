package scmbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// Baseline is the checked-in deterministic reference for a QA suite (issue
// #48). It captures the artifact digest, the measured quality floors, and the
// thresholds so CI/preflight can record and compare them across runs without
// depending on machine speed (latency is excluded).
type Baseline struct {
	Suite             string         `json:"suite"`
	Lane              string         `json:"lane"`
	Digest            string         `json:"digest"`
	HitRateAt1        float64        `json:"hit_rate_at_1"`
	HitRateAt5        float64        `json:"hit_rate_at_5"`
	HitRateAt10       float64        `json:"hit_rate_at_10"`
	MeanPrecision     float64        `json:"mean_precision"`
	TokenSavingsRatio float64        `json:"token_savings_ratio"`
	Failures          map[string]int `json:"failures"`
	Thresholds        Thresholds     `json:"thresholds"`
}

// BaselineFromReport snapshots the deterministic core of a QA report.
func BaselineFromReport(rep QAReport) Baseline {
	return Baseline{
		Suite:             rep.Suite,
		Lane:              rep.Lane,
		Digest:            rep.Digest,
		HitRateAt1:        rep.HitRateAt1,
		HitRateAt5:        rep.HitRateAt5,
		HitRateAt10:       rep.HitRateAt10,
		MeanPrecision:     rep.MeanPrecision,
		TokenSavingsRatio: rep.TokenSavingsRatio,
		Failures:          rep.Failures,
		Thresholds:        rep.Thresholds,
	}
}

// Retrieval lanes (issue #48). Benchmark claims must state which lane produced
// them. Only the local heuristic lane is implemented in the standalone
// product; dense/reranked and compiler-authority lanes are explicit deferred
// disclosures, never silently substituted.
const (
	LaneLocalHeuristic    = "local_heuristic"
	LaneDenseReranked     = "dense_reranked"     // deferred, not measured
	LaneCompilerAuthority = "compiler_authority" // deferred, not measured
)

// QAQuery is one retrieval probe with known expected targets. Expect lists
// repo-relative paths; the probe counts as a hit if any expected path appears
// in the ranked results.
type QAQuery struct {
	Name   string         `json:"name"`
	Q      string         `json:"q"`
	Expect []string       `json:"expect"`
	Args   map[string]any `json:"args,omitempty"`
}

// QAResult is the measured outcome of one probe.
type QAResult struct {
	Name      string  `json:"name"`
	Q         string  `json:"q"`
	OK        bool    `json:"ok"`
	NumHits   int     `json:"num_hits"`
	HitRank   int     `json:"hit_rank"` // 1-based rank of first expected hit; 0 = miss
	HitAt1    bool    `json:"hit_at_1"`
	HitAt5    bool    `json:"hit_at_5"`
	HitAt10   bool    `json:"hit_at_10"`
	Precision float64 `json:"precision"` // expected hits / returned hits
	LatencyMS int64   `json:"latency_ms"`
	RespBytes int     `json:"resp_bytes"`
	EstTokens int     `json:"est_tokens"`
	Failure   string  `json:"failure,omitempty"` // "" | empty | miss | error
}

// Thresholds are the regression gates the artifact is checked against.
type Thresholds struct {
	MinHitRateAt1        float64 `json:"min_hit_rate_at_1"`
	MinHitRateAt5        float64 `json:"min_hit_rate_at_5"`
	MinHitRateAt10       float64 `json:"min_hit_rate_at_10"`
	MaxP95LatencyMS      int64   `json:"max_p95_latency_ms"`
	MinTokenSavingsRatio float64 `json:"min_token_savings_ratio"`
	MaxFailedQueries     int     `json:"max_failed_queries"`
}

// QAReport is the deterministic retrieval-quality artifact (issue #48).
type QAReport struct {
	Contract string `json:"contract"`
	Suite    string `json:"suite"`
	Lane     string `json:"lane"`

	Queries []QAResult `json:"queries"`

	HitRateAt1    float64 `json:"hit_rate_at_1"`
	HitRateAt5    float64 `json:"hit_rate_at_5"`
	HitRateAt10   float64 `json:"hit_rate_at_10"`
	MeanPrecision float64 `json:"mean_precision"`

	MeanLatencyMS float64 `json:"mean_latency_ms"`
	P95LatencyMS  int64   `json:"p95_latency_ms"`

	Totals Totals `json:"totals"`

	BaselineBytes     int64   `json:"baseline_bytes"`
	BaselineTokens    int     `json:"baseline_tokens"`
	SavedBytes        int64   `json:"saved_bytes"`
	SavedTokens       int     `json:"saved_tokens"`
	TokenSavingsRatio float64 `json:"token_savings_ratio"`

	Failures map[string]int `json:"failures"`

	Thresholds Thresholds `json:"thresholds"`
	Pass       bool       `json:"pass"`
	Reasons    []string   `json:"reasons,omitempty"`

	// Digest is a sha256 over the deterministic core (hit@k, precision,
	// failures, token totals) with latency excluded, so CI can detect a
	// retrieval regression independent of machine speed.
	Digest string `json:"digest"`
}

// QASuite is a deterministic retrieval-quality benchmark against one root.
type QASuite struct {
	Name       string    `json:"name"`
	Root       string    `json:"root"`
	IndexCache string    `json:"index_cache"`
	Lane       string    `json:"lane"`
	Queries    []QAQuery `json:"queries"`
}

// RunQA indexes the root and measures every probe through codeserve. It is
// offline and deterministic for hit@k; latency is recorded, never asserted
// here (thresholds decide).
func RunQA(ctx context.Context, suite QASuite) (QAReport, error) {
	lane := suite.Lane
	if lane == "" {
		lane = LaneLocalHeuristic
	}
	// Resolve absolute paths up front: codecrawl canonicalizes root internally,
	// so normalization must scrub the same absolute prefix to stay deterministic
	// across checkout locations.
	if abs, err := filepath.Abs(suite.Root); err == nil {
		suite.Root = abs
	}
	if abs, err := filepath.Abs(suite.IndexCache); err == nil {
		suite.IndexCache = abs
	}
	rep := QAReport{
		Contract: codeserve.ContractID,
		Suite:    suite.Name,
		Lane:     lane,
		Failures: map[string]int{},
	}

	// Build the index once so every probe uses the warm path.
	indexResp := codeserve.Handle(ctx, codeserve.Request{
		"verb": "code_index", "root": suite.Root, "index_cache": suite.IndexCache,
	})
	if indexResp["ok"] != true {
		return rep, fmt.Errorf("qa index failed: %v", indexResp)
	}

	for _, q := range suite.Queries {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Queries = append(rep.Queries, runQAQuery(ctx, suite, q))
	}

	aggregateQA(&rep)
	return rep, nil
}

func runQAQuery(ctx context.Context, suite QASuite, q QAQuery) QAResult {
	req := codeserve.Request{
		"verb":        "code_search",
		"root":        suite.Root,
		"index_cache": suite.IndexCache,
		"q":           q.Q,
		"top_k":       10,
		"no_refresh":  true,
	}
	for k, v := range q.Args {
		req[k] = v
	}

	t0 := time.Now()
	resp := codeserve.Handle(ctx, req)
	latency := time.Since(t0).Milliseconds()

	res := QAResult{Name: q.Name, Q: q.Q, LatencyMS: latency}

	ok, _ := resp["ok"].(bool)
	res.OK = ok
	if !ok {
		res.Failure = "error"
		return res
	}

	// code_search returns a raw ranked []codecrawl.Hit (Path, Score). Reading
	// the typed slice directly keeps hit@k independent of wire tag casing.
	hits, _ := resp["hits"].([]codecrawl.Hit)

	// Normalize path-bearing bytes before accounting so checkout location does
	// not masquerade as token usage.
	wire, _ := json.Marshal(resp)
	var decoded any
	_ = json.Unmarshal(wire, &decoded)
	raw, _ := json.Marshal(normalizeValue(decoded, suite.Root, suite.IndexCache))
	res.RespBytes = len(raw)
	res.EstTokens = EstimateTokens(string(raw))

	res.NumHits = len(hits)
	if len(hits) == 0 {
		res.Failure = "empty"
		return res
	}

	expected := map[string]bool{}
	for _, e := range q.Expect {
		expected[e] = true
	}
	relevant := 0
	for i, h := range hits {
		if expected[h.Path] {
			relevant++
			if res.HitRank == 0 {
				res.HitRank = i + 1
			}
		}
	}
	res.Precision = float64(relevant) / float64(len(hits))
	res.HitAt1 = res.HitRank == 1
	res.HitAt5 = res.HitRank >= 1 && res.HitRank <= 5
	res.HitAt10 = res.HitRank >= 1 && res.HitRank <= 10
	if res.HitRank == 0 {
		res.Failure = "miss"
	}
	return res
}

func aggregateQA(rep *QAReport) {
	n := len(rep.Queries)
	if n == 0 {
		return
	}
	var hit1, hit5, hit10, failed int
	var precSum float64
	var latencies []int64
	for _, q := range rep.Queries {
		if q.HitAt1 {
			hit1++
		}
		if q.HitAt5 {
			hit5++
		}
		if q.HitAt10 {
			hit10++
		}
		if q.Failure != "" {
			failed++
			rep.Failures[q.Failure]++
		}
		precSum += q.Precision
		latencies = append(latencies, q.LatencyMS)

		rep.Totals.ResponseBytes += q.RespBytes
		rep.Totals.EstTokens += q.EstTokens
		rep.Totals.ToolCalls++
		rep.Totals.DurationMS += q.LatencyMS
	}
	rep.HitRateAt1 = float64(hit1) / float64(n)
	rep.HitRateAt5 = float64(hit5) / float64(n)
	rep.HitRateAt10 = float64(hit10) / float64(n)
	rep.MeanPrecision = precSum / float64(n)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var sum int64
	for _, l := range latencies {
		sum += l
	}
	rep.MeanLatencyMS = float64(sum) / float64(n)
	rep.P95LatencyMS = percentile(latencies, 0.95)

	rep.Digest = qaDigest(rep)
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// MeasureQABaseline fills the token-savings fields against the whole-tree
// naive baseline, matching the scenario baseline semantics.
func (rep *QAReport) MeasureQABaseline(root string) error {
	b, err := NaiveBaselineBytes(root)
	if err != nil {
		return err
	}
	rep.BaselineBytes = b
	rep.BaselineTokens = estimateBytes(b)
	rep.SavedBytes = b - int64(rep.Totals.ResponseBytes)
	rep.SavedTokens = rep.BaselineTokens - rep.Totals.EstTokens
	if rep.BaselineTokens > 0 {
		rep.TokenSavingsRatio = float64(rep.SavedTokens) / float64(rep.BaselineTokens)
	}
	// The savings fields feed the digest, so recompute it once they are set.
	rep.Digest = qaDigest(rep)
	return nil
}

// CheckThresholds gates the report against its thresholds, populating Pass and
// the human-readable Reasons for any violation.
func (rep *QAReport) CheckThresholds(t Thresholds) {
	rep.Thresholds = t
	rep.Pass = true
	rep.Reasons = nil
	add := func(format string, args ...any) {
		rep.Pass = false
		rep.Reasons = append(rep.Reasons, fmt.Sprintf(format, args...))
	}
	if rep.HitRateAt1 < t.MinHitRateAt1 {
		add("hit@1 %.2f below threshold %.2f", rep.HitRateAt1, t.MinHitRateAt1)
	}
	if rep.HitRateAt5 < t.MinHitRateAt5 {
		add("hit@5 %.2f below threshold %.2f", rep.HitRateAt5, t.MinHitRateAt5)
	}
	if rep.HitRateAt10 < t.MinHitRateAt10 {
		add("hit@10 %.2f below threshold %.2f", rep.HitRateAt10, t.MinHitRateAt10)
	}
	if t.MaxP95LatencyMS > 0 && rep.P95LatencyMS > t.MaxP95LatencyMS {
		add("p95 latency %dms above threshold %dms", rep.P95LatencyMS, t.MaxP95LatencyMS)
	}
	if rep.TokenSavingsRatio < t.MinTokenSavingsRatio {
		add("token savings ratio %.2f below threshold %.2f", rep.TokenSavingsRatio, t.MinTokenSavingsRatio)
	}
	failed := 0
	for _, q := range rep.Queries {
		if q.Failure == "error" {
			failed++
		}
	}
	if failed > t.MaxFailedQueries {
		add("%d errored queries above threshold %d", failed, t.MaxFailedQueries)
	}
}

// qaDigest hashes the deterministic core of the report (hit@k, precision,
// failure classification, and token totals) with latency excluded, so the
// digest is stable across machines but changes on any retrieval regression.
func qaDigest(rep *QAReport) string {
	type coreQuery struct {
		Name      string  `json:"name"`
		Q         string  `json:"q"`
		OK        bool    `json:"ok"`
		NumHits   int     `json:"num_hits"`
		HitRank   int     `json:"hit_rank"`
		HitAt1    bool    `json:"hit_at_1"`
		HitAt5    bool    `json:"hit_at_5"`
		HitAt10   bool    `json:"hit_at_10"`
		Precision float64 `json:"precision"`
		Failure   string  `json:"failure,omitempty"`
		RespBytes int     `json:"resp_bytes"`
		EstTokens int     `json:"est_tokens"`
	}
	core := struct {
		Suite             string         `json:"suite"`
		Lane              string         `json:"lane"`
		Queries           []coreQuery    `json:"queries"`
		HitRateAt1        float64        `json:"hit_rate_at_1"`
		HitRateAt5        float64        `json:"hit_rate_at_5"`
		HitRateAt10       float64        `json:"hit_rate_at_10"`
		MeanPrecision     float64        `json:"mean_precision"`
		Totals            Totals         `json:"totals"`
		BaselineBytes     int64          `json:"baseline_bytes"`
		SavedTokens       int            `json:"saved_tokens"`
		TokenSavingsRatio float64        `json:"token_savings_ratio"`
		Failures          map[string]int `json:"failures"`
	}{
		Suite: rep.Suite, Lane: rep.Lane,
		HitRateAt1: rep.HitRateAt1, HitRateAt5: rep.HitRateAt5,
		HitRateAt10: rep.HitRateAt10, MeanPrecision: rep.MeanPrecision,
		Totals: Totals{ // latency excluded from the digest
			ResponseBytes: rep.Totals.ResponseBytes,
			EstTokens:     rep.Totals.EstTokens,
			ToolCalls:     rep.Totals.ToolCalls,
		},
		BaselineBytes:     rep.BaselineBytes,
		SavedTokens:       rep.SavedTokens,
		TokenSavingsRatio: rep.TokenSavingsRatio,
		Failures:          rep.Failures,
	}
	core.Queries = make([]coreQuery, 0, len(rep.Queries))
	for _, q := range rep.Queries {
		core.Queries = append(core.Queries, coreQuery{
			Name: q.Name, Q: q.Q, OK: q.OK, NumHits: q.NumHits, HitRank: q.HitRank,
			HitAt1: q.HitAt1, HitAt5: q.HitAt5, HitAt10: q.HitAt10,
			Precision: q.Precision, Failure: q.Failure,
			RespBytes: q.RespBytes, EstTokens: q.EstTokens,
		})
	}
	raw, err := json.Marshal(core)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

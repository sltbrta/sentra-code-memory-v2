package hosted

import (
	"context"
	"time"
)

// ProdProfile budgets one production retrieve/answer for interactive + Modal SLA.
// Precedence: BENCHMAX > QUALITY > PROD default.
//
// OUROBOROS_ERB_BENCHMAX=1 (or OUROBOROS_ERB_BENCH_MAX=1) enables an intentionally
// unbounded score-max profile for a pinned ERB brain; it is never the product default.
// Default-on: set OUROBOROS_ERB_QUALITY=1 for wider multi-arm (still wall-capped).
//
// Latency targets (warm HotLex via --hosted-loop):
//
//	light / basic:     ≤30s wall
//	agentic multi-doc: ≤60s wall
//
// Arms are parallel so wall ≈ max(arm), not sum. Nested expand uses ExpandLite.
type ProdProfile struct {
	// Enabled when production budgets apply (default true).
	Enabled bool
	// Quality disables prod budgets (full residual multi-arm).
	Quality bool
	// Benchmax is the ERB score-max profile: no latency/cost limits.
	// It implies Quality=true and Enabled=false.
	Benchmax bool

	// Lexical
	LexCap          int           // max FTS variants
	LexTerms        int           // max OR terms in tsquery
	LexTimeout      time.Duration // per FTS query
	LexLimit        int           // rows per FTS
	LexExpandIfThin int           // expand only if primary hits < this

	// Dense
	DenseQueries int // max embed+ANN queries (1 = primary only)

	// Structure
	StructurePoolOnly bool // no structure_lexical_rescue FTS
	StructureMaxNeigh int

	// Hydrate / pool
	HydrateDocs        int
	HydrateChunks      int
	HydrateDocsMulti   int
	HydrateChunksMulti int
	PoolK              int

	// Answer / agentic
	Agentic       bool
	Corrective    bool
	MaxSynthRetry int
	AgenticRounds int // 0 = caller default
	AgenticExtra  int // 0 = caller default
}

// benchmaxProfile is the unlimited ERB score-chase config. Use only against a
// pinned stored ERB brain; never enable it for lean product serving.
func benchmaxEnabled() bool {
	return envTruthy("OUROBOROS_ERB_BENCHMAX", false) ||
		envTruthy("OUROBOROS_ERB_BENCH_MAX", false)
}

func benchmaxProfile() ProdProfile {
	lexTimeoutMS := envIntAllowZero("OUROBOROS_ERB_BENCHMAX_LEX_TIMEOUT_MS", 0)
	return ProdProfile{
		Enabled:            false,
		Quality:            true,
		Benchmax:           true,
		LexCap:             envInt("OUROBOROS_ERB_BENCHMAX_LEX_CAP", 8),
		LexTerms:           envInt("OUROBOROS_ERB_BENCHMAX_LEX_TERMS", 48),
		LexTimeout:         time.Duration(lexTimeoutMS) * time.Millisecond,
		LexLimit:           envInt("OUROBOROS_ERB_BENCHMAX_LEX_LIMIT", 80),
		LexExpandIfThin:    envInt("OUROBOROS_ERB_BENCHMAX_LEX_THIN", 16),
		DenseQueries:       envInt("OUROBOROS_ERB_BENCHMAX_DENSE_QUERIES", 6),
		StructurePoolOnly:  false,
		StructureMaxNeigh:  envInt("OUROBOROS_ERB_BENCHMAX_STRUCTURE_NEIGH", 24),
		HydrateDocs:        envInt("OUROBOROS_ERB_BENCHMAX_HYDRATE_DOCS", 12),
		HydrateChunks:      envInt("OUROBOROS_ERB_BENCHMAX_HYDRATE_CHUNKS", 8),
		HydrateDocsMulti:   envInt("OUROBOROS_ERB_BENCHMAX_HYDRATE_DOCS_MULTI", 16),
		HydrateChunksMulti: envInt("OUROBOROS_ERB_BENCHMAX_HYDRATE_CHUNKS_MULTI", 16),
		PoolK:              envInt("OUROBOROS_ERB_BENCHMAX_POOL_K", 96),
		Agentic:            true,
		Corrective:         true,
		MaxSynthRetry:      envInt("OUROBOROS_ERB_BENCHMAX_SYNTH_RETRY", 4),
		AgenticRounds:      envInt("OUROBOROS_ERB_BENCHMAX_AGENTIC_ROUNDS", 5),
		AgenticExtra:       envInt("OUROBOROS_ERB_BENCHMAX_AGENTIC_EXTRA", 16),
	}
}

// prodProfileFromEnv loads production budgets.
//
//	OUROBOROS_ERB_PROD=1     (default true) apply budgets
//	OUROBOROS_ERB_QUALITY=1  wider residual (still ≤30–60s warm target)
//	OUROBOROS_ERB_LEAN stays respected as secondary hint
func prodProfileFromEnv() ProdProfile {
	if benchmaxEnabled() {
		return benchmaxProfile()
	}
	quality := envTruthy("OUROBOROS_ERB_QUALITY", false)
	prod := envTruthy("OUROBOROS_ERB_PROD", true)
	if quality {
		// QUALITY/bench: SOTA recall with usable wall (warm p50 ≤30s, agentic ≤60s).
		// Never reintroduce 60s lex / 8s structure floors (p50~294s / 300s hangs).
		lexMS := envInt("OUROBOROS_ERB_LEX_TIMEOUT_MS", 2500)
		return ProdProfile{
			Enabled:            false,
			Quality:            true,
			LexCap:             envInt("OUROBOROS_ERB_LEX_VARIANT_CAP", 3),
			LexTerms:           envInt("OUROBOROS_ERB_LEX_TERMS", 14),
			LexTimeout:         time.Duration(lexMS) * time.Millisecond,
			LexLimit:           envInt("OUROBOROS_ERB_HOSTED_LEXICAL_LIMIT", 36),
			LexExpandIfThin:    envInt("OUROBOROS_ERB_LEX_THIN", 8),
			DenseQueries:       envInt("OUROBOROS_ERB_DENSE_QUERIES", 2),
			StructurePoolOnly:  envTruthy("OUROBOROS_ERB_STRUCTURE_POOL_ONLY", true),
			StructureMaxNeigh:  envInt("OUROBOROS_ERB_STRUCTURE_NEIGH", 12),
			HydrateDocs:        envInt("OUROBOROS_ERB_HYDRATE_DOCS", 4),
			HydrateChunks:      envInt("OUROBOROS_ERB_HYDRATE_CHUNKS", 2),
			HydrateDocsMulti:   envInt("OUROBOROS_ERB_HYDRATE_DOCS_MULTI", 6),
			HydrateChunksMulti: envInt("OUROBOROS_ERB_HYDRATE_CHUNKS_MULTI", 4),
			PoolK:              envInt("OUROBOROS_ERB_HOSTED_POOL_K", 64),
			Agentic:            envTruthy("OUROBOROS_BRAIN_AGENTIC", true),
			Corrective:         envTruthy("OUROBOROS_ERB_CORRECTIVE", false),
			MaxSynthRetry:      envInt("OUROBOROS_ERB_MAX_SYNTH_RETRY", 1),
		}
	}
	if !prod {
		lexMS := envInt("OUROBOROS_ERB_LEX_TIMEOUT_MS", 3000)
		return ProdProfile{
			Enabled:            false,
			LexCap:             envInt("OUROBOROS_ERB_LEX_VARIANT_CAP", 2),
			LexTerms:           16,
			LexTimeout:         time.Duration(lexMS) * time.Millisecond,
			LexExpandIfThin:    8,
			DenseQueries:       envInt("OUROBOROS_ERB_DENSE_QUERIES", 2),
			StructurePoolOnly:  envTruthy("OUROBOROS_ERB_STRUCTURE_POOL_ONLY", true),
			StructureMaxNeigh:  12,
			HydrateDocs:        4,
			HydrateChunks:      2,
			HydrateDocsMulti:   6,
			HydrateChunksMulti: 4,
			PoolK:              envInt("OUROBOROS_ERB_HOSTED_POOL_K", 56),
			Agentic:            envTruthy("OUROBOROS_BRAIN_AGENTIC", true),
			Corrective:         envTruthy("OUROBOROS_ERB_CORRECTIVE", false),
			MaxSynthRetry:      envInt("OUROBOROS_ERB_MAX_SYNTH_RETRY", 1),
		}
	}
	// Light serve: hybrid arms, tight budgets, ExpandLite agentic.
	return ProdProfile{
		Enabled:            true,
		Quality:            false,
		LexCap:             envInt("OUROBOROS_ERB_LEX_VARIANT_CAP", 2),
		LexTerms:           envInt("OUROBOROS_ERB_LEX_TERMS", 12),
		LexTimeout:         time.Duration(envInt("OUROBOROS_ERB_LEX_TIMEOUT_MS", 2000)) * time.Millisecond,
		LexLimit:           envInt("OUROBOROS_ERB_HOSTED_LEXICAL_LIMIT", 28),
		LexExpandIfThin:    envInt("OUROBOROS_ERB_LEX_THIN", 6),
		DenseQueries:       envInt("OUROBOROS_ERB_DENSE_QUERIES", 2),
		StructurePoolOnly:  envTruthy("OUROBOROS_ERB_STRUCTURE_POOL_ONLY", true),
		StructureMaxNeigh:  envInt("OUROBOROS_ERB_STRUCTURE_NEIGH", 10),
		HydrateDocs:        envInt("OUROBOROS_ERB_HYDRATE_DOCS", 3),
		HydrateChunks:      envInt("OUROBOROS_ERB_HYDRATE_CHUNKS", 2),
		HydrateDocsMulti:   envInt("OUROBOROS_ERB_HYDRATE_DOCS_MULTI", 5),
		HydrateChunksMulti: envInt("OUROBOROS_ERB_HYDRATE_CHUNKS_MULTI", 3),
		PoolK:              envInt("OUROBOROS_ERB_HOSTED_POOL_K", 48),
		Agentic:            envTruthy("OUROBOROS_BRAIN_AGENTIC", true),
		Corrective:         envTruthy("OUROBOROS_ERB_CORRECTIVE", false),
		MaxSynthRetry:      envInt("OUROBOROS_ERB_MAX_SYNTH_RETRY", 1),
	}
}

// leanMode reports whether lean multi-query/arm skips apply. Benchmax forces it off.
func leanMode() bool {
	if benchmaxEnabled() {
		return false
	}
	return envTruthy("OUROBOROS_ERB_LEAN", true)
}

// envIntAllowZero parses a non-negative integer where zero is meaningful.
func envIntAllowZero(key string, def int) int {
	v := envOr(key, "")
	if v == "" {
		return def
	}
	n := 0
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// withTimeout derives a child context with timeout, or returns parent if d<=0.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// structureSQLBudget: path2 entity/fact/rel SQL. Target wall share ≤2s QUALITY.
// Override: OUROBOROS_BRAIN_STRUCTURE_SQL_MS.
func structureSQLBudget(prod ProdProfile) time.Duration {
	if ms := envInt("OUROBOROS_BRAIN_STRUCTURE_SQL_MS", 0); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if prod.Benchmax {
		return 0
	}
	if prod.Quality {
		return 2 * time.Second
	}
	if !prod.Enabled {
		return 2500 * time.Millisecond
	}
	return 1500 * time.Millisecond
}

// hydrateBudget: sibling hydrate share ≤1.5s QUALITY.
// Override: OUROBOROS_ERB_HYDRATE_TIMEOUT_MS.
func hydrateBudget(prod ProdProfile) time.Duration {
	if ms := envInt("OUROBOROS_ERB_HYDRATE_TIMEOUT_MS", 0); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if prod.Benchmax {
		return 0
	}
	if prod.Quality {
		return 1500 * time.Millisecond
	}
	if !prod.Enabled {
		return 1500 * time.Millisecond
	}
	return time.Second
}

// structureHydrateBudget: path2-promoted doc hydrate ≤1s.
// Override: OUROBOROS_BRAIN_STRUCTURE_HYDRATE_MS.
func structureHydrateBudget(prod ProdProfile) time.Duration {
	if ms := envInt("OUROBOROS_BRAIN_STRUCTURE_HYDRATE_MS", 0); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if prod.Benchmax {
		return 0
	}
	if prod.Quality {
		return time.Second
	}
	return 800 * time.Millisecond
}

// denseBudget: embed+ANN parallel with lex. QUALITY ≤2.5s, light ≤2s.
// Override: OUROBOROS_ERB_DENSE_TIMEOUT_MS.
func denseBudget(prod ProdProfile) time.Duration {
	if ms := envInt("OUROBOROS_ERB_DENSE_TIMEOUT_MS", 0); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if prod.Benchmax {
		return 0
	}
	d := prod.LexTimeout
	if d <= 0 {
		d = 2500 * time.Millisecond
	}
	if prod.Quality && d > 2500*time.Millisecond {
		return 2500 * time.Millisecond
	}
	if prod.Enabled && d > 2*time.Second {
		return 2 * time.Second
	}
	if d > 2500*time.Millisecond {
		return 2500 * time.Millisecond
	}
	return d
}

// phaseABudget is the parallel HotLex+dense+FTS wall for interactive retrieve.
// Target: ≤2.5s QUALITY, ≤2s light, ≤1.5s ExpandLite nested.
func phaseABudget(prod ProdProfile, expandLite bool) time.Duration {
	if prod.Benchmax {
		return 0
	}
	b := denseBudget(prod)
	if lt := prod.LexTimeout; lt > 0 && (b <= 0 || lt > b) {
		b = lt
	}
	if b <= 0 {
		b = 2500 * time.Millisecond
	}
	if expandLite {
		if b > 1500*time.Millisecond {
			return 1500 * time.Millisecond
		}
		return b
	}
	if prod.Quality && b > 2500*time.Millisecond {
		return 2500 * time.Millisecond
	}
	if prod.Enabled && !prod.Quality && b > 2*time.Second {
		return 2 * time.Second
	}
	if b > 3*time.Second {
		return 3 * time.Second
	}
	return b
}

const (
	missingHotLexFTSQueryCap = 1
	maxLiveFTSBudget         = 3 * time.Second
)

// officialRetrievalMode mirrors the official evaluation boundary inside the
// hosted package. Official runs may widen recall, but they must retain live
// request safety even if a conflicting BENCHMAX or force flag is present.
func officialRetrievalMode() bool {
	return envTruthy("OUROBOROS_ERB_OFFICIAL", false) ||
		envTruthy("OUROBOROS_ERB_OFFICIAL_JUDGE", false)
}

// boundedFTSBudget returns a finite shared wall budget for every product/default
// and official FTS arm. A deliberately non-official BENCHMAX run may still use
// its requested budget, including zero (caller-deadline-only), for offline score
// investigation. Parallel variants share this one wall; it is not multiplied by
// the number of queries.
func boundedFTSBudget(prod ProdProfile, requested time.Duration) time.Duration {
	if prod.Benchmax && !officialRetrievalMode() {
		return requested
	}
	if requested <= 0 || requested > maxLiveFTSBudget {
		return maxLiveFTSBudget
	}
	return requested
}

// interactiveFTSQueryCap bounds the emergency lexical fallback more tightly
// than the normal hybrid arm. A usable projection retains the existing
// parallel variant cap; an unavailable projection gets one Neon query only.
func interactiveFTSQueryCap(hotAvailable bool) int {
	if !hotAvailable {
		return missingHotLexFTSQueryCap
	}
	return 3
}

// interactiveFTSBudget keeps the missing-HotLex fallback bounded even in
// benchmax, whose normal phase budget is intentionally unlimited. Product and
// quality modes retain their already-tighter phase-A limits.
func interactiveFTSBudget(prod ProdProfile, hotAvailable bool) time.Duration {
	b := phaseABudget(prod, false)
	if !hotAvailable && (b <= 0 || b > maxLiveFTSBudget) {
		b = maxLiveFTSBudget
	}
	return boundedFTSBudget(prod, b)
}

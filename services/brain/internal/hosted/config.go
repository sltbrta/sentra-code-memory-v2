package hosted

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config pins the hosted full-bench brain and answer-stack budgets.
type Config struct {
	NeonDatabaseURL string
	QdrantURL       string
	QdrantAPIKey    string
	BrainID         string
	ChunkCollection string
	ChunkVectorName string
	CohereModel     string
	CohereDim       int
	LexicalLimit    int
	DenseLimit      int
	RRFK            int
	// PoolK is post-RRF candidate pool before CE (wider than final window).
	PoolK int
	// TopK is final context-window size after CE + retain (tight dump).
	TopK int
	// MaxCite is hard cap on cited_document_ids (basic/default).
	MaxCite int
	// MaxPassageChars clips evidence bodies in the LLM prompt.
	MaxPassageChars int
}

// Enabled reports whether the product brain should use hosted retrieval.
func Enabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("OUROBOROS_BRAIN_HOSTED")))
	if v == "" {
		v = strings.TrimSpace(strings.ToLower(os.Getenv("OUROBOROS_ERB_HOSTED")))
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		// An explicit local/fixture switch must win over credentials present in
		// the container; otherwise company JSONL asks accidentally route to the
		// hosted ERB store.
		return false
	}
	if os.Getenv("NEON_DATABASE_URL") != "" &&
		os.Getenv("QDRANT_URL") != "" &&
		os.Getenv("QDRANT_API_KEY") != "" {
		return true
	}
	return false
}

// FromEnv builds Config or returns a clear error naming missing vars.
func FromEnv() (Config, error) {
	neon := strings.TrimSpace(os.Getenv("NEON_DATABASE_URL"))
	if neon == "" {
		neon = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	qurl := strings.TrimSpace(os.Getenv("QDRANT_URL"))
	qkey := strings.TrimSpace(os.Getenv("QDRANT_API_KEY"))
	var missing []string
	if neon == "" {
		missing = append(missing, "NEON_DATABASE_URL")
	}
	if qurl == "" {
		missing = append(missing, "QDRANT_URL")
	}
	if qkey == "" {
		missing = append(missing, "QDRANT_API_KEY")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("hosted config missing: %s", strings.Join(missing, ", "))
	}
	return Config{
		NeonDatabaseURL: neon,
		QdrantURL:       strings.TrimRight(qurl, "/"),
		QdrantAPIKey:    qkey,
		BrainID:         envOr("OUROBOROS_ERB_BRAIN_ID", "full-bench-v2"),
		ChunkCollection: envOr("OUROBOROS_ERB_QDRANT_CHUNK_COLLECTION", "path2_chunk_vectors"),
		ChunkVectorName: envOr("OUROBOROS_ERB_QDRANT_VECTOR_NAME", "chunk"),
		CohereModel:     envOr("OUROBOROS_ERB_COHERE_MODEL", "embed-v4.0"),
		CohereDim:       envInt("OUROBOROS_ERB_COHERE_DIM", 1536),
		// Prod defaults: tighter caps. Override freely via env.
		LexicalLimit: envInt("OUROBOROS_ERB_HOSTED_LEXICAL_LIMIT", 20),
		DenseLimit:   envInt("OUROBOROS_ERB_HOSTED_DENSE_LIMIT", 20),
		RRFK:         envInt("OUROBOROS_ERB_HOSTED_RRF_K", 60),
		PoolK:        envInt("OUROBOROS_ERB_HOSTED_POOL_K", 28),
		TopK:         envInt("OUROBOROS_ERB_TOP_K", 8),
		MaxCite:      envInt("OUROBOROS_ERB_MAX_CITES", 3),
		// 2400 keeps most ERB timeline/correction tails (p90 chunk length is
		// near 2k; blind 1600 clips mid-doc freezes and superseding rates).
		MaxPassageChars: envInt("OUROBOROS_ERB_PASSAGE_CHARS", 2400),
	}, nil
}

// passageFactRicher reports whether a has more ISO/money/duration atoms than b.
// Used when upgrading hydrate text of similar length.
func passageFactRicher(a, b string) bool {
	score := func(s string) int {
		n := len(isoDateRE.FindAllString(s, -1))
		n += len(moneyRE.FindAllString(s, -1))
		n += len(durationAtomRE.FindAllString(s, -1))
		return n
	}
	return score(a) > score(b)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// geminiAPIKey returns GEMINI_API_KEY or GOOGLE_API_KEY (first non-empty).
func geminiAPIKey() string {
	if k := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv("GOOGLE_API_KEY"))
}

func envInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envTruthy(k string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(k)))
	if v == "" {
		return def
	}
	switch v {
	case "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	}
	return def
}

// SynthTemperature returns the configured synthesis temperature.
// Default: 0 for deterministic ancillary calls (query plan, multi-query),
// and callers may pass their own default for the primary synthesis call.
func SynthTemperature(fallback float64) float64 {
	v := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_TEMPERATURE"))
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// synthTemperature is the unexported alias used within the hosted package.
func synthTemperature(fallback float64) float64 {
	return SynthTemperature(fallback)
}

// SynthSeed returns the configured synthesis seed or nil when unset.
// Production defaults leave seed unset; official eval may pin it.
func SynthSeed() *int {
	v := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_SEED"))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

// synthSeed is the unexported alias used within the hosted package.
func synthSeed() *int {
	return SynthSeed()
}

// seedSupportedProviders lists provider names known to accept the seed parameter.
var seedSupportedProviders = map[string]bool{
	"openai":     true,
	"openrouter": true,
	"gemini":     true,
}

// ProviderSupportsSeed reports whether the named provider accepts a seed parameter.
func ProviderSupportsSeed(name string) bool {
	return seedSupportedProviders[name]
}

// providerSupportsSeed is the unexported alias used within the hosted package.
func providerSupportsSeed(name string) bool {
	return ProviderSupportsSeed(name)
}

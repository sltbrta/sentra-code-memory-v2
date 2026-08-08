package hosted

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/dense"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/gardener"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/memory"
)

// Substrate module names (ADR 0024). Values are implementation ids, not product forks.
const (
	SubstrateChunksFS     = "fs"
	SubstrateChunksMemory = "memory"
	SubstrateChunksNeon   = "neon"

	// Queue substrates. Residual/hosted always prefer durable.
	// sqlite: zero-deps laptop (WAL, single-writer). postgres: parallel local/team R/W.
	SubstrateQueueSQLite    = "sqlite"    // durable gardener.db
	SubstrateQueuePostgres  = "postgres"  // durable multi-worker (FOR UPDATE SKIP LOCKED)
	SubstrateQueueMemory    = "memory"    // durable when Dir set (alias → sqlite); process-only only without Dir
	SubstrateQueueEphemeral = "ephemeral" // tests only — process-local MemoryQueue
	SubstrateQueueNone      = "none"

	SubstrateCortexFS     = "fs"
	SubstrateCortexMemory = "memory" // FS under Dir/temp — same memory.Store API
	SubstrateCortexNone   = "none"

	// Dense ANN substrates.
	SubstrateDenseNone     = "none"
	SubstrateDenseEnv      = "env"      // Qdrant when keys set (legacy alias for qdrant)
	SubstrateDenseQdrant   = "qdrant"   // remote ANN
	SubstrateDenseSQLite   = "sqlite"   // durable vectors + bounded local ANN under Dir
	SubstrateDensePostgres = "postgres" // vectors in Postgres (app-side cosine; pgvector optional later)
	SubstrateDenseMemory   = "memory"   // in-process bag store (tests / ultra-light)
	SubstrateDenseFAISS    = "faiss"    // HTTP FAISS/usearch sidecar (BYOC)

	// ProfileSolo is all-local substrates (FS chunks + SQLite queue + FS cortex).
	ProfileSolo = "solo"
	// ProfileTeam is remote chunks (neon) with local queue/cortex defaults when Dir set.
	ProfileTeam = "team"
	// ProfileBench leaves dense/env open; still one residual pipeline.
	ProfileBench = "bench"
)

// SubstrateConfig binds residual modules independently (ADR 0024).
// Empty fields resolve via Profile or solo defaults when Dir is set.
type SubstrateConfig struct {
	// Profile is solo|team|bench|custom (empty = infer).
	Profile string
	// Dir is the durable root for FS queue/cortex/dense (and local chunks when chunks=fs).
	Dir string
	// Chunks is fs|memory|neon (informational once Client is open; used by open helpers).
	Chunks string
	// Queue is sqlite|memory|ephemeral|none.
	Queue string
	// QueuePath overrides SQLite path (default <Dir>/gardener.db or env).
	QueuePath string
	// Cortex is fs|memory|none (memory uses Dir or a temp under Dir).
	Cortex string
	// CortexPath overrides cortex open path (default <Dir>).
	CortexPath string
	// Dense is none|env|qdrant|sqlite|postgres|faiss|memory.
	Dense string
	// DenseDSN overrides Postgres dense DSN (default queue DSN / DATABASE_URL).
	DenseDSN string
	// DenseURL is FAISS/usearch HTTP sidecar base (OUROBOROS_BRAIN_DENSE_URL).
	DenseURL string
	// DenseSearchMode is auto|exact|ann for local HNSW. Exact and ANN are
	// explicit operator/bakeoff overrides; auto is the production default.
	DenseSearchMode string
	// LLM / Embed / Ranker are API-side substrates: hosted|mlx|none (defaults hosted-prefer).
	LLM    string
	Embed  string
	Ranker string
	// Workers is local burst/gardener concurrency hint (0 = defaultLocalWorkers).
	Workers int
	// AutoGardener starts background drain when queue is bound.
	AutoGardener bool
	// queueDurableAlias records when memory→sqlite coercion happened.
	queueDurableAlias bool
}

// SoloSubstrate returns the default laptop residual binding under dir.
// Dense defaults to sqlite so local ANN is available offline (bag embed without keys).
func SoloSubstrate(dir string) SubstrateConfig {
	return SubstrateConfig{
		Profile: ProfileSolo,
		Dir:     dir,
		Chunks:  SubstrateChunksFS,
		Queue:   SubstrateQueueSQLite,
		Cortex:  SubstrateCortexFS,
		Dense:   SubstrateDenseSQLite,
		LLM:     SubstrateAPINone,
		Embed:   SubstrateAPINone,
		Ranker:  SubstrateAPINone,
	}
}

// TeamSubstrate binds neon-style chunks with durable queue+cortex under dir (mixed).
// Hosted dense default = qdrant/env; API sides prefer hosted vendors.
func TeamSubstrate(dir string) SubstrateConfig {
	return SubstrateConfig{
		Profile: ProfileTeam,
		Dir:     dir,
		Chunks:  SubstrateChunksNeon,
		Queue:   SubstrateQueueSQLite, // always durable for team/hosted
		Cortex:  SubstrateCortexFS,
		Dense:   SubstrateDenseQdrant,
		LLM:     SubstrateAPIHosted,
		Embed:   SubstrateAPIHosted,
		Ranker:  SubstrateAPIHosted,
	}
}

// SubstrateFromEnv reads OUROBOROS_BRAIN_* substrate knobs.
//
//	OUROBOROS_BRAIN_PROFILE=solo|team|bench
//	OUROBOROS_BRAIN_DIR=/path
//	OUROBOROS_BRAIN_QUEUE=sqlite|postgres|memory|ephemeral|none
//	OUROBOROS_BRAIN_QUEUE_DSN=postgres://…   (queue=postgres; else DATABASE_URL)
//	OUROBOROS_BRAIN_DENSE=none|env|qdrant|sqlite|postgres|faiss|memory
//	OUROBOROS_BRAIN_DENSE_DSN=…  OUROBOROS_BRAIN_DENSE_URL=http://faiss-sidecar
//	OUROBOROS_BRAIN_DENSE_SEARCH_MODE=auto|exact|ann (local HNSW only)
//	OUROBOROS_BRAIN_LLM|EMBED|RANKER=hosted|mlx|none
//	OUROBOROS_BRAIN_MLX_BASE_URL=http://127.0.0.1:8080/v1
//	OUROBOROS_BRAIN_WORKERS=N   local burst/gardener concurrency
//	OUROBOROS_BRAIN_GARDENER_AUTO=1
func SubstrateFromEnv() SubstrateConfig {
	cfg := SubstrateConfig{
		Profile:         strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_PROFILE"))),
		Dir:             strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_DIR")),
		Chunks:          strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_CHUNKS"))),
		Queue:           strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_QUEUE"))),
		QueuePath:       strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_QUEUE_PATH")),
		Cortex:          strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_CORTEX"))),
		CortexPath:      strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_CORTEX_PATH")),
		Dense:           strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_DENSE"))),
		DenseDSN:        strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_DENSE_DSN")),
		DenseURL:        strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_DENSE_URL")),
		DenseSearchMode: strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_DENSE_SEARCH_MODE"))),
		LLM:             strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_LLM"))),
		Embed:           strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_EMBED"))),
		Ranker:          strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_RANKER"))),
	}
	// Queue DSN for postgres (dedicated env or shared DATABASE_URL / NEON).
	if cfg.QueuePath == "" && cfg.Queue == SubstrateQueuePostgres {
		cfg.QueuePath = firstNonEmpty(
			os.Getenv("OUROBOROS_BRAIN_QUEUE_DSN"),
			os.Getenv("DATABASE_URL"),
			os.Getenv("NEON_DATABASE_URL"),
		)
	}
	if os.Getenv("OUROBOROS_BRAIN_GARDENER_AUTO") == "1" {
		cfg.AutoGardener = true
	}
	if cfg.QueuePath == "" {
		if env := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_GARDENER_DB")); env != "" {
			cfg.QueuePath = env
			if cfg.Queue == "" {
				cfg.Queue = SubstrateQueueSQLite
			}
		}
	}
	return cfg.withDefaults()
}

func (cfg SubstrateConfig) withDefaults() SubstrateConfig {
	if strings.TrimSpace(cfg.DenseSearchMode) == "" {
		cfg.DenseSearchMode = strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_DENSE_SEARCH_MODE")))
	}
	p := cfg.Profile
	if p == "" {
		if cfg.Dir != "" {
			p = ProfileSolo
		}
	}
	// Normalize dense aliases (bindDense implements postgres/faiss).
	switch cfg.Dense {
	case SubstrateDenseEnv:
		cfg.Dense = SubstrateDenseQdrant
	case "pgvector":
		cfg.Dense = SubstrateDensePostgres
	}
	switch p {
	case ProfileSolo:
		if cfg.Chunks == "" {
			cfg.Chunks = SubstrateChunksFS
		}
		if cfg.Queue == "" {
			cfg.Queue = SubstrateQueueSQLite
		}
		if cfg.Cortex == "" {
			cfg.Cortex = SubstrateCortexFS
		}
		if cfg.Dense == "" {
			cfg.Dense = SubstrateDenseSQLite
		}
	case ProfileTeam:
		if cfg.Chunks == "" {
			cfg.Chunks = SubstrateChunksNeon
		}
		if cfg.Queue == "" || cfg.Queue == SubstrateQueueEphemeral {
			// Hosted residual: never process-only queue.
			cfg.Queue = SubstrateQueueSQLite
		}
		if cfg.Cortex == "" {
			cfg.Cortex = SubstrateCortexFS
		}
		if cfg.Dense == "" {
			cfg.Dense = SubstrateDenseQdrant
		}
	case ProfileBench:
		if cfg.Chunks == "" {
			cfg.Chunks = SubstrateChunksNeon
		}
		if cfg.Queue == "" || cfg.Queue == SubstrateQueueEphemeral {
			cfg.Queue = SubstrateQueueSQLite
		}
		if cfg.Cortex == "" {
			cfg.Cortex = SubstrateCortexFS
		}
		if cfg.Dense == "" {
			cfg.Dense = SubstrateDenseQdrant
		}
	default:
		if cfg.Dir != "" {
			if cfg.Queue == "" {
				cfg.Queue = SubstrateQueueSQLite
			}
			if cfg.Cortex == "" {
				cfg.Cortex = SubstrateCortexFS
			}
			if cfg.Dense == "" {
				cfg.Dense = SubstrateDenseSQLite
			}
		}
		if cfg.Chunks == "" {
			cfg.Chunks = SubstrateChunksFS
		}
		if cfg.Dense == "" {
			cfg.Dense = SubstrateDenseNone
		}
	}
	// memory queue + durable root → durable sqlite (hosted/residual rule).
	if cfg.Queue == SubstrateQueueMemory {
		if cfg.Dir != "" || cfg.QueuePath != "" {
			cfg.queueDurableAlias = true
			cfg.Queue = SubstrateQueueSQLite
		}
	}
	// API substrates: resolve empty to hosted-prefer.
	cfg.LLM = resolveAPISubstrate(cfg.LLM, "OUROBOROS_BRAIN_LLM")
	cfg.Embed = resolveAPISubstrate(cfg.Embed, "OUROBOROS_BRAIN_EMBED")
	cfg.Ranker = resolveAPISubstrate(cfg.Ranker, "OUROBOROS_BRAIN_RANKER")
	// Solo offline: if no remote keys and not explicit mlx, keep none.
	if p == ProfileSolo || p == "" {
		if cfg.LLM == SubstrateAPIHosted && os.Getenv("OPENAI_API_KEY") == "" &&
			os.Getenv("OPENROUTER_API_KEY") == "" && os.Getenv("GROQ_API_KEY") == "" {
			cfg.LLM = SubstrateAPINone
		}
		if cfg.Embed == SubstrateAPIHosted && os.Getenv("COHERE_API_KEY") == "" &&
			os.Getenv("CO_API_KEY") == "" {
			cfg.Embed = SubstrateAPINone
		}
		if cfg.Ranker == SubstrateAPIHosted && os.Getenv("ZEROENTROPY_API_KEY") == "" &&
			os.Getenv("ZE_API_KEY") == "" {
			cfg.Ranker = SubstrateAPINone
		}
	}
	cfg.Profile = p
	return cfg
}

// ApplySubstrates binds queue + cortex + dense (+ records API substrate choices).
// Chunks are assumed already chosen by the open helper; this never forks ask.
func ApplySubstrates(c *Client, cfg SubstrateConfig) error {
	if c == nil {
		return fmt.Errorf("hosted: nil client")
	}
	cfg = cfg.withDefaults()
	if mode := strings.ToLower(strings.TrimSpace(cfg.DenseSearchMode)); mode != "" &&
		mode != string(dense.SearchModeAuto) && mode != string(dense.SearchModeExact) && mode != string(dense.SearchModeANN) {
		return fmt.Errorf("hosted: invalid dense search mode %q (want auto|exact|ann)", cfg.DenseSearchMode)
	}
	c.substrates = cfg

	if err := bindQueue(c, cfg); err != nil {
		return err
	}
	if err := bindCortex(c, cfg); err != nil {
		return err
	}
	if err := bindDense(c, cfg); err != nil {
		return err
	}
	if cfg.AutoGardener && c.gardenerQ != nil {
		c.startAutoGardener()
	}
	return nil
}

func bindQueue(c *Client, cfg SubstrateConfig) error {
	switch cfg.Queue {
	case "", SubstrateQueueNone:
		// Team/bench with Dir must not silently run without a durable queue.
		if (cfg.Profile == ProfileTeam || cfg.Profile == ProfileBench) && cfg.Dir != "" {
			cfg.Queue = SubstrateQueueSQLite
			return bindQueue(c, cfg)
		}
		return nil
	case SubstrateQueueEphemeral:
		if c.gardenerQ != nil {
			if _, ok := c.gardenerQ.(*gardener.MemoryQueue); ok {
				return nil
			}
			if cl := c.gardenerCloser; cl != nil {
				_ = cl.Close()
				c.gardenerCloser = nil
			}
			c.gardenerQ = nil
		}
		c.AttachGardenerQueue(&gardener.MemoryQueue{})
		c.substrates.Queue = SubstrateQueueEphemeral
		return nil
	case SubstrateQueueMemory:
		// Without Dir: process-local (legacy tests). With Dir: coerced in withDefaults.
		if c.gardenerQ != nil {
			if _, ok := c.gardenerQ.(*gardener.MemoryQueue); ok {
				return nil
			}
			if cl := c.gardenerCloser; cl != nil {
				_ = cl.Close()
				c.gardenerCloser = nil
			}
			c.gardenerQ = nil
		}
		c.AttachGardenerQueue(&gardener.MemoryQueue{})
		return nil
	case SubstrateQueueSQLite:
		path := cfg.QueuePath
		if path == "" {
			if env := strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_GARDENER_DB")); env != "" {
				path = env
			}
		}
		if path == "" {
			dir := cfg.Dir
			if dir == "" {
				dir = c.LocalDir()
			}
			if dir == "" {
				return fmt.Errorf("hosted: sqlite queue requires Dir or QueuePath")
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			path = filepath.Join(dir, "gardener.db")
		}
		if c.gardenerQ != nil {
			if sq, ok := c.gardenerQ.(*gardener.SQLiteQueue); ok {
				if sq.Path() == path {
					return nil
				}
			}
			if cl := c.gardenerCloser; cl != nil {
				_ = cl.Close()
				c.gardenerCloser = nil
			}
			c.gardenerQ = nil
		}
		q, err := gardener.OpenSQLiteQueue(path)
		if err != nil {
			return err
		}
		c.AttachGardenerQueue(q)
		c.substrates.Queue = SubstrateQueueSQLite
		c.substrates.QueuePath = path
		return nil
	case SubstrateQueuePostgres:
		dsn := cfg.QueuePath
		if dsn == "" {
			dsn = firstNonEmpty(
				os.Getenv("OUROBOROS_BRAIN_QUEUE_DSN"),
				os.Getenv("DATABASE_URL"),
				os.Getenv("NEON_DATABASE_URL"),
			)
		}
		if dsn == "" {
			return fmt.Errorf("hosted: queue=postgres requires OUROBOROS_BRAIN_QUEUE_DSN or DATABASE_URL")
		}
		if c.gardenerQ != nil {
			if cl := c.gardenerCloser; cl != nil {
				_ = cl.Close()
				c.gardenerCloser = nil
			}
			c.gardenerQ = nil
		}
		q, err := gardener.OpenPostgresQueue(dsn)
		if err != nil {
			return err
		}
		c.AttachGardenerQueue(q)
		c.substrates.Queue = SubstrateQueuePostgres
		c.substrates.QueuePath = dsn
		return nil
	default:
		return fmt.Errorf("hosted: unknown queue substrate %q", cfg.Queue)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func bindCortex(c *Client, cfg SubstrateConfig) error {
	switch cfg.Cortex {
	case "", SubstrateCortexNone:
		return nil
	case SubstrateCortexFS, SubstrateCortexMemory:
		if c.Mem != nil {
			return nil
		}
		path := cfg.CortexPath
		if path == "" {
			path = cfg.Dir
		}
		if path == "" {
			path = c.LocalDir()
		}
		if path == "" {
			path = filepath.Join(os.TempDir(), "ouroboros-cortex-"+sanitizeBrainID(c.cfg.BrainID))
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		st, err := memory.Open(path)
		if err != nil {
			return err
		}
		c.Mem = st
		return nil
	default:
		return fmt.Errorf("hosted: unknown cortex substrate %q", cfg.Cortex)
	}
}

func bindDense(c *Client, cfg SubstrateConfig) error {
	// Close prior local dense if rebinding.
	if c.localDense != nil {
		switch cfg.Dense {
		case SubstrateDenseSQLite, SubstrateDenseMemory:
			// keep if same mode
		default:
			_ = c.localDense.Close()
			c.localDense = nil
		}
	}
	switch cfg.Dense {
	case "", SubstrateDenseNone:
		if c.db == nil {
			c.cfg.DenseLimit = 0
		}
		return nil
	case SubstrateDenseQdrant, SubstrateDenseEnv:
		// Residual dense=qdrant uses HTTP Qdrant as denseBackend (not path2-only).
		url := strings.TrimSpace(os.Getenv("QDRANT_URL"))
		key := strings.TrimSpace(os.Getenv("QDRANT_API_KEY"))
		if url == "" || key == "" {
			return fmt.Errorf("hosted: dense=qdrant requires QDRANT_URL and QDRANT_API_KEY")
		}
		c.cfg.QdrantURL = strings.TrimRight(url, "/")
		c.cfg.QdrantAPIKey = key
		if c.cfg.ChunkCollection == "" {
			c.cfg.ChunkCollection = envOr("OUROBOROS_ERB_QDRANT_CHUNK_COLLECTION", "product_chunk_vectors")
		}
		if c.cfg.ChunkVectorName == "" {
			c.cfg.ChunkVectorName = envOr("OUROBOROS_ERB_QDRANT_VECTOR_NAME", "chunk")
		}
		if c.cfg.DenseLimit <= 0 {
			c.cfg.DenseLimit = 20
		}
		c.localDense = &residualQdrantDense{cfg: c.cfg}
		return nil
	case SubstrateDenseSQLite:
		dir := cfg.Dir
		if dir == "" {
			dir = c.LocalDir()
		}
		if dir == "" {
			return fmt.Errorf("hosted: dense=sqlite requires Dir")
		}
		if c.localDense != nil {
			return nil
		}
		ld, err := openLocalDense(dir, c.cfg.BrainID, denseSearchMode(cfg.DenseSearchMode))
		if err != nil {
			return err
		}
		c.localDense = ld
		if c.cfg.DenseLimit <= 0 {
			c.cfg.DenseLimit = 20
		}
		return nil
	case SubstrateDensePostgres, "pgvector":
		// pgvector is the operator name; we store BYTEA + app cosine (no extension required).
		return bindDensePostgres(c, cfg)
	case SubstrateDenseFAISS:
		url := cfg.DenseURL
		if url == "" {
			url = strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_DENSE_URL"))
		}
		if c.localDense != nil {
			_ = c.localDense.Close()
			c.localDense = nil
		}
		if url != "" {
			// HTTP FAISS/usearch BYOC sidecar.
			c.localDense = openFAISSDense(url)
			c.faissURL = strings.TrimRight(url, "/")
		} else {
			// In-process pure-Go HNSW (FAISS-class local ANN, no CGo required).
			dir := cfg.Dir
			if dir == "" {
				dir = c.LocalDir()
			}
			if dir == "" {
				return fmt.Errorf("hosted: dense=faiss requires Dir (in-process) or OUROBOROS_BRAIN_DENSE_URL")
			}
			hd, err := openHNSWDense(dir, c.cfg.BrainID, denseSearchMode(cfg.DenseSearchMode))
			if err != nil {
				return err
			}
			c.localDense = hd
		}
		c.substrates.Dense = SubstrateDenseFAISS
		if c.cfg.DenseLimit <= 0 {
			c.cfg.DenseLimit = 20
		}
		return nil
	case SubstrateDenseMemory:
		// In-process only: open sqlite under temp if no dir (tests).
		dir := cfg.Dir
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "ouroboros-dense-"+sanitizeBrainID(c.cfg.BrainID))
		}
		if c.localDense != nil {
			return nil
		}
		ld, err := openLocalDense(dir, c.cfg.BrainID, denseSearchMode(cfg.DenseSearchMode))
		if err != nil {
			return err
		}
		c.localDense = ld
		if c.cfg.DenseLimit <= 0 {
			c.cfg.DenseLimit = 20
		}
		return nil
	default:
		return fmt.Errorf("hosted: unknown dense substrate %q", cfg.Dense)
	}
}

func bindDensePostgres(c *Client, cfg SubstrateConfig) error {
	dsn := cfg.DenseDSN
	if dsn == "" {
		dsn = firstNonEmpty(
			os.Getenv("OUROBOROS_BRAIN_DENSE_DSN"),
			cfg.QueuePath,
			os.Getenv("OUROBOROS_BRAIN_QUEUE_DSN"),
			os.Getenv("DATABASE_URL"),
			os.Getenv("NEON_DATABASE_URL"),
		)
	}
	if dsn == "" {
		return fmt.Errorf("hosted: dense=postgres requires DSN (OUROBOROS_BRAIN_DENSE_DSN or DATABASE_URL)")
	}
	if c.localDense != nil {
		_ = c.localDense.Close()
		c.localDense = nil
	}
	pd, err := openPostgresDense(dsn, c.cfg.BrainID)
	if err != nil {
		return err
	}
	c.localDense = pd
	c.substrates.Dense = SubstrateDensePostgres
	if c.cfg.DenseLimit <= 0 {
		c.cfg.DenseLimit = 20
	}
	return nil
}

func sanitizeBrainID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// SubstrateReport returns the bound module map for diagnostics / CLI.
func (c *Client) SubstrateReport() map[string]string {
	out := map[string]string{
		"chunks": "none",
		"queue":  "none",
		"cortex": "none",
		"dense":  "none",
		"llm":    "none",
		"embed":  "none",
		"ranker": "none",
	}
	if c == nil {
		return out
	}
	out["chunks"] = c.StoreKind()
	if c.substrates.Profile != "" {
		out["profile"] = c.substrates.Profile
	}
	if c.substrates.Dir != "" {
		out["dir"] = c.substrates.Dir
	}
	if c.gardenerQ != nil {
		if c.substrates.Queue != "" {
			out["queue"] = c.substrates.Queue
		} else {
			out["queue"] = "bound"
		}
		if c.substrates.QueuePath != "" {
			out["queue_path"] = c.substrates.QueuePath
		}
		if c.substrates.queueDurableAlias {
			out["queue_alias"] = "memory_to_sqlite"
		}
		switch c.gardenerQ.(type) {
		case *gardener.SQLiteQueue, *gardener.PostgresQueue:
			out["queue_durable"] = "true"
		default:
			out["queue_durable"] = "false"
		}
		if c.substrates.Workers > 0 {
			out["workers"] = fmt.Sprintf("%d", c.substrates.Workers)
		}
	}
	if c.Mem != nil {
		if c.substrates.Cortex != "" {
			out["cortex"] = c.substrates.Cortex
		} else {
			out["cortex"] = "bound"
		}
		if c.Mem.Dir() != "" {
			out["cortex_path"] = c.Mem.Dir()
		}
	}
	if c.substrates.Dense != "" {
		out["dense"] = c.substrates.Dense
	} else if c.localDense != nil {
		out["dense"] = SubstrateDenseSQLite
	} else if !c.productOwned && (c.cfg.DenseLimit > 0 || c.db != nil) {
		// path2 bench/eval ANN only — not residual dense substrate ownership.
		out["dense"] = "path2_qdrant"
		out["dense_plane"] = "path2_eval"
	}
	// Residual graph ownership: memory edges are adjacency truth; structureIndex is projection.
	if c.productOwned {
		out["graph_truth"] = "memory_edges"
		out["structure_index"] = "projection"
		out["plane"] = "residual"
	} else if c.db != nil {
		out["plane"] = "path2_eval"
	}
	if c.substrates.LLM != "" {
		out["llm"] = c.substrates.LLM
	}
	if c.substrates.Embed != "" {
		out["embed"] = c.substrates.Embed
	}
	if c.substrates.Ranker != "" {
		out["ranker"] = c.substrates.Ranker
	}
	return out
}

// OpenResidual opens residual Client from substrate config (chunks + bind queue/cortex/dense).
func OpenResidual(brainID string, cfg SubstrateConfig) (*Client, error) {
	cfg = cfg.withDefaults()
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		brainID = "local"
	}
	switch cfg.Chunks {
	case SubstrateChunksFS, "":
		if cfg.Dir == "" {
			return nil, fmt.Errorf("hosted: OpenResidual chunks=fs requires Dir")
		}
		c, err := OpenLocal(cfg.Dir, brainID)
		if err != nil {
			return nil, err
		}
		if err := ApplySubstrates(c, cfg); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	case SubstrateChunksMemory:
		c := OpenMemory(brainID)
		if err := ApplySubstrates(c, cfg); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	case SubstrateChunksNeon:
		neonURL := strings.TrimSpace(os.Getenv("NEON_DATABASE_URL"))
		if neonURL == "" {
			neonURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
		}
		if neonURL == "" {
			return nil, fmt.Errorf("hosted: OpenResidual chunks=neon requires NEON_DATABASE_URL")
		}
		c, err := Open(Config{
			NeonDatabaseURL: neonURL,
			BrainID:         brainID,
			LexicalLimit:    30,
			TopK:            8,
			MaxPassageChars: 2000,
			RRFK:            60,
			PoolK:           40,
			DenseLimit:      30,
		})
		if err != nil {
			return nil, err
		}
		// Residual product neon must NOT use SMF path2 retrieve (product_chunk_metadata).
		c.productOwned = true
		if c.store == nil && c.db != nil {
			c.store = &neonChunkStore{db: c.db}
		}
		if err := ApplySubstrates(c, cfg); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("hosted: unknown chunks substrate %q", cfg.Chunks)
	}
}

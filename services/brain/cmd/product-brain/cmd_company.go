// product-brain company-doc commands: create / ingest / ask / hosted variants.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsec"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/tenant"
)

func runCreate(args []string) {
	// One residual pipeline; --backend selects chunks substrate (ADR 0024).
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	dir := fs.String("dir", "", "durable root for FS chunks/queue/cortex (solo default)")
	id := fs.String("brain-id", "local", "brain id")
	backend := fs.String("backend", "local", "chunks substrate: local|memory|neon (alias for --chunks)")
	profile := fs.String("profile", "", "substrate preset: solo|team|bench (ADR 0024)")
	queue := fs.String("queue", "", "queue substrate: sqlite|memory|none")
	cortex := fs.String("cortex", "", "cortex substrate: fs|memory|none")
	_ = fs.Parse(args)
	applyCLISubstrateEnv(*dir, *profile, *queue, *cortex, *backend)
	ctx := context.Background()
	switch strings.ToLower(*backend) {
	case "local", "fs", "":
		if *dir == "" {
			fatal("create: --dir required for chunks=fs/local")
		}
		c, err := hosted.CreateLocal(*dir, *id)
		if err != nil {
			// Open if already exists — still honor queue/cortex overrides.
			c, err = openCLIClient(*dir, *id)
			if err != nil {
				fatal(err.Error())
			}
		} else {
			// CreateLocal used solo defaults; re-apply CLI overrides.
			sub := hosted.SubstrateFromEnv()
			sub.Dir = *dir
			sub.Chunks = hosted.SubstrateChunksFS
			if err := hosted.ApplySubstrates(c, sub); err != nil {
				fatal(err.Error())
			}
		}
		defer c.Close()
		emitJSON(map[string]any{
			"brain_id": c.Config().BrainID, "generation_id": c.GenerationID(),
			"dir": *dir, "product_owned": true, "store": c.StoreKind(),
			"substrates": c.SubstrateReport(),
		})
	case "memory":
		cfg := hosted.SubstrateFromEnv()
		cfg.Chunks = hosted.SubstrateChunksMemory
		if *dir != "" {
			cfg.Dir = *dir
		}
		c, err := hosted.OpenMemoryWithSubstrates(*id, cfg)
		if err != nil {
			// Fall back to plain memory if no durable dir for queue/cortex.
			c = hosted.OpenMemory(*id)
		}
		if err := c.EnsureSchema(ctx); err != nil {
			fatal(err.Error())
		}
		defer c.Close()
		emitJSON(map[string]any{
			"brain_id": c.Config().BrainID, "product_owned": true,
			"store": c.StoreKind(), "substrates": c.SubstrateReport(),
		})
	case "neon":
		cfg := hosted.SubstrateFromEnv()
		cfg.Chunks = hosted.SubstrateChunksNeon
		if *dir != "" {
			cfg.Dir = *dir
		}
		c, err := hosted.OpenResidual(*id, cfg)
		if err != nil {
			fatal(err.Error())
		}
		defer c.Close()
		if err := c.EnsureSchema(ctx); err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{
			"brain_id": c.Config().BrainID, "product_owned": true,
			"store": c.StoreKind(), "schema": "product_chunk_metadata",
			"substrates": c.SubstrateReport(),
		})
	default:
		fatal("create: unknown --backend " + *backend)
	}
}

// applyCLISubstrateEnv exports CLI substrate flags into env for SubstrateFromEnv.
// chunks is fs|local|memory|neon (empty leaves env/default).
func applyCLISubstrateEnv(dir, profile, queue, cortex, chunks string) {
	if dir != "" {
		_ = os.Setenv("OUROBOROS_BRAIN_DIR", dir)
	}
	if profile != "" {
		_ = os.Setenv("OUROBOROS_BRAIN_PROFILE", profile)
	}
	if queue != "" {
		_ = os.Setenv("OUROBOROS_BRAIN_QUEUE", queue)
	}
	if cortex != "" {
		_ = os.Setenv("OUROBOROS_BRAIN_CORTEX", cortex)
	}
	if chunks != "" {
		ch := strings.ToLower(strings.TrimSpace(chunks))
		switch ch {
		case "local", "fs":
			ch = hosted.SubstrateChunksFS
		case "memory", "neon":
			// keep
		}
		_ = os.Setenv("OUROBOROS_BRAIN_CHUNKS", ch)
	}
}

// openCLIClient opens residual Client via SubstrateFromEnv / OpenResidual (ADR 0024).
// Prefer this over raw OpenLocal so --queue/--cortex/--chunks/--profile apply on
// ingest/ask/gardener, not only create.
func openCLIClient(dir, brainID string) (*hosted.Client, error) {
	cfg := hosted.SubstrateFromEnv()
	if dir != "" {
		cfg.Dir = dir
	}
	if cfg.Dir == "" && cfg.Chunks != hosted.SubstrateChunksMemory && cfg.Chunks != hosted.SubstrateChunksNeon {
		return nil, fmt.Errorf("open: --dir required for chunks=%s", cfg.Chunks)
	}
	// FS default when dir set and chunks unset after defaults.
	if cfg.Chunks == "" || cfg.Chunks == hosted.SubstrateChunksFS {
		if cfg.Dir == "" {
			cfg.Dir = dir
		}
	}
	return hosted.OpenResidual(brainID, cfg)
}

func runCreateHosted(args []string) {
	// Deprecated alias → create --backend memory|neon
	fs := flag.NewFlagSet("create-hosted", flag.ExitOnError)
	id := fs.String("brain-id", "product-local", "brain id")
	neon := fs.Bool("neon", false, "use Neon when NEON_DATABASE_URL is set (else memory)")
	_ = fs.Parse(args)
	redispatch := []string{"create", "--brain-id", *id, "--backend", "memory"}
	if *neon {
		redispatch = []string{"create", "--brain-id", *id, "--backend", "neon"}
	}
	// Re-dispatch via recursive main is awkward; inline:
	os.Args = append([]string{os.Args[0]}, redispatch...)
	// fallthrough not available — call create path by re-executing switch is hard.
	// Inline same as create neon/memory:
	ctx := context.Background()
	if *neon {
		cfg, err := hosted.FromEnv()
		if err != nil {
			neonURL := strings.TrimSpace(os.Getenv("NEON_DATABASE_URL"))
			if neonURL == "" {
				neonURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
			}
			cfg = hosted.Config{NeonDatabaseURL: neonURL, BrainID: *id, LexicalLimit: 30, TopK: 8, MaxPassageChars: 2000, RRFK: 60, PoolK: 40}
		}
		cfg.BrainID = *id
		c, err := hosted.Open(cfg)
		if err != nil {
			fatal(err.Error())
		}
		defer c.Close()
		if err := c.EnsureSchema(ctx); err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{"brain_id": cfg.BrainID, "product_owned": true, "store": "product_neon", "deprecated": "create-hosted"})
		return
	}
	c := hosted.OpenMemory(*id)
	_ = c.EnsureSchema(ctx)
	emitJSON(map[string]any{"brain_id": c.Config().BrainID, "product_owned": true, "store": "memory", "deprecated": "create-hosted"})
}

func runHostedBurst(args []string) {
	fs := flag.NewFlagSet("hosted-burst", flag.ExitOnError)
	id := fs.String("brain-id", "product-local", "brain id")
	jsonl := fs.String("jsonl", "", "chunk jsonl (document_id/chunk_id/text/source_uri)")
	workers := fs.Int("workers", 4, "burst workers")
	neon := fs.Bool("neon", false, "use Neon product_chunk_metadata (NEON_DATABASE_URL or DATABASE_URL)")
	_ = fs.Parse(args)
	if *jsonl == "" {
		fatal("hosted-burst: --jsonl required")
	}
	chunks, err := loadChunks(*jsonl)
	if err != nil {
		fatal(err.Error())
	}
	ctx := context.Background()
	var c *hosted.Client
	backend := "memory"
	neonURL := strings.TrimSpace(os.Getenv("NEON_DATABASE_URL"))
	if neonURL == "" {
		neonURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if *neon {
		if neonURL == "" {
			fatal("hosted-burst --neon requires NEON_DATABASE_URL or DATABASE_URL")
		}
		cfg, err := hosted.FromEnv()
		if err != nil {
			cfg = hosted.Config{
				NeonDatabaseURL: neonURL,
				BrainID:         *id,
				LexicalLimit:    30,
				TopK:            8,
				MaxPassageChars: 2000,
				RRFK:            60,
				PoolK:           40,
			}
		}
		if *id != "" {
			cfg.BrainID = *id
		}
		if cfg.NeonDatabaseURL == "" {
			cfg.NeonDatabaseURL = neonURL
		}
		c, err = hosted.Open(cfg)
		if err != nil {
			fatal(err.Error())
		}
		backend = "neon"
	} else {
		c = hosted.OpenMemory(*id)
	}
	defer c.Close()
	if err := c.EnsureSchema(ctx); err != nil {
		fatal(err.Error())
	}
	t0 := time.Now()
	res, err := c.BurstUpsert(ctx, *id, chunks, *workers)
	if err != nil {
		fatal(err.Error())
	}
	out := map[string]any{
		"result": res, "backend": backend, "brain_id": *id,
		"burst_ms": time.Since(t0).Milliseconds(), "product_owned": true,
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func runIngest(args []string) {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	dir := fs.String("dir", "", "brain directory (queue/cortex root; FS chunks when --chunks fs)")
	id := fs.String("brain-id", "local", "brain id")
	jsonl := fs.String("jsonl", "", "docs jsonl")
	workers := fs.Int("workers", 0, "burst workers (0 = OUROBOROS_BRAIN_WORKERS or GOMAXPROCS local fleet)")
	delta := fs.Bool("delta", false, "continual delta mode")
	subProfile := fs.String("substrate-profile", "", "substrate preset solo|team|bench")
	queue := fs.String("queue", "", "queue substrate sqlite|memory|none")
	cortex := fs.String("cortex", "", "cortex substrate fs|memory|none")
	chunks := fs.String("chunks", "", "chunks substrate fs|memory|neon (alias --backend)")
	backend := fs.String("backend", "", "alias for --chunks")
	_ = fs.Parse(args)
	ch := *chunks
	if ch == "" {
		ch = *backend
	}
	applyCLISubstrateEnv(*dir, *subProfile, *queue, *cortex, ch)
	if *jsonl == "" {
		fatal("ingest: --jsonl required")
	}
	// memory/neon chunks may omit --dir only when queue/cortex also non-durable.
	if *dir == "" && ch != "memory" && ch != hosted.SubstrateChunksMemory {
		fatal("ingest: --dir required (or --chunks memory with in-process queue)")
	}
	c, err := openCLIClient(*dir, *id)
	if err != nil {
		fatal(err.Error())
	}
	defer c.Close()
	docs, err := loadDocs(*jsonl)
	if err != nil {
		fatal(err.Error())
	}
	ctx := context.Background()
	if err := c.EnsureSchema(ctx); err != nil {
		fatal(err.Error())
	}
	if *delta {
		deltaRec, err := c.ContinualDeltaLocal(ctx, docs)
		if err != nil {
			fatal(err.Error())
		}
		emitJSON(map[string]any{
			"mode": "delta", "generation_id": deltaRec.GenerationID, "product_owned": true,
			"brain_id": c.Config().BrainID, "ingested": deltaRec.Ingested, "store": c.StoreKind(),
			"substrates": c.SubstrateReport(),
		})
		return
	}
	// BurstIngestLocal works for any ChunkStore (FS durable or memory).
	rec, err := c.BurstIngestLocal(ctx, docs, *workers)
	if err != nil {
		fatal(err.Error())
	}
	emitJSON(map[string]any{
		"generation_id": rec.GenerationID,
		"ingested":      rec.Ingested,
		"upserted":      rec.Upserted,
		"workers":       rec.Workers,
		"product_owned": true,
		"brain_id":      c.Config().BrainID,
		"mode":          "burst",
		"store":         c.StoreKind(),
		"receipts":      rec.Receipts,
		"substrates":    c.SubstrateReport(),
		"enrich_jobs":   rec.EnrichJobs,
	})
}

func runAsk(args []string) {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	dir := fs.String("dir", "", "brain directory")
	id := fs.String("brain-id", "local", "brain id")
	q := fs.String("q", "", "question")
	topK := fs.Int("top-k", 6, "window")
	session := fs.String("session", "", "session id for product chat turns")
	profile := fs.String("profile", "", "single_user|multi_principal (Phase 2)")
	principal := fs.String("principal", "", "acting principal (multi_principal)")
	// Phase 4: tenant-scoped ask (fail-closed path isolation).
	tenantID := fs.String("tenant", "", "tenant id (requires --tenant-root)")
	tenantRoot := fs.String("tenant-root", "", "tenant registry root (or OUROBOROS_TENANT_ROOT)")
	asOf := fs.String("as-of", "", "bi-temporal valid-at (RFC3339); sets OUROBOROS_BRAIN_AS_OF")
	knownAt := fs.String("known-at", "", "bi-temporal known-at (RFC3339); sets OUROBOROS_BRAIN_KNOWN_AT")
	subProfile := fs.String("substrate-profile", "", "substrate preset solo|team|bench (not security --profile)")
	queue := fs.String("queue", "", "queue substrate sqlite|memory|none")
	cortex := fs.String("cortex", "", "cortex substrate fs|memory|none")
	chunks := fs.String("chunks", "", "chunks substrate fs|memory|neon")
	backend := fs.String("backend", "", "alias for --chunks")
	_ = fs.Parse(args)
	ch := *chunks
	if ch == "" {
		ch = *backend
	}
	applyCLISubstrateEnv(*dir, *subProfile, *queue, *cortex, ch)
	if *q == "" {
		buf, _ := os.ReadFile("/dev/stdin")
		*q = strings.TrimSpace(string(buf))
	}
	if *q == "" {
		fatal("ask: --q or stdin required")
	}
	if *asOf != "" {
		_ = os.Setenv("OUROBOROS_BRAIN_AS_OF", *asOf)
	}
	if *knownAt != "" {
		_ = os.Setenv("OUROBOROS_BRAIN_KNOWN_AT", *knownAt)
	}
	// Resolve brain dir: either --dir or tenant-scoped brain path.
	brainDir := *dir
	if *tenantID != "" {
		rpath := *tenantRoot
		if rpath == "" {
			rpath = os.Getenv("OUROBOROS_TENANT_ROOT")
		}
		if rpath == "" {
			fatal("ask: --tenant requires --tenant-root or OUROBOROS_TENANT_ROOT")
		}
		if *id == "" || *id == "local" {
			fatal("ask: --tenant requires --brain-id")
		}
		reg := &tenant.Registry{Root: rpath}
		if _, err := reg.Status(*tenantID); err != nil {
			fatal("ask: " + err.Error())
		}
		brainDir = reg.BrainDir(*tenantID, *id)
		// Fail-closed: refuse dirs outside tenant root (TEN-005).
		if err := reg.AuthorizeBrainPath(*tenantID, brainDir); err != nil {
			// Emit JSON deny for product callers (not only stderr exit).
			emitJSON(map[string]any{
				"failure": "cross_tenant_denied", "search_mode": "tenant_isolation",
				"product_owned": true, "tenant": *tenantID, "brain_id": *id,
			})
			os.Exit(2)
		}
		// Optional attack: if --dir points at another tenant, deny.
		if *dir != "" {
			if err := reg.AuthorizeBrainPath(*tenantID, *dir); err != nil {
				emitJSON(map[string]any{
					"failure": "cross_tenant_denied", "search_mode": "tenant_isolation",
					"product_owned": true, "tenant": *tenantID,
				})
				os.Exit(2)
			}
			brainDir = *dir
		}
		// Tenant path is always FS chunks under registry.
		applyCLISubstrateEnv(brainDir, *subProfile, *queue, *cortex, "fs")
	} else if brainDir == "" && ch != "memory" && ch != hosted.SubstrateChunksMemory {
		fatal("ask: --dir required (or --tenant + --tenant-root + --brain-id, or --chunks memory)")
	}
	c, err := openCLIClient(brainDir, *id)
	if err != nil {
		fatal(err.Error())
	}
	defer c.Close()
	if *profile != "" || *principal != "" {
		secDir := brainDir
		if secDir == "" {
			secDir = c.LocalDir()
		}
		sec, _ := productsec.ContextFromBrain(secDir, *principal, productsec.ParseProfile(*profile))
		if *profile != "" {
			sec.Profile = productsec.ParseProfile(*profile)
		}
		if *principal != "" {
			sec.Principal = *principal
		}
		c.SetSecurity(sec)
	}
	sid := *session
	if sid == "" {
		sid = os.Getenv("OUROBOROS_BRAIN_SESSION_ID")
	}
	ans := c.AnswerOpts(context.Background(), hosted.AnswerOptions{
		Question:  *q,
		TopK:      *topK,
		SessionID: sid,
		// Mode from product UI via OUROBOROS_ERB_MODE (light|deep|research|bench).
		Mode:      os.Getenv("OUROBOROS_ERB_MODE"),
		Principal: *principal,
		Profile:   *profile,
	})
	if ans.RetrievalDiagnostics == nil {
		ans.RetrievalDiagnostics = map[string]any{}
	}
	ans.RetrievalDiagnostics["store"] = c.StoreKind()
	ans.RetrievalDiagnostics["generation_id"] = c.GenerationID()
	ans.RetrievalDiagnostics["substrates"] = c.SubstrateReport()
	if *tenantID != "" {
		ans.RetrievalDiagnostics["tenant"] = *tenantID
	}
	// Seal session turn when vault-capable (Phase 2 SEC-003).
	sealDir := brainDir
	if sealDir == "" {
		sealDir = c.LocalDir()
	}
	if sid != "" && ans.Failure == "" && sealDir != "" {
		owner := c.Security.Owner
		if owner == "" {
			owner = *id
		}
		_ = productsec.SealSession(sealDir, owner, sid, "user", *q)
		if ans.Answer != "" {
			_ = productsec.SealSession(sealDir, owner, sid, "assistant", ans.Answer)
		}
	}
	if sealDir != "" {
		_, _ = productsec.UpdateEvidenceDigest(sealDir)
	}
	_ = json.NewEncoder(os.Stdout).Encode(ans)
}

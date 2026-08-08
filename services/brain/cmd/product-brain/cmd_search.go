// product-brain unified search + hotlex projection.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsearch"
)

func runProjectHotlex(args []string) {
	fs := flag.NewFlagSet("project-hotlex", flag.ExitOnError)
	dir := fs.String("dir", "", "local brain dir (meta.json + chunks.jsonl)")
	id := fs.String("brain-id", "local", "brain id")
	out := fs.String("out", "", "output hotlex.gob path (default: <dir>/hotlex.gob)")
	jsonl := fs.String("jsonl", "", "optional chunk/doc jsonl instead of --dir")
	neon := fs.Bool("neon", false, "stream path2_chunk_metadata via NEON_DATABASE_URL")
	maxDocs := fs.Int("max-docs", 0, "cap rows (0=all; used with --neon/--jsonl)")
	textChars := fs.Int("text-chars", 800, "left(text) for tokenization when --neon")
	stripText := fs.Bool("strip-text", false, "drop body text after index (hydrate-by-id at query)")
	workers := fs.Int("workers", 8, "in-process parallel shard builders (--neon)")
	shards := fs.Int("shards", 0, "hash partitions (default = workers)")
	shardID := fs.Int("shard-id", -1, "build only this shard [0,shards); -1=all (Modal burst)")
	pageSize := fs.Int("page-size", 5000, "keyset page size per shard")
	mergeGobs := fs.String("merge", "", "comma-separated shard gob paths to merge into --out")
	generation := fs.String("generation-id", "", "generation identity pinned into the snapshot")
	format := fs.String("format", "hotlex2", "output format: hotlex2 or legacy-gob")
	rollbackGob := fs.String("rollback-gob", "", "optional gob-only rollback output (hotlex2 format only)")
	_ = fs.Parse(args)
	outPath := *out
	save := func(h *hosted.HotLex) (string, string) {
		writtenFormat, rollbackPath, err := saveProjectedHotLex(h, outPath, *format, *rollbackGob)
		if err != nil {
			fatal(err.Error())
		}
		return writtenFormat, rollbackPath
	}

	if *mergeGobs != "" {
		if outPath == "" {
			fatal("project-hotlex: --out required with --merge")
		}
		var parts []*hosted.HotLex
		for _, p := range strings.Split(*mergeGobs, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			h, err := hosted.LoadHotLexGob(p)
			if err != nil {
				fatal("merge load " + p + ": " + err.Error())
			}
			parts = append(parts, h)
		}
		brainID := *id
		if brainID == "" || brainID == "local" {
			if len(parts) > 0 && parts[0].BrainID != "" {
				brainID = strings.Split(parts[0].BrainID, "#")[0]
			} else {
				brainID = "full-bench-v2"
			}
		}
		merged, err := hosted.MergeHotLexShards(brainID, parts)
		if err != nil {
			fatal("merge scope: " + err.Error())
		}
		if *generation != "" && merged.Generation != "" && merged.Generation != *generation {
			fatal("merge generation does not match --generation-id")
		}
		if *generation != "" {
			merged.Generation = *generation
		}
		writtenFormat, rollbackPath := save(merged)
		emitJSON(map[string]any{
			"brain_id": brainID, "docs": merged.Len(), "out": outPath,
			"generation_id": merged.Generation, "merged_shards": len(parts),
			"product_owned": true, "index": "hot_lex", "mode": "merge",
			"format": writtenFormat, "rollback_gob": rollbackPath,
		})
		return
	}

	if *neon {
		if outPath == "" {
			fatal("project-hotlex: --out required with --neon")
		}
		dsn := strings.TrimSpace(os.Getenv("NEON_DATABASE_URL"))
		if dsn == "" {
			dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
		}
		if dsn == "" {
			fatal("project-hotlex --neon requires NEON_DATABASE_URL")
		}
		brainID := *id
		if brainID == "" || brainID == "local" {
			brainID = "full-bench-v2"
		}
		// Fast path: multi-worker keyset+hash shards (default timeout 1h).
		timeoutS := 3600
		if v := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_HOTLEX_PROJECT_TIMEOUT_S")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				timeoutS = n
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutS)*time.Second)
		defer cancel()
		sh := *shards
		if sh <= 0 {
			sh = *workers
		}
		res, err := hosted.ProjectHotLexFromDSNFast(ctx, dsn, hosted.ProjectOptions{
			BrainID:   brainID,
			MaxDocs:   *maxDocs,
			TextChars: *textChars,
			StripText: *stripText,
			Workers:   *workers,
			Shards:    sh,
			ShardID:   *shardID,
			PageSize:  *pageSize,
		})
		if err != nil {
			fatal(err.Error())
		}
		res.Index.Generation = *generation
		writtenFormat, rollbackPath := save(res.Index)
		emitJSON(map[string]any{
			"brain_id": brainID, "docs": res.Index.Len(), "out": outPath,
			"generation_id": res.Index.Generation,
			"product_owned": true, "index": "hot_lex", "source": "neon_path2_fast",
			"strip_text": *stripText, "text_chars": *textChars, "max_docs": *maxDocs,
			"workers": res.Workers, "shards": res.Shards, "shard_id": res.ShardID,
			"page_size": res.PageSize, "pages": res.PageCount, "bytes_est": res.BytesEst,
			"duration_ms": res.Duration.Milliseconds(), "rows": res.Rows,
			"format": writtenFormat, "rollback_gob": rollbackPath,
		})
		return
	}

	if *jsonl != "" {
		chunks, err := loadChunks(*jsonl)
		if err != nil {
			// try docs as single-chunk
			docs, err2 := loadDocs(*jsonl)
			if err2 != nil {
				fatal(err.Error())
			}
			chunks = hosted.DocumentsToChunks(docs)
		}
		if *maxDocs > 0 && len(chunks) > *maxDocs {
			chunks = chunks[:*maxDocs]
		}
		h := hosted.ProjectChunks(*id, chunks)
		h.Generation = *generation
		if *stripText {
			h.StripStoredText()
		}
		if outPath == "" {
			fatal("project-hotlex: --out required with --jsonl")
		}
		writtenFormat, rollbackPath := save(h)
		emitJSON(map[string]any{
			"brain_id": *id, "docs": h.Len(), "out": outPath, "product_owned": true, "index": "hot_lex",
			"generation_id": h.Generation, "strip_text": *stripText,
			"format": writtenFormat, "rollback_gob": rollbackPath,
		})
		return
	}
	if *dir == "" {
		fatal("project-hotlex: --dir, --jsonl, or --neon required")
	}
	c, err := hosted.OpenLocal(*dir, *id)
	if err != nil {
		fatal(err.Error())
	}
	defer c.Close()
	c.EnsureHotLex()
	h := c.HotLex()
	if h == nil || h.Len() == 0 {
		fatal("project-hotlex: empty index (ingest chunks first)")
	}
	if *generation != "" && h.Generation != "" && h.Generation != *generation {
		fatal("local generation does not match --generation-id")
	}
	if *generation != "" {
		h.Generation = *generation
	}
	if *stripText {
		h.StripStoredText()
	}
	if outPath == "" {
		outPath = filepath.Join(*dir, "hotlex.gob")
	}
	writtenFormat, rollbackPath := save(h)
	emitJSON(map[string]any{
		"brain_id": c.Config().BrainID, "docs": h.Len(), "out": outPath,
		"generation_id": c.GenerationID(), "product_owned": true, "index": "hot_lex",
		"strip_text": *stripText, "format": writtenFormat, "rollback_gob": rollbackPath,
	})
}

func saveProjectedHotLex(h *hosted.HotLex, path, format, rollbackPath string) (string, string, error) {
	format = strings.TrimSpace(strings.ToLower(format))
	rollbackPath = strings.TrimSpace(rollbackPath)
	switch format {
	case "hotlex2":
		if rollbackPath != "" {
			if err := h.SaveGobWithRollback(path, rollbackPath); err != nil {
				return "", "", err
			}
		} else if err := h.SaveGob(path); err != nil {
			return "", "", err
		}
		if rollbackPath == "" {
			candidate := hosted.LegacyRollbackPath(path)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				rollbackPath = candidate
			}
		}
		return string(hosted.HotLexFormatHOTLEX2), rollbackPath, nil
	case "legacy-gob":
		if rollbackPath != "" {
			return "", "", errors.New("project-hotlex: --rollback-gob is only valid with --format hotlex2")
		}
		if err := h.SaveLegacyGob(path); err != nil {
			return "", "", err
		}
		return string(hosted.HotLexFormatLegacyGob), "", nil
	default:
		return "", "", fmt.Errorf("project-hotlex: unsupported --format %q (want hotlex2 or legacy-gob)", format)
	}
}

func runUnifiedSearch(args []string, ask bool) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	profile := fs.String("profile", "auto", "code|code_exact|local|hosted|auto (memory=local alias)")
	q := fs.String("q", "", "question")
	topK := fs.Int("top-k", 8, "window")
	root := fs.String("root", "", "code root")
	dir := fs.String("dir", "", "memory brain dir")
	id := fs.String("brain-id", "local", "brain id")
	workers := fs.Int("workers", 4, "code crawl workers")
	hop := fs.Bool("symbol-hop", false, "enable stack-graph style symbol hop")
	_ = fs.Parse(args)
	if *q == "" {
		fatal("search: --q required")
	}
	req := productsearch.Request{
		Profile:   productsearch.Profile(*profile),
		Question:  *q,
		TopK:      *topK,
		CodeRoot:  *root,
		MemoryDir: *dir,
		BrainID:   *id,
		Workers:   *workers,
		SymbolHop: *hop,
		Hosted:    *profile == "hosted",
	}
	var res productsearch.Result
	if ask {
		res = productsearch.Ask(context.Background(), req)
	} else {
		res = productsearch.Search(context.Background(), req)
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

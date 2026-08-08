// product-brain code-* codecrawl operators.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/productsearch"
)

const codeIndexSummaryName = "code-index-summary.json"

func runCodeExact(args []string) {
	fs := flag.NewFlagSet("code-exact", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	q := fs.String("q", "", "symbol / identifier")
	kind := fs.String("kind", "any", "any|definition|reference|import")
	topK := fs.Int("top-k", 20, "max hits")
	_ = fs.Parse(args)
	if *root == "" || *q == "" {
		fatal("code-exact: --root and --q required")
	}
	res := productsearch.Search(context.Background(), productsearch.Request{
		Profile: productsearch.ProfileCodeExact, CodeRoot: *root, Question: *q,
		ExactKind: *kind, TopK: *topK,
	})
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

func runCodeIndex(args []string) {
	fs := flag.NewFlagSet("code-index", flag.ExitOnError)
	root := fs.String("root", "", "source tree root to crawl")
	workers := fs.Int("workers", 4, "crawl workers")
	dir := fs.String("dir", "", "output directory for summary + code-index.gob (default: --root)")
	force := fs.Bool("force", false, "full reindex (ignore durable gob)")
	_ = fs.Parse(args)
	if *root == "" {
		fatal("code-index: --root required")
	}
	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		fatal(err.Error())
	}
	outDir := *dir
	if outDir == "" {
		outDir = rootAbs
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err.Error())
	}
	gobPath := filepath.Join(outDir, codecrawl.DefaultIndexFile)
	idx, st, wrote, meta, err := codecrawl.OpenOrRefresh(rootAbs, gobPath, *workers, *force)
	if err != nil {
		fatal(err.Error())
	}
	_ = idx
	summary := map[string]any{
		"schema":           "product-brain.code-index-summary.v1",
		"root":             rootAbs,
		"workers":          st.Workers,
		"files_indexed":    st.FilesIndexed,
		"bytes_read":       st.BytesRead,
		"errors":           st.Errors,
		"changed":          st.Changed,
		"unchanged":        st.Unchanged,
		"skipped_by_stamp": st.SkippedByStamp,
		"hashed":           st.Hashed,
		"duration_ms":      st.Duration.Milliseconds(),
		"indexed_at":       meta.IndexedAt.Format(time.RFC3339Nano),
		"git_head":         meta.GitHead,
		"gob_path":         gobPath,
		"gob_rewritten":    wrote,
		"product_owned":    true,
		"search_backend":   "codecrawl",
		"delta":            st.Unchanged > 0 || st.Changed > 0,
	}
	path := filepath.Join(outDir, codeIndexSummaryName)
	raw, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		fatal(err.Error())
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		fatal(err.Error())
	}
	summary["summary_path"] = path
	_ = json.NewEncoder(os.Stdout).Encode(summary)
}

func runCodeSearch(args []string) {
	fs := flag.NewFlagSet("code-search", flag.ExitOnError)
	root := fs.String("root", "", "source tree root to crawl")
	indexCache := fs.String("index-cache", "", "dir or path to code-index-summary.json / code-index.gob")
	q := fs.String("q", "", "search query")
	workers := fs.Int("workers", 4, "crawl workers (delta refresh)")
	topK := fs.Int("top-k", 20, "max hits")
	noRefresh := fs.Bool("no-refresh", false, "load gob only; skip hash delta (stale OK)")
	force := fs.Bool("force-refresh", false, "full reindex before search")
	symbolHop := fs.Bool("symbol-hop", false, "expand via shared symbol names")
	_ = fs.Parse(args)
	if *q == "" {
		fatal("code-search: --q required")
	}
	crawlRoot := strings.TrimSpace(*root)
	cachePath := strings.TrimSpace(*indexCache)
	if crawlRoot == "" && cachePath == "" {
		fatal("code-search: --root or --index-cache required")
	}
	outDir := ""
	if cachePath != "" {
		// Directory containing gob/summary, or a file path.
		st, err := os.Stat(cachePath)
		if err == nil && st.IsDir() {
			outDir = cachePath
		} else if strings.HasSuffix(cachePath, codeIndexSummaryName) || strings.HasSuffix(cachePath, codecrawl.DefaultIndexFile) {
			outDir = filepath.Dir(cachePath)
		} else if err == nil {
			outDir = filepath.Dir(cachePath)
		} else {
			// missing path: treat as dir to create on refresh
			outDir = cachePath
		}
		// Resolve root from summary if needed.
		if crawlRoot == "" {
			raw, err := os.ReadFile(filepath.Join(outDir, codeIndexSummaryName))
			if err != nil {
				raw, err = os.ReadFile(cachePath)
			}
			if err == nil {
				var summary map[string]any
				if json.Unmarshal(raw, &summary) == nil {
					if r, ok := summary["root"].(string); ok {
						crawlRoot = r
					}
				}
			}
		}
	}
	if crawlRoot == "" {
		fatal("code-search: could not resolve root (pass --root)")
	}
	rootAbs, err := filepath.Abs(crawlRoot)
	if err != nil {
		fatal(err.Error())
	}
	if outDir == "" {
		outDir = rootAbs
	}
	gobPath := filepath.Join(outDir, codecrawl.DefaultIndexFile)
	var (
		idx *codecrawl.Index
		st  codecrawl.Stats
	)
	if *noRefresh {
		loaded, _, err := codecrawl.Load(gobPath)
		if err != nil {
			fatal("code-search: load gob: " + err.Error() + " (run code-index first, or drop --no-refresh)")
		}
		idx = loaded
		st.Workers = *workers
		st.FilesIndexed = idx.FileCount()
	} else {
		var err error
		idx, st, _, _, err = codecrawl.OpenOrRefresh(rootAbs, gobPath, *workers, *force)
		if err != nil {
			fatal(err.Error())
		}
	}
	hop := *symbolHop || os.Getenv("OUROBOROS_CODE_SYMBOL_HOP") == "1"
	hits := idx.SearchOpts(*q, *topK, hop)
	outHits := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		outHits = append(outHits, map[string]any{"path": h.Path, "score": h.Score})
	}
	defs, refs := idx.SymbolStats()
	emitJSON(map[string]any{
		"query":            *q,
		"root":             rootAbs,
		"workers":          st.Workers,
		"files_indexed":    st.FilesIndexed,
		"changed":          st.Changed,
		"unchanged":        st.Unchanged,
		"duration_ms":      st.Duration.Milliseconds(),
		"hit_count":        len(outHits),
		"hits":             outHits,
		"product_owned":    true,
		"search_backend":   "codecrawl",
		"index_cache":      outDir,
		"gob_path":         gobPath,
		"symbol_hop":       hop,
		"symbol_defs":      defs,
		"symbol_refs":      refs,
		"no_refresh":       *noRefresh,
		"skipped_by_stamp": st.SkippedByStamp,
	})
}

// resolveCodeRootCache resolves --root / --index-cache like code-search.
func resolveCodeRootCache(rootFlag, cacheFlag string) (rootAbs, outDir, gobPath string) {
	crawlRoot := strings.TrimSpace(rootFlag)
	cachePath := strings.TrimSpace(cacheFlag)
	if cachePath != "" {
		st, err := os.Stat(cachePath)
		if err == nil && st.IsDir() {
			outDir = cachePath
		} else if strings.HasSuffix(cachePath, codeIndexSummaryName) || strings.HasSuffix(cachePath, codecrawl.DefaultIndexFile) {
			outDir = filepath.Dir(cachePath)
		} else if err == nil {
			outDir = filepath.Dir(cachePath)
		} else {
			outDir = cachePath
		}
		if crawlRoot == "" {
			raw, err := os.ReadFile(filepath.Join(outDir, codeIndexSummaryName))
			if err != nil {
				raw, err = os.ReadFile(cachePath)
			}
			if err == nil {
				var summary map[string]any
				if json.Unmarshal(raw, &summary) == nil {
					if r, ok := summary["root"].(string); ok {
						crawlRoot = r
					}
				}
			}
		}
	}
	if crawlRoot == "" {
		fatal("code-*: could not resolve root (pass --root)")
	}
	var err error
	rootAbs, err = filepath.Abs(crawlRoot)
	if err != nil {
		fatal(err.Error())
	}
	if outDir == "" {
		outDir = rootAbs
	}
	gobPath = filepath.Join(outDir, codecrawl.DefaultIndexFile)
	return rootAbs, outDir, gobPath
}

func loadCodeIndex(rootAbs, gobPath string, workers int, noRefresh, force bool) (*codecrawl.Index, codecrawl.Stats) {
	if noRefresh {
		idx, _, err := codecrawl.Load(gobPath)
		if err != nil {
			fatal("load gob: " + err.Error() + " (run code-index first)")
		}
		return idx, codecrawl.Stats{Workers: workers, FilesIndexed: idx.FileCount()}
	}
	idx, st, _, _, err := codecrawl.OpenOrRefresh(rootAbs, gobPath, workers, force)
	if err != nil {
		fatal(err.Error())
	}
	return idx, st
}

func runCodeFindRelevant(args []string) {
	fs := flag.NewFlagSet("code-find-relevant", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	q := fs.String("q", "", "intent / query")
	topK := fs.Int("top-k", 5, "max hits (SCM lean default 5)")
	preview := fs.Bool("preview", true, "include source previews")
	workers := fs.Int("workers", 4, "workers")
	noRefresh := fs.Bool("no-refresh", false, "skip refresh")
	_ = fs.Parse(args)
	if *q == "" {
		fatal("code-find-relevant: --q required")
	}
	rootAbs, _, gobPath := resolveCodeRootCache(*root, *cache)
	idx, st := loadCodeIndex(rootAbs, gobPath, *workers, *noRefresh, false)
	payload := idx.FindRelevant(rootAbs, *q, *topK, *preview)
	emitJSON(map[string]any{
		"verb":           "find_relevant",
		"payload":        payload,
		"files_indexed":  st.FilesIndexed,
		"duration_ms":    st.Duration.Milliseconds(),
		"search_backend": "codecrawl",
		"product_owned":  true,
	})
}

func runCodeExpand(args []string) {
	fs := flag.NewFlagSet("code-expand", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	seed := fs.String("seed", "", "file path or symbol name")
	maxN := fs.Int("max", 12, "max files")
	workers := fs.Int("workers", 4, "workers")
	noRefresh := fs.Bool("no-refresh", false, "skip refresh")
	_ = fs.Parse(args)
	if *seed == "" {
		fatal("code-expand: --seed required")
	}
	rootAbs, _, gobPath := resolveCodeRootCache(*root, *cache)
	idx, _ := loadCodeIndex(rootAbs, gobPath, *workers, *noRefresh, false)
	hits := idx.Expand([]string{*seed}, *maxN)
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{"path": h.Path, "score": h.Score})
	}
	emitJSON(map[string]any{
		"verb": "expand", "seed": *seed, "hits": out, "product_owned": true, "search_backend": "codecrawl",
	})
}

func runCodeImpact(args []string) {
	fs := flag.NewFlagSet("code-impact", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	seed := fs.String("seed", "", "symbol or file path")
	depth := fs.Int("depth", 2, "hop depth")
	maxFiles := fs.Int("max", 40, "max closure files")
	workers := fs.Int("workers", 4, "workers")
	noRefresh := fs.Bool("no-refresh", false, "skip refresh")
	_ = fs.Parse(args)
	if *seed == "" {
		fatal("code-impact: --seed required")
	}
	rootAbs, _, gobPath := resolveCodeRootCache(*root, *cache)
	idx, _ := loadCodeIndex(rootAbs, gobPath, *workers, *noRefresh, false)
	rec := idx.Impact(*seed, *depth, *maxFiles)
	emitJSON(map[string]any{
		"verb": "impact", "receipt": rec, "product_owned": true, "search_backend": "codecrawl",
	})
}

func runCodeFindRoute(args []string) {
	fs := flag.NewFlagSet("code-find-route", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	from := fs.String("from", "", "source file or symbol")
	to := fs.String("to", "", "target file or symbol")
	maxB := fs.Int("max", 12, "max bridges")
	workers := fs.Int("workers", 4, "workers")
	noRefresh := fs.Bool("no-refresh", false, "skip refresh")
	_ = fs.Parse(args)
	if *from == "" || *to == "" {
		fatal("code-find-route: --from and --to required")
	}
	rootAbs, _, gobPath := resolveCodeRootCache(*root, *cache)
	idx, _ := loadCodeIndex(rootAbs, gobPath, *workers, *noRefresh, false)
	rec := idx.FindRoute(*from, *to, *maxB)
	emitJSON(map[string]any{
		"verb": "find_route", "receipt": rec, "product_owned": true, "search_backend": "codecrawl",
	})
}

func runCodeDefsRefs(args []string, defs bool) {
	name := "code-refs"
	if defs {
		name = "code-defs"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	sym := fs.String("symbol", "", "symbol name")
	workers := fs.Int("workers", 4, "workers")
	noRefresh := fs.Bool("no-refresh", false, "skip refresh")
	_ = fs.Parse(args)
	if *sym == "" {
		fatal(name + ": --symbol required")
	}
	rootAbs, _, gobPath := resolveCodeRootCache(*root, *cache)
	idx, _ := loadCodeIndex(rootAbs, gobPath, *workers, *noRefresh, false)
	var files []string
	if defs {
		files = idx.DefsOf(*sym)
	} else {
		files = idx.RefsOf(*sym)
	}
	emitJSON(map[string]any{
		"verb": name, "symbol": *sym, "files": files, "product_owned": true, "search_backend": "codecrawl",
	})
}

func runCodeFreshness(args []string) {
	fs := flag.NewFlagSet("code-freshness", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	_ = fs.Parse(args)
	rootAbs, _, gobPath := resolveCodeRootCache(*root, *cache)
	idx, meta, err := codecrawl.Load(gobPath)
	if err != nil {
		fatal("code-freshness: " + err.Error())
	}
	rep := idx.Freshness(rootAbs, meta)
	rep.GobPath = gobPath
	emitJSON(map[string]any{
		"verb": "freshness", "report": rep, "product_owned": true,
	})
}

func runCodeIngestPaths(args []string) {
	fs := flag.NewFlagSet("code-ingest-paths", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	paths := fs.String("paths", "", "comma-separated relative paths")
	_ = fs.Parse(args)
	if *paths == "" {
		fatal("code-ingest-paths: --paths required")
	}
	rootAbs, outDir, gobPath := resolveCodeRootCache(*root, *cache)
	idx, meta, err := codecrawl.Load(gobPath)
	if err != nil {
		// cold: full index first
		idx, _, _, meta, err = codecrawl.OpenOrRefresh(rootAbs, gobPath, 4, true)
		if err != nil {
			fatal(err.Error())
		}
	}
	var rels []string
	for _, p := range strings.Split(*paths, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			rels = append(rels, p)
		}
	}
	n, err := idx.IngestPaths(rootAbs, rels)
	if err != nil {
		fatal(err.Error())
	}
	if err := idx.Save(gobPath, rootAbs); err != nil {
		fatal(err.Error())
	}
	emitJSON(map[string]any{
		"verb": "ingest_paths", "changed": n, "paths": rels, "gob_path": gobPath,
		"root": rootAbs, "dir": outDir, "git_head": meta.GitHead, "product_owned": true,
	})
}

func runCodeWatch(args []string) {
	fs := flag.NewFlagSet("code-watch", flag.ExitOnError)
	root := fs.String("root", "", "source root")
	cache := fs.String("index-cache", "", "index dir")
	interval := fs.Duration("interval", time.Second, "poll interval")
	debounce := fs.Duration("debounce", 300*time.Millisecond, "dirty debounce")
	workers := fs.Int("workers", 4, "workers")
	cycles := fs.Int("max-cycles", 0, "0=forever")
	useFS := fs.Bool("fsnotify", true, "use fsnotify when available (else poll)")
	_ = fs.Parse(args)
	rootAbs, outDir, gobPath := resolveCodeRootCache(*root, *cache)
	_ = os.MkdirAll(outDir, 0o755)
	ctx := context.Background()
	opt := codecrawl.WatchOptions{
		Root: rootAbs, GobPath: gobPath, Workers: *workers,
		Interval: *interval, Debounce: *debounce, MaxCycles: *cycles,
		OnRefresh: func(st codecrawl.Stats, wrote bool) {
			emitJSON(map[string]any{
				"event": "refresh", "changed": st.Changed, "unchanged": st.Unchanged,
				"skipped_by_stamp": st.SkippedByStamp, "duration_ms": st.Duration.Milliseconds(),
				"wrote": wrote,
			})
		},
	}
	var err error
	if *useFS {
		err = codecrawl.WatchFS(ctx, opt)
	} else {
		err = codecrawl.WatchPoll(ctx, opt)
	}
	if err != nil && err != context.Canceled {
		fatal(err.Error())
	}
}

package continual

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

// DocWatchOptions configures company-doc continual ingestion into a product brain.
type DocWatchOptions struct {
	// Client must be a product hosted client (OpenLocal / OpenMemory).
	Client *hosted.Client
	// DocsPath is a JSONL file of documents or a directory of .md/.txt/.jsonl files.
	DocsPath string
	// Interval between freshness checks (default 1s).
	Interval time.Duration
	// Debounce after first change before delta (default 300ms).
	Debounce time.Duration
	// MaxCycles stops after N successful delta attempts (0 = forever).
	MaxCycles int
	// OnDelta is called after each ContinualDeltaLocal (may be zero-ingest).
	OnDelta func(res hosted.IngestResult)
	// OnError is called for non-fatal watch loop errors (stamp/load/delta). Optional.
	OnError func(stage string, err error)
}

// WatchDocs polls DocsPath; on change loads docs and runs ContinualDeltaLocal.
// Ingest is retrieval_ready first; gardener enqueue follows Client enrich mode
// (async by default on local_fs with gardener.db).
func WatchDocs(ctx context.Context, opt DocWatchOptions) error {
	if opt.Client == nil {
		return fmt.Errorf("continual: nil client")
	}
	docsPath := strings.TrimSpace(opt.DocsPath)
	if docsPath == "" {
		return fmt.Errorf("continual: empty docs path")
	}
	if opt.Interval <= 0 {
		opt.Interval = time.Second
	}
	if opt.Debounce <= 0 {
		opt.Debounce = 300 * time.Millisecond
	}

	var lastStamp int64
	var dirtySince time.Time
	cycles := 0
	ticker := time.NewTicker(opt.Interval)
	defer ticker.Stop()

	// Initial load so first cycle has a baseline.
	if st, err := pathStamp(docsPath); err == nil {
		lastStamp = st
		docs, err := LoadDocuments(docsPath)
		if err == nil {
			if err := applyWatchDelta(ctx, opt.Client, docs, opt.OnDelta, opt.OnError); err == nil {
				cycles++
				if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
					return nil
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			st, err := pathStamp(docsPath)
			if err != nil {
				if opt.OnError != nil {
					opt.OnError("path_stamp", err)
				}
				continue
			}
			if st == lastStamp {
				continue
			}
			if dirtySince.IsZero() {
				dirtySince = time.Now()
			}
			if time.Since(dirtySince) < opt.Debounce {
				continue
			}
			docs, err := LoadDocuments(docsPath)
			if err != nil {
				if opt.OnError != nil {
					opt.OnError("load_docs", err)
				}
				dirtySince = time.Time{}
				lastStamp = st
				continue
			}
			dirtySince = time.Time{}
			lastStamp = st
			if err := applyWatchDelta(ctx, opt.Client, docs, opt.OnDelta, opt.OnError); err != nil {
				continue
			}
			cycles++
			if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
				return nil
			}
		}
	}
}

// applyWatchDelta upserts live docs and prunes store docs missing from source.
func applyWatchDelta(
	ctx context.Context,
	c *hosted.Client,
	docs []hosted.LocalDocument,
	onDelta func(hosted.IngestResult),
	onError func(stage string, err error),
) error {
	live := map[string]struct{}{}
	for _, d := range docs {
		if id := strings.TrimSpace(d.ID); id != "" {
			live[id] = struct{}{}
		}
	}
	// Prune first so deleted files leave the store even when no upserts remain.
	if n, err := c.PruneMissingDocuments(ctx, live); err != nil {
		if onError != nil {
			onError("prune", err)
		}
		// non-fatal: still try delta
	} else if n > 0 && onDelta != nil {
		onDelta(hosted.IngestResult{
			BrainID: c.Config().BrainID, Mode: "prune", Ingested: 0, Upserted: n, ProductOwned: true,
		})
	}
	if len(docs) == 0 {
		return nil
	}
	res, err := c.ContinualDeltaLocal(ctx, docs)
	if err != nil {
		if onError != nil {
			onError("delta", err)
		}
		return err
	}
	if onDelta != nil {
		onDelta(res)
	}
	return nil
}

// LoadDocuments reads a JSONL file or walks a directory for text documents.
func LoadDocuments(path string) ([]hosted.LocalDocument, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return loadJSONL(path)
	}
	var out []hosted.LocalDocument
	err = filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil || fi.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		switch ext {
		case ".md", ".txt", ".markdown":
			raw, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(path, p)
			id := strings.TrimSuffix(rel, ext)
			id = strings.ReplaceAll(id, string(filepath.Separator), "/")
			out = append(out, hosted.LocalDocument{
				ID: id, Text: string(raw), SourceURI: "file://" + p,
			})
		case ".jsonl":
			docs, err := loadJSONL(p)
			if err == nil {
				out = append(out, docs...)
			}
		}
		return nil
	})
	return out, err
}

func loadJSONL(path string) ([]hosted.LocalDocument, error) {
	// Reuse product-brain JSONL shape via hosted helpers if available;
	// keep local parse to avoid cmd dependency.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var docs []hosted.LocalDocument
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Minimal parse without full json import cycle issues — use encoding/json.
		doc, err := parseDocLine(line)
		if err != nil {
			// Skip corrupt lines; partial load beats whole-file fail-closed.
			continue
		}
		if doc.ID != "" && doc.Text != "" {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

// pathStamp fingerprints a file or directory so additions, edits, and deletions
// all flip the stamp (max mtime alone misses deletes of non-newest files).
func pathStamp(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		// size mixes into mtime so truncate/rewrite same second still changes.
		return info.ModTime().UnixNano() ^ (info.Size() << 1), nil
	}
	var h int64 = 1469598103934665603 // FNV offset basis-ish
	var n int64
	err = filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil || fi.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		switch ext {
		case ".md", ".txt", ".markdown", ".jsonl":
		default:
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		// Mix name + mtime + size so renames/deletes change the fingerprint.
		for _, b := range []byte(rel) {
			h = (h * 1099511628211) ^ int64(b)
		}
		h ^= fi.ModTime().UnixNano()
		h ^= fi.Size() * 2654435761 // 32-bit golden ratio, fits int64 multiply
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return h ^ n ^ (n << 17), nil
}

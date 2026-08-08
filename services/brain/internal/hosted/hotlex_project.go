package hosted

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
	"time"
)

// ProjectOptions configures fast multi-worker HotLex projection from Neon.
type ProjectOptions struct {
	BrainID   string
	MaxDocs   int  // 0 = all (global; approximate per-shard when sharded)
	TextChars int  // left(text) for tokenize; default 800
	StripText bool // index-only (no body in gob)
	// Workers is in-process parallel shard builders (default 8).
	Workers int
	// Shards is hash partition count (default = Workers).
	Shards int
	// ShardID when >=0 restricts this process to one shard [0, Shards).
	// Used by Modal burst containers. -1 = all shards in this process.
	ShardID int
	// PageSize for keyset pagination per shard (default 5000).
	PageSize int
	// ProgressEvery logs every N kept rows (default 50000).
	ProgressEvery int
}

// ProjectResult is project timing + stats.
type ProjectResult struct {
	Index     *HotLex
	Rows      int
	Duration  time.Duration
	Workers   int
	Shards    int
	ShardID   int
	PageSize  int
	BytesEst  int64
	PageCount int
}

// ProjectHotLexFast streams path2 with keyset pagination + multi-worker shards.
//
// Each shard filters in Postgres:
//
//	mod(abs(hashtext(chunk_id)), shards) = shard_id
//
// so workers never full-scan the whole table N times.
func ProjectHotLexFast(ctx context.Context, db *sql.DB, opt ProjectOptions) (*ProjectResult, error) {
	if db == nil {
		return nil, fmt.Errorf("project hotlex: nil db")
	}
	if opt.BrainID == "" {
		return nil, fmt.Errorf("project hotlex: empty brain_id")
	}
	if opt.TextChars <= 0 {
		opt.TextChars = 800
	}
	if opt.Workers <= 0 {
		opt.Workers = 8
	}
	if opt.Shards <= 0 {
		opt.Shards = opt.Workers
	}
	if opt.PageSize <= 0 {
		opt.PageSize = 5000
	}
	if opt.ProgressEvery <= 0 {
		opt.ProgressEvery = 50000
	}
	if opt.ShardID < -1 || opt.ShardID >= opt.Shards {
		return nil, fmt.Errorf("project hotlex: shard_id %d out of range [0,%d)", opt.ShardID, opt.Shards)
	}

	t0 := time.Now()
	if opt.ShardID >= 0 {
		h, rows, pages, bytesEst, err := projectOneShard(ctx, db, opt, opt.ShardID, opt.MaxDocs)
		if err != nil {
			return nil, err
		}
		h.Finalize()
		return &ProjectResult{
			Index: h, Rows: rows, Duration: time.Since(t0),
			Workers: 1, Shards: opt.Shards, ShardID: opt.ShardID,
			PageSize: opt.PageSize, BytesEst: bytesEst, PageCount: pages,
		}, nil
	}

	type shardJob struct{ shard int }
	jobs := make(chan shardJob, opt.Shards)
	for s := 0; s < opt.Shards; s++ {
		jobs <- shardJob{shard: s}
	}
	close(jobs)

	perShardCap := 0
	if opt.MaxDocs > 0 {
		perShardCap = (opt.MaxDocs / opt.Shards) + 1
	}

	var (
		mu       sync.Mutex
		parts    = make([]*HotLex, 0, opt.Shards)
		totalRow int64
		totalPg  int64
		totalB   int64
		firstErr error
		wg       sync.WaitGroup
	)
	workers := opt.Workers
	if workers > opt.Shards {
		workers = opt.Shards
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				h, rows, pages, bytesEst, err := projectOneShard(ctx, db, opt, job.shard, perShardCap)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				parts = append(parts, h)
				totalRow += int64(rows)
				totalPg += int64(pages)
				totalB += bytesEst
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	merged := MergeShards(opt.BrainID, parts)
	merged.Finalize()
	if int(totalRow) == 0 {
		return nil, fmt.Errorf("project hotlex: zero rows brain_id=%s", opt.BrainID)
	}
	fmt.Fprintf(os.Stderr, "project hotlex fast: rows=%d shards=%d workers=%d pages=%d in %s\n",
		totalRow, opt.Shards, workers, totalPg, time.Since(t0).Round(time.Millisecond))
	return &ProjectResult{
		Index: merged, Rows: int(totalRow), Duration: time.Since(t0),
		Workers: workers, Shards: opt.Shards, ShardID: -1,
		PageSize: opt.PageSize, BytesEst: totalB, PageCount: int(totalPg),
	}, nil
}

// projectOneShard keyset-pages one hash partition via Postgres hashtext.
func projectOneShard(ctx context.Context, db *sql.DB, opt ProjectOptions, shard, maxDocs int) (*HotLex, int, int, int64, error) {
	h := NewHotLex(fmt.Sprintf("%s#s%d", opt.BrainID, shard))
	// hashtext is stable for a PG major; mod(abs(...)) partitions evenly enough.
	const q = `
SELECT chunk_id, dsid, left(text_content, $4), COALESCE(source_uri, '')
FROM path2_chunk_metadata
WHERE brain_id = $1
  AND mod(abs(hashtext(chunk_id)), $2) = $3
  AND chunk_id > $5
ORDER BY chunk_id
LIMIT $6
`
	cursor := ""
	n := 0
	pages := 0
	var bytesEst int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, n, pages, bytesEst, fmt.Errorf("shard %d after %d: %w", shard, n, err)
		}
		rows, err := db.QueryContext(ctx, q, opt.BrainID, opt.Shards, shard, opt.TextChars, cursor, opt.PageSize)
		if err != nil {
			// Fallback if hashtext unavailable: client-side filter full keyset (slower).
			return projectOneShardClientFilter(ctx, db, opt, shard, maxDocs)
		}
		pageN := 0
		var lastID string
		for rows.Next() {
			var chunkID, dsid, text, uri string
			if err := rows.Scan(&chunkID, &dsid, &text, &uri); err != nil {
				_ = rows.Close()
				return nil, n, pages, bytesEst, err
			}
			lastID = chunkID
			pageN++
			bytesEst += int64(len(text))
			h.AddChunkBulk(chunkID, dsid, text, uri, !opt.StripText)
			n++
			if maxDocs > 0 && n >= maxDocs {
				_ = rows.Close()
				h.BrainID = opt.BrainID
				h.Finalize()
				return h, n, pages + 1, bytesEst, nil
			}
			if opt.ProgressEvery > 0 && n%opt.ProgressEvery == 0 {
				fmt.Fprintf(os.Stderr, "project hotlex shard=%d rows=%d\n", shard, n)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, n, pages, bytesEst, err
		}
		_ = rows.Close()
		pages++
		if pageN == 0 {
			break
		}
		if lastID == "" || lastID == cursor {
			break
		}
		cursor = lastID
		if pageN < opt.PageSize {
			break
		}
	}
	h.BrainID = opt.BrainID
	h.Finalize()
	return h, n, pages, bytesEst, nil
}

// projectOneShardClientFilter pages whole brain and keeps hash%shards==shard.
// Used only if hashtext SQL fails.
func projectOneShardClientFilter(ctx context.Context, db *sql.DB, opt ProjectOptions, shard, maxDocs int) (*HotLex, int, int, int64, error) {
	h := NewHotLex(opt.BrainID)
	const q = `
SELECT chunk_id, dsid, left(text_content, $3), COALESCE(source_uri, '')
FROM path2_chunk_metadata
WHERE brain_id = $1 AND chunk_id > $2
ORDER BY chunk_id
LIMIT $4
`
	cursor := ""
	n := 0
	pages := 0
	var bytesEst int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, n, pages, bytesEst, err
		}
		rows, err := db.QueryContext(ctx, q, opt.BrainID, cursor, opt.TextChars, opt.PageSize)
		if err != nil {
			return nil, n, pages, bytesEst, err
		}
		pageN := 0
		var lastID string
		for rows.Next() {
			var chunkID, dsid, text, uri string
			if err := rows.Scan(&chunkID, &dsid, &text, &uri); err != nil {
				_ = rows.Close()
				return nil, n, pages, bytesEst, err
			}
			lastID = chunkID
			pageN++
			if HashShardID(chunkID, opt.Shards) != shard {
				continue
			}
			bytesEst += int64(len(text))
			h.AddChunkBulk(chunkID, dsid, text, uri, !opt.StripText)
			n++
			if maxDocs > 0 && n >= maxDocs {
				_ = rows.Close()
				h.Finalize()
				return h, n, pages + 1, bytesEst, nil
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, n, pages, bytesEst, err
		}
		_ = rows.Close()
		pages++
		if pageN == 0 || lastID == "" || lastID == cursor {
			break
		}
		cursor = lastID
		if pageN < opt.PageSize {
			break
		}
	}
	h.Finalize()
	return h, n, pages, bytesEst, nil
}

// ProjectHotLexFromDSNFast opens Neon and runs ProjectHotLexFast.
func ProjectHotLexFromDSNFast(ctx context.Context, dsn string, opt ProjectOptions) (*ProjectResult, error) {
	db, err := openDB(dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(maxInt(opt.Workers*2, 16))
	db.SetMaxIdleConns(maxInt(opt.Workers, 8))
	return ProjectHotLexFast(ctx, db, opt)
}

// HashShardID returns a stable FNV shard (client-side; may differ from hashtext).
func HashShardID(chunkID string, shards int) int {
	if shards <= 1 {
		return 0
	}
	hh := fnv.New32a()
	_, _ = hh.Write([]byte(chunkID))
	return int(hh.Sum32() % uint32(shards))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

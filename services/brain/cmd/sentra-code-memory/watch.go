package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
)

// runWatch keeps a durable code index fresh until interrupted. codecrawl owns
// event coalescing, debounce, retry, overflow rescan, and multi-worker refresh.
func runWatch(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", ".", "source root (default: current working directory)")
	cache := fs.String("index-cache", "", "index directory or code-index.gob path")
	workers := fs.Int("workers", 4, "refresh worker count")
	interval := fs.Duration("interval", time.Second, "poll interval when --poll is selected")
	debounce := fs.Duration("debounce", 300*time.Millisecond, "quiet period after edits")
	queueSize := fs.Int("queue-size", 4096, "coalesced event queue capacity")
	retryInitial := fs.Duration("retry-initial", 100*time.Millisecond, "first refresh retry delay")
	retryMax := fs.Duration("retry-max", 5*time.Second, "maximum refresh retry delay")
	maxCycles := fs.Int("max-cycles", 0, "stop after this many successful refreshes; 0 means forever")
	useFS := fs.Bool("fsnotify", true, "use kernel events; false uses polling")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "watch: positional arguments are not accepted")
		return 2
	}
	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	gobPath := *cache
	if gobPath == "" {
		gobPath = filepath.Join(rootAbs, ".sentra", codecrawl.DefaultIndexFile)
	} else if info, statErr := os.Stat(gobPath); statErr == nil && info.IsDir() {
		gobPath = filepath.Join(gobPath, codecrawl.DefaultIndexFile)
	} else if filepath.Base(gobPath) != codecrawl.DefaultIndexFile {
		gobPath = filepath.Join(gobPath, codecrawl.DefaultIndexFile)
	}
	if err := os.MkdirAll(filepath.Dir(gobPath), 0o755); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	writeEvent := func(value any) {
		_ = json.NewEncoder(out).Encode(value)
	}
	opt := codecrawl.WatchOptions{
		Root:         rootAbs,
		GobPath:      gobPath,
		Workers:      *workers,
		Interval:     *interval,
		Debounce:     *debounce,
		QueueSize:    *queueSize,
		RetryInitial: *retryInitial,
		RetryMax:     *retryMax,
		MaxCycles:    *maxCycles,
		OnRefresh: func(st codecrawl.Stats, wrote bool) {
			writeEvent(map[string]any{
				"event": "refresh", "root": rootAbs, "gob_path": gobPath,
				"files_indexed": st.FilesIndexed, "changed": st.Changed,
				"unchanged": st.Unchanged, "skipped_by_stamp": st.SkippedByStamp,
				"duration_ms": st.Duration.Milliseconds(), "workers": st.Workers,
				"wrote": wrote, "queue_depth": st.QueueDepth,
				"retry_count": st.RetryCount, "full_rescan": st.FullRescan,
			})
		},
		OnError: func(refreshErr error, retryCount int) {
			writeEvent(map[string]any{
				"event": "refresh_error", "retry_count": retryCount,
				"error": refreshErr.Error(),
			})
		},
	}
	var watchErr error
	if *useFS {
		watchErr = codecrawl.WatchFS(ctx, opt)
	} else {
		watchErr = codecrawl.WatchPoll(ctx, opt)
	}
	if watchErr != nil && watchErr != context.Canceled {
		fmt.Fprintln(errOut, watchErr)
		return 1
	}
	return 0
}

package codecrawl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchFS uses fsnotify for near-real-time dirty detection, then debounced
// OpenOrRefresh. Events are coalesced into a bounded queue; refresh failures
// remain queued and retry with capped exponential backoff. It falls back to
// WatchPoll if the kernel watcher cannot be created.
func WatchFS(ctx context.Context, opt WatchOptions) error {
	if opt.Workers < 1 {
		opt.Workers = 4
	}
	if opt.Debounce <= 0 {
		opt.Debounce = 300 * time.Millisecond
	}
	rootAbs, err := filepath.Abs(opt.Root)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opt.GobPath) == "" {
		opt.GobPath = filepath.Join(rootAbs, DefaultIndexFile)
	}
	if _, err := os.Stat(opt.GobPath); err != nil {
		if _, _, _, _, err := OpenOrRefresh(rootAbs, opt.GobPath, opt.Workers, true); err != nil {
			return err
		}
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return WatchPoll(ctx, opt)
	}
	defer w.Close()

	// Watch root + bounded subdirectories. The queue's overflow/full-rescan
	// path still catches files below the watcher depth.
	_ = w.Add(rootAbs)
	_ = filepath.Walk(rootAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.IsDir() {
			return nil
		}
		if _, ok := skipDir[info.Name()]; ok {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(rootAbs, path)
		if rel != "." && strings.Count(rel, string(filepath.Separator)) > 3 {
			return filepath.SkipDir
		}
		_ = w.Add(path)
		return nil
	})

	queue := newRefreshQueue(opt.QueueSize)
	var dirtyAt time.Time
	var retryAt time.Time
	retryCount := 0
	cycles := 0

	refresh := func() {
		version, _, fullRescan := queue.begin()
		_, st, wrote, _, refreshErr := OpenOrRefresh(rootAbs, opt.GobPath, opt.Workers, false)
		if refreshErr != nil {
			retryCount++
			retryAt = time.Now().Add(retryDelay(retryCount, opt.RetryInitial, opt.RetryMax))
			if opt.OnError != nil {
				opt.OnError(refreshErr, retryCount)
			}
			return
		}

		committed := queue.commit(version)
		st.QueueDepth = queue.depth()
		st.RetryCount = retryCount
		st.FullRescan = fullRescan
		retryCount = 0
		retryAt = time.Time{}
		if committed {
			dirtyAt = time.Time{}
		} else {
			// An event arrived during the scan. Keep it queued and debounce the
			// follow-up rather than clearing a change we did not observe.
			dirtyAt = time.Now()
		}
		if opt.OnRefresh != nil {
			opt.OnRefresh(st, wrote)
		}
		cycles++
	}

	// Initial cycle so --max-cycles N exits without waiting for a file event.
	refresh()
	if retryCount == 0 && opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
		return nil
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if info, statErr := os.Stat(ev.Name); statErr == nil && info.IsDir() {
				_ = w.Add(ev.Name)
				queue.enqueue("")
				dirtyAt = time.Now()
				continue
			}
			ext := strings.ToLower(filepath.Ext(ev.Name))
			if ext != "" {
				if _, ok := extOK[ext]; !ok {
					continue
				}
			}
			queue.enqueue(ev.Name)
			dirtyAt = time.Now()
		case watcherErr, ok := <-w.Errors:
			if !ok {
				return nil
			}
			queue.enqueue("")
			dirtyAt = time.Now()
			if opt.OnError != nil {
				opt.OnError(watcherErr, retryCount)
			}
		case now := <-ticker.C:
			if !queue.pending() || dirtyAt.IsZero() || now.Sub(dirtyAt) < opt.Debounce {
				continue
			}
			if !retryAt.IsZero() && now.Before(retryAt) {
				continue
			}
			refresh()
			if retryCount == 0 && opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
				return nil
			}
		}
	}
}

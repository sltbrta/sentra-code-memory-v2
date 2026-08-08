package codecrawl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchFS uses fsnotify for near-real-time dirty detection, then debounced OpenOrRefresh.
// Falls back to WatchPoll if the watcher cannot be created.
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
		// No kernel watchers available — poll.
		return WatchPoll(ctx, opt)
	}
	defer w.Close()

	// Watch root + immediate subdirs (bounded).
	_ = w.Add(rootAbs)
	_ = filepath.Walk(rootAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.IsDir() {
			return nil
		}
		if _, ok := skipDir[info.Name()]; ok {
			return filepath.SkipDir
		}
		// Limit depth to avoid watching huge trees with too many watches.
		rel, _ := filepath.Rel(rootAbs, path)
		if rel != "." && strings.Count(rel, string(filepath.Separator)) > 3 {
			return filepath.SkipDir
		}
		_ = w.Add(path)
		return nil
	})

	dirty := false
	var dirtyAt time.Time
	cycles := 0
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	refresh := func() {
		_, st, wrote, _, err := OpenOrRefresh(rootAbs, opt.GobPath, opt.Workers, false)
		if err != nil {
			return
		}
		dirty = false
		dirtyAt = time.Time{}
		if opt.OnRefresh != nil {
			opt.OnRefresh(st, wrote)
		}
		cycles++
	}

	// Initial cycle so --max-cycles N exits without waiting forever for mtime changes
	// (same class of bug as multi-folder docs watch). Smoke uses max-cycles=1.
	refresh()
	if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
		return nil
	}

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
			// Only care about indexable extensions / dirs.
			ext := strings.ToLower(filepath.Ext(ev.Name))
			if ext != "" {
				if _, ok := extOK[ext]; !ok {
					continue
				}
			}
			if !dirty {
				dirty = true
				dirtyAt = time.Now()
			}
		case <-w.Errors:
			// ignore transient watcher errors
		case <-ticker.C:
			if dirty && !dirtyAt.IsZero() && time.Since(dirtyAt) >= opt.Debounce {
				refresh()
				if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
					return nil
				}
			}
		}
	}
}

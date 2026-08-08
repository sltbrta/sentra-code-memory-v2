package codecrawl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WatchOptions configures a filesystem or poll-based index watcher. Events are
// coalesced and refresh failures are retried without dropping freshness work.
type WatchOptions struct {
	Root         string
	GobPath      string
	Workers      int
	Interval     time.Duration // default 1s
	Debounce     time.Duration // default 300ms after first dirty stamp
	QueueSize    int           // default 4096 coalesced paths
	RetryInitial time.Duration // default 100ms
	RetryMax     time.Duration // default 5s
	OnRefresh    func(st Stats, wrote bool)
	OnError      func(error, int)
	MaxCycles    int // 0 = forever
}

// WatchPoll runs OpenOrRefresh on an interval until ctx is done.
// Latency benefit: stamp-fast path makes clean cycles near-free after first gob.
func WatchPoll(ctx context.Context, opt WatchOptions) error {
	if opt.Workers < 1 {
		opt.Workers = 4
	}
	if opt.Interval <= 0 {
		opt.Interval = time.Second
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
	// Ensure index exists.
	if _, err := os.Stat(opt.GobPath); err != nil {
		if _, _, _, _, err := OpenOrRefresh(rootAbs, opt.GobPath, opt.Workers, true); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(opt.Interval)
	defer ticker.Stop()
	cycles := 0
	var dirtySince time.Time
	var retryAt time.Time
	retryCount := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			// Quick freshness: if stamps all match, skip OpenOrRefresh heavy path
			// by loading and Freshness.
			idx, meta, err := Load(opt.GobPath)
			if err == nil {
				rep := idx.Freshness(rootAbs, meta)
				if rep.Fresh {
					cycles++
					if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
						return nil
					}
					continue
				}
				if dirtySince.IsZero() {
					dirtySince = now
				}
				if now.Sub(dirtySince) < opt.Debounce || (!retryAt.IsZero() && now.Before(retryAt)) {
					continue
				}
			}
			_, st, wrote, _, refreshErr := OpenOrRefresh(rootAbs, opt.GobPath, opt.Workers, false)
			if refreshErr != nil {
				retryCount++
				retryAt = now.Add(retryDelay(retryCount, opt.RetryInitial, opt.RetryMax))
				if opt.OnError != nil {
					opt.OnError(refreshErr, retryCount)
				}
				continue
			}
			dirtySince = time.Time{}
			retryAt = time.Time{}
			st.RetryCount = retryCount
			retryCount = 0
			if opt.OnRefresh != nil {
				opt.OnRefresh(st, wrote)
			}
			cycles++
			if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
				return nil
			}
		}
	}
}

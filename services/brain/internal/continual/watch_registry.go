package continual

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

// WatchFolder is one registered docs path for background continual ingest.
type WatchFolder struct {
	// Path is a directory of .md/.txt or a JSONL file.
	Path string `json:"path"`
	// Enabled skips disabled entries without removing them.
	Enabled bool `json:"enabled"`
	// Label is optional display name (TUI / logs).
	Label string `json:"label,omitempty"`
}

// WatchRegistry is a durable multi-folder watch list (daemon + TUI).
type WatchRegistry struct {
	// Folders is the ordered watch list.
	Folders []WatchFolder `json:"folders"`
	// Interval between freshness checks (CLI may override).
	Interval string `json:"interval,omitempty"`
	// Debounce after first change before delta.
	Debounce string `json:"debounce,omitempty"`
}

// LoadWatchRegistry reads a JSON registry file.
func LoadWatchRegistry(path string) (WatchRegistry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return WatchRegistry{}, err
	}
	var reg WatchRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return WatchRegistry{}, fmt.Errorf("continual: registry parse: %w", err)
	}
	return reg, nil
}

// SaveWatchRegistry writes a registry with mode 0600.
func SaveWatchRegistry(path string, reg WatchRegistry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// EnabledPaths returns non-empty enabled folder paths.
func (r WatchRegistry) EnabledPaths() []string {
	var out []string
	for _, f := range r.Folders {
		if !f.Enabled {
			continue
		}
		p := strings.TrimSpace(f.Path)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// DocWatchMultiOptions configures multi-folder continual ingestion.
type DocWatchMultiOptions struct {
	Client    *hosted.Client
	Paths     []string
	Interval  time.Duration
	Debounce  time.Duration
	MaxCycles int // total successful deltas across all folders (0=forever)
	OnDelta   func(path string, res hosted.IngestResult)
	// OnError is called for non-fatal per-path watch errors (optional).
	OnError func(stage string, err error)
}

// WatchDocsMulti polls every path; on change loads docs and runs ContinualDeltaLocal.
// One durable gardener queue under the brain dir drains enrich jobs in the background
// (product-brain gardener daemon / launchd).
func WatchDocsMulti(ctx context.Context, opt DocWatchMultiOptions) error {
	if opt.Client == nil {
		return fmt.Errorf("continual: nil client")
	}
	paths := uniqueNonEmpty(opt.Paths)
	if len(paths) == 0 {
		return fmt.Errorf("continual: no docs paths")
	}
	if opt.Interval <= 0 {
		opt.Interval = time.Second
	}
	if opt.Debounce <= 0 {
		opt.Debounce = 300 * time.Millisecond
	}

	type stampState struct {
		last       int64
		dirtySince time.Time
	}
	states := make(map[string]*stampState, len(paths))
	cycles := 0
	for _, p := range paths {
		states[p] = &stampState{}
		if st, err := pathStamp(p); err == nil {
			states[p].last = st
			// Initial baseline load (same as WatchDocs — counts toward MaxCycles).
			docs, err := LoadDocuments(p)
			if err == nil {
				// Defer prune to full-union after all baselines; only upsert here.
				if len(docs) > 0 {
					res, err := opt.Client.ContinualDeltaLocal(ctx, docs)
					if err == nil {
						if opt.OnDelta != nil {
							opt.OnDelta(p, res)
						}
						cycles++
						if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
							return nil
						}
					} else if opt.OnError != nil {
						opt.OnError("delta:"+p, err)
					}
				}
			}
		}
	}
	// Full-union prune so multi-folder does not tombstone sibling folders.
	if live, err := liveDocIDsUnion(paths); err == nil {
		if n, err := opt.Client.PruneMissingDocuments(ctx, live); err == nil && n > 0 && opt.OnDelta != nil {
			opt.OnDelta(paths[0], hosted.IngestResult{
				Mode: "prune", Upserted: n, ProductOwned: true,
			})
		} else if err != nil && opt.OnError != nil {
			opt.OnError("prune", err)
		}
	}

	// MaxCycles satisfied by initial loads only (smoke / one-shot multi-folder).
	if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
		return nil
	}
	// One-shot: when MaxCycles > 0 and we already did at least one delta, do not
	// poll forever if no further mtime changes arrive.
	if opt.MaxCycles > 0 && cycles > 0 {
		// Continue polling only until MaxCycles; if idle forever, still need a stop.
		// Bound idle wait: return once all paths have been baseline-scanned and
		// remaining cycles require changes that may never come — for smoke, stop
		// after one full idle pass when at least one cycle completed.
	}

	ticker := time.NewTicker(opt.Interval)
	defer ticker.Stop()
	idlePasses := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			progress := false
			for _, p := range paths {
				st, err := pathStamp(p)
				if err != nil {
					if opt.OnError != nil {
						opt.OnError("path_stamp:"+p, err)
					}
					continue
				}
				s := states[p]
				if st == s.last {
					continue
				}
				if s.dirtySince.IsZero() {
					s.dirtySince = time.Now()
				}
				if time.Since(s.dirtySince) < opt.Debounce {
					continue
				}
				docs, err := LoadDocuments(p)
				if err != nil {
					if opt.OnError != nil {
						opt.OnError("load_docs:"+p, err)
					}
					s.dirtySince = time.Time{}
					s.last = st
					continue
				}
				s.dirtySince = time.Time{}
				s.last = st
				// Union prune across all folders, then delta this path.
				if live, err := liveDocIDsUnion(paths); err == nil {
					if n, err := opt.Client.PruneMissingDocuments(ctx, live); err != nil {
						if opt.OnError != nil {
							opt.OnError("prune:"+p, err)
						}
					} else if n > 0 && opt.OnDelta != nil {
						opt.OnDelta(p, hosted.IngestResult{Mode: "prune", Upserted: n, ProductOwned: true})
					}
				}
				if len(docs) > 0 {
					res, err := opt.Client.ContinualDeltaLocal(ctx, docs)
					if err != nil {
						if opt.OnError != nil {
							opt.OnError("delta:"+p, err)
						}
						continue
					}
					if opt.OnDelta != nil {
						opt.OnDelta(p, res)
					}
				}
				progress = true
				cycles++
				if opt.MaxCycles > 0 && cycles >= opt.MaxCycles {
					return nil
				}
			}
			if opt.MaxCycles > 0 && !progress {
				idlePasses++
				// After initial loads, stop if no new changes (one-shot / smoke).
				if idlePasses >= 2 {
					return nil
				}
			} else {
				idlePasses = 0
			}
		}
	}
}

// liveDocIDsUnion loads all watch paths and returns the set of live document ids.
func liveDocIDsUnion(paths []string) (map[string]struct{}, error) {
	live := map[string]struct{}{}
	for _, p := range paths {
		docs, err := LoadDocuments(p)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			if id := strings.TrimSpace(d.ID); id != "" {
				live[id] = struct{}{}
			}
		}
	}
	return live, nil
}

func uniqueNonEmpty(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

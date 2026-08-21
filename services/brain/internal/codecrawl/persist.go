package codecrawl

import (
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
)

// DefaultIndexFile is the durable codecrawl index basename.
const DefaultIndexFile = "code-index.gob"

// DefaultIndexPath is the canonical project-scoped durable index location.
func DefaultIndexPath(root string) string {
	return filepath.Join(root, ".sentra", DefaultIndexFile)
}

// DurableMeta is sidecar metadata stored with the gob.
type DurableMeta struct {
	Root      string    `json:"root"`
	GitHead   string    `json:"git_head,omitempty"`
	IndexedAt time.Time `json:"indexed_at"`
	Files     int       `json:"files"`
	Schema    string    `json:"schema"`
}

// durableSnap is the gob payload (all fields exported for encoding/gob).
// Schema v3 optionally stores global inverted + symbol maps so warm Load skips
// full rebuildGlobalFromFiles (warm wall-time fix).
//
// Schema v4 (Phase 2) adds the optional FileEdges map so the typed-edge
// projection survives warm loads without re-running AST extraction. The
// field is omitempty-safe: gob decodes missing fields as nil and the
// Index Graph() helper degrades to the heuristic path when nil, so v3
// snapshots (and v2 / v1) still load cleanly. Schema names follow the
// "product-brain.code-index.vN" convention so future loaders can branch.
type durableSnap struct {
	Meta         DurableMeta
	FilePostings map[string]map[string]int
	FileDefs     map[string][]string
	FileRefs     map[string][]string
	FileImps     map[string][]string
	FileHashes   map[string]string
	FileStamps   map[string]FileStamp
	// GlobalInverted: token → path → tf (optional warm acceleration).
	GlobalInverted map[string]map[string]int
	// GlobalDefs / GlobalRefs: symbol name → files.
	GlobalDefs map[string][]string
	GlobalRefs map[string][]string
	// FileEdges is the typed-edge projection added in v4 (Phase 2). Missing
	// in older snapshots: the index degrades to the name-graph heuristic and
	// ImpactReceipt gets an explicit "graph_unavailable" coverage note.
	FileEdges map[string][]Edge
}

func init() {
	gob.Register(durableSnap{})
	gob.Register(map[string]map[string]int{})
	gob.Register(map[string][]string{})
	gob.Register(map[string]string{})
	gob.Register(map[string]FileStamp{})
	gob.Register(FileStamp{})
	gob.Register([]Edge{})
	gob.Register(Edge{})
	gob.Register(map[string][]Edge{})
	gob.Register(Provenance{})
}

// ErrRootMismatch marks a durable index bound to a different workspace root
// than the caller requested. Load paths must fail closed on it; refresh paths
// (OpenOrRefresh) recover by reindexing the caller's root.
var ErrRootMismatch = errors.New("codecrawl: durable index root mismatch")

// ValidateRoot fails closed when the persisted root binding disagrees with the
// caller's root. An empty caller root means no expectation (nil). An index
// with no recorded root cannot prove the binding and is rejected.
func ValidateRoot(meta DurableMeta, root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	if strings.TrimSpace(meta.Root) == "" {
		return fmt.Errorf("%w: index has no root binding, caller root %q",
			ErrRootMismatch, root)
	}
	bound, berr := canonicalRoot(meta.Root)
	want, werr := canonicalRoot(root)
	if berr == nil && werr == nil && bound == want {
		return nil
	}
	return fmt.Errorf("%w: index bound to %q, caller root %q",
		ErrRootMismatch, meta.Root, root)
}

// canonicalRoot normalizes a root for binding comparison, resolving symlinks
// (e.g. /var → /private/var on macOS) so the same workspace compares equal.
func canonicalRoot(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// persistGuards serializes Save/Load within this process per gob path; the
// flock sidecar (lockIndexFile) extends the same coordination across
// processes. Both layers fail closed in Save.
var (
	persistGuardsMu sync.Mutex
	persistGuards   = map[string]*sync.Mutex{}
)

func persistGuard(path string) *sync.Mutex {
	persistGuardsMu.Lock()
	defer persistGuardsMu.Unlock()
	mu, ok := persistGuards[path]
	if !ok {
		mu = &sync.Mutex{}
		persistGuards[path] = mu
	}
	return mu
}

// Save writes a durable index next to path (atomic temp+rename).
//
// Durability contract (issue #42): an in-process mutex plus an advisory flock
// sidecar coordinate concurrent readers/writers; the payload is fsynced
// before rename and the parent directory is fsynced after rename where
// supported; a stale .tmp from an interrupted writer is discarded before
// writing. A crash therefore always leaves old-or-complete-new state, never a
// partial gob.
func (idx *Index) Save(path string, root string) error {
	if idx == nil {
		return fmt.Errorf("codecrawl: save nil index")
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("codecrawl: empty save path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	guard := persistGuard(path)
	guard.Lock()
	defer guard.Unlock()
	release, err := lockIndexFile(path, true)
	if err != nil {
		return err // fail closed: writers must coordinate
	}
	defer release()
	// Ensure globals present before persist (warm load can skip rebuild).
	if len(idx.inverted) == 0 {
		rebuildGlobalFromFiles(idx)
	}
	meta := DurableMeta{
		Root:      root,
		GitHead:   gitHead(root),
		IndexedAt: time.Now().UTC(),
		Files:     len(idx.files),
		Schema:    "product-brain.code-index.v4",
	}
	snap := durableSnap{
		Meta:           meta,
		FilePostings:   idx.filePostings,
		FileDefs:       idx.fileDefs,
		FileRefs:       idx.fileRefs,
		FileImps:       idx.fileImps,
		FileHashes:     idx.fileHashes,
		FileStamps:     idx.fileStamps,
		GlobalInverted: idx.inverted,
		FileEdges:      idx.fileEdges,
	}
	if idx.symbols != nil {
		snap.GlobalDefs = idx.symbols.Defs
		snap.GlobalRefs = idx.symbols.Refs
	}
	tmp := path + ".tmp"
	// Discard an interrupted writer's stale temp file so a crashed Save never
	// blocks recovery; the live gob (old state) stays untouched until rename.
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := gob.NewEncoder(f)
	if err := enc.Encode(&snap); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncParentDir(filepath.Dir(path))
}

// syncParentDir fsyncs a directory after rename so the new gob name is
// durable. Filesystems that provide atomic rename but reject directory fsync
// (EINVAL/ENOTSUP) are tolerated; every other failure fails closed.
func syncParentDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, syscall.ENOTSUP) {
		return syncErr
	}
	return closeErr
}

// Load reads a durable index and rebuilds global inverted + symbol graph. A
// shared lock coordinates with concurrent writers; a corrupt or partial gob
// is a hard decode error, never a silently degraded index.
func Load(path string) (*Index, DurableMeta, error) {
	guard := persistGuard(path)
	guard.Lock()
	defer guard.Unlock()
	// Best-effort shared coordination: a read-only directory must not make a
	// valid gob unreadable (rename atomicity remains the primary guarantee).
	if release, err := lockIndexFile(path, false); err == nil {
		defer release()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, DurableMeta{}, err
	}
	defer f.Close()
	var snap durableSnap
	if err := gob.NewDecoder(f).Decode(&snap); err != nil {
		return nil, DurableMeta{}, err
	}
	idx := newEmptyIndex()
	idx.filePostings = snap.FilePostings
	if idx.filePostings == nil {
		idx.filePostings = map[string]map[string]int{}
	}
	idx.fileDefs = snap.FileDefs
	if idx.fileDefs == nil {
		idx.fileDefs = map[string][]string{}
	}
	idx.fileRefs = snap.FileRefs
	if idx.fileRefs == nil {
		idx.fileRefs = map[string][]string{}
	}
	idx.fileImps = snap.FileImps
	if idx.fileImps == nil {
		idx.fileImps = map[string][]string{}
	}
	idx.fileHashes = snap.FileHashes
	if idx.fileHashes == nil {
		idx.fileHashes = map[string]string{}
	}
	idx.fileStamps = snap.FileStamps
	if idx.fileStamps == nil {
		idx.fileStamps = map[string]FileStamp{}
	}
	// Phase 2 (v4): typed-edge projection. Missing from older snapshots
	// means the index degrades to the Phase 1 name-graph heuristic, which
	// ImpactReceipt surfaces via an explicit "graph_unavailable" note.
	idx.fileEdges = snap.FileEdges
	if idx.fileEdges == nil {
		idx.fileEdges = map[string][]Edge{}
	}
	idx.graph = nil // invalidate cached projection; rebuild on demand
	// Warm acceleration: reuse persisted global inverted when present (v3).
	if len(snap.GlobalInverted) > 0 {
		idx.inverted = snap.GlobalInverted
		for path := range idx.filePostings {
			idx.files[path] = struct{}{}
		}
		sym := newSymbolGraph()
		if snap.GlobalDefs != nil {
			sym.Defs = snap.GlobalDefs
		}
		if snap.GlobalRefs != nil {
			sym.Refs = snap.GlobalRefs
		}
		// Imports still from per-file maps.
		for file, imps := range idx.fileImps {
			for _, im := range imps {
				sym.addImport(file, im)
			}
		}
		idx.symbols = sym
	} else {
		rebuildGlobalFromFiles(idx)
	}
	return idx, snap.Meta, nil
}

// OpenOrRefresh loads a durable index when present and applies stamp/hash delta.
//
// Returns index, stats, whether the gob was rewritten, meta.
func OpenOrRefresh(root, gobPath string, workers int, forceFull bool) (*Index, Stats, bool, DurableMeta, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, Stats{}, false, DurableMeta{}, err
	}
	workers = normalizeWorkers(workers)
	var prev *Index
	var meta DurableMeta
	if !forceFull {
		if p, m, err := Load(gobPath); err == nil {
			// Reject a gob bound to a different workspace (symlink-aware) and
			// reindex rather than serve a mismatched root/index pair.
			if bound, berr := canonicalRoot(m.Root); berr != nil || bound == "" {
				prev = nil
			} else if want, werr := canonicalRoot(rootAbs); werr == nil && bound == want {
				prev = p
				meta = m
			}
		}
	}
	if prev == nil {
		idx, st, err := CrawlDir(rootAbs, workers)
		if err != nil {
			return nil, st, false, DurableMeta{}, err
		}
		ensureHashes(idx, rootAbs)
		if err := idx.Save(gobPath, rootAbs); err != nil {
			return idx, st, false, DurableMeta{}, err
		}
		meta = DurableMeta{
			Root: rootAbs, GitHead: gitHead(rootAbs), IndexedAt: time.Now().UTC(),
			Files: len(idx.files), Schema: "product-brain.code-index.v4",
		}
		return idx, st, true, meta, nil
	}
	// Fully warm: stamps all match and globals already loaded — skip delta walk
	// when prev already has inverted (v3 warm path).
	if prev != nil && len(prev.inverted) > 0 && stAllStampsMatch(rootAbs, prev) {
		st := Stats{
			FilesIndexed:   len(prev.files),
			BytesRead:      0,
			Workers:        workers,
			Duration:       0, // stamp walk only; caller may time outer
			Unchanged:      len(prev.files),
			SkippedByStamp: len(prev.files),
		}
		return prev, st, false, meta, nil
	}
	idx, st, _, err := CrawlDeltaFrom(rootAbs, workers, nil, prev)
	if err != nil {
		return nil, st, false, meta, err
	}
	wrote := st.Changed > 0 || st.Unchanged == 0
	if wrote || meta.GitHead != gitHead(rootAbs) || (meta.Schema != "product-brain.code-index.v2" && meta.Schema != "product-brain.code-index.v3" && meta.Schema != "product-brain.code-index.v4") {
		if err := idx.Save(gobPath, rootAbs); err != nil {
			return idx, st, false, meta, err
		}
		wrote = true
		meta = DurableMeta{
			Root: rootAbs, GitHead: gitHead(rootAbs), IndexedAt: time.Now().UTC(),
			Files: len(idx.files), Schema: "product-brain.code-index.v4",
		}
	}
	return idx, st, wrote, meta, nil
}

// stAllStampsMatch is a cheap walk: every on-disk stamp matches index stamps,
// and every indexed path still exists (no silent deletions).
func stAllStampsMatch(root string, prev *Index) bool {
	if prev == nil || prev.fileStamps == nil || len(prev.fileStamps) == 0 {
		return false
	}
	// The census has to be taken under the same policy that produced the
	// stamps it is compared against.
	//
	// This walked with the hardcoded skipDir set while every other walk in the
	// package loads repoignore, so its file census was a strict superset of
	// the indexed set: any repository with a single ignore rule -- this one
	// has .pytest_cache/ -- failed the len(live) != len(prev.fileStamps) gate
	// permanently, and the warm fast path could never be taken. A fast path
	// that is structurally unreachable is worse than none, because the
	// documentation and the cost model both assume it fires.
	ignores, err := repoignore.Load(root)
	if err != nil {
		return false
	}
	live := map[string]FileStamp{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "" {
			rel = path
		}
		if ignores.Ignored(rel, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			if _, ok := skipDir[info.Name()]; ok {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := extOK[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		live[rel] = FileStamp{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
		return nil
	})
	if len(live) != len(prev.fileStamps) {
		return false
	}
	for rel, old := range prev.fileStamps {
		st, ok := live[rel]
		if !ok || !StampEqual(old, st) {
			return false
		}
	}
	return true
}

// ensureHashes fills fileHashes after a plain CrawlDir (which may not set them).
func ensureHashes(idx *Index, root string) {
	if idx == nil {
		return
	}
	if idx.fileHashes == nil {
		idx.fileHashes = map[string]string{}
	}
	if idx.fileStamps == nil {
		idx.fileStamps = map[string]FileStamp{}
	}
	for rel := range idx.files {
		if _, ok := idx.fileHashes[rel]; ok {
			if _, ok2 := idx.fileStamps[rel]; ok2 {
				continue
			}
		}
		p := filepath.Join(root, rel)
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		// As in IngestPaths: this info came from os.Stat, which follows the
		// link, so the symlink exclusion needs an explicit Lstat.
		if lst, lerr := os.Lstat(p); lerr != nil || !indexableFile(lst) {
			continue
		}
		if !indexableFile(info) {
			continue
		}
		idx.fileStamps[rel] = FileStamp{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
		if _, ok := idx.fileHashes[rel]; ok {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		idx.fileHashes[rel] = FileHash(raw)
	}
}

func gitHead(root string) string {
	cmd := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

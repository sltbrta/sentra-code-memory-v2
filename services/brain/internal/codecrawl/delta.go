package codecrawl

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
)

// FileHash is content identity for delta crawls (stack-graph file incrementality).
func FileHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// CrawlDelta re-indexes a tree with stack-graph file incrementality.
func CrawlDelta(root string, workers int, prevHashes map[string]string) (*Index, Stats, map[string]string, error) {
	return CrawlDeltaFrom(root, workers, prevHashes, nil)
}

// CrawlDeltaFrom reuses per-file subgraphs when mtime+size (stamp) and/or content
// hash match. Warm trees with zero dirty files return prev without re-tokenizing.
//
// Phase 1: walk + stamp; if stamp matches prev, reuse without reading body.
// Phase 2: read+hash only stamp-mismatched / new files; re-tokenize those.
// Phase 3: copy reused subgraphs; rebuild global inverted + symbols.
func CrawlDeltaFrom(root string, workers int, prevHashes map[string]string, prev *Index) (*Index, Stats, map[string]string, error) {
	if workers < 1 {
		workers = 1
	}
	if prevHashes == nil && prev != nil {
		prevHashes = prev.fileHashes
	}
	ignores, err := repoignore.Load(root)
	if err != nil {
		return nil, Stats{}, nil, err
	}
	var paths []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if ignores.Ignored(rel, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if _, ok := extOK[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		paths = append(paths, path)
		return nil
	})

	t0 := time.Now()
	type workItem struct {
		abs, rel string
		raw      []byte
		hash     string
		stamp    FileStamp
		reuse    bool
		// bodyRead is false when reused via stamp (no content).
		bodyRead bool
	}
	items := make([]workItem, 0, len(paths))
	var (
		changed       int64
		skipped       int64
		skippedStamp  int64
		hashed        int64
		errCnt        int64
		bytesFromDisk int64
	)

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			atomic.AddInt64(&errCnt, 1)
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "" {
			rel = path
		}
		stamp := FileStamp{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
		// Stamp hit: reuse without reading content.
		if prev != nil && prev.fileStamps != nil {
			if old, ok := prev.fileStamps[rel]; ok && StampEqual(old, stamp) {
				if _, have := prev.filePostings[rel]; have {
					h := ""
					if prev.fileHashes != nil {
						h = prev.fileHashes[rel]
					}
					items = append(items, workItem{
						abs: path, rel: rel, hash: h, stamp: stamp, reuse: true, bodyRead: false,
					})
					atomic.AddInt64(&skipped, 1)
					atomic.AddInt64(&skippedStamp, 1)
					continue
				}
			}
		}
		// Stamp miss or no prev: read body + hash.
		raw, err := os.ReadFile(path)
		if err != nil {
			atomic.AddInt64(&errCnt, 1)
			continue
		}
		atomic.AddInt64(&bytesFromDisk, int64(len(raw)))
		h := FileHash(raw)
		atomic.AddInt64(&hashed, 1)
		reuse := false
		if prev != nil && prevHashes != nil {
			if old, ok := prevHashes[rel]; ok && old == h {
				if _, have := prev.filePostings[rel]; have {
					reuse = true
				}
			}
		}
		if reuse {
			atomic.AddInt64(&skipped, 1)
		} else {
			atomic.AddInt64(&changed, 1)
		}
		items = append(items, workItem{
			abs: path, rel: rel, raw: raw, hash: h, stamp: stamp, reuse: reuse, bodyRead: true,
		})
	}

	// Fast path: everything reused via stamp → clone prev index + update stamps.
	if prev != nil && int(changed) == 0 && int(skipped) == len(items) && len(items) > 0 {
		idx := cloneIndexShallow(prev)
		// Refresh stamps from walk (same values).
		if idx.fileStamps == nil {
			idx.fileStamps = map[string]FileStamp{}
		}
		// Drop deleted files (present in prev but not in walk).
		live := map[string]struct{}{}
		for _, it := range items {
			live[it.rel] = struct{}{}
			idx.fileStamps[it.rel] = it.stamp
			if it.hash != "" {
				idx.fileHashes[it.rel] = it.hash
			}
		}
		pruneDeleted(idx, live)
		rebuildGlobalFromFiles(idx)
		hashes := map[string]string{}
		for k, v := range idx.fileHashes {
			hashes[k] = v
		}
		st := Stats{
			FilesIndexed:   len(items),
			BytesRead:      0,
			Workers:        workers,
			Duration:       time.Since(t0),
			Errors:         int(errCnt),
			Changed:        0,
			Unchanged:      int(skipped),
			SkippedByStamp: int(skippedStamp),
			Hashed:         int(hashed),
		}
		return idx, st, hashes, nil
	}

	// Phase 2: index only non-reuse files in parallel.
	ch := make(chan workItem, len(items))
	for _, it := range items {
		if !it.reuse {
			ch <- it
		}
	}
	close(ch)

	locals := make([]*localIndex, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		locals[w] = newLocalIndex()
		wg.Add(1)
		go func(loc *localIndex) {
			defer wg.Done()
			for it := range ch {
				loc.add(it.rel, string(it.raw), len(it.raw))
				loc.muHash.Lock()
				if loc.hashes == nil {
					loc.hashes = map[string]string{}
				}
				loc.hashes[it.rel] = it.hash
				if loc.stamps == nil {
					loc.stamps = map[string]FileStamp{}
				}
				loc.stamps[it.rel] = it.stamp
				loc.muHash.Unlock()
			}
		}(locals[w])
	}
	wg.Wait()

	idx := newEmptyIndex()
	var bytesRd int64
	for _, loc := range locals {
		loc.mergeInto(idx)
		bytesRd += loc.bytesRd
	}

	// Phase 3: copy reused file subgraphs from prev.
	if prev != nil {
		for _, it := range items {
			if !it.reuse {
				continue
			}
			copyFileFrom(prev, idx, it.rel, it.hash, it.stamp)
		}
	} else {
		for _, it := range items {
			idx.fileHashes[it.rel] = it.hash
			if idx.fileStamps == nil {
				idx.fileStamps = map[string]FileStamp{}
			}
			idx.fileStamps[it.rel] = it.stamp
		}
	}

	for _, it := range items {
		if _, ok := idx.fileHashes[it.rel]; !ok {
			idx.fileHashes[it.rel] = it.hash
		}
		if idx.fileStamps == nil {
			idx.fileStamps = map[string]FileStamp{}
		}
		idx.fileStamps[it.rel] = it.stamp
	}

	rebuildGlobalFromFiles(idx)

	hashes := map[string]string{}
	for k, v := range idx.fileHashes {
		hashes[k] = v
	}
	st := Stats{
		FilesIndexed:   len(items),
		BytesRead:      bytesFromDisk,
		Workers:        workers,
		Duration:       time.Since(t0),
		Errors:         int(errCnt),
		Changed:        int(changed),
		Unchanged:      int(skipped),
		SkippedByStamp: int(skippedStamp),
		Hashed:         int(hashed),
	}
	_ = bytesRd
	return idx, st, hashes, nil
}

func newEmptyIndex() *Index {
	return &Index{
		inverted:     map[string]map[string]int{},
		files:        map[string]struct{}{},
		symbols:      newSymbolGraph(),
		filePostings: map[string]map[string]int{},
		fileDefs:     map[string][]string{},
		fileRefs:     map[string][]string{},
		fileImps:     map[string][]string{},
		fileHashes:   map[string]string{},
		fileStamps:   map[string]FileStamp{},
	}
}

// copyFileFrom reuses one file's stored subgraph from prev into dst.
func copyFileFrom(prev, dst *Index, rel, hash string, stamp FileStamp) {
	if prev == nil || dst == nil || rel == "" {
		return
	}
	dst.files[rel] = struct{}{}
	if hash != "" {
		dst.fileHashes[rel] = hash
	} else if prev.fileHashes != nil {
		dst.fileHashes[rel] = prev.fileHashes[rel]
	}
	if dst.fileStamps == nil {
		dst.fileStamps = map[string]FileStamp{}
	}
	if stamp.Size > 0 {
		dst.fileStamps[rel] = stamp
	} else if prev.fileStamps != nil {
		dst.fileStamps[rel] = prev.fileStamps[rel]
	}
	if fp, ok := prev.filePostings[rel]; ok {
		cp := make(map[string]int, len(fp))
		for k, v := range fp {
			cp[k] = v
		}
		dst.filePostings[rel] = cp
	}
	if defs, ok := prev.fileDefs[rel]; ok {
		dst.fileDefs[rel] = append([]string(nil), defs...)
	}
	if refs, ok := prev.fileRefs[rel]; ok {
		dst.fileRefs[rel] = append([]string(nil), refs...)
	}
	if imps, ok := prev.fileImps[rel]; ok {
		dst.fileImps[rel] = append([]string(nil), imps...)
	}
}

func cloneIndexShallow(prev *Index) *Index {
	idx := newEmptyIndex()
	if prev == nil {
		return idx
	}
	for path, fp := range prev.filePostings {
		cp := make(map[string]int, len(fp))
		for k, v := range fp {
			cp[k] = v
		}
		idx.filePostings[path] = cp
		idx.files[path] = struct{}{}
	}
	for path, defs := range prev.fileDefs {
		idx.fileDefs[path] = append([]string(nil), defs...)
	}
	for path, refs := range prev.fileRefs {
		idx.fileRefs[path] = append([]string(nil), refs...)
	}
	for path, imps := range prev.fileImps {
		idx.fileImps[path] = append([]string(nil), imps...)
	}
	for path, h := range prev.fileHashes {
		idx.fileHashes[path] = h
	}
	for path, st := range prev.fileStamps {
		idx.fileStamps[path] = st
	}
	return idx
}

func pruneDeleted(idx *Index, live map[string]struct{}) {
	if idx == nil {
		return
	}
	for path := range idx.files {
		if _, ok := live[path]; ok {
			continue
		}
		delete(idx.files, path)
		delete(idx.filePostings, path)
		delete(idx.fileDefs, path)
		delete(idx.fileRefs, path)
		delete(idx.fileImps, path)
		delete(idx.fileHashes, path)
		delete(idx.fileStamps, path)
	}
}

// rebuildGlobalFromFiles reconstructs inverted + SymbolGraph from per-file maps.
func rebuildGlobalFromFiles(idx *Index) {
	if idx == nil {
		return
	}
	inv := map[string]map[string]int{}
	sym := newSymbolGraph()
	for path, fp := range idx.filePostings {
		idx.files[path] = struct{}{}
		for tok, n := range fp {
			if inv[tok] == nil {
				inv[tok] = map[string]int{}
			}
			inv[tok][path] = n
		}
	}
	for file, defs := range idx.fileDefs {
		for _, d := range defs {
			sym.addDef(d, file)
			// Also index lowercased for case-insensitive hop/search.
			if dl := strings.ToLower(d); dl != d {
				sym.addDef(dl, file)
			}
		}
	}
	for file, refs := range idx.fileRefs {
		for _, r := range refs {
			sym.addRef(r, file)
			if rl := strings.ToLower(r); rl != r {
				sym.addRef(rl, file)
			}
		}
	}
	for file, imps := range idx.fileImps {
		for _, p := range imps {
			sym.addImport(file, p)
		}
	}
	idx.inverted = inv
	idx.symbols = sym
}

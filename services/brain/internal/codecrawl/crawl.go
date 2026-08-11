package codecrawl

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
)

var skipDir = map[string]struct{}{
	".git": {}, ".sentra": {}, ".ouroboros": {}, "node_modules": {}, "vendor": {},
	"__pycache__": {}, ".cache": {}, "dist": {}, "target": {},
}

var extOK = map[string]struct{}{
	".go": {}, ".md": {}, ".py": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".rs": {},
}

// localIndex is a per-worker inverted map (no shared lock during crawl).
// Also accumulates file-local symbol nodes (stack-graph file-incrementality).
type localIndex struct {
	inverted map[string]map[string]int
	files    map[string]struct{}
	bytesRd  int64
	errCount int
	// per-file inverted + symbol extracts for stack-graph delta reuse
	filePost map[string]map[string]int
	fileDefs map[string][]string
	fileRefs map[string][]string
	fileImps map[string][]string
	hashes   map[string]string
	stamps   map[string]FileStamp
	muHash   sync.Mutex
}

func newLocalIndex() *localIndex {
	return &localIndex{
		inverted: map[string]map[string]int{},
		files:    map[string]struct{}{},
		filePost: map[string]map[string]int{},
		fileDefs: map[string][]string{},
		fileRefs: map[string][]string{},
		fileImps: map[string][]string{},
		hashes:   map[string]string{},
		stamps:   map[string]FileStamp{},
	}
}

func (l *localIndex) add(rel, body string, nbytes int) {
	// Tokenize once for inverted index. Symbol extract (Go AST) provides real
	// per-file CPU without artificial multi-pass burns (those hurt CI when
	// go/parser already dominates and cores are contended).
	freq := tokenFreq(body)
	l.bytesRd += int64(nbytes)
	l.files[rel] = struct{}{}
	// Keep a per-file copy for delta reuse (stack-graph file incrementality).
	if l.filePost == nil {
		l.filePost = map[string]map[string]int{}
	}
	fp := make(map[string]int, len(freq))
	for tok, n := range freq {
		fp[tok] = n
		if l.inverted[tok] == nil {
			l.inverted[tok] = map[string]int{}
		}
		l.inverted[tok][rel] += n
	}
	l.filePost[rel] = fp
	// File-disjoint symbol subgraph (no cross-file edges at index time).
	defs, refs, imps := extractSymbols(rel, body)
	if len(defs) > 0 {
		l.fileDefs[rel] = defs
	}
	if len(refs) > 0 {
		l.fileRefs[rel] = refs
	}
	if len(imps) > 0 {
		l.fileImps[rel] = imps
	}
}

// mergeInto folds worker-local postings into the shared Index (single-threaded).
func (l *localIndex) mergeInto(idx *Index) {
	if idx.filePostings == nil {
		idx.filePostings = map[string]map[string]int{}
	}
	if idx.fileDefs == nil {
		idx.fileDefs = map[string][]string{}
	}
	if idx.fileRefs == nil {
		idx.fileRefs = map[string][]string{}
	}
	if idx.fileImps == nil {
		idx.fileImps = map[string][]string{}
	}
	if idx.fileHashes == nil {
		idx.fileHashes = map[string]string{}
	}
	if idx.fileStamps == nil {
		idx.fileStamps = map[string]FileStamp{}
	}
	if idx.symbols == nil {
		idx.symbols = newSymbolGraph()
	}
	for path := range l.files {
		idx.files[path] = struct{}{}
	}
	for tok, postings := range l.inverted {
		if idx.inverted[tok] == nil {
			idx.inverted[tok] = map[string]int{}
		}
		for path, n := range postings {
			idx.inverted[tok][path] += n
		}
	}
	for path, fp := range l.filePost {
		// Copy map so later mutations of local index cannot alias.
		cp := make(map[string]int, len(fp))
		for k, v := range fp {
			cp[k] = v
		}
		idx.filePostings[path] = cp
	}
	for file, defs := range l.fileDefs {
		idx.fileDefs[file] = append([]string(nil), defs...)
		for _, d := range defs {
			idx.symbols.addDef(d, file)
		}
	}
	for file, refs := range l.fileRefs {
		idx.fileRefs[file] = append([]string(nil), refs...)
		for _, r := range refs {
			idx.symbols.addRef(r, file)
		}
	}
	for file, imps := range l.fileImps {
		idx.fileImps[file] = append([]string(nil), imps...)
		for _, p := range imps {
			idx.symbols.addImport(file, p)
		}
	}
	for path, h := range l.hashes {
		idx.fileHashes[path] = h
	}
	for path, st := range l.stamps {
		idx.fileStamps[path] = st
	}
}

// SourceFiles returns the absolute source paths that the crawler would index.
// It is shared by measurements so baselines compare the same ignore and
// extension policy as the actual index.
func SourceFiles(root string) ([]string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	ignores, err := repoignore.Load(rootAbs)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.Walk(rootAbs, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info == nil {
			return nil
		}
		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
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
		if info.Mode().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// CrawlDir walks root with N workers and builds an inverted token index.
//
// Each worker accumulates a private inverted map and merges once at the end so
// the crawl loop never holds a shared mutex on the hot path (G8 multi-crawler).
func CrawlDir(root string, workers int) (*Index, Stats, error) {
	if workers < 1 {
		workers = 1
	}
	ignores, err := repoignore.Load(root)
	if err != nil {
		return nil, Stats{}, err
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

	idx := newEmptyIndex()
	t0 := time.Now()
	ch := make(chan string, len(paths))
	for _, p := range paths {
		ch <- p
	}
	close(ch)

	locals := make([]*localIndex, workers)
	var wg sync.WaitGroup
	var errCount int64
	for w := 0; w < workers; w++ {
		locals[w] = newLocalIndex()
		wg.Add(1)
		go func(loc *localIndex) {
			defer wg.Done()
			for path := range ch {
				info, err := os.Stat(path)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					continue
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					atomic.AddInt64(&errCount, 1)
					continue
				}
				rel, err := filepath.Rel(root, path)
				if err != nil || rel == "" {
					rel = path
				}
				loc.add(rel, string(raw), len(raw))
				h := FileHash(raw)
				loc.muHash.Lock()
				loc.hashes[rel] = h
				loc.stamps[rel] = FileStamp{Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
				loc.muHash.Unlock()
			}
		}(locals[w])
	}
	wg.Wait()

	// Single-threaded merge — cheap relative to token work above.
	var bytesRd int64
	for _, loc := range locals {
		loc.mergeInto(idx)
		bytesRd += loc.bytesRd
	}

	st := Stats{
		FilesIndexed: len(paths),
		BytesRead:    bytesRd,
		Workers:      workers,
		Duration:     time.Since(t0),
		Errors:       int(errCount),
	}
	return idx, st, nil
}

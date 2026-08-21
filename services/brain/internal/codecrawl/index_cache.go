package codecrawl

import (
	"os"
	"sync"
)

// Every verb call gob-decoded the whole index.
//
// OpenOrRefresh reads the persisted index to decide whether the tree is warm,
// and that read is a full gob decode under a file lock. Measured on this
// repository (1,067 indexed files): 66ms per decode, against 136ms for a
// served code_search -- so roughly half the cost of answering a query was
// re-reading an index the process had already read, for every query.
//
// The cache holds the decoded index per gob path, keyed on the file's identity
// so an external rewrite -- another process, a `code_index --force`, a watch
// refresh -- is picked up rather than served stale.
//
// It is deliberately narrow. Only OpenOrRefresh's warm read uses it, and only
// the fully-warm path hands the shared index back, so every caller that
// mutates an index either goes through Load (which still decodes a private
// copy) or through the force path (which builds a fresh one). Sharing one
// Index between concurrent readers is what made Graph()'s lazy assignment a
// real race rather than a latent one; it is synchronised now.

// cachedIndex is one decoded index and the file identity it was decoded from.
type cachedIndex struct {
	index   *Index
	meta    DurableMeta
	size    int64
	modTime int64
}

var indexCache = struct {
	mu      sync.RWMutex
	entries map[string]cachedIndex
}{entries: map[string]cachedIndex{}}

// indexFileIdentity returns the size and modification time of path, and
// whether it could be read at all.
func indexFileIdentity(path string) (size, modTime int64, ok bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, 0, false
	}
	return info.Size(), info.ModTime().UnixNano(), true
}

// loadCached returns the decoded index for path, decoding it only when the
// file has changed since it was last read.
//
// The returned index is shared: callers must not mutate it. Everything that
// does takes Load or the force path instead.
func loadCached(path string) (*Index, DurableMeta, error) {
	size, modTime, ok := indexFileIdentity(path)
	if !ok {
		// No readable file: fall through to Load, which produces the real
		// error the caller expects.
		return Load(path)
	}

	indexCache.mu.RLock()
	entry, hit := indexCache.entries[path]
	indexCache.mu.RUnlock()
	if hit && entry.size == size && entry.modTime == modTime {
		return entry.index, entry.meta, nil
	}

	index, meta, err := Load(path)
	if err != nil {
		return nil, DurableMeta{}, err
	}
	// Re-stat after the decode. A rewrite that landed while it was in flight
	// would otherwise be cached under the pre-write identity and served until
	// the next change, which is the one way a cache like this goes stale
	// permanently rather than briefly.
	afterSize, afterModTime, afterOK := indexFileIdentity(path)
	if !afterOK || afterSize != size || afterModTime != modTime {
		return index, meta, nil
	}

	indexCache.mu.Lock()
	indexCache.entries[path] = cachedIndex{
		index: index, meta: meta, size: size, modTime: modTime,
	}
	indexCache.mu.Unlock()
	return index, meta, nil
}

// invalidateCachedIndex drops path's cached entry. Save calls it so a writer
// in this process never leaves a reader holding the previous index.
func invalidateCachedIndex(path string) {
	indexCache.mu.Lock()
	delete(indexCache.entries, path)
	indexCache.mu.Unlock()
}

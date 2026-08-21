package productsearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
)

// code_exact re-read and re-parsed the whole repository on every query.
//
// searchCodeExact walks the tree, reads each P5 source, and projects it
// through codeindex -- a real parse per file per query. Measured on this
// repository: 400ms per code_exact call, against 76ms for a code_search.
// Nothing was reused between two identical queries a second apart.
//
// The parse cannot simply be skipped once enough hits are collected: the
// result's receipt digest covers every file's projection digest, so an early
// exit would change the receipt. What can be avoided is parsing content that
// has not changed, which is almost all of it.
//
// codeindex.Project is a pure function of (content, limits), so a projection
// keyed on the content hash is exactly the value the parse would have
// produced. The cache is keyed on the hash rather than on mtime and size
// because the input here is bytes already in memory -- there is no file
// identity to consult, and a hash of what is being parsed cannot be stale.

// maxCachedProjections bounds the cache. A projection holds one file's
// occurrences, so this is memory-shaped rather than count-shaped; the eviction
// is a wholesale clear because a query walks the entire repository and an LRU
// over that access pattern evicts precisely what is about to be read again.
const maxCachedProjections = 8192

type projectionCacheEntry struct {
	projection codeindex.FileProjection
}

var projectionCache = struct {
	mu      sync.Mutex
	entries map[string]projectionCacheEntry
}{entries: map[string]projectionCacheEntry{}}

// projectCached returns the projection of source, parsing it only when this
// exact content and limits have not been seen.
func projectCached(
	ctx context.Context, source codeindex.SourceFile, limits codeindex.Limits,
) (codeindex.FileProjection, error) {
	key := projectionKey(source, limits)

	projectionCache.mu.Lock()
	entry, hit := projectionCache.entries[key]
	projectionCache.mu.Unlock()
	if hit {
		return entry.projection, nil
	}

	projection, err := codeindex.Project(ctx, source, limits)
	if err != nil {
		return codeindex.FileProjection{}, err
	}

	projectionCache.mu.Lock()
	if len(projectionCache.entries) >= maxCachedProjections {
		projectionCache.entries = make(map[string]projectionCacheEntry, maxCachedProjections)
	}
	projectionCache.entries[key] = projectionCacheEntry{projection: projection}
	projectionCache.mu.Unlock()
	return projection, nil
}

// projectionKey identifies the exact parse a projection is the result of. The
// path is included because it appears in the projection's own ranges, so two
// identical files at different paths are not the same projection.
func projectionKey(source codeindex.SourceFile, limits codeindex.Limits) string {
	sum := sha256.New()
	_, _ = sum.Write([]byte(source.Path))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write([]byte(source.Language))
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write(source.Content)
	_, _ = sum.Write([]byte{0})
	_, _ = sum.Write(limitsFingerprint(limits))
	return hex.EncodeToString(sum.Sum(nil))
}

// limitsFingerprint renders the limits that affect a projection's content.
// Every field is included rather than the ones that look relevant, so adding a
// limit cannot silently make the cache serve a projection produced under a
// different one.
func limitsFingerprint(limits codeindex.Limits) []byte {
	return fmt.Appendf(nil, "%d/%d/%d/%d/%d/%d",
		limits.MaxFiles, limits.MaxInputBytes, limits.MaxTokens,
		limits.MaxResults, limits.MaxLines, limits.MaxColumn)
}

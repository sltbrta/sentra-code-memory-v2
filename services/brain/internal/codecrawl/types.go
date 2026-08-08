package codecrawl

import "time"

// Index is an in-memory inverted token map over crawled source files.
// Keys are lowercased alphanumeric tokens; values map relative path → term frequency.
// Optionally holds a file-incremental SymbolGraph (stack-graph inspired) for hop search.
//
// Per-file maps (filePostings / fileDefs / fileRefs / fileImps / fileHashes / fileStamps)
// enable stack-graph-style delta: unchanged files reuse stored subgraphs without
// re-tokenization. Stamps (mtime+size) skip content reads on warm trees.
type Index struct {
	// inverted maps token → path → term frequency in that file.
	inverted map[string]map[string]int
	// files is the set of indexed relative paths (from crawl root).
	files map[string]struct{}
	// symbols is file-disjoint name bindings; cross-file hop is query-time.
	symbols *SymbolGraph
	// filePostings: path → token → tf (delta reuse).
	filePostings map[string]map[string]int
	// Per-file symbol subgraphs (file-disjoint at index time).
	fileDefs map[string][]string
	fileRefs map[string][]string
	fileImps map[string][]string
	// fileHashes: path → content hash (FileHash).
	fileHashes map[string]string
	// fileStamps: path → mtime+size (skip read when unchanged).
	fileStamps map[string]FileStamp
}

// FileStamp is a cheap identity probe before content hash.
type FileStamp struct {
	Size    int64
	MtimeNs int64
}

// Stats records crawl execution metrics.
type Stats struct {
	FilesIndexed int
	BytesRead    int64
	Workers      int
	Duration     time.Duration
	// Errors is a count of files that failed to read (skipped, not fatal).
	Errors int
	// Changed/Unchanged count content-hash delta opportunity (CrawlDelta).
	Changed   int
	Unchanged int
	// SkippedByStamp: files reused via mtime+size without content read.
	SkippedByStamp int
	// Hashed: files whose content was hashed (stamp miss or force).
	Hashed int
	// QueueDepth is the number of coalesced watch paths still pending after a
	// refresh attempt.
	QueueDepth int
	// RetryCount is the consecutive refresh retry count at observation time.
	RetryCount int
	// FullRescan reports that the bounded event queue overflowed.
	FullRescan bool
}

// Hit is one ranked file result from Search.
type Hit struct {
	Path  string
	Score float64
}

// Files returns a copy of indexed relative paths.
func (idx *Index) Files() []string {
	if idx == nil || idx.files == nil {
		return nil
	}
	out := make([]string, 0, len(idx.files))
	for p := range idx.files {
		out = append(out, p)
	}
	return out
}

// FileCount is len(files).
func (idx *Index) FileCount() int {
	if idx == nil {
		return 0
	}
	return len(idx.files)
}

// StampEqual reports whether two stamps match.
func StampEqual(a, b FileStamp) bool {
	return a.Size == b.Size && a.MtimeNs == b.MtimeNs && a.Size > 0
}

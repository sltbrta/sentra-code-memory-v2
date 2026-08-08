package hosted

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

// The v2 layout deliberately uses fixed-width little-endian tables. Loading a
// current snapshot maps the file and validates it without rebuilding corpus
// maps, slices, or strings on the Go heap.
const (
	hotLexMagic          = "HOTLEX2\x00"
	hotLexFormatVersion  = uint16(2)
	hotLexHeaderSize     = uint64(256)
	hotLexDocRecordSize  = uint64(64)
	hotLexTermRecordSize = uint64(32)
	hotLexPostingSize    = uint64(8)

	hotLexDigestOffset = 128
	hotLexDigestSize   = sha256.Size

	// Hard decode/mapping limits turn counts and offsets into an input contract,
	// not allocation suggestions. They cover the current 1.55M-chunk corpus
	// with substantial headroom while rejecting accidental or hostile bombs.
	hotLexMaxFileBytes   = uint64(8 << 30)
	hotLexMaxDocs        = uint64(5_000_000)
	hotLexMaxTerms       = uint64(20_000_000)
	hotLexMaxPostings    = uint64(250_000_000)
	hotLexMaxStringBytes = uint64(3 << 30)
	hotLexMaxScopeBytes  = uint64(4096)

	hotDocFlagHasText = uint32(1)
)

var (
	ErrHotLexCorrupt = errors.New("hotlex: corrupt snapshot")
	ErrHotLexStale   = errors.New("hotlex: stale generation")
	ErrHotLexScope   = errors.New("hotlex: wrong brain scope")
	ErrHotLexLegacy  = errors.New("hotlex: legacy gob recovery disabled")
)

// HotLexSnapshotFormat is the validated representation backing an index.
// Mutable means the index was built in memory rather than loaded from disk.
type HotLexSnapshotFormat string

const (
	HotLexFormatMutable   HotLexSnapshotFormat = "mutable"
	HotLexFormatHOTLEX2   HotLexSnapshotFormat = "hotlex2"
	HotLexFormatLegacyGob HotLexSnapshotFormat = "legacy-gob"
)

// SnapshotFormat reports the validated source representation for migration
// diagnostics. It does not inspect filename extensions.
func (h *HotLex) SnapshotFormat() HotLexSnapshotFormat {
	if h == nil {
		return HotLexFormatMutable
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.sourceFormat != "" {
		return h.sourceFormat
	}
	return HotLexFormatMutable
}

// LegacyRollbackPath is the stable sidecar used to preserve a pre-migration
// gob before a HOTLEX2 publication replaces the historical serving path.
func LegacyRollbackPath(path string) string { return path + ".rollback.gob" }

// HotLexSnapshotScope binds a load to the caller's current serving scope.
// Empty expected values mean "not available to this caller", not wildcards
// embedded in the snapshot. AllowLegacyGob is recovery-only; current writes
// always use the v2 format even when the historical filename ends in .gob.
type HotLexSnapshotScope struct {
	BrainID        string
	Generation     string
	AllowLegacyGob bool
}

type hotLexMapped struct {
	data  []byte
	unmap func() error

	docsOffset     int
	docCount       int
	termsOffset    int
	termCount      int
	postingsOffset int
	postingCount   int
	stringsOffset  int
	stringsSize    int
}

// LoadHotLexGob retains the established API and filename compatibility. New
// snapshots take the zero-decode mmap path; old gob bytes take the explicitly
// bounded recovery decoder.
func LoadHotLexGob(path string) (*HotLex, error) {
	return LoadHotLexSnapshot(path, HotLexSnapshotScope{AllowLegacyGob: true})
}

// LoadHotLexSnapshot loads and scope-checks one published snapshot.
func LoadHotLexSnapshot(path string, scope HotLexSnapshotScope) (*HotLex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || uint64(st.Size()) > hotLexMaxFileBytes {
		return nil, fmt.Errorf("%w: invalid file size %d", ErrHotLexCorrupt, st.Size())
	}
	if uint64(st.Size()) > uint64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("%w: address space", ErrHotLexCorrupt)
	}
	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrHotLexCorrupt, err)
	}
	if string(magic[:]) != hotLexMagic {
		if !scope.AllowLegacyGob {
			return nil, fmt.Errorf("%w: detected non-HOTLEX2 bytes; enable explicit legacy recovery", ErrHotLexLegacy)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return loadLegacyHotLexGob(f, uint64(st.Size()), scope)
	}
	data, unmap, err := mapHotLexFile(f, int(st.Size()))
	if err != nil {
		return nil, fmt.Errorf("hotlex: mmap: %w", err)
	}
	h, err := parseMappedHotLex(data, unmap, scope)
	if err != nil {
		_ = unmap()
		return nil, err
	}
	runtime.SetFinalizer(h, func(index *HotLex) { _ = index.Close() })
	return h, nil
}

func parseMappedHotLex(data []byte, unmap func() error, scope HotLexSnapshotScope) (*HotLex, error) {
	if len(data) < int(hotLexHeaderSize) || string(data[:8]) != hotLexMagic {
		return nil, fmt.Errorf("%w: short or bad header", ErrHotLexCorrupt)
	}
	if binary.LittleEndian.Uint16(data[8:10]) != hotLexFormatVersion ||
		binary.LittleEndian.Uint16(data[10:12]) != uint16(hotLexHeaderSize) {
		return nil, fmt.Errorf("%w: unsupported version/header", ErrHotLexCorrupt)
	}
	if binary.LittleEndian.Uint32(data[12:16]) != 0 {
		return nil, fmt.Errorf("%w: unknown flags", ErrHotLexCorrupt)
	}
	fileSize := binary.LittleEndian.Uint64(data[16:24])
	if fileSize != uint64(len(data)) || fileSize > hotLexMaxFileBytes {
		return nil, fmt.Errorf("%w: file size", ErrHotLexCorrupt)
	}

	// The digest authenticates both header offsets/counts and every payload byte.
	header := make([]byte, int(hotLexHeaderSize))
	copy(header, data[:int(hotLexHeaderSize)])
	wantDigest := append([]byte(nil), header[hotLexDigestOffset:hotLexDigestOffset+hotLexDigestSize]...)
	clear(header[hotLexDigestOffset : hotLexDigestOffset+hotLexDigestSize])
	digest := sha256.New()
	_, _ = digest.Write(header)
	_, _ = digest.Write(data[int(hotLexHeaderSize):])
	if subtle.ConstantTimeCompare(wantDigest, digest.Sum(nil)) != 1 {
		return nil, fmt.Errorf("%w: checksum", ErrHotLexCorrupt)
	}

	docsOffset := binary.LittleEndian.Uint64(data[24:32])
	docCount := binary.LittleEndian.Uint64(data[32:40])
	termsOffset := binary.LittleEndian.Uint64(data[40:48])
	termCount := binary.LittleEndian.Uint64(data[48:56])
	postingsOffset := binary.LittleEndian.Uint64(data[56:64])
	postingCount := binary.LittleEndian.Uint64(data[64:72])
	stringsOffset := binary.LittleEndian.Uint64(data[72:80])
	stringsSize := binary.LittleEndian.Uint64(data[80:88])
	if docCount > hotLexMaxDocs || termCount > hotLexMaxTerms ||
		postingCount > hotLexMaxPostings || stringsSize > hotLexMaxStringBytes {
		return nil, fmt.Errorf("%w: count limit", ErrHotLexCorrupt)
	}
	wantTerms, ok := checkedTableEnd(docsOffset, docCount, hotLexDocRecordSize)
	if !ok || docsOffset != hotLexHeaderSize || wantTerms != termsOffset {
		return nil, fmt.Errorf("%w: document section", ErrHotLexCorrupt)
	}
	wantPostings, ok := checkedTableEnd(termsOffset, termCount, hotLexTermRecordSize)
	if !ok || wantPostings != postingsOffset {
		return nil, fmt.Errorf("%w: term section", ErrHotLexCorrupt)
	}
	wantStrings, ok := checkedTableEnd(postingsOffset, postingCount, hotLexPostingSize)
	if !ok || wantStrings != stringsOffset || stringsOffset+stringsSize != fileSize || stringsOffset+stringsSize < stringsOffset {
		return nil, fmt.Errorf("%w: postings/string section", ErrHotLexCorrupt)
	}
	if fileSize > uint64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("%w: address space", ErrHotLexCorrupt)
	}

	m := &hotLexMapped{
		data: data, unmap: unmap,
		docsOffset: int(docsOffset), docCount: int(docCount),
		termsOffset: int(termsOffset), termCount: int(termCount),
		postingsOffset: int(postingsOffset), postingCount: int(postingCount),
		stringsOffset: int(stringsOffset), stringsSize: int(stringsSize),
	}
	brainOffset := binary.LittleEndian.Uint64(data[88:96])
	brainLen := binary.LittleEndian.Uint32(data[96:100])
	genLen := binary.LittleEndian.Uint32(data[100:104])
	genOffset := binary.LittleEndian.Uint64(data[104:112])
	if uint64(brainLen) > hotLexMaxScopeBytes || uint64(genLen) > hotLexMaxScopeBytes {
		return nil, fmt.Errorf("%w: scope length", ErrHotLexCorrupt)
	}
	brainBytes, ok := m.stringBytes(brainOffset, brainLen)
	if !ok || !utf8.Valid(brainBytes) {
		return nil, fmt.Errorf("%w: brain scope", ErrHotLexCorrupt)
	}
	genBytes, ok := m.stringBytes(genOffset, genLen)
	if !ok || !utf8.Valid(genBytes) {
		return nil, fmt.Errorf("%w: generation", ErrHotLexCorrupt)
	}
	brainID, generation := string(brainBytes), string(genBytes)
	if err := checkHotLexScope(scope, brainID, generation); err != nil {
		return nil, err
	}
	avgDL := math.Float64frombits(binary.LittleEndian.Uint64(data[112:120]))
	sumLen := int64(binary.LittleEndian.Uint64(data[120:128]))
	if math.IsNaN(avgDL) || math.IsInf(avgDL, 0) || avgDL < 0 || sumLen < 0 ||
		(docCount == 0 && (avgDL != 0 || sumLen != 0)) ||
		(docCount > 0 && (avgDL <= 0 || sumLen <= 0)) {
		return nil, fmt.Errorf("%w: document statistics", ErrHotLexCorrupt)
	}
	if err := m.validate(sumLen, avgDL); err != nil {
		return nil, err
	}
	return &HotLex{
		BrainID: brainID, Generation: generation, N: int(docCount), AvgDL: avgDL,
		sumLen: sumLen, mapped: m, sourceFormat: HotLexFormatHOTLEX2,
	}, nil
}

func checkedTableEnd(offset, count, width uint64) (uint64, bool) {
	if width != 0 && count > (^uint64(0)-offset)/width {
		return 0, false
	}
	return offset + count*width, true
}

func (m *hotLexMapped) stringBytes(offset uint64, length uint32) ([]byte, bool) {
	end := offset + uint64(length)
	if end < offset || end > uint64(m.stringsSize) {
		return nil, false
	}
	start := m.stringsOffset + int(offset)
	return m.data[start : start+int(length)], true
}

func (m *hotLexMapped) docRecord(i int) []byte {
	start := m.docsOffset + i*int(hotLexDocRecordSize)
	return m.data[start : start+int(hotLexDocRecordSize)]
}

func (m *hotLexMapped) termRecord(i int) []byte {
	start := m.termsOffset + i*int(hotLexTermRecordSize)
	return m.data[start : start+int(hotLexTermRecordSize)]
}

func (m *hotLexMapped) postingRecord(i int) []byte {
	start := m.postingsOffset + i*int(hotLexPostingSize)
	return m.data[start : start+int(hotLexPostingSize)]
}

func (m *hotLexMapped) validate(wantSum int64, wantAvg float64) error {
	var sum int64
	var previousChunk []byte
	for i := 0; i < m.docCount; i++ {
		r := m.docRecord(i)
		chunk, ok := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
		if !ok || len(chunk) == 0 || !utf8.Valid(chunk) || (i > 0 && bytes.Compare(previousChunk, chunk) >= 0) {
			return fmt.Errorf("%w: document order/string at %d", ErrHotLexCorrupt, i)
		}
		for _, field := range [][2]int{{16, 12}, {24, 32}, {40, 36}} {
			offset := binary.LittleEndian.Uint64(r[field[0] : field[0]+8])
			length := binary.LittleEndian.Uint32(r[field[1] : field[1]+4])
			value, valid := m.stringBytes(offset, length)
			if !valid || !utf8.Valid(value) {
				return fmt.Errorf("%w: document metadata at %d", ErrHotLexCorrupt, i)
			}
		}
		length := binary.LittleEndian.Uint32(r[48:52])
		flags := binary.LittleEndian.Uint32(r[52:56])
		if length == 0 || flags&^hotDocFlagHasText != 0 {
			return fmt.Errorf("%w: document fields at %d", ErrHotLexCorrupt, i)
		}
		textLen := binary.LittleEndian.Uint32(r[36:40])
		if (flags&hotDocFlagHasText != 0) != (textLen > 0) {
			return fmt.Errorf("%w: document text flag at %d", ErrHotLexCorrupt, i)
		}
		if int64(length) > math.MaxInt64-sum {
			return fmt.Errorf("%w: length overflow", ErrHotLexCorrupt)
		}
		sum += int64(length)
		previousChunk = chunk
	}
	if sum != wantSum || (m.docCount > 0 && math.Float64bits(float64(sum)/float64(m.docCount)) != math.Float64bits(wantAvg)) {
		return fmt.Errorf("%w: inconsistent document statistics", ErrHotLexCorrupt)
	}

	var previousTerm []byte
	nextPosting := uint64(0)
	for i := 0; i < m.termCount; i++ {
		r := m.termRecord(i)
		term, ok := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
		if !ok || len(term) == 0 || !utf8.Valid(term) || (i > 0 && bytes.Compare(previousTerm, term) >= 0) {
			return fmt.Errorf("%w: term order/string at %d", ErrHotLexCorrupt, i)
		}
		if binary.LittleEndian.Uint32(r[12:16]) != 0 {
			return fmt.Errorf("%w: term flags at %d", ErrHotLexCorrupt, i)
		}
		start := binary.LittleEndian.Uint64(r[16:24])
		count := binary.LittleEndian.Uint64(r[24:32])
		if count == 0 || start != nextPosting || start+count < start || start+count > uint64(m.postingCount) {
			return fmt.Errorf("%w: posting range at %d", ErrHotLexCorrupt, i)
		}
		var previousDoc uint32
		for j := uint64(0); j < count; j++ {
			p := m.postingRecord(int(start + j))
			doc := binary.LittleEndian.Uint32(p[0:4])
			tf := binary.LittleEndian.Uint32(p[4:8])
			if uint64(doc) >= uint64(m.docCount) || tf == 0 || (j > 0 && doc <= previousDoc) {
				return fmt.Errorf("%w: posting at term=%d offset=%d", ErrHotLexCorrupt, i, j)
			}
			previousDoc = doc
		}
		nextPosting += count
		previousTerm = term
	}
	if nextPosting != uint64(m.postingCount) || (m.termCount == 0) != (m.postingCount == 0) {
		return fmt.Errorf("%w: unclaimed postings", ErrHotLexCorrupt)
	}
	return nil
}

func (h *HotLex) searchMappedLocked(query string, limit int, opts HotLexSearchOptions) ([]Hit, HotLexSearchStats) {
	var stats HotLexSearchStats
	m := h.mapped
	if m == nil {
		return nil, stats
	}
	qtoks := hotTokenize(query)
	if len(qtoks) == 0 {
		return nil, stats
	}
	seenQ := map[string]struct{}{}
	terms := make([]string, 0, len(qtoks))
	for _, term := range qtoks {
		if _, seen := seenQ[term]; seen {
			continue
		}
		seenQ[term] = struct{}{}
		terms = append(terms, term)
	}
	params := defaultBM25()
	scores := map[int]float64{}
	N, avgDL := float64(h.N), h.AvgDL
	if avgDL < 1 {
		avgDL = 1
	}
	for _, term := range terms {
		start, count, ok := m.findTerm(term)
		if !ok {
			continue
		}
		if hotPruneStats(&stats, term, count, opts.MaxDocumentFrequency) {
			continue
		}
		df := float64(count)
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))
		for i := 0; i < count; i++ {
			p := m.postingRecord(start + i)
			doc := int(binary.LittleEndian.Uint32(p[0:4]))
			tf := float64(binary.LittleEndian.Uint32(p[4:8]))
			dl := float64(binary.LittleEndian.Uint32(m.docRecord(doc)[48:52]))
			if dl < 1 {
				dl = 1
			}
			scores[doc] += idf * (tf * (params.K1 + 1)) /
				(tf + params.K1*(1-params.B+params.B*dl/avgDL))
		}
	}
	stats.MatchedDocs = len(scores)
	// Same bounded selection and strict total order as the mutable path.
	arr := hotTopKMap(scores, limit, func(a, b hotScored) bool {
		if scoreOrder := hotScoreCompare(a.s, b.s); scoreOrder != 0 {
			return scoreOrder > 0
		}
		return bytes.Compare(m.docChunkID(a.i), m.docChunkID(b.i)) < 0
	})
	limit = len(arr)
	out := make([]Hit, 0, limit)
	for i := 0; i < limit; i++ {
		r := m.docRecord(arr[i].i)
		chunk, _ := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
		dsid, _ := m.stringBytes(binary.LittleEndian.Uint64(r[16:24]), binary.LittleEndian.Uint32(r[12:16]))
		uri, _ := m.stringBytes(binary.LittleEndian.Uint64(r[24:32]), binary.LittleEndian.Uint32(r[32:36]))
		hit := Hit{ChunkID: string(chunk), DSID: string(dsid), SourceURI: string(uri), Score: arr[i].s, Channel: "hot_lex"}
		if binary.LittleEndian.Uint32(r[52:56])&hotDocFlagHasText != 0 {
			body, _ := m.stringBytes(binary.LittleEndian.Uint64(r[40:48]), binary.LittleEndian.Uint32(r[36:40]))
			hit.Text = string(body)
		}
		out = append(out, hit)
	}
	return out, stats
}

func (m *hotLexMapped) findTerm(term string) (int, int, bool) {
	want := []byte(term)
	i := sort.Search(m.termCount, func(i int) bool {
		r := m.termRecord(i)
		got, _ := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
		return bytes.Compare(got, want) >= 0
	})
	if i == m.termCount {
		return 0, 0, false
	}
	r := m.termRecord(i)
	got, _ := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
	if !bytes.Equal(got, want) {
		return 0, 0, false
	}
	return int(binary.LittleEndian.Uint64(r[16:24])), int(binary.LittleEndian.Uint64(r[24:32])), true
}

func (m *hotLexMapped) docChunkID(i int) []byte {
	r := m.docRecord(i)
	value, _ := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
	return value
}

// Close releases the file mapping. Client.Close calls this for deterministic
// lifecycle; a finalizer is only a leak safety net for direct loader callers.
func (h *HotLex) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mapped == nil {
		return nil
	}
	runtime.SetFinalizer(h, nil)
	unmap := h.mapped.unmap
	h.mapped.data = nil
	h.mapped = nil
	h.N = 0
	h.AvgDL = 0
	h.sumLen = 0
	if unmap != nil {
		return unmap()
	}
	return nil
}

func (h *HotLex) materializeMapped() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.materializeMappedLocked()
}

// materializeMappedLocked is the bounded compatibility path for code that
// mutates an index loaded from disk. Normal serving never calls it; local
// incremental writes rebuild from their authoritative chunk store instead.
func (h *HotLex) materializeMappedLocked() {
	m := h.mapped
	if m == nil {
		return
	}
	docs := make([]hotDoc, m.docCount)
	byChunk := make(map[string]int, m.docCount)
	for i := range docs {
		r := m.docRecord(i)
		chunk, _ := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
		dsid, _ := m.stringBytes(binary.LittleEndian.Uint64(r[16:24]), binary.LittleEndian.Uint32(r[12:16]))
		uri, _ := m.stringBytes(binary.LittleEndian.Uint64(r[24:32]), binary.LittleEndian.Uint32(r[32:36]))
		body, _ := m.stringBytes(binary.LittleEndian.Uint64(r[40:48]), binary.LittleEndian.Uint32(r[36:40]))
		docs[i] = hotDoc{
			ChunkID: string(chunk), DSID: string(dsid), SourceURI: string(uri), Text: string(body),
			Length:  int32(binary.LittleEndian.Uint32(r[48:52])),
			HasText: binary.LittleEndian.Uint32(r[52:56])&hotDocFlagHasText != 0,
		}
		byChunk[docs[i].ChunkID] = i
	}
	postings := make(map[string][]hotPosting, m.termCount)
	for i := 0; i < m.termCount; i++ {
		r := m.termRecord(i)
		term, _ := m.stringBytes(binary.LittleEndian.Uint64(r[0:8]), binary.LittleEndian.Uint32(r[8:12]))
		start := int(binary.LittleEndian.Uint64(r[16:24]))
		count := int(binary.LittleEndian.Uint64(r[24:32]))
		plist := make([]hotPosting, count)
		for j := range plist {
			p := m.postingRecord(start + j)
			plist[j] = hotPosting{Doc: int32(binary.LittleEndian.Uint32(p[0:4])), TF: int32(binary.LittleEndian.Uint32(p[4:8]))}
		}
		postings[string(term)] = plist
	}
	runtime.SetFinalizer(h, nil)
	h.docs, h.byChunk, h.postings = docs, byChunk, postings
	h.mapped = nil
	if m.unmap != nil {
		_ = m.unmap()
	}
}

type hotLexWritePlan struct {
	docOrder    []int
	oldToNew    []uint32
	terms       []string
	postings    uint64
	strings     uint64
	sumLen      int64
	avgDL       float64
	docsOffset  uint64
	termsOffset uint64
	postsOffset uint64
	strsOffset  uint64
	fileSize    uint64
}

// SaveGob atomically publishes HOTLEX2. If path currently contains a validated
// legacy gob, its exact bytes are first preserved at LegacyRollbackPath(path).
// This one-time migration sidecar lets a gob-only binary recover the old image.
func (h *HotLex) SaveGob(path string) error {
	if h == nil {
		return errors.New("hotlex: nil")
	}
	h.materializeMapped()
	if err := h.preserveLegacyRollback(path); err != nil {
		return err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	plan, err := h.makeWritePlanLocked()
	if err != nil {
		return err
	}
	return h.writeSnapshotLocked(path, plan)
}

// SaveGobWithRollback publishes an explicit legacy-gob rollback image before
// publishing HOTLEX2. It is intended for a fresh migration output that has no
// pre-existing gob for SaveGob to preserve.
func (h *HotLex) SaveGobWithRollback(path, rollbackPath string) error {
	if strings.TrimSpace(rollbackPath) == "" {
		return errors.New("hotlex: rollback gob path is required")
	}
	if filepath.Clean(path) == filepath.Clean(rollbackPath) {
		return errors.New("hotlex: rollback gob path must differ from HOTLEX2 path")
	}
	if err := h.SaveLegacyGob(rollbackPath); err != nil {
		return fmt.Errorf("hotlex: publish rollback gob: %w", err)
	}
	if err := h.SaveGob(path); err != nil {
		return fmt.Errorf("hotlex: publish HOTLEX2: %w", err)
	}
	return nil
}

// SaveLegacyGob atomically writes the wire representation understood by
// gob-only binaries. New serving code should use it only for rollback assets.
func (h *HotLex) SaveLegacyGob(path string) error {
	if h == nil {
		return errors.New("hotlex: nil")
	}
	h.materializeMapped()
	h.mu.RLock()
	defer h.mu.RUnlock()
	if _, err := h.makeWritePlanLocked(); err != nil {
		return err
	}
	return h.writeLegacyGobLocked(path)
}

func (h *HotLex) preserveLegacyRollback(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var magic [8]byte
	_, readErr := io.ReadFull(f, magic[:])
	_ = f.Close()
	if readErr != nil {
		return fmt.Errorf("hotlex: inspect existing migration source: %w", readErr)
	}
	if string(magic[:]) == hotLexMagic {
		return nil
	}

	rollbackPath := LegacyRollbackPath(path)
	if _, err := os.Stat(rollbackPath); err == nil {
		rollback, loadErr := LoadHotLexSnapshot(rollbackPath, HotLexSnapshotScope{
			BrainID: h.BrainID, AllowLegacyGob: true,
		})
		if loadErr != nil {
			return fmt.Errorf("hotlex: existing rollback gob diagnostic failed: %w", loadErr)
		}
		format := rollback.SnapshotFormat()
		_ = rollback.Close()
		if format != HotLexFormatLegacyGob {
			return fmt.Errorf("hotlex: existing rollback path format=%s, want=%s", format, HotLexFormatLegacyGob)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	legacy, err := LoadHotLexSnapshot(path, HotLexSnapshotScope{
		BrainID: h.BrainID, AllowLegacyGob: true,
	})
	if err != nil {
		return fmt.Errorf("hotlex: preserve legacy migration source: %w", err)
	}
	format := legacy.SnapshotFormat()
	_ = legacy.Close()
	if format != HotLexFormatLegacyGob {
		return fmt.Errorf("hotlex: migration source format=%s, want=%s", format, HotLexFormatLegacyGob)
	}
	if err := copyFileAtomic(path, rollbackPath, hotLexMaxFileBytes); err != nil {
		return fmt.Errorf("hotlex: preserve rollback gob: %w", err)
	}
	return nil
}

func (h *HotLex) makeWritePlanLocked() (hotLexWritePlan, error) {
	if uint64(len(h.docs)) > hotLexMaxDocs {
		return hotLexWritePlan{}, fmt.Errorf("hotlex: document slots exceed limit")
	}
	if uint64(len(h.BrainID)) > hotLexMaxScopeBytes || uint64(len(h.Generation)) > hotLexMaxScopeBytes ||
		!utf8.ValidString(h.BrainID) || !utf8.ValidString(h.Generation) {
		return hotLexWritePlan{}, fmt.Errorf("hotlex: invalid scope metadata")
	}
	plan := hotLexWritePlan{
		docOrder: make([]int, 0, len(h.docs)),
		oldToNew: make([]uint32, len(h.docs)),
		strings:  uint64(len(h.BrainID) + len(h.Generation)),
	}
	for i := range plan.oldToNew {
		plan.oldToNew[i] = math.MaxUint32
	}
	for i, doc := range h.docs {
		if doc.ChunkID != "" {
			plan.docOrder = append(plan.docOrder, i)
		}
	}
	if uint64(len(plan.docOrder)) > hotLexMaxDocs {
		return hotLexWritePlan{}, fmt.Errorf("hotlex: live documents exceed limit")
	}
	sort.Slice(plan.docOrder, func(i, j int) bool {
		return h.docs[plan.docOrder[i]].ChunkID < h.docs[plan.docOrder[j]].ChunkID
	})
	for newIndex, oldIndex := range plan.docOrder {
		doc := h.docs[oldIndex]
		if newIndex > 0 && h.docs[plan.docOrder[newIndex-1]].ChunkID == doc.ChunkID {
			return hotLexWritePlan{}, fmt.Errorf("hotlex: duplicate chunk id %q", doc.ChunkID)
		}
		if doc.Length <= 0 || !validHotLexDocStrings(doc) {
			return hotLexWritePlan{}, fmt.Errorf("hotlex: invalid document %q", doc.ChunkID)
		}
		if int64(doc.Length) > math.MaxInt64-plan.sumLen {
			return hotLexWritePlan{}, fmt.Errorf("hotlex: document length overflow")
		}
		plan.sumLen += int64(doc.Length)
		plan.oldToNew[oldIndex] = uint32(newIndex)
		for _, value := range []string{doc.ChunkID, doc.DSID, doc.SourceURI} {
			if err := addHotLexStringBytes(&plan.strings, value); err != nil {
				return hotLexWritePlan{}, err
			}
		}
		if doc.HasText {
			if err := addHotLexStringBytes(&plan.strings, doc.Text); err != nil {
				return hotLexWritePlan{}, err
			}
		}
	}
	if len(plan.docOrder) > 0 {
		plan.avgDL = float64(plan.sumLen) / float64(len(plan.docOrder))
	}
	plan.terms = make([]string, 0, len(h.postings))
	for term, plist := range h.postings {
		if term == "" || !utf8.ValidString(term) || len(plist) == 0 {
			return hotLexWritePlan{}, fmt.Errorf("hotlex: invalid term")
		}
		plan.terms = append(plan.terms, term)
		if uint64(len(plist)) > hotLexMaxPostings-plan.postings {
			return hotLexWritePlan{}, fmt.Errorf("hotlex: postings exceed limit")
		}
		plan.postings += uint64(len(plist))
		if err := addHotLexStringBytes(&plan.strings, term); err != nil {
			return hotLexWritePlan{}, err
		}
	}
	if uint64(len(plan.terms)) > hotLexMaxTerms || plan.strings > hotLexMaxStringBytes {
		return hotLexWritePlan{}, fmt.Errorf("hotlex: term/string limit")
	}
	sort.Strings(plan.terms)
	plan.docsOffset = hotLexHeaderSize
	plan.termsOffset = plan.docsOffset + uint64(len(plan.docOrder))*hotLexDocRecordSize
	plan.postsOffset = plan.termsOffset + uint64(len(plan.terms))*hotLexTermRecordSize
	plan.strsOffset = plan.postsOffset + plan.postings*hotLexPostingSize
	plan.fileSize = plan.strsOffset + plan.strings
	if plan.fileSize < plan.strsOffset || plan.fileSize > hotLexMaxFileBytes {
		return hotLexWritePlan{}, fmt.Errorf("hotlex: snapshot exceeds file limit")
	}
	return plan, nil
}

func validHotLexDocStrings(doc hotDoc) bool {
	if doc.ChunkID == "" || !utf8.ValidString(doc.ChunkID) || !utf8.ValidString(doc.DSID) || !utf8.ValidString(doc.SourceURI) {
		return false
	}
	if uint64(len(doc.ChunkID)) > math.MaxUint32 || uint64(len(doc.DSID)) > math.MaxUint32 || uint64(len(doc.SourceURI)) > math.MaxUint32 {
		return false
	}
	return !doc.HasText || (doc.Text != "" && utf8.ValidString(doc.Text) && uint64(len(doc.Text)) <= math.MaxUint32)
}

func addHotLexStringBytes(total *uint64, value string) error {
	if uint64(len(value)) > math.MaxUint32 || *total > hotLexMaxStringBytes-uint64(len(value)) {
		return fmt.Errorf("hotlex: string section exceeds limit")
	}
	*total += uint64(len(value))
	return nil
}

func (h *HotLex) writeSnapshotLocked(path string, plan hotLexWritePlan) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	header := make([]byte, int(hotLexHeaderSize))
	copy(header[:8], hotLexMagic)
	binary.LittleEndian.PutUint16(header[8:10], hotLexFormatVersion)
	binary.LittleEndian.PutUint16(header[10:12], uint16(hotLexHeaderSize))
	binary.LittleEndian.PutUint64(header[16:24], plan.fileSize)
	binary.LittleEndian.PutUint64(header[24:32], plan.docsOffset)
	binary.LittleEndian.PutUint64(header[32:40], uint64(len(plan.docOrder)))
	binary.LittleEndian.PutUint64(header[40:48], plan.termsOffset)
	binary.LittleEndian.PutUint64(header[48:56], uint64(len(plan.terms)))
	binary.LittleEndian.PutUint64(header[56:64], plan.postsOffset)
	binary.LittleEndian.PutUint64(header[64:72], plan.postings)
	binary.LittleEndian.PutUint64(header[72:80], plan.strsOffset)
	binary.LittleEndian.PutUint64(header[80:88], plan.strings)
	binary.LittleEndian.PutUint64(header[88:96], 0)
	binary.LittleEndian.PutUint32(header[96:100], uint32(len(h.BrainID)))
	binary.LittleEndian.PutUint32(header[100:104], uint32(len(h.Generation)))
	binary.LittleEndian.PutUint64(header[104:112], uint64(len(h.BrainID)))
	binary.LittleEndian.PutUint64(header[112:120], math.Float64bits(plan.avgDL))
	binary.LittleEndian.PutUint64(header[120:128], uint64(plan.sumLen))
	if _, err := tmp.Write(header); err != nil {
		return err
	}
	digest := sha256.New()
	_, _ = digest.Write(header)
	bw := bufio.NewWriterSize(io.MultiWriter(tmp, digest), 1<<20)
	stringCursor := uint64(len(h.BrainID) + len(h.Generation))
	var record [64]byte
	for _, oldIndex := range plan.docOrder {
		clear(record[:])
		doc := h.docs[oldIndex]
		binary.LittleEndian.PutUint64(record[0:8], stringCursor)
		binary.LittleEndian.PutUint32(record[8:12], uint32(len(doc.ChunkID)))
		stringCursor += uint64(len(doc.ChunkID))
		binary.LittleEndian.PutUint32(record[12:16], uint32(len(doc.DSID)))
		binary.LittleEndian.PutUint64(record[16:24], stringCursor)
		stringCursor += uint64(len(doc.DSID))
		binary.LittleEndian.PutUint64(record[24:32], stringCursor)
		binary.LittleEndian.PutUint32(record[32:36], uint32(len(doc.SourceURI)))
		stringCursor += uint64(len(doc.SourceURI))
		if doc.HasText {
			binary.LittleEndian.PutUint32(record[36:40], uint32(len(doc.Text)))
			binary.LittleEndian.PutUint64(record[40:48], stringCursor)
			binary.LittleEndian.PutUint32(record[52:56], hotDocFlagHasText)
			stringCursor += uint64(len(doc.Text))
		} else {
			binary.LittleEndian.PutUint64(record[40:48], stringCursor)
		}
		binary.LittleEndian.PutUint32(record[48:52], uint32(doc.Length))
		if _, err := bw.Write(record[:hotLexDocRecordSize]); err != nil {
			return err
		}
	}
	postCursor := uint64(0)
	for _, term := range plan.terms {
		clear(record[:])
		binary.LittleEndian.PutUint64(record[0:8], stringCursor)
		binary.LittleEndian.PutUint32(record[8:12], uint32(len(term)))
		binary.LittleEndian.PutUint64(record[16:24], postCursor)
		binary.LittleEndian.PutUint64(record[24:32], uint64(len(h.postings[term])))
		stringCursor += uint64(len(term))
		postCursor += uint64(len(h.postings[term]))
		if _, err := bw.Write(record[:hotLexTermRecordSize]); err != nil {
			return err
		}
	}
	postingBuf := make([]hotPosting, 0)
	var postingRecord [8]byte
	for _, term := range plan.terms {
		postingBuf = append(postingBuf[:0], h.postings[term]...)
		for i := range postingBuf {
			old := int(postingBuf[i].Doc)
			if old < 0 || old >= len(plan.oldToNew) || plan.oldToNew[old] == math.MaxUint32 || postingBuf[i].TF <= 0 {
				return fmt.Errorf("hotlex: invalid posting for term %q", term)
			}
			postingBuf[i].Doc = int32(plan.oldToNew[old])
		}
		sort.Slice(postingBuf, func(i, j int) bool {
			if postingBuf[i].Doc != postingBuf[j].Doc {
				return postingBuf[i].Doc < postingBuf[j].Doc
			}
			return postingBuf[i].TF < postingBuf[j].TF
		})
		for i, posting := range postingBuf {
			if i > 0 && postingBuf[i-1].Doc == posting.Doc {
				return fmt.Errorf("hotlex: duplicate posting for term %q", term)
			}
			binary.LittleEndian.PutUint32(postingRecord[0:4], uint32(posting.Doc))
			binary.LittleEndian.PutUint32(postingRecord[4:8], uint32(posting.TF))
			if _, err := bw.Write(postingRecord[:]); err != nil {
				return err
			}
		}
	}
	for _, value := range []string{h.BrainID, h.Generation} {
		if _, err := bw.WriteString(value); err != nil {
			return err
		}
	}
	for _, oldIndex := range plan.docOrder {
		doc := h.docs[oldIndex]
		for _, value := range []string{doc.ChunkID, doc.DSID, doc.SourceURI} {
			if _, err := bw.WriteString(value); err != nil {
				return err
			}
		}
		if doc.HasText {
			if _, err := bw.WriteString(doc.Text); err != nil {
				return err
			}
		}
	}
	for _, term := range plan.terms {
		if _, err := bw.WriteString(term); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if stringCursor != plan.strings || postCursor != plan.postings {
		return fmt.Errorf("hotlex: internal section accounting mismatch")
	}
	copy(header[hotLexDigestOffset:hotLexDigestOffset+hotLexDigestSize], digest.Sum(nil))
	if _, err := tmp.WriteAt(header, 0); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncHotLexDirectory(dir)
}

func (h *HotLex) writeLegacyGobLocked(path string) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	terms := make([]string, 0, len(h.postings))
	for term := range h.postings {
		terms = append(terms, term)
	}
	sort.Strings(terms)
	snap := hotLexSnap{
		BrainID: h.BrainID, Generation: h.Generation, N: h.N, AvgDL: h.AvgDL,
		SumLen: h.sumLen, Docs: h.docs, Terms: make([]hotTermSnap, 0, len(terms)),
	}
	for _, term := range terms {
		snap.Terms = append(snap.Terms, hotTermSnap{Term: term, Postings: h.postings[term]})
	}
	if err := gob.NewEncoder(tmp).Encode(&snap); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncHotLexDirectory(dir)
}

func copyFileAtomic(source, target string, maxBytes uint64) (retErr error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || uint64(st.Size()) > maxBytes {
		return fmt.Errorf("invalid source size %d", st.Size())
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	written, err := io.Copy(tmp, io.LimitReader(in, int64(maxBytes)+1))
	if err != nil {
		return err
	}
	if written != st.Size() || uint64(written) > maxBytes {
		return fmt.Errorf("source changed or exceeds limit")
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	return syncHotLexDirectory(dir)
}

func syncHotLexDirectory(dir string) error {
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := dirFile.Sync()
	closeErr := dirFile.Close()
	// Some FUSE/object-volume implementations provide atomic rename but reject
	// directory fsync. File fsync above remains mandatory; only the documented
	// unsupported-directory cases are tolerated for deployment compatibility.
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, syscall.ENOTSUP) {
		return syncErr
	}
	return closeErr
}

func checkHotLexScope(scope HotLexSnapshotScope, brainID, generation string) error {
	if scope.BrainID != "" && brainID != scope.BrainID {
		return fmt.Errorf("%w: got %q want %q", ErrHotLexScope, brainID, scope.BrainID)
	}
	if scope.Generation != "" && generation != scope.Generation {
		return fmt.Errorf("%w: got %q want %q", ErrHotLexStale, generation, scope.Generation)
	}
	return nil
}

func validateHotLexShardScope(brainID string, shard *HotLex) error {
	if shard == nil {
		return nil
	}
	if shard.BrainID != brainID && !strings.HasPrefix(shard.BrainID, brainID+"#s") {
		return fmt.Errorf("%w: shard %q target %q", ErrHotLexScope, shard.BrainID, brainID)
	}
	return nil
}

// Legacy structs are retained solely so existing projected gob files can be
// recovered and republished. They are never emitted by current code.
type hotLexSnap struct {
	BrainID    string
	Generation string
	N          int
	AvgDL      float64
	SumLen     int64
	Docs       []hotDoc
	Terms      []hotTermSnap
}

type hotTermSnap struct {
	Term     string
	Postings []hotPosting
}

func loadLegacyHotLexGob(r io.Reader, size uint64, scope HotLexSnapshotScope) (*HotLex, error) {
	if size > hotLexMaxFileBytes {
		return nil, fmt.Errorf("%w: legacy file size", ErrHotLexCorrupt)
	}
	decoder := gob.NewDecoder(io.LimitReader(r, int64(size)))
	var snap hotLexSnap
	if err := decoder.Decode(&snap); err != nil {
		return nil, fmt.Errorf("%w: legacy decode: %v", ErrHotLexCorrupt, err)
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: legacy trailing bytes", ErrHotLexCorrupt)
	}
	if err := checkHotLexScope(scope, snap.BrainID, snap.Generation); err != nil {
		return nil, err
	}
	if len(snap.Docs) > int(hotLexMaxDocs) || len(snap.Terms) > int(hotLexMaxTerms) {
		return nil, fmt.Errorf("%w: legacy count limit", ErrHotLexCorrupt)
	}
	h := &HotLex{
		BrainID: snap.BrainID, Generation: snap.Generation,
		docs: snap.Docs, postings: make(map[string][]hotPosting, len(snap.Terms)), byChunk: map[string]int{},
		sourceFormat: HotLexFormatLegacyGob,
	}
	var sum int64
	for i, doc := range h.docs {
		if doc.ChunkID == "" {
			continue
		}
		if !validHotLexDocStrings(doc) || doc.Length <= 0 {
			return nil, fmt.Errorf("%w: legacy document", ErrHotLexCorrupt)
		}
		if _, exists := h.byChunk[doc.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate legacy chunk", ErrHotLexCorrupt)
		}
		h.byChunk[doc.ChunkID] = i
		sum += int64(doc.Length)
	}
	var postings uint64
	for _, term := range snap.Terms {
		if term.Term == "" || !utf8.ValidString(term.Term) || len(term.Postings) == 0 {
			return nil, fmt.Errorf("%w: legacy term", ErrHotLexCorrupt)
		}
		if _, exists := h.postings[term.Term]; exists {
			return nil, fmt.Errorf("%w: duplicate legacy term", ErrHotLexCorrupt)
		}
		seenDocs := map[int32]struct{}{}
		for _, posting := range term.Postings {
			if posting.Doc < 0 || int(posting.Doc) >= len(h.docs) || h.docs[posting.Doc].ChunkID == "" || posting.TF <= 0 {
				return nil, fmt.Errorf("%w: legacy posting", ErrHotLexCorrupt)
			}
			if _, duplicate := seenDocs[posting.Doc]; duplicate {
				return nil, fmt.Errorf("%w: duplicate legacy posting", ErrHotLexCorrupt)
			}
			seenDocs[posting.Doc] = struct{}{}
		}
		postings += uint64(len(term.Postings))
		if postings > hotLexMaxPostings {
			return nil, fmt.Errorf("%w: legacy postings limit", ErrHotLexCorrupt)
		}
		h.postings[term.Term] = term.Postings
	}
	h.N = len(h.byChunk)
	h.sumLen = sum
	if h.N > 0 {
		h.AvgDL = float64(sum) / float64(h.N)
	}
	return h, nil
}

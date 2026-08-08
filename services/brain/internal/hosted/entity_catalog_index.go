package hosted

import (
	"sort"
	"strings"
	"sync/atomic"
)

// Indexed entity-catalog lookup for recovery expand (issue #303).
//
// Before: every recovery query linearly scanned the whole catalog map
// (offline gob can hold ~70k names; the live brain sample up to ~16k keys)
// once per seed with strings.Contains. Now: a token index built once per
// catalog load answers seed lookups in ~O(candidates); for small catalogs
// (≤ entityIndexFallbackScanMax keys) the old linear scan runs as a reference
// fallback whenever the index result is absent or partial, so small catalogs
// keep the old substring predicate semantics while using deterministic
// longest-first ordering. Atomic diagnostics count what the bounds skipped so
// reduced recall is observable instead of silent.
//
// The index is a candidate generator only: every candidate is re-verified
// with the caller's original substring predicate, so index matches can
// never be false positives relative to the previous scan semantics.

const (
	// entityIndexFallbackScanMax bounds the linear-scan fallback. Catalogs
	// larger than this never fall back — the unbounded per-query scan is
	// exactly what #303 removes — and the skip is counted in stats.
	entityIndexFallbackScanMax = 4096
	// entityIndexPrefixTokenMax bounds token-prefix range expansion per seed.
	entityIndexPrefixTokenMax = 256
	// entityIndexPostingScanMax bounds candidate verification per posting list.
	entityIndexPostingScanMax = 2048
	// entityIndexSeedNgramMax bounds contiguous seed n-gram exact-key lookups.
	entityIndexSeedNgramMax = 4
	// entityIndexByteNgramSize gives large catalogs a bounded candidate path
	// for the old mid-token substring predicate (for example, "cache" in
	// "kvcache"). Packed byte trigrams avoid allocating one Go string per
	// catalog n-gram.
	entityIndexByteNgramSize = 3
)

// entityIndexStats is a diagnostics snapshot for one index.
type entityIndexStats struct {
	Candidates    uint64 // candidate keys verified against the predicate
	IndexMatches  uint64 // keys matched via index candidates
	FallbackScans uint64 // seeds served (fully or topped up) by the reference linear scan
	FallbackSkips uint64 // seeds returned index-only because the catalog is too large to scan (recall may drop)
	Truncations   uint64 // seeds that hit a prefix/posting cap (recall may drop)
}

// entityIndexMetrics describes steady-state logical index payload. PayloadBytes
// counts key/token bytes and posting/key-position integers; it deliberately
// excludes Go map buckets, slice headers, allocator classes, and atomics, so it
// is a comparable workload metric rather than a claim about process RSS.
type entityIndexMetrics struct {
	Keys          int
	Tokens        int
	Grams         int
	TokenPostings int
	GramPostings  int
	PayloadBytes  uint64
}

// entityNameIndex indexes lowercase catalog keys for seed matching.
type entityNameIndex struct {
	// keys in deterministic priority order: longest first (most specific,
	// mirrors the loader's ORDER BY length DESC), then lexicographic.
	keys         []string
	keyPos       map[string]int
	byToken      map[string][]int // whole token → key positions (ascending)
	sortedTokens []string
	byGram       map[uint32][]int // packed lowercase byte trigram → key positions
	keyLengths   []int            // distinct key byte lengths for reverse containment

	candidates    atomic.Uint64
	indexMatches  atomic.Uint64
	fallbackScans atomic.Uint64
	fallbackSkips atomic.Uint64
	truncations   atomic.Uint64
}

func entityKeyTokens(s string) []string {
	return wordRE.FindAllString(strings.ToLower(s), -1)
}

// newEntityNameIndex builds the index once per catalog load.
func newEntityNameIndex(keys []string) *entityNameIndex {
	ix := &entityNameIndex{
		keyPos:  map[string]int{},
		byToken: map[string][]int{},
		byGram:  map[uint32][]int{},
	}
	uniq := make(map[string]struct{}, len(keys))
	lengths := map[int]struct{}{}
	for _, k := range keys {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if _, ok := uniq[k]; ok {
			continue
		}
		uniq[k] = struct{}{}
		ix.keys = append(ix.keys, k)
		lengths[len(k)] = struct{}{}
	}
	sort.Slice(ix.keys, func(i, j int) bool {
		if len(ix.keys[i]) != len(ix.keys[j]) {
			return len(ix.keys[i]) > len(ix.keys[j])
		}
		return ix.keys[i] < ix.keys[j]
	})
	for pos, k := range ix.keys {
		ix.keyPos[k] = pos
		seenTok := map[string]struct{}{}
		for _, t := range entityKeyTokens(k) {
			if _, ok := seenTok[t]; ok {
				continue
			}
			seenTok[t] = struct{}{}
			ix.byToken[t] = append(ix.byToken[t], pos)
		}
		// Repeated grams are intentionally left in the posting list: avoiding a
		// temporary map per key saves substantial build allocations on 70k+
		// catalogs, and candidate de-duplication below is already bounded.
		for i := 0; i+entityIndexByteNgramSize <= len(k); i++ {
			g := packEntityGram(k[i : i+entityIndexByteNgramSize])
			ix.byGram[g] = append(ix.byGram[g], pos)
		}
	}
	ix.sortedTokens = make([]string, 0, len(ix.byToken))
	for t := range ix.byToken {
		ix.sortedTokens = append(ix.sortedTokens, t)
	}
	sort.Strings(ix.sortedTokens)
	ix.keyLengths = make([]int, 0, len(lengths))
	for n := range lengths {
		ix.keyLengths = append(ix.keyLengths, n)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ix.keyLengths)))
	return ix
}

func packEntityGram(s string) uint32 {
	if len(s) < entityIndexByteNgramSize {
		return 0
	}
	return uint32(s[0])<<16 | uint32(s[1])<<8 | uint32(s[2])
}

func (ix *entityNameIndex) size() int {
	if ix == nil {
		return 0
	}
	return len(ix.keys)
}

func (ix *entityNameIndex) stats() entityIndexStats {
	if ix == nil {
		return entityIndexStats{}
	}
	return entityIndexStats{
		Candidates:    ix.candidates.Load(),
		IndexMatches:  ix.indexMatches.Load(),
		FallbackScans: ix.fallbackScans.Load(),
		FallbackSkips: ix.fallbackSkips.Load(),
		Truncations:   ix.truncations.Load(),
	}
}

func (ix *entityNameIndex) metrics() entityIndexMetrics {
	if ix == nil {
		return entityIndexMetrics{}
	}
	m := entityIndexMetrics{
		Keys:   len(ix.keys),
		Tokens: len(ix.byToken),
		Grams:  len(ix.byGram),
	}
	const intBytes = 64 / 8 // hosted serving targets are 64-bit.
	for _, key := range ix.keys {
		m.PayloadBytes += uint64(len(key) + intBytes)
	}
	for token, postings := range ix.byToken {
		m.TokenPostings += len(postings)
		m.PayloadBytes += uint64(len(token) + len(postings)*intBytes)
	}
	for _, postings := range ix.byGram {
		m.GramPostings += len(postings)
		m.PayloadBytes += uint64(4 + len(postings)*intBytes)
	}
	return m
}

// candidatePositions gathers a bounded, deterministic candidate set for seed:
//  1. seed as an exact key;
//  2. each seed token / contiguous space-joined seed n-gram as an exact key
//     (covers keys contained in a longer seed);
//  3. rarest packed seed trigram (covers mid-token containment);
//  4. single-token seed: keys containing the seed as a whole token, plus keys
//     whose tokens have the seed as a prefix ("postgres" → "postgresql");
//  5. multi-token seed: the rarest seed token's posting list (verified by the
//     caller's predicate, e.g. "acme corp" ⊂ "acme corporation" via "acme").
func (ix *entityNameIndex) candidatePositions(seed string, collectMax int) ([]int, bool) {
	if collectMax < 64 {
		collectMax = 64
	}
	if collectMax > entityIndexPostingScanMax {
		collectMax = entityIndexPostingScanMax
	}
	seen := map[int]struct{}{}
	var out []int
	truncated := false
	addPos := func(p int) bool {
		if _, ok := seen[p]; ok {
			return true
		}
		if len(out) >= collectMax {
			truncated = true
			return false
		}
		seen[p] = struct{}{}
		out = append(out, p)
		return true
	}
	addPostings := func(ps []int) {
		if len(ps) > entityIndexPostingScanMax {
			ps = ps[:entityIndexPostingScanMax]
			truncated = true
		}
		for _, p := range ps {
			if !addPos(p) {
				break
			}
		}
	}
	if p, ok := ix.keyPos[seed]; ok {
		_ = addPos(p)
	}
	// Preserve the live predicate's reverse-containment half
	// (strings.Contains(seed, key)) without a catalog scan. Looking up only
	// substring lengths present in the catalog finds every contained key; key
	// positions are sorted first so a crowded seed retains longest-first order.
	var reverse []int
	for _, n := range ix.keyLengths {
		if n > len(seed) {
			continue
		}
		for start := 0; start+n <= len(seed); start++ {
			if p, ok := ix.keyPos[seed[start:start+n]]; ok {
				reverse = append(reverse, p)
			}
		}
	}
	if len(reverse) > 0 {
		sort.Ints(reverse)
		write := 1
		for read := 1; read < len(reverse); read++ {
			if reverse[read] == reverse[write-1] {
				continue
			}
			reverse[write] = reverse[read]
			write++
		}
		addPostings(reverse[:write])
	}
	toks := entityKeyTokens(seed)
	for _, t := range toks {
		if p, ok := ix.keyPos[t]; ok {
			_ = addPos(p)
		}
	}
	for n := 2; n <= entityIndexSeedNgramMax && n <= len(toks); n++ {
		for i := 0; i+n <= len(toks); i++ {
			if p, ok := ix.keyPos[strings.Join(toks[i:i+n], " ")]; ok {
				_ = addPos(p)
			}
		}
	}
	// The rarest seed trigram is a bounded superset for keys containing the
	// complete seed. It restores mid-token candidates on large catalogs; the
	// caller still re-verifies the complete old substring predicate. Exact
	// token/ngram keys above are admitted first so reverse containment cannot
	// be displaced by a common trigram posting.
	var rareGramPostings []int
	seenSeedGram := map[uint32]struct{}{}
	for i := 0; i+entityIndexByteNgramSize <= len(seed); i++ {
		g := packEntityGram(seed[i : i+entityIndexByteNgramSize])
		if _, ok := seenSeedGram[g]; ok {
			continue
		}
		seenSeedGram[g] = struct{}{}
		ps := ix.byGram[g]
		if len(ps) == 0 {
			// No catalog key containing the complete seed can exist. Other
			// candidate routes still cover a catalog key contained in seed.
			continue
		}
		if rareGramPostings == nil || len(ps) < len(rareGramPostings) {
			rareGramPostings = ps
		}
	}
	addPostings(rareGramPostings)
	switch {
	case len(toks) == 1:
		t := toks[0]
		addPostings(ix.byToken[t])
		lo := sort.SearchStrings(ix.sortedTokens, t)
		scanned := 0
		for i := lo; i < len(ix.sortedTokens) && strings.HasPrefix(ix.sortedTokens[i], t); i++ {
			if scanned >= entityIndexPrefixTokenMax {
				truncated = true
				break
			}
			scanned++
			addPostings(ix.byToken[ix.sortedTokens[i]])
		}
	case len(toks) > 1:
		var best []int
		for _, t := range toks {
			ps := ix.byToken[t]
			if len(ps) == 0 {
				continue
			}
			if best == nil || len(ps) < len(best) {
				best = ps
			}
		}
		addPostings(best)
	}
	sort.Ints(out)
	return out, truncated
}

// match returns catalog keys matching seed under pred, in deterministic
// priority order (longest key first), capped at max. Index candidates are
// always verified with pred; on small catalogs (≤ entityIndexFallbackScanMax
// keys), whenever the index yields fewer than max accepted candidates the old
// linear substring scan runs as the reference fallback, so small-catalog
// results are exactly the pre-#303 scan output. Large catalogs never scan.
func (ix *entityNameIndex) match(seed string, max int, pred func(key string) bool) []string {
	if ix == nil || max < 1 || pred == nil {
		return nil
	}
	seed = strings.ToLower(strings.TrimSpace(seed))
	if seed == "" {
		return nil
	}
	// Collecting at most 32x the requested output bounds temporary maps/slices
	// even for very common postings while leaving room for predicate rejects.
	pos, truncated := ix.candidatePositions(seed, max*32)
	if truncated {
		ix.truncations.Add(1)
	}
	var out []string
	for _, p := range pos {
		ix.candidates.Add(1)
		k := ix.keys[p]
		if !pred(k) {
			continue
		}
		out = append(out, k)
		if len(out) >= max {
			break
		}
	}
	if len(out) > 0 {
		ix.indexMatches.Add(uint64(len(out)))
	}
	if len(out) >= max {
		return out
	}
	if len(ix.keys) > entityIndexFallbackScanMax {
		// The reference scan would run here under the old semantics but the
		// catalog is too large — count the skip even when the index produced
		// a partial result, so possible recall loss is always observable.
		ix.fallbackSkips.Add(1)
		return out
	}
	// Small catalog, index result absent or partial: rebuild from the
	// reference linear scan so the output preserves the pre-#303 substring
	// predicate with deterministic longest-first ordering, capped at max.
	// Rebuilding (rather than appending to the partial index result) both
	// deduplicates keys the index already matched and restores missed keys.
	ix.fallbackScans.Add(1)
	out = out[:0]
	for _, k := range ix.keys {
		if !pred(k) {
			continue
		}
		out = append(out, k)
		if len(out) >= max {
			break
		}
	}
	return out
}

func mapKeyStrings(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mapKeySliceStrings(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

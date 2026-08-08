package chunking

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Roles distinguish retrievable chunks from context parents.
const (
	RoleChunk  = "chunk"
	RoleParent = "parent"
)

// SourceDocument is one chunkable source. Offsets in receipts are byte
// offsets into Source(), the exact string chunks slice.
type SourceDocument struct {
	ID        string
	Title     string
	Body      string
	SourceURI string
	Kind      Kind // empty means prose
}

// Source returns the chunked string: title prefix plus body, matching the
// legacy whole-document chunk content order.
func (d SourceDocument) Source() string {
	title := strings.TrimSpace(d.Title)
	body := strings.TrimSpace(d.Body)
	if title == "" {
		return body
	}
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}

// Receipt is one chunk outcome of a policy run. It preserves everything the
// issue #332 contract requires: tokenizer/version stamps, source byte
// offsets, parent identity, and a content hash for rebuild verification.
type Receipt struct {
	ChunkID       string `json:"chunk_id"`
	DocumentID    string `json:"document_id"`
	ParentID      string `json:"parent_id,omitempty"`
	Role          string `json:"role"`
	Kind          string `json:"kind"`
	Strategy      string `json:"strategy"`
	PolicyID      string `json:"policy_id"`
	PolicyVersion int    `json:"policy_version"`
	TokenizerID   string `json:"tokenizer_id"`
	Seq           int    `json:"seq"`
	Start         int    `json:"start"` // byte offset into SourceDocument.Source()
	End           int    `json:"end"`   // byte offset, exclusive
	Tokens        int    `json:"tokens"`
	SHA256        string `json:"sha256"` // hex sha256 of Text
	Text          string `json:"text"`
}

// span is a byte range inside one source string.
type span struct {
	start, end int
}

// Chunk chunks docs under p. It is deterministic: identical inputs and
// policy always produce identical receipts (rebuild identity).
func Chunk(docs []SourceDocument, p Policy) ([]Receipt, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var out []Receipt
	for _, d := range docs {
		id := strings.TrimSpace(d.ID)
		src := d.Source()
		if id == "" || strings.TrimSpace(src) == "" {
			continue
		}
		kind := d.Kind
		if kind == "" {
			kind = KindProse
		}
		switch p.Strategy {
		case StrategyWholeDoc:
			out = append(out, receiptsFor(id, kind, src, []span{{0, len(src)}}, p, RoleChunk)...)
		case StrategyFixed:
			out = append(out, receiptsFor(id, kind, src,
				fixedSpans(src, p.TargetTokens, p.OverlapTokens), p, RoleChunk)...)
		case StrategyStructure:
			spans := structureSpans(src, kind, p.TargetTokens, p.OverlapTokens)
			out = append(out, receiptsFor(id, kind, src, spans, p, RoleChunk)...)
		case StrategyParentChild:
			parents := receiptsFor(id, kind, src,
				fixedSpans(src, p.ParentTargetTokens, p.ParentOverlapTokens), p, RoleParent)
			out = append(out, parents...)
			children := receiptsFor(id, kind, src,
				fixedSpans(src, p.ChildTargetTokens, p.ChildOverlapTokens), p, RoleChunk)
			assignParents(parents, children)
			out = append(out, children...)
		}
	}
	return out, nil
}

// receiptsFor materializes spans into stamped receipts. Seq counts per
// document and role; chunk identity is policy-scoped so a version bump never
// silently collides with prior receipts.
func receiptsFor(docID string, kind Kind, src string, spans []span, p Policy, role string) []Receipt {
	out := make([]Receipt, 0, len(spans))
	seq := 0
	for _, sp := range spans {
		toks := Tokenize(src[sp.start:sp.end])
		if len(toks) == 0 {
			continue
		}
		start := sp.start + toks[0].Start
		end := sp.start + toks[len(toks)-1].End
		text := src[start:end]
		sum := sha256.Sum256([]byte(text))
		prefix := ""
		switch {
		case role == RoleParent:
			prefix = "p"
		case p.Strategy == StrategyParentChild:
			prefix = "c"
		}
		out = append(out, Receipt{
			ChunkID:       fmt.Sprintf("%s#%s.v%d.%s%d", docID, p.Strategy, p.Version, prefix, seq),
			DocumentID:    docID,
			Role:          role,
			Kind:          string(kind),
			Strategy:      string(p.Strategy),
			PolicyID:      p.ID,
			PolicyVersion: p.Version,
			TokenizerID:   p.TokenizerID,
			Seq:           seq,
			Start:         start,
			End:           end,
			Tokens:        len(toks),
			SHA256:        hex.EncodeToString(sum[:]),
			Text:          text,
		})
		seq++
	}
	return out
}

// assignParents links each child to the parent with the largest byte
// overlap (earliest parent wins ties). Deterministic by construction.
func assignParents(parents, children []Receipt) {
	for i := range children {
		best := -1
		bestOverlap := 0
		for j := range parents {
			lo := children[i].Start
			if parents[j].Start > lo {
				lo = parents[j].Start
			}
			hi := children[i].End
			if parents[j].End < hi {
				hi = parents[j].End
			}
			if ov := hi - lo; ov > bestOverlap {
				bestOverlap = ov
				best = j
			}
		}
		if best >= 0 {
			children[i].ParentID = parents[best].ChunkID
		}
	}
}

// fixedSpans slides a token window: chunks hold at most target tokens and
// the next window starts overlap tokens before the previous window ended.
func fixedSpans(src string, target, overlap int) []span {
	toks := Tokenize(src)
	n := len(toks)
	if n == 0 {
		return nil
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= target {
		overlap = target - 1
	}
	var out []span
	i := 0
	for {
		j := i + target
		if j > n {
			j = n
		}
		out = append(out, span{toks[i].Start, toks[j-1].End})
		if j == n {
			break
		}
		next := j - overlap
		if next <= i { // guarantee strict progress
			next = i + 1
		}
		i = next
	}
	return out
}

// structureSpans packs kind-aware blocks into token-bounded chunks.
func structureSpans(src string, kind Kind, target, overlap int) []span {
	blocks := splitBlocks(src, kind)
	return packBlocks(src, blocks, target, overlap, oversizedSplitter(src, kind, target, overlap))
}

// oversizedSplitter decides how a single block larger than target splits.
// Tables split at row boundaries so rows never tear; everything else falls
// back to the token window.
func oversizedSplitter(src string, kind Kind, target, overlap int) func(span) []span {
	if kind == KindTable {
		return func(b span) []span {
			rows := nonEmptyLineSpans(src, b)
			return packBlocks(src, rows, target, overlap, func(row span) []span {
				return shiftSpans(fixedSpans(src[row.start:row.end], target, overlap), row.start)
			})
		}
	}
	return func(b span) []span {
		return shiftSpans(fixedSpans(src[b.start:b.end], target, overlap), b.start)
	}
}

// nonEmptyLineSpans lists token-bearing lines of block b as absolute spans.
func nonEmptyLineSpans(src string, b span) []span {
	var out []span
	for _, ln := range lineSpans(src[b.start:b.end]) {
		if CountTokens(src[b.start+ln.start:b.start+ln.end]) == 0 {
			continue
		}
		out = append(out, span{b.start + ln.start, b.start + ln.end})
	}
	return out
}

func shiftSpans(spans []span, by int) []span {
	out := make([]span, len(spans))
	for i, sp := range spans {
		out[i] = span{sp.start + by, sp.end + by}
	}
	return out
}

// packBlocks greedily packs whole blocks into chunks of at most target
// tokens. Overlap is honored at block granularity: the next chunk restarts
// at trailing blocks holding at least overlap tokens, capped at target/2 so
// one large block can never ride along every following chunk. Blocks larger
// than target defer to the oversized splitter.
func packBlocks(src string, blocks []span, target, overlap int, oversized func(span) []span) []span {
	type blockRef struct {
		sp   span
		toks int
	}
	refs := make([]blockRef, 0, len(blocks))
	for _, b := range blocks {
		if t := CountTokens(src[b.start:b.end]); t > 0 {
			refs = append(refs, blockRef{b, t})
		}
	}
	var out []span
	i, n := 0, len(refs)
	for i < n {
		if refs[i].toks > target {
			out = append(out, oversized(refs[i].sp)...)
			i++
			continue
		}
		j, tok := i, 0
		for j < n && refs[j].toks <= target {
			if tok+refs[j].toks > target && j > i {
				break
			}
			tok += refs[j].toks
			j++
		}
		out = append(out, span{refs[i].sp.start, refs[j-1].sp.end})
		if j == n {
			break
		}
		// Restart at trailing blocks covering >= overlap tokens, but never
		// carry more than half the target (bounds chunk size and duplication).
		k, carry := j, 0
		for k > i+1 && carry < overlap {
			k--
			carry += refs[k].toks
			if carry > target/2 {
				k, carry = j, 0
				break
			}
		}
		if k <= i {
			k = j
		}
		i = k
	}
	return dedupeSpans(out)
}

// dedupeSpans drops empty spans and spans fully contained in the previous
// emitted span (pure carry-over chunks duplicate no content).
func dedupeSpans(spans []span) []span {
	out := make([]span, 0, len(spans))
	for _, sp := range spans {
		if sp.end <= sp.start {
			continue
		}
		if n := len(out); n > 0 && sp.start >= out[n-1].start && sp.end <= out[n-1].end {
			continue
		}
		out = append(out, sp)
	}
	return out
}

// splitBlocks routes to the per-kind boundary rules.
func splitBlocks(src string, kind Kind) []span {
	lines := lineSpans(src)
	switch kind {
	case KindCode:
		return codeBlocks(src, lines)
	case KindTable:
		return tableBlocks(src, lines)
	case KindSlides:
		return slideBlocks(src, lines)
	case KindChat:
		return chatBlocks(src, lines)
	default:
		return proseBlocks(src, lines)
	}
}

// lineSpans returns byte spans of each line without its trailing newline.
func lineSpans(src string) []span {
	var out []span
	start := 0
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			out = append(out, span{start, i})
			start = i + 1
		}
	}
	if start < len(src) {
		out = append(out, span{start, len(src)})
	}
	return out
}

// proseBlocks splits on blank lines; heading lines open a new block so a
// heading always travels with the text that follows it.
func proseBlocks(src string, lines []span) []span {
	var blocks []span
	var cur []span
	flush := func() {
		if len(cur) == 0 {
			return
		}
		blocks = append(blocks, span{cur[0].start, cur[len(cur)-1].end})
		cur = nil
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(src[ln.start:ln.end])
		if trimmed == "" {
			flush()
			continue
		}
		if strings.HasPrefix(trimmed, "#") && len(cur) > 0 {
			flush()
		}
		cur = append(cur, ln)
	}
	flush()
	return blocks
}

// codeBlocks keeps fenced code regions whole; prose between fences splits on
// blank lines. An unterminated fence still forms one block.
func codeBlocks(src string, lines []span) []span {
	var blocks []span
	var cur []span
	inFence := false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		blocks = append(blocks, span{cur[0].start, cur[len(cur)-1].end})
		cur = nil
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(src[ln.start:ln.end])
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				cur = append(cur, ln)
				flush()
				inFence = false
				continue
			}
			flush()
			inFence = true
			cur = append(cur, ln)
			continue
		}
		if inFence {
			cur = append(cur, ln)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	return blocks
}

// tableBlocks keeps consecutive table rows (pipe-prefixed lines) in one
// block so a table never splits across chunks unless it exceeds the target.
func tableBlocks(src string, lines []span) []span {
	var blocks []span
	var cur []span
	inTable := false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		blocks = append(blocks, span{cur[0].start, cur[len(cur)-1].end})
		cur = nil
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(src[ln.start:ln.end])
		row := strings.HasPrefix(trimmed, "|")
		if row != inTable {
			flush()
			inTable = row
		}
		if !inTable && trimmed == "" {
			flush()
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	return blocks
}

// slideSeparatorRE matches horizontal-rule slide separators (--- and friends).
var slideSeparatorRE = regexp.MustCompile(`^-{3,}$`)

// slideBlocks treats each slide (text between separator rules) as one block.
func slideBlocks(src string, lines []span) []span {
	var blocks []span
	var cur []span
	flush := func() {
		if len(cur) == 0 {
			return
		}
		blocks = append(blocks, span{cur[0].start, cur[len(cur)-1].end})
		cur = nil
	}
	for _, ln := range lines {
		trimmed := strings.TrimSpace(src[ln.start:ln.end])
		if slideSeparatorRE.MatchString(trimmed) {
			flush()
			continue
		}
		if trimmed == "" {
			continue // blank lines inside a slide do not split it
		}
		cur = append(cur, ln)
	}
	flush()
	return blocks
}

// chatTurnRE matches a speaker prefix like "Alice:" or "Dr. Chen:" at line start.
var chatTurnRE = regexp.MustCompile(`^[\p{L}\p{N}_][\p{L}\p{N}_ .'\-]{0,31}:( |$)`)

// chatBlocks keeps each speaker turn whole; wrapped continuation lines join
// the current turn.
func chatBlocks(src string, lines []span) []span {
	var blocks []span
	var cur []span
	flush := func() {
		if len(cur) == 0 {
			return
		}
		blocks = append(blocks, span{cur[0].start, cur[len(cur)-1].end})
		cur = nil
	}
	for _, ln := range lines {
		text := src[ln.start:ln.end]
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			flush()
			continue
		}
		if chatTurnRE.MatchString(trimmed) {
			flush()
		}
		cur = append(cur, ln)
	}
	flush()
	return blocks
}

package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PageNode is one node in a deterministic hierarchical TOC tree for a document.
// Inspired by VectifyAI/PageIndex (MIT concepts), reimplemented natively in Go —
// no Python import. Optional LLM tree walk is out of scope (extension only).
type PageNode struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Summary    string     `json:"summary,omitempty"`
	Text       string     `json:"text,omitempty"`
	DocumentID string     `json:"document_id"`
	Start      int        `json:"start"` // rune offset into full doc text
	End        int        `json:"end"`
	Children   []PageNode `json:"children,omitempty"`
}

// pageHeadingLine matches markdown AT1-#6 or numbered section headers.
var (
	reMDHeading   = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	reNumSection  = regexp.MustCompile(`^(\d+(?:\.\d+){0,4})\s+([A-Z][\w\s\-/,:]{2,80})$`)
	reTitleCaseLn = regexp.MustCompile(`^([A-Z][A-Za-z0-9]+(?:\s+[A-Z][A-Za-z0-9]+){0,8})$`)
)

// BuildPageIndexTree builds a deterministic hierarchical TOC from document text.
// Headings: markdown #/##, Title Case lines, numbered sections (1. / 1.2.),
// else paragraph clusters as leaves under a synthetic root.
func BuildPageIndexTree(docID, text string) PageNode {
	docID = strings.TrimSpace(docID)
	text = strings.TrimSpace(text)
	root := PageNode{
		ID:         docID + "#root",
		Title:      docID,
		DocumentID: docID,
		Start:      0,
		End:        utf8.RuneCountInString(text),
		Summary:    firstLineSummary(text, 160),
	}
	if text == "" || docID == "" {
		return root
	}

	type heading struct {
		level int
		title string
		start int // rune offset
		line  int
	}
	lines := strings.Split(text, "\n")
	var heads []heading
	runeAt := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		level, title := detectHeading(trimmed)
		if level > 0 && title != "" {
			heads = append(heads, heading{level: level, title: title, start: runeAt, line: i})
		}
		runeAt += utf8.RuneCountInString(line) + 1 // +1 for \n (last may overshoot — ok)
	}
	if len(heads) == 0 {
		// Paragraph cluster leaves under root.
		root.Children = paragraphClusterNodes(docID, text)
		if len(root.Children) == 0 {
			root.Text = text
			root.Summary = firstLineSummary(text, 200)
		}
		return root
	}

	// Section bodies: from this heading start to next heading start (or EOF).
	runes := []rune(text)
	type section struct {
		level int
		title string
		start int
		end   int
		text  string
	}
	secs := make([]section, 0, len(heads))
	for i, h := range heads {
		end := len(runes)
		if i+1 < len(heads) {
			end = heads[i+1].start
		}
		if end < h.start {
			end = h.start
		}
		body := strings.TrimSpace(string(runes[h.start:end]))
		// Drop the heading line itself from body text when possible.
		if nl := strings.Index(body, "\n"); nl >= 0 {
			body = strings.TrimSpace(body[nl+1:])
		} else {
			body = ""
		}
		secs = append(secs, section{level: h.level, title: h.title, start: h.start, end: end, text: body})
	}

	// Build tree with a stack of (level, node index path via parents).
	// Children attach to nearest parent with lower level.
	nodes := make([]PageNode, len(secs))
	for i, s := range secs {
		nodes[i] = PageNode{
			ID:         fmt.Sprintf("%s#s%d", docID, i),
			Title:      s.title,
			Summary:    firstLineSummary(s.text, 120),
			Text:       s.text,
			DocumentID: docID,
			Start:      s.start,
			End:        s.end,
		}
	}
	// parent index stack: pairs of (level, index into nodes)
	type frame struct {
		level int
		idx   int
	}
	var stack []frame
	var topLevel []int
	for i, s := range secs {
		for len(stack) > 0 && stack[len(stack)-1].level >= s.level {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			topLevel = append(topLevel, i)
		} else {
			p := stack[len(stack)-1].idx
			nodes[p].Children = append(nodes[p].Children, nodes[i])
			// Keep a placeholder; we'll re-materialize from indices below.
			// Actually append by value then rewrite — better build bottom-up.
		}
		stack = append(stack, frame{level: s.level, idx: i})
	}
	// Rebuild parent→children correctly (previous appends may nest wrong when
	// children themselves get children). Rebuild from levels:
	childrenOf := map[int][]int{}
	stack = stack[:0]
	topLevel = topLevel[:0]
	for i, s := range secs {
		for len(stack) > 0 && stack[len(stack)-1].level >= s.level {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			topLevel = append(topLevel, i)
		} else {
			p := stack[len(stack)-1].idx
			childrenOf[p] = append(childrenOf[p], i)
		}
		stack = append(stack, frame{level: s.level, idx: i})
	}
	var materialize func(i int) PageNode
	materialize = func(i int) PageNode {
		n := nodes[i]
		n.Children = nil
		for _, c := range childrenOf[i] {
			n.Children = append(n.Children, materialize(c))
		}
		return n
	}
	for _, i := range topLevel {
		root.Children = append(root.Children, materialize(i))
	}
	return root
}

func detectHeading(line string) (level int, title string) {
	if line == "" {
		return 0, ""
	}
	if m := reMDHeading.FindStringSubmatch(line); len(m) == 3 {
		return len(m[1]), strings.TrimSpace(m[2])
	}
	if m := reNumSection.FindStringSubmatch(line); len(m) == 3 {
		// Depth from number of dots in section number.
		depth := 1 + strings.Count(m[1], ".")
		if depth > 6 {
			depth = 6
		}
		return depth, strings.TrimSpace(m[2])
	}
	// Title Case short line (not a sentence ending with period).
	if len(line) <= 80 && !strings.HasSuffix(line, ".") && reTitleCaseLn.MatchString(line) {
		// Require at least one space or Camel multi-word to avoid single tokens.
		if strings.Contains(line, " ") || looksTitleCasePhrase(line) {
			return 2, line
		}
	}
	return 0, ""
}

func looksTitleCasePhrase(s string) bool {
	// Single CapitalizedWord ≥ 4 chars counts as weak H2 for TOC.
	if len(s) < 4 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

func paragraphClusterNodes(docID, text string) []PageNode {
	paras := strings.Split(text, "\n\n")
	var out []PageNode
	offset := 0
	runes := []rune(text)
	_ = runes
	pos := 0
	for i, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			pos += 2 // rough
			continue
		}
		// Find p in text from pos for offsets.
		start := strings.Index(text[pos:], p)
		if start < 0 {
			start = 0
		} else {
			start += pos
		}
		end := start + len(p)
		title := firstLineSummary(p, 60)
		if title == "" {
			title = fmt.Sprintf("para-%d", i)
		}
		out = append(out, PageNode{
			ID:         fmt.Sprintf("%s#p%d", docID, i),
			Title:      title,
			Summary:    firstLineSummary(p, 120),
			Text:       p,
			DocumentID: docID,
			Start:      utf8.RuneCountInString(text[:start]),
			End:        utf8.RuneCountInString(text[:minInt(end, len(text))]),
		})
		pos = end
		offset++
		if len(out) >= 32 {
			break
		}
	}
	return out
}

func firstLineSummary(text string, max int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if i := strings.IndexAny(text, "\n"); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(text)
	if max > 0 && utf8.RuneCountInString(text) > max {
		rs := []rune(text)
		text = string(rs[:max]) + "…"
	}
	return text
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// StorePageIndex replaces stored pageindex trees (one root per document typically).
func (s *Store) StorePageIndex(trees []PageNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil {
		return nil
	}
	if s.data.PageIndex == nil {
		s.data.PageIndex = []PageNode{}
	}
	// Merge by DocumentID: replace existing roots for same doc.
	byDoc := map[string]PageNode{}
	for _, t := range s.data.PageIndex {
		if t.DocumentID != "" {
			byDoc[t.DocumentID] = t
		}
	}
	for _, t := range trees {
		if t.DocumentID == "" {
			continue
		}
		byDoc[t.DocumentID] = t
	}
	ids := make([]string, 0, len(byDoc))
	for id := range byDoc {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]PageNode, 0, len(ids))
	for _, id := range ids {
		out = append(out, byDoc[id])
	}
	s.data.PageIndex = out
	return s.persistLocked()
}

// ListPageIndex returns stored TOC roots.
func (s *Store) ListPageIndex() []PageNode {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PageNode(nil), s.data.PageIndex...)
}

// SearchPageIndex ranks leaf/section nodes by token overlap of query vs title+summary.
// Returns up to limit nodes (prefer leaves with text for passage injection).
func (s *Store) SearchPageIndex(query string, limit int) []PageNode {
	if s == nil || limit == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 0 {
		limit = 8
	}
	qToks := pageIndexTokens(query)
	if len(qToks) == 0 {
		return nil
	}
	type scored struct {
		n  PageNode
		sc float64
	}
	var hits []scored
	var walk func(n PageNode)
	walk = func(n PageNode) {
		// Score this node.
		blob := strings.ToLower(n.Title + " " + n.Summary)
		var sc float64
		for _, t := range qToks {
			if strings.Contains(blob, t) {
				sc += 1.0
			}
		}
		// Prefer nodes that have section text (passage material).
		if sc > 0 {
			if strings.TrimSpace(n.Text) != "" {
				sc += 0.25
			}
			hits = append(hits, scored{n: n, sc: sc})
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, root := range s.data.PageIndex {
		walk(root)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].sc == hits[j].sc {
			return hits[i].n.ID < hits[j].n.ID
		}
		return hits[i].sc > hits[j].sc
	})
	if limit > len(hits) {
		limit = len(hits)
	}
	out := make([]PageNode, limit)
	for i := 0; i < limit; i++ {
		out[i] = hits[i].n
	}
	return out
}

func pageIndexTokens(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	var out []string
	seen := map[string]struct{}{}
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

package contextpack

import (
	"fmt"
	"sort"
	"strings"
)

// SchemaID identifies the packed-context wire shape.
const SchemaID = "sentra-scm.contextpack/v1"

// bytesPerToken is the deterministic estimate used for token budgets
// (ceil(bytes/4) tokens per source; budgets convert to bytes upstream).
const bytesPerToken = 4

// DefaultDirectFloorFraction is the share of the budget reserved for
// direct (definition-bearing) sources before proportional allocation.
const DefaultDirectFloorFraction = 0.4

// EstimateTokens is the deterministic token estimate for content.
func EstimateTokens(content string) int {
	return (len(content) + bytesPerToken - 1) / bytesPerToken
}

// Budget is a hard cap on emitted context. MaxBytes and MaxTokens combine as
// the stricter of the two; both zero means unlimited.
type Budget struct {
	MaxBytes  int `json:"max_bytes,omitempty"`
	MaxTokens int `json:"max_tokens,omitempty"`
}

// ByteLimit converts the budget to a single byte cap (tokens are estimated
// at 4 bytes/token, so byteLimit <= MaxTokens*4 also enforces the token cap).
// Returns 0 when unbounded.
func (b Budget) ByteLimit() int {
	lim := 0
	if b.MaxBytes > 0 {
		lim = b.MaxBytes
	}
	if b.MaxTokens > 0 {
		tb := b.MaxTokens * bytesPerToken
		if lim == 0 || tb < lim {
			lim = tb
		}
	}
	return lim
}

// Source is one candidate context item. Direct marks definition-bearing
// (task-spine) sources that receive the floor share of the budget.
type Source struct {
	Path      string  `json:"path"`
	StartLine int     `json:"start_line,omitempty"`
	EndLine   int     `json:"end_line,omitempty"`
	Content   string  `json:"-"`
	Score     float64 `json:"score"`
	Direct    bool    `json:"direct,omitempty"`
}

// Request is one deterministic packing run.
type Request struct {
	Sources []Source
	Budget  Budget
	Render  RenderMode
	// DirectFloorFraction overrides DefaultDirectFloorFraction when > 0.
	DirectFloorFraction float64
	// Registry enables session dedup and handle resolution; nil disables both.
	Registry *Registry
	// Governor enforces candidate/output/wall limits fail-safe; nil is
	// unlimited.
	Governor *Governor
}

// PackedItem is one emitted (or dedup-referenced) source range.
type PackedItem struct {
	Path       string  `json:"path"`
	StartLine  int     `json:"start_line,omitempty"`
	EndLine    int     `json:"end_line,omitempty"`
	Score      float64 `json:"score"`
	Direct     bool    `json:"direct,omitempty"`
	Render     string  `json:"render"`
	Content    string  `json:"content,omitempty"`
	Ref        string  `json:"ref"`
	Dedup      bool    `json:"dedup,omitempty"`
	DedupRef   string  `json:"dedup_ref,omitempty"`
	Bytes      int     `json:"bytes"`
	Tokens     int     `json:"tokens"`
	Truncated  bool    `json:"truncated,omitempty"`
	Handle     string  `json:"handle"`
	HandleKind string  `json:"handle_kind"`
}

// Omission records a source left out of the packed output, with the reason
// and a valid expansion handle so nothing is dropped silently.
type Omission struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Reason    string `json:"reason"` // budget | limit_candidates | limit_output_bytes | limit_wall_time
	Handle    string `json:"handle"`
}

// Meta is the explicit accounting for one packed result.
type Meta struct {
	Schema         string  `json:"schema"`
	Render         string  `json:"render"`
	BudgetBytes    int     `json:"budget_bytes"` // 0 = unlimited
	BudgetTokens   int     `json:"budget_tokens,omitempty"`
	UsedBytes      int     `json:"used_bytes"`
	UsedTokens     int     `json:"used_tokens"`
	Unallocated    int     `json:"unallocated_bytes"`
	Items          int     `json:"items"`
	Truncated      int     `json:"truncated"`
	Omitted        int     `json:"omitted"`
	Deduped        int     `json:"deduped"`
	DedupSavedByte int     `json:"dedup_saved_bytes"`
	Governor       *Report `json:"governor,omitempty"`
}

// Result is the packed context plus its omission and accounting metadata.
type Result struct {
	Items   []PackedItem `json:"items"`
	Omitted []Omission   `json:"omitted,omitempty"`
	Meta    Meta         `json:"meta"`
}

// Pack renders, budgets, dedups, and accounts for sources deterministically:
// identical requests (with a fresh or equally-advanced registry) produce
// byte-identical results.
func Pack(req Request) Result {
	ordered := append([]Source(nil), req.Sources...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		return a.EndLine < b.EndLine
	})

	mode := req.Render
	if mode == "" {
		mode = RenderFull
	}
	limit := req.Budget.ByteLimit()

	rendered := make([]string, len(ordered))
	sizes := make([]int, len(ordered))
	for i, s := range ordered {
		rendered[i] = Render(s.Content, mode)
		sizes[i] = len(rendered[i])
	}
	alloc := allocate(ordered, sizes, limit, floorFraction(req))

	res := Result{
		Items: []PackedItem{},
		Meta: Meta{
			Schema:       SchemaID,
			Render:       string(mode),
			BudgetBytes:  limit,
			BudgetTokens: req.Budget.MaxTokens,
		},
	}
	allocated := 0
	for _, a := range alloc {
		allocated += a
	}
	if limit > 0 {
		res.Meta.Unallocated = limit - allocated
	}

	for i, s := range ordered {
		key := Key{Path: s.Path, StartLine: s.StartLine, EndLine: s.EndLine,
			Variant: string(mode) + fmt.Sprintf(":%d", alloc[i])}
		handle := NewHandle(key, s.Content)
		if req.Registry != nil {
			req.Registry.Issue(handle)
		}
		omit := func(reason string) {
			res.Meta.Omitted++
			res.Omitted = append(res.Omitted, Omission{
				Path: s.Path, StartLine: s.StartLine, EndLine: s.EndLine,
				Reason: reason, Handle: handle.ID,
			})
		}
		if req.Governor != nil {
			if err := req.Governor.CheckTime(); err != nil {
				omit("limit_wall_time")
				continue
			}
			if !req.Governor.AllowCandidate() {
				omit("limit_candidates")
				continue
			}
		}

		content := rendered[i]
		truncated := false
		if limit > 0 && alloc[i] < len(content) {
			content = truncateLines(content, alloc[i])
			truncated = true
		}
		if truncated && content == "" {
			// Allocation could not fit even one line: omit explicitly
			// rather than emitting a zero-byte item.
			truncated = false
			omit("budget")
			continue
		}
		if req.Governor != nil && !req.Governor.ReserveOutputBytes(len(content)) {
			omit("limit_output_bytes")
			continue
		}

		item := PackedItem{
			Path: s.Path, StartLine: s.StartLine, EndLine: s.EndLine,
			Score: s.Score, Direct: s.Direct, Render: string(mode),
			Content: content, Bytes: len(content), Tokens: EstimateTokens(content),
			Truncated: truncated, Handle: handle.ID, HandleKind: string(handle.Kind),
		}
		if req.Registry != nil {
			dedup, ref := req.Registry.Track(key, handle.Fingerprint)
			item.Ref = ref
			if dedup {
				item.Dedup = true
				item.DedupRef = ref
				res.Meta.DedupSavedByte += item.Bytes
				item.Content = ""
				item.Bytes = 0
				item.Tokens = 0
				item.Truncated = false
				res.Meta.Deduped++
			}
		}
		if truncated && !item.Dedup {
			res.Meta.Truncated++
		}
		res.Meta.UsedBytes += item.Bytes
		res.Meta.UsedTokens += item.Tokens
		res.Items = append(res.Items, item)
	}
	res.Meta.Items = len(res.Items)
	if req.Governor != nil {
		rep := req.Governor.Report()
		res.Meta.Governor = &rep
	}
	return res
}

func floorFraction(req Request) float64 {
	f := req.DirectFloorFraction
	if f <= 0 {
		f = DefaultDirectFloorFraction
	}
	if f > 1 {
		f = 1
	}
	return f
}

// allocate splits a byte limit across sources: direct sources first receive
// an equal share of the floor pool (capped at their size), then the
// remainder is distributed proportional to score. Allocations never exceed
// rendered size; unused bytes are reported via Meta.Unallocated.
func allocate(sources []Source, sizes []int, limit int, floorFrac float64) []int {
	alloc := make([]int, len(sources))
	if limit <= 0 {
		copy(alloc, sizes)
		return alloc
	}
	direct := 0
	for _, s := range sources {
		if s.Direct {
			direct++
		}
	}
	remaining := limit
	if direct > 0 {
		perDirect := int(float64(limit) * floorFrac / float64(direct))
		for i, s := range sources {
			if !s.Direct {
				continue
			}
			grant := perDirect
			if grant > sizes[i] {
				grant = sizes[i]
			}
			alloc[i] = grant
			remaining -= grant
		}
	}
	totalWeight := 0.0
	weights := make([]float64, len(sources))
	for i, s := range sources {
		w := s.Score
		if w < 0 {
			w = 0
		}
		weights[i] = w
		totalWeight += w
	}
	for i := range sources {
		share := 0
		if totalWeight > 0 {
			share = int(float64(remaining) * weights[i] / totalWeight)
		} else if len(sources) > 0 {
			share = remaining / len(sources)
		}
		alloc[i] += share
		if alloc[i] > sizes[i] {
			alloc[i] = sizes[i]
		}
	}
	return alloc
}

// truncateLines cuts content to at most maxBytes keeping whole lines.
func truncateLines(content string, maxBytes int) string {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(content) <= maxBytes {
		return content
	}
	var b strings.Builder
	for i, line := range strings.Split(content, "\n") {
		need := len(line)
		if i > 0 {
			need++ // newline separator
		}
		if b.Len()+need > maxBytes {
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

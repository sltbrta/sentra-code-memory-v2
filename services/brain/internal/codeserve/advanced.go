package codeserve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codecrawl"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/contextpack"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/workflow"
)

// QueryMode is an explicit request-level quality/cost choice.
type QueryMode string

const (
	ModeFast    QueryMode = "fast"
	ModeQuality QueryMode = "quality"
	ModeDeep    QueryMode = "deep"
)

type queryLimits struct{ files, symbols, iterations, matches, bytes int }

func requestLimits(req Request) (QueryMode, queryLimits, error) {
	mode := QueryMode(str(req, "mode"))
	if mode == "" {
		mode = ModeQuality
	}
	var l queryLimits
	switch mode {
	case ModeFast:
		l = queryLimits{16, 8, 4, 32, 16 << 10}
	case ModeQuality:
		l = queryLimits{48, 16, 8, 128, 64 << 10}
	case ModeDeep:
		l = queryLimits{128, 32, 16, 512, 256 << 10}
	default:
		return "", l, fmt.Errorf("mode must be fast, quality, or deep")
	}
	if n := intField(req, "max_files", 0); n > 0 && n < l.files {
		l.files = n
	}
	if n := intField(req, "max_matches", 0); n > 0 && n < l.matches {
		l.matches = n
	}
	if n := budgetBytes(req, l.bytes); n > 0 && n < l.bytes {
		l.bytes = n
	}
	return mode, l, nil
}
func budgetBytes(req Request, def int) int {
	n := intField(req, "max_bytes", 0)
	t := intField(req, "max_tokens", 0) * 4
	if n <= 0 {
		n = def
	}
	if t > 0 && t < n {
		n = t
	}
	if n < 64 {
		n = 64
	}
	return n
}

// RepoMapRequest is the canonical typed code_repo_map request.
type RepoMapRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Q         string    `json:"q"`
	Mode      QueryMode `json:"mode,omitempty"`
	MaxBytes  int       `json:"max_bytes,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	MaxFiles  int       `json:"max_files,omitempty"`
}
type RepoMapResponse struct {
	ResponseMeta
	Query     string                   `json:"q"`
	Mode      QueryMode                `json:"mode"`
	Authority string                   `json:"authority"`
	Entries   []codecrawl.RepoMapEntry `json:"entries"`
	Map       string                   `json:"map"`
	Context   contextpack.Meta         `json:"context_meta"`
	Truncated bool                     `json:"truncated"`
}

func handleRepoMap(req Request) Response {
	q := str(req, "q")
	if q == "" {
		return errResp(string(VerbRepoMap), "q required")
	}
	mode, l, err := requestLimits(req)
	if err != nil {
		return errResp(string(VerbRepoMap), err.Error())
	}
	idx, _, _, err := loadIndex(req)
	if err != nil {
		return idxErrResp(string(VerbRepoMap), err)
	}
	entries := idx.RepoMap(q, codecrawl.RepoMapOptions{MaxFiles: l.files, MaxSymbols: l.symbols, Iterations: l.iterations})
	sources := make([]contextpack.Source, 0, len(entries))
	for _, e := range entries {
		var b strings.Builder
		fmt.Fprintf(&b, "%s:\n", e.Path)
		for _, s := range e.Symbols {
			fmt.Fprintf(&b, "  %s %s\n", s.Kind, s.Name)
		}
		sources = append(sources, contextpack.Source{Path: e.Path, Content: b.String(), Score: e.Score, Direct: e.Direct})
	}
	packed := contextpack.Pack(contextpack.Request{Sources: sources, Budget: contextpack.Budget{MaxBytes: l.bytes}, Render: contextpack.RenderFull, DirectFloorFraction: contextpack.DefaultDirectFloorFraction})
	var b strings.Builder
	for _, item := range packed.Items {
		b.WriteString(item.Content)
	}
	return okResp(string(VerbRepoMap), map[string]any{"q": q, "mode": mode, "authority": "heuristic", "authority_note": "personalized PageRank over lexical/symbol/AST-derived links; not compiler truth", "entries": entries, "map": b.String(), "context_meta": packed.Meta, "truncated": packed.Meta.Truncated > 0 || packed.Meta.Omitted > 0})
}

// StructuralMatch is one bounded deterministic pattern occurrence.
type StructuralMatch struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Excerpt string `json:"excerpt"`
}
type StructuralSearchRequest struct {
	Verb       string    `json:"verb"`
	Root       string    `json:"root"`
	Pattern    string    `json:"pattern"`
	RuleID     string    `json:"rule_id,omitempty"`
	Mode       QueryMode `json:"mode,omitempty"`
	MaxFiles   int       `json:"max_files,omitempty"`
	MaxMatches int       `json:"max_matches,omitempty"`
	MaxBytes   int       `json:"max_bytes,omitempty"`
	MaxTokens  int       `json:"max_tokens,omitempty"`
}
type StructuralSearchResponse struct {
	ResponseMeta
	Pattern   string            `json:"pattern"`
	RuleID    string            `json:"rule_id,omitempty"`
	Mode      QueryMode         `json:"mode"`
	Authority string            `json:"authority"`
	Matches   []StructuralMatch `json:"matches"`
	Truncated bool              `json:"truncated"`
}

func compileHeuristicPattern(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("pattern required")
	}
	var b strings.Builder
	for i := 0; i < len(pattern); {
		if pattern[i] == '$' {
			j := i + 1
			for j < len(pattern) && ((pattern[j] >= 'A' && pattern[j] <= 'Z') || pattern[j] == '_') {
				j++
			}
			if j > i+1 {
				b.WriteString(`[A-Za-z_][A-Za-z0-9_]*`)
				i = j
				continue
			}
		}
		b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		i++
	}
	return regexp.Compile(b.String())
}
func handleStructuralSearch(req Request) Response {
	root := str(req, "root")
	pattern := str(req, "pattern")
	if root == "" || pattern == "" {
		return errResp(string(VerbStructuralSearch), "root and pattern required")
	}
	mode, l, err := requestLimits(req)
	if err != nil {
		return errResp(string(VerbStructuralSearch), err.Error())
	}
	rx, err := compileHeuristicPattern(pattern)
	if err != nil {
		return errResp(string(VerbStructuralSearch), err.Error())
	}
	files, err := codecrawl.SourceFiles(root)
	if err != nil {
		return idxErrResp(string(VerbStructuralSearch), err)
	}
	sort.Strings(files)
	rootAbs, _ := filepath.Abs(root)
	matches := make([]StructuralMatch, 0)
	used := 0
	truncated := false
	for fi, path := range files {
		if fi >= l.files {
			truncated = true
			break
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(raw) > 1<<20 {
			raw = raw[:1<<20]
			truncated = true
		}
		lines := strings.Split(string(raw), "\n")
		for n, line := range lines {
			locs := rx.FindAllStringIndex(line, -1)
			for _, loc := range locs {
				excerpt := strings.TrimSpace(line)
				if len(excerpt) > 240 {
					excerpt = excerpt[:240]
				}
				cost := len(excerpt)
				if len(matches) >= l.matches || used+cost > l.bytes {
					truncated = true
					break
				}
				rel, _ := filepath.Rel(rootAbs, path)
				matches = append(matches, StructuralMatch{Path: filepath.ToSlash(rel), Line: n + 1, Column: loc[0] + 1, Excerpt: excerpt})
				used += cost
			}
			if truncated && len(matches) >= l.matches {
				break
			}
		}
		if len(matches) >= l.matches {
			break
		}
	}
	return okResp(string(VerbStructuralSearch), map[string]any{"pattern": pattern, "rule_id": str(req, "rule_id"), "mode": mode, "authority": "heuristic", "authority_note": "deterministic text-pattern rule; metavariables match identifiers; not AST/compiler truth", "matches": matches, "truncated": truncated})
}

// BuildMetadata is detected from repository manifests without executing tools.
type BuildMetadata struct {
	Ecosystem string   `json:"ecosystem"`
	Manifest  string   `json:"manifest"`
	Commands  []string `json:"commands"`
}
type DiagnosticsRequest struct {
	Verb string `json:"verb"`
	IndexSelector
	Mode      QueryMode `json:"mode,omitempty"`
	MaxBytes  int       `json:"max_bytes,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}
type DiagnosticsResponse struct {
	ResponseMeta
	Mode              QueryMode             `json:"mode"`
	Authority         string                `json:"authority"`
	IndexedFiles      int                   `json:"indexed_files"`
	SymbolDefinitions int                   `json:"symbol_definitions"`
	SymbolReferences  int                   `json:"symbol_references"`
	Graph             *codecrawl.GraphStats `json:"graph,omitempty"`
	Build             []BuildMetadata       `json:"build"`
}

func handleDiagnostics(req Request) Response {
	mode, _, err := requestLimits(req)
	if err != nil {
		return errResp(string(VerbDiagnostics), err.Error())
	}
	idx, root, _, err := loadIndex(req)
	if err != nil {
		return idxErrResp(string(VerbDiagnostics), err)
	}
	defs, refs := idx.SymbolStats()
	var gs *codecrawl.GraphStats
	if g := idx.Graph(); g != nil {
		x := g.Stats
		gs = &x
	}
	build := detectBuild(root)
	return okResp(string(VerbDiagnostics), map[string]any{"mode": mode, "authority": "heuristic", "authority_note": "index and manifest metadata only; no compiler/build was run", "indexed_files": idx.FileCount(), "symbol_definitions": defs, "symbol_references": refs, "graph": gs, "build": build})
}
func detectBuild(root string) []BuildMetadata {
	candidates := []BuildMetadata{{"go", "go.mod", []string{"go test ./...", "go vet ./..."}}, {"node", "package.json", []string{"npm test"}}, {"rust", "Cargo.toml", []string{"cargo test", "cargo check"}}, {"python", "pyproject.toml", []string{"python -m pytest"}}}
	out := []BuildMetadata{}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(root, c.Manifest)); err == nil && !st.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

// ApplyChangeSetRequest is the canonical typed mutating surface.
type ApplyChangeSetRequest struct {
	Verb       string             `json:"verb"`
	Root       string             `json:"root"`
	IndexCache string             `json:"index_cache,omitempty"`
	Workers    int                `json:"workers,omitempty"`
	ChangeSet  workflow.ChangeSet `json:"changeset"`
}
type ApplyChangeSetResponse struct {
	ResponseMeta
	Receipt      workflow.ApplyReceipt `json:"receipt"`
	Reindexed    bool                  `json:"reindexed"`
	IndexDigest  string                `json:"index_digest,omitempty"`
	IndexMatches bool                  `json:"index_matches"`
}

func handleApplyChangeSet(ctx context.Context, req Request) Response {
	root := str(req, "root")
	raw, err := json.Marshal(req["changeset"])
	if root == "" || err != nil || string(raw) == "null" {
		return errResp(string(VerbApplyChangeSet), "root and changeset required")
	}
	var cs workflow.ChangeSet
	if err := json.Unmarshal(raw, &cs); err != nil {
		return errResp(string(VerbApplyChangeSet), "invalid changeset")
	}
	receipt, applyErr := workflow.ApplyChangeSet(ctx, root, cs, workflow.ApplyOptions{})
	if applyErr != nil {
		return Response{"ok": false, "verb": string(VerbApplyChangeSet), "error": "changeset rejected", "error_code": string(ErrChangeSetRejected), "product_owned": true, "receipt": receipt}
	}
	workers := intField(req, "workers", 4)
	cache := str(req, "index_cache")
	if cache == "" {
		cache = filepath.Join(root, ".sentra")
	}
	idx, _, _, _, indexErr := codecrawl.OpenOrRefresh(root, filepath.Join(cache, codecrawl.DefaultIndexFile), workers, false)
	reindexed := indexErr == nil
	indexDigest := ""
	indexMatches := reindexed
	predicted := map[string]string{}
	for _, edit := range cs.Edits {
		predicted[edit.Path] = edit.PredictedDigest
	}
	if idx != nil {
		parts := make([]string, 0, len(receipt.Paths))
		for _, p := range receipt.Paths {
			observed := idx.FileDigest(p)
			parts = append(parts, p+"="+observed)
			if observed == "" || observed != predicted[p] {
				indexMatches = false
			}
		}
		indexDigest = workflow.Digest([]byte(strings.Join(parts, "\n")))
	}
	out := map[string]any{"receipt": receipt, "reindexed": reindexed, "index_digest": indexDigest, "index_matches": indexMatches}
	if indexErr != nil {
		out["reindex_error"] = "index refresh failed"
	}
	return okResp(string(VerbApplyChangeSet), out)
}

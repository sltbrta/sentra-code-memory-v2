package codecrawl

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"unicode"
)

// SymbolKind classifies a file-local binding (stack-graph inspired, Go-native).
type SymbolKind string

const (
	SymbolDef    SymbolKind = "def"
	SymbolRef    SymbolKind = "ref"
	SymbolImport SymbolKind = "import"
)

// Symbol is one name occurrence in a single file (file-disjoint subgraph node).
type Symbol struct {
	Name string
	Kind SymbolKind
	File string
}

// SymbolGraph is a file-incremental name graph: defs and refs keyed by symbol
// name, with no edges crossing files at construction time. Cross-file
// resolution is query-time path search over shared names (virtual edges).
type SymbolGraph struct {
	// Defs: name → files that define it.
	Defs map[string][]string
	// Refs: name → files that reference it.
	Refs map[string][]string
	// Imports: file → imported paths (best-effort string form).
	Imports map[string][]string
}

func newSymbolGraph() *SymbolGraph {
	return &SymbolGraph{
		Defs:    map[string][]string{},
		Refs:    map[string][]string{},
		Imports: map[string][]string{},
	}
}

func (g *SymbolGraph) addDef(name, file string) {
	if name == "" || file == "" {
		return
	}
	g.Defs[name] = appendUniqueStr(g.Defs[name], file)
}

func (g *SymbolGraph) addRef(name, file string) {
	if name == "" || file == "" {
		return
	}
	g.Refs[name] = appendUniqueStr(g.Refs[name], file)
}

func (g *SymbolGraph) addImport(file, path string) {
	if file == "" || path == "" {
		return
	}
	g.Imports[file] = appendUniqueStr(g.Imports[file], path)
}

func appendUniqueStr(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// extractSymbols builds a file-local stack-graph-ish subgraph.
// Go uses go/parser; other languages use lightweight identifier heuristics.
func extractSymbols(rel, body string) (defs, refs []string, imports []string) {
	if body == "" {
		return nil, nil, nil
	}
	ext := strings.ToLower(filepath.Ext(rel))
	if ext != ".go" {
		return extractHeuristicSymbols(ext, body)
	}
	return extractGoSymbols(rel, body)
}

func extractGoSymbols(rel, body string) (defs, refs []string, imports []string) {
	if body == "" {
		return nil, nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, body, parser.SkipObjectResolution)
	if err != nil || f == nil {
		// Fallback: identifier scan without AST.
		return fallbackIdents(body)
	}
	defSet := map[string]struct{}{}
	refSet := map[string]struct{}{}
	var imps []string

	for _, imp := range f.Imports {
		if imp.Path != nil {
			p := strings.Trim(imp.Path.Value, `"`)
			imps = appendUniqueStr(imps, p)
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil && d.Name.Name != "" && d.Name.Name != "_" {
				defSet[d.Name.Name] = struct{}{}
			}
			if d.Body != nil {
				ast.Inspect(d.Body, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if !ok || id.Name == "" || id.Name == "_" {
						return true
					}
					if !unicode.IsUpper(rune(id.Name[0])) && id.Name[0] != '_' {
						// Prefer exported + multi-char identifiers as hop seeds.
					}
					if len(id.Name) >= 3 {
						refSet[id.Name] = struct{}{}
					}
					return true
				})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name != nil {
						defSet[s.Name.Name] = struct{}{}
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n != nil && n.Name != "_" {
							defSet[n.Name] = struct{}{}
						}
					}
				}
			}
		}
	}
	// Defs are not refs.
	for d := range defSet {
		delete(refSet, d)
	}
	for d := range defSet {
		defs = append(defs, d)
	}
	for r := range refSet {
		refs = append(refs, r)
	}
	return defs, refs, imps
}

func fallbackIdents(body string) (defs, refs, imports []string) {
	return extractHeuristicSymbols(".go", body)
}

// importStop words must not enter the import graph (keyword pollution).
var importStop = map[string]struct{}{
	"import": {}, "from": {}, "use": {}, "as": {}, "package": {},
	"export": {}, "require": {}, "include": {}, "type": {}, "const": {},
	"static": {}, "pub": {}, "crate": {}, "super": {}, "self": {},
	"mod": {}, "extern": {}, "with": {}, "select": {}, "default": {},
}

// extractHeuristicSymbols is multi-language file-local name binding without a full
// tree-sitter stack-graphs DSL: defs ≈ def/class/fn lines; refs ≈ other idents.
func extractHeuristicSymbols(ext, body string) (defs, refs, imports []string) {
	seenDef := map[string]struct{}{}
	seenRef := map[string]struct{}{}
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		lower := strings.ToLower(trim)
		// imports — skip language keywords so the import graph stays clean
		if strings.HasPrefix(lower, "import ") || strings.HasPrefix(lower, "from ") || strings.HasPrefix(lower, "use ") {
			for _, tok := range tokenizeLine(trim) {
				if len(tok) < 3 {
					continue
				}
				if _, stop := importStop[strings.ToLower(tok)]; stop {
					continue
				}
				imports = appendUniqueStr(imports, tok)
			}
		}
		// defs
		isDefLine := strings.HasPrefix(lower, "def ") ||
			strings.HasPrefix(lower, "class ") ||
			strings.HasPrefix(lower, "function ") ||
			strings.HasPrefix(lower, "fn ") ||
			strings.HasPrefix(lower, "func ") ||
			strings.HasPrefix(lower, "pub fn ") ||
			strings.HasPrefix(lower, "export function ") ||
			strings.HasPrefix(lower, "export const ") ||
			strings.HasPrefix(lower, "export class ") ||
			strings.HasPrefix(lower, "export interface ") ||
			strings.HasPrefix(lower, "export type ") ||
			strings.HasPrefix(lower, "export default ") ||
			strings.HasPrefix(lower, "type ") ||
			strings.HasPrefix(lower, "interface ") ||
			strings.HasPrefix(lower, "const ") ||
			strings.HasPrefix(lower, "let ") ||
			strings.HasPrefix(lower, "struct ") ||
			strings.HasPrefix(lower, "impl ") ||
			strings.HasPrefix(lower, "enum ") ||
			// TypeScript-style "export class Foo implements Bar"
			strings.Contains(lower, " implements ") ||
			// Python async def
			strings.HasPrefix(lower, "async def ")
		for _, tok := range tokenizeLine(trim) {
			if len(tok) < 3 {
				continue
			}
			if isDefLine {
				if _, ok := seenDef[tok]; !ok {
					seenDef[tok] = struct{}{}
					defs = append(defs, tok)
				}
			} else {
				if _, ok := seenRef[tok]; !ok {
					seenRef[tok] = struct{}{}
					refs = append(refs, tok)
				}
			}
		}
	}
	// Drop defs from refs.
	for d := range seenDef {
		delete(seenRef, d)
	}
	refs = refs[:0]
	for r := range seenRef {
		refs = append(refs, r)
	}
	_ = ext
	return defs, refs, imports
}

func tokenizeLine(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			out = append(out, b.String())
		}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// SymbolHop expands seed file paths by one hop: shared def/ref names (virtual
// cross-file edges). Caps result size. Pure search over the prebuilt graph.
func (idx *Index) SymbolHop(seeds []string, maxN int) []string {
	if idx == nil || idx.symbols == nil || maxN <= 0 {
		return nil
	}
	g := idx.symbols
	seedSet := map[string]struct{}{}
	for _, s := range seeds {
		seedSet[s] = struct{}{}
	}
	// Collect names defined or referenced in seeds.
	names := map[string]struct{}{}
	for name, files := range g.Defs {
		for _, f := range files {
			if _, ok := seedSet[f]; ok {
				names[name] = struct{}{}
			}
		}
	}
	for name, files := range g.Refs {
		for _, f := range files {
			if _, ok := seedSet[f]; ok {
				names[name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, maxN)
	seen := map[string]struct{}{}
	for n := range names {
		for _, f := range g.Defs[n] {
			if _, ok := seedSet[f]; ok {
				continue
			}
			if _, ok := seen[f]; ok {
				continue
			}
			seen[f] = struct{}{}
			out = append(out, f)
			if len(out) >= maxN {
				return out
			}
		}
		for _, f := range g.Refs[n] {
			if _, ok := seedSet[f]; ok {
				continue
			}
			if _, ok := seen[f]; ok {
				continue
			}
			seen[f] = struct{}{}
			out = append(out, f)
			if len(out) >= maxN {
				return out
			}
		}
	}
	return out
}

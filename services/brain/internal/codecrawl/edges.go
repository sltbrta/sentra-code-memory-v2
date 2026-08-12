package codecrawl

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// extractTypedEdges produces the bounded, deterministic typed-edge list for
// one file (issue #13). Go files use go/parser for call/reference/import
// edges; everything else falls back to the lexical identifier scan with
// EdgeLexical kind + AuthorityLexical. The result is sorted by
// (Kind, From, To, Line, Column, File) so callers iterating it without an
// explicit sort see a stable order.
//
// The cap is enforced via the package-level edgeCap (default
// MaxEdgesPerFile); exceeding it counts toward GraphStats.Truncated and
// returns the leading edges in deterministic order.
func extractTypedEdges(rel, body string) []Edge {
	if body == "" {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(rel))
	if ext == ".go" {
		edges := extractGoTypedEdges(rel, body)
		return sortEdges(edges)
	}
	edges := extractLexicalTypedEdges(rel, body, ext)
	return sortEdges(edges)
}

// extractGoTypedEdges uses go/parser to extract call/reference/import edges.
// Confidence is high for call edges (we resolved the callee by name) and
// drops to a heuristic floor when a reference cannot be tied to a local
// definition (e.g. an external package symbol). Implementations and
// inheritance are reserved for future phases; the parser hints are
// captured but never asserted (interface satisfaction checks need type
// info that go/parser does not provide).
func extractGoTypedEdges(rel, body string) []Edge {
	if body == "" {
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, body, parser.SkipObjectResolution)
	if err != nil || f == nil {
		// Fall through to lexical even for Go when AST fails; ensures we
		// still produce a deterministic, bounded graph for malformed files.
		return extractLexicalTypedEdges(rel, body, ".go")
	}

	// defs: package-level function and type names per file.
	defs := map[string]struct{}{}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name != nil && d.Name.Name != "" && d.Name.Name != "_" {
				defs[d.Name.Name] = struct{}{}
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name != nil {
						defs[s.Name.Name] = struct{}{}
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n != nil && n.Name != "" && n.Name != "_" {
							defs[n.Name] = struct{}{}
						}
					}
				}
			}
		}
	}

	var out []Edge
	push := func(e Edge) {
		if len(out) >= edgeCap {
			return
		}
		out = append(out, e)
	}

	// Imports: every import is an unresolved EdgeImport (AuthorityAST,
	// low confidence until Phase 2.5 ties packages to local modules).
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "" {
			continue
		}
		pos := fset.Position(imp.Pos())
		// imp.Name lets callers see "alias foo \"pkg/path\"" forms.
		from := ""
		if imp.Name != nil {
			from = imp.Name.Name
		}
		push(Edge{
			From:       from,
			To:         path,
			Kind:       EdgeImport,
			Authority:  AuthorityAST,
			Confidence: 0.6,
			Provenance: Provenance{
				File:     rel,
				Line:     pos.Line,
				Column:   pos.Column,
				Parser:   "go/parser",
				Language: "go",
			},
		})
	}

	// Walk every call and reference in the AST.
	withEnclosingFunc(f, func(stack []ast.Node, n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		calleeName, calleePos, confidence := resolveCalleeName(call, fset)
		if calleeName == "" {
			return true
		}
		callerName := enclosingFuncName(stack)
		if callerName == calleeName {
			// Recursion: keep but mark call from caller to caller for tests.
		}
		push(Edge{
			From:       callerName,
			To:         calleeName,
			Kind:       EdgeCall,
			Authority:  AuthorityAST,
			Confidence: confidence,
			Provenance: Provenance{
				File:     rel,
				Line:     calleePos.Line,
				Column:   calleePos.Column,
				Parser:   "go/parser",
				Language: "go",
			},
		})
		return true
	})

	// References to defined types are recorded with EdgeReference so callers
	// can distinguish plain lexical mentions from real call sites. We only
	// tag identifiers whose name matches a defined symbol; anything else
	// stays in the lexical fallback so we don't pretend we know its kind.
	withEnclosingFunc(f, func(stack []ast.Node, n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name == "" || id.Name == "_" {
			return true
		}
		if _, isDef := defs[id.Name]; !isDef {
			return true
		}
		pos := fset.Position(id.Pos())
		push(Edge{
			From:       enclosingFuncName(stack),
			To:         id.Name,
			Kind:       EdgeReference,
			Authority:  AuthorityAST,
			Confidence: 0.9,
			Provenance: Provenance{
				File:     rel,
				Line:     pos.Line,
				Column:   pos.Column,
				Parser:   "go/parser",
				Language: "go",
			},
		})
		return true
	})

	return out
}

// resolveCalleeName flattens Go call expressions to a best-effort callee
// name and confidence. Method calls on selectors resolve to
// "Receiver.Method" when the receiver is an identifier; package-qualified
// calls resolve to "pkg.Name" so callers can later join them with import
// edges.
func resolveCalleeName(call *ast.CallExpr, fset *token.FileSet) (string, token.Position, float64) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		pos := fset.Position(fn.Pos())
		return fn.Name, pos, 0.95
	case *ast.SelectorExpr:
		pos := fset.Position(fn.Sel.Pos())
		switch x := fn.X.(type) {
		case *ast.Ident:
			return x.Name + "." + fn.Sel.Name, pos, 0.85
		}
		return fn.Sel.Name, pos, 0.55
	case *ast.CallExpr:
		// Higher-order: func()()(). Returning the inner callee keeps BFS
		// going, but confidence drops so callers can flag for review.
		name, pos, conf := resolveCalleeName(fn, fset)
		return name, pos, conf * 0.5
	}
	return "", token.Position{}, 0
}

// enclosingFuncName returns the name of the innermost FuncDecl in stack,
// or "<top-level>" when no FuncDecl ancestor exists. The caller must pass
// the stack of ast.Nodes built by withEnclosingFunc — go/parser does not
// populate Parent pointers by default.
func enclosingFuncName(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		if fn, ok := stack[i].(*ast.FuncDecl); ok {
			if fn.Name != nil {
				return fn.Name.Name
			}
			return "<anon>"
		}
	}
	return "<top-level>"
}

// withEnclosingFunc walks f using ast.Walk, calling visit with the current
// ancestor stack (the most recent node is stack[len-1]). FuncDecl
// boundaries are reflected in the stack so enclosingFuncName(stack) finds
// the nearest enclosing function on demand. The walker relies on
// ast.Walk's full coverage rather than a hand-rolled AST dispatcher; this
// keeps the helper small and immune to new node types added in future Go
// versions.
//
// ast.Walk calls the visitor with the current node on the way down and
// again with nil after each subtree (post-traversal sentinel). We push
// on non-nil nodes and pop on the nil sentinels so the stack always
// reflects the ancestors of the current visit.
func withEnclosingFunc(f ast.Node, visit func(stack []ast.Node, n ast.Node) bool) {
	if f == nil {
		return
	}
	stack := make([]ast.Node, 0, 8)
	ast.Walk(visitor(func(n ast.Node) bool {
		if n == nil {
			// End-of-subtree sentinel: pop the most recent ancestor so the
			// next iteration sees the parent frame.
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			return true
		}
		stack = append(stack, n)
		return visit(stack, n)
	}), f)
}

// visitor wraps a func as the ast.Visitor interface so it can be passed to
// ast.Walk. ast.Walk calls the visitor with the current node; returning
// nil from Visit stops the walk.
type visitor func(ast.Node) bool

func (v visitor) Visit(n ast.Node) ast.Visitor {
	if v == nil || v(n) {
		return v
	}
	return nil
}

// extractLexicalTypedEdges is the fallback for non-Go files. It scans
// identifier lines and emits EdgeLexical entries where the same identifier
// appears in both "definition" and "use" lines. This is intentionally
// conservative — multi-language AST is out of scope for Phase 2 — and
// labels every edge AuthorityLexical with a low confidence so callers
// fail closed.
func extractLexicalTypedEdges(rel, body, ext string) []Edge {
	if body == "" {
		return nil
	}
	lang := languageFromExt(ext)
	parser := "lexical:" + strings.TrimPrefix(ext, ".")
	if parser == "lexical:" {
		parser = "lexical"
	}

	defSet := map[string]struct{}{}
	refLines := map[string][]lexLinePos{}
	importLines := map[string][]lexLinePos{}

	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		lower := strings.ToLower(trim)
		isDef := strings.HasPrefix(lower, "def ") ||
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
			strings.HasPrefix(lower, "var ") ||
			strings.HasPrefix(lower, "static ")
		isImport := strings.HasPrefix(lower, "import ") ||
			strings.HasPrefix(lower, "from ") ||
			strings.HasPrefix(lower, "use ") ||
			strings.HasPrefix(lower, "require(")
		tokens := tokenizeLine(trim)
		if isDef {
			for _, tok := range tokens {
				if isLexicalKeyword(tok) {
					continue
				}
				defSet[tok] = struct{}{}
			}
			continue
		}
		if isImport {
			for _, tok := range tokens {
				if _, stop := importStop[strings.ToLower(tok)]; stop {
					continue
				}
				if len(tok) < 3 {
					continue
				}
				importLines[tok] = append(importLines[tok], lexLinePos{Line: i + 1})
			}
			continue
		}
		for _, tok := range tokens {
			if isLexicalKeyword(tok) {
				continue
			}
			if len(tok) < 3 {
				continue
			}
			refLines[tok] = append(refLines[tok], lexLinePos{Line: i + 1})
		}
	}

	var out []Edge
	push := func(e Edge) {
		if len(out) >= edgeCap {
			return
		}
		out = append(out, e)
	}

	defNames := make([]string, 0, len(defSet))
	for n := range defSet {
		defNames = append(defNames, n)
	}
	sort.Strings(defNames)
	refNames := make([]string, 0, len(refLines))
	for n := range refLines {
		refNames = append(refNames, n)
	}
	sort.Strings(refNames)
	importNames := make([]string, 0, len(importLines))
	for n := range importLines {
		importNames = append(importNames, n)
	}
	sort.Strings(importNames)

	for _, name := range refNames {
		if _, ok := defSet[name]; !ok {
			continue
		}
		// EdgeLexical is symmetric for lexical fallback: use "<file>" as
		// the from-side so the projection stays compatible with the AST
		// shape. Callers can resolve "<file>" against the per-file def
		// graph when they need a tighter origin.
		pos := refLines[name]
		sort.SliceStable(pos, func(i, j int) bool { return pos[i].Line < pos[j].Line })
		first := pos[0]
		push(Edge{
			From:       "<file>",
			To:         name,
			Kind:       EdgeLexical,
			Authority:  AuthorityLexical,
			Confidence: 0.4,
			Provenance: Provenance{
				File:     rel,
				Line:     first.Line,
				Column:   0,
				Parser:   parser,
				Language: lang,
			},
		})
	}
	for _, name := range importNames {
		push(Edge{
			From:       "<file>",
			To:         name,
			Kind:       EdgeImport,
			Authority:  AuthorityLexical,
			Confidence: 0.3,
			Provenance: Provenance{
				File:     rel,
				Line:     importLines[name][0].Line,
				Column:   0,
				Parser:   parser,
				Language: lang,
			},
		})
	}

	return out
}

// lexLinePos tracks where an identifier was observed in lexical scans.
type lexLinePos struct {
	Line int
}

// languageFromExt returns a short human label for the file's language.
// Used to tag the Provenance field; not authoritative.
func languageFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js":
		return "javascript"
	case ".rs":
		return "rust"
	case ".md":
		return "markdown"
	}
	return ""
}

func isLexicalKeyword(tok string) bool {
	if tok == "" {
		return true
	}
	first := rune(tok[0])
	if !unicode.IsLetter(first) {
		return true
	}
	switch strings.ToLower(tok) {
	case "function", "return", "if", "else", "while", "for", "switch",
		"case", "break", "continue", "true", "false", "nil", "null",
		"this", "self", "fn", "let", "const", "var", "static",
		"export", "default", "import", "from", "as", "use", "require",
		"class", "interface", "type", "extends", "implements", "pub",
		"package", "func", "def":
		return true
	}
	return false
}

// sortEdges applies the canonical deterministic order. Returning a fresh
// slice keeps callers from accidentally mutating package-internal state.
func sortEdges(in []Edge) []Edge {
	if len(in) == 0 {
		return nil
	}
	out := make([]Edge, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return string(out[i].Kind) < string(out[j].Kind)
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		if out[i].Provenance.Line != out[j].Provenance.Line {
			return out[i].Provenance.Line < out[j].Provenance.Line
		}
		if out[i].Provenance.Column != out[j].Provenance.Column {
			return out[i].Provenance.Column < out[j].Provenance.Column
		}
		return out[i].Provenance.File < out[j].Provenance.File
	})
	if len(out) > edgeCap {
		out = out[:edgeCap]
	}
	return out
}

// CallersFor is the public wrapper around callersFor so tests and
// diagnostic binaries can introspect the call graph without poking
// private state. It is identical in semantics to the private helper.
func (g *Graph) CallersFor(symbol string, maxN int) []Edge {
	return g.callersFor(symbol, maxN)
}

// callersFor walks the call-site edges pointing at symbol in any file.
// Self-references (edges whose From and To are both symbol) are filtered
// out because they do not represent external callers. The result is
// sorted by (Path, From, Provenance.Line) so impact BFS sees the same
// order regardless of map iteration order. Caps at maxN.
func (g *Graph) callersFor(symbol string, maxN int) []Edge {
	if g == nil || symbol == "" {
		return nil
	}
	if maxN <= 0 {
		maxN = 64
	}
	files := g.fileEdgeKeys()
	var out []Edge
	for _, file := range files {
		list := g.Edges[file]
		for _, e := range list {
			if e.Kind != EdgeCall && e.Kind != EdgeReference && e.Kind != EdgeLexical {
				continue
			}
			if e.To != symbol {
				continue
			}
			if e.From == symbol {
				// Self-reference: same symbol referenced from inside its
				// own file (function name Ident). Not a useful caller for
				// blast-radius analysis.
				continue
			}
			out = append(out, e)
			if len(out) >= maxN {
				return out
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provenance.File != out[j].Provenance.File {
			return out[i].Provenance.File < out[j].Provenance.File
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].Provenance.Line < out[j].Provenance.Line
	})
	return out
}

// calleesFor returns call/reference edges originating from symbol. Mirrors
// callersFor for the reverse direction. The cap is honored symmetrically.
func (g *Graph) calleesFor(symbol string, maxN int) []Edge {
	if g == nil || symbol == "" {
		return nil
	}
	if maxN <= 0 {
		maxN = 64
	}
	files := g.fileEdgeKeys()
	var out []Edge
	for _, file := range files {
		list := g.Edges[file]
		for _, e := range list {
			if e.Kind != EdgeCall && e.Kind != EdgeReference && e.Kind != EdgeLexical {
				continue
			}
			if e.From != symbol {
				continue
			}
			out = append(out, e)
			if len(out) >= maxN {
				return out
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provenance.File != out[j].Provenance.File {
			return out[i].Provenance.File < out[j].Provenance.File
		}
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		return out[i].Provenance.Line < out[j].Provenance.Line
	})
	return out
}

// fileEdgesByImportStem returns unresolved edges whose To looks like an
// import path containing stem. It is bounded so impact BFS over the import
// graph does not balloon on noisy dependency trees.
func (g *Graph) fileEdgesByImportStem(stem string, maxN int) []Edge {
	if g == nil || stem == "" {
		return nil
	}
	if maxN <= 0 {
		maxN = 32
	}
	stemLow := strings.ToLower(stem)
	files := g.fileEdgeKeys()
	var out []Edge
	for _, file := range files {
		list := g.Edges[file]
		for _, e := range list {
			if e.Kind != EdgeImport {
				continue
			}
			if !strings.Contains(strings.ToLower(e.To), stemLow) {
				continue
			}
			out = append(out, e)
			if len(out) >= maxN {
				return out
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provenance.File != out[j].Provenance.File {
			return out[i].Provenance.File < out[j].Provenance.File
		}
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		return out[i].Provenance.Line < out[j].Provenance.Line
	})
	return out
}

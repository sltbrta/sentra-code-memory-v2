package productsearch

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
)

// searchCodeExact builds a P5 codeindex snapshot from CodeRoot and returns
// exact definition/reference/import matches for the query string.
// This is the Stage SearchCode capability on the product facade (working-tree
// sources; generation pin is the crawl receipt digest).
func searchCodeExact(ctx context.Context, req Request) Result {
	root := strings.TrimSpace(req.CodeRoot)
	if root == "" {
		return Result{Failure: "productsearch: code_exact requires CodeRoot", ProductOwned: true}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return Result{Failure: "productsearch: code_exact path: " + err.Error(), ProductOwned: true}
	}
	sources, err := collectP5Sources(rootAbs)
	if err != nil {
		return Result{Failure: "productsearch: code_exact collect: " + err.Error(), ProductOwned: true}
	}
	if len(sources) == 0 {
		return Result{
			Failure: "", ProductOwned: true, Profile: ProfileCodeExact,
			SearchMode: "product_codeindex_exact", Hits: nil,
			RetrievalDiagnostics: map[string]any{"files": 0, "note": "no_p5_sources"},
		}
	}
	snap, err := codeindex.Build(ctx, sources, codeindex.DefaultLimits())
	if err != nil {
		return Result{Failure: "productsearch: code_exact build: " + err.Error(), ProductOwned: true}
	}
	q := strings.TrimSpace(req.Question)
	kind := strings.ToLower(strings.TrimSpace(req.ExactKind))
	hits := make([]Hit, 0, req.TopK)
	for _, file := range snap.Files {
		for _, occ := range file.Occurrences {
			if !exactKindMatch(kind, occ.Kind) {
				continue
			}
			if q != "" && !strings.EqualFold(occ.Text, q) && !strings.Contains(strings.ToLower(occ.Text), strings.ToLower(q)) {
				continue
			}
			hits = append(hits, Hit{
				ID:    occ.Range.Path,
				Title: string(occ.Kind) + " " + occ.Text,
				Text:  occ.Text + " @ " + occ.Range.Path,
				Score: 1,
				Arm:   "codeindex_exact",
			})
			if len(hits) >= req.TopK {
				break
			}
		}
		if len(hits) >= req.TopK {
			break
		}
	}
	return Result{
		Hits: hits, ProductOwned: true, Profile: ProfileCodeExact,
		SearchMode: "product_codeindex_exact",
		Guarantee:  GuaranteeExactP5Codeindex,
		RetrievalDiagnostics: map[string]any{
			"files":          len(snap.Files),
			"receipt_digest": snap.ReceiptDigest,
			"hits":           len(hits),
			"query":          q,
			"kind":           kind,
			"backend":        "codeindex",
			"guarantee":      GuaranteeExactP5Codeindex,
			"not_heuristic":  true,
			"plane":          "code_operator_exact",
		},
	}
}

func exactKindMatch(want string, kind codeindex.Kind) bool {
	switch want {
	case "", "any", "all":
		return true
	case "def", "definition", "defs":
		return kind == codeindex.KindDefinition
	case "ref", "reference", "refs":
		return kind == codeindex.KindReference
	case "import", "imports":
		return kind == codeindex.KindImport
	default:
		return string(kind) == want
	}
}

func collectP5Sources(rootAbs string) ([]codeindex.SourceFile, error) {
	var out []codeindex.SourceFile
	err := filepath.Walk(rootAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == "bazel-*" ||
				name == ".ouroboros" || name == "dist" || name == "target" {
				if name != "." && name != ".." {
					return filepath.SkipDir
				}
			}
			if strings.HasPrefix(name, "bazel-") {
				return filepath.SkipDir
			}
			return nil
		}
		lang, ok := p5Language(path)
		if !ok {
			return nil
		}
		// Cap file size for product interactive use.
		if info.Size() > 1<<20 {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out = append(out, codeindex.SourceFile{
			Path: rel, Language: lang, Content: raw,
		})
		if len(out) >= codeindex.DefaultLimits().MaxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	return out, err
}

func p5Language(path string) (codeindex.Language, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return codeindex.LanguageGo, true
	case ".ts", ".tsx":
		return codeindex.LanguageTypeScript, true
	case ".py":
		return codeindex.LanguagePython, true
	case ".rs":
		return codeindex.LanguageRust, true
	case ".java":
		return codeindex.LanguageJava, true
	default:
		return "", false
	}
}

package productsearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeindex"
	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/repoignore"
)

const maxCodeExactFiles = 16_384

// searchCodeExact projects the P5 files under CodeRoot one at a time and
// returns exact definition/reference/import matches for the query string.
// Per-file projection keeps large repositories within bounded memory and
// avoids turning one aggregate snapshot limit into a repository-wide failure.
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
	q := strings.TrimSpace(req.Question)
	kind := strings.ToLower(strings.TrimSpace(req.ExactKind))
	hitLimit := req.TopK
	if hitLimit < 0 {
		hitLimit = 0
	}
	hits := make([]Hit, 0, hitLimit)
	digest := sha256.New()
	projectedFiles := 0
	exactLimits := codeindex.DefaultLimits()
	// Exact search runs over real repositories, where a generated source file
	// can exceed the conservative snapshot defaults. Stay within codeindex's
	// hard caps while avoiding a whole-repository failure for one large file.
	exactLimits.MaxTokens = 500_000
	exactLimits.MaxResults = 250_000
	exactLimits.MaxLines = 500_000
	exactLimits.MaxColumn = 1 << 20
	for _, source := range sources {
		// Cached on content: the parse is a pure function of the bytes, and
		// this walk re-reads a repository that has usually not changed. See
		// projection_cache.go.
		projection, projectErr := projectCached(ctx, source, exactLimits)
		if projectErr != nil {
			return Result{Failure: "productsearch: code_exact project: " + projectErr.Error(), ProductOwned: true}
		}
		projectedFiles++
		_, _ = digest.Write([]byte(source.Path))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(projection.ReceiptDigest))
		_, _ = digest.Write([]byte{0})
		if len(hits) >= hitLimit {
			continue
		}
		for _, occ := range projection.Occurrences {
			if !exactKindMatch(kind, occ.Kind) {
				continue
			}
			if q != "" && !strings.EqualFold(occ.Text, q) && !strings.Contains(strings.ToLower(occ.Text), strings.ToLower(q)) {
				continue
			}
			hits = append(hits, Hit{
				ID:    occ.Range.Path,
				Title: string(occ.Kind) + " " + occ.Text,
				Text:  fmt.Sprintf("%s @ %s:%d:%d", occ.Text, occ.Range.Path, occ.Range.Start.Line, occ.Range.Start.Column),
				Score: 1,
				Arm:   "codeindex_exact",
			})
			if len(hits) >= hitLimit {
				break
			}
		}
	}
	return Result{
		Hits: hits, ProductOwned: true, Profile: ProfileCodeExact,
		SearchMode: "product_codeindex_exact",
		Guarantee:  GuaranteeExactP5Codeindex,
		RetrievalDiagnostics: map[string]any{
			"files":          projectedFiles,
			"receipt_digest": "sha256:" + hex.EncodeToString(digest.Sum(nil)),
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

// collectP5Sources returns supported-language files in deterministic filepath
// walk order, excluding generated/build/vendor trees and oversized files.
func collectP5Sources(rootAbs string) ([]codeindex.SourceFile, error) {
	ignores, err := repoignore.Load(rootAbs)
	if err != nil {
		return nil, err
	}
	var out []codeindex.SourceFile
	err = filepath.Walk(rootAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		rel, relErr := filepath.Rel(rootAbs, path)
		if relErr != nil {
			return nil
		}
		if ignores.Ignored(rel, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
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
		rel, err = filepath.Rel(rootAbs, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		out = append(out, codeindex.SourceFile{
			Path: rel, Language: lang, Content: raw,
		})
		// Exact projection is processed file-by-file below, so it can cover a
		// larger repository than codeindex.Build's in-memory snapshot cap.
		if len(out) >= maxCodeExactFiles {
			return filepath.SkipAll
		}
		return nil
	})
	return out, err
}

// p5Language maps a supported source extension to the exact P5 language set.
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

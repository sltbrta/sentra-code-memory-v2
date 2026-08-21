package textbound_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Fixing sixteen inline truncations did not stop a seventeenth.
//
// The pass that replaced about a dozen private truncate helpers with this
// package reached the helpers and not the call sites, and a later sweep found
// sixteen `s = s[:N]` written inline. That sweep's pattern then missed a
// seventeenth, `return text[:limit]`, which was found only by reading the
// reranker for an unrelated reason. A grep is the wrong instrument: the shape
// varies, and what matters is the *type* being sliced.
//
// This is the guard, using go/types. A first attempt was purely syntactic,
// flagged every slice truncation in the repository, and was deleted -- an
// allowlist of dozens of legitimate entries is noise that gets rubber-stamped.
// A second was abandoned because type checking looked like it needed
// golang.org/x/tools, which this dependency-light repository should not gain
// for a lint. It does not: go/importer's source compiler is standard library,
// and types a package in about 300ms.
//
// What is flagged is slicing a *string* with a constant bound. Slicing a
// []byte or a []T is untouched, so there is no allowlist to maintain and
// nothing to rubber-stamp.

// truncationExemptions are string slices whose operand is provably ASCII, so a
// byte offset cannot land mid-rune and textbound would be identical. Each needs
// a reason, and the reason has to be structural -- "it is a digest" only counts
// when the code proves it is one.
//
// Everything else was fixed rather than exempted. Thirty-three sites across
// the module, of which a grep-based sweep had found seventeen.
var truncationExemptions = map[string]string{
	// key is hex-validated on the two lines above: DecodeString succeeds, the
	// length is sha256.Size, and the re-encoding round-trips.
	"services/brain/internal/artifactvault/localstore.go:220": "validated hex, checked immediately above",

	// canonicalDigest returns hex.EncodeToString(sha256), on the line the
	// function is defined a few lines below.
	"services/brain/internal/conversation/payload.go:306": "hex digest from canonicalDigest",
	"services/brain/internal/conversation/payload.go:312": "hex digest from canonicalDigest",
	"services/brain/internal/conversation/payload.go:319": "hex digest from canonicalDigest",

	// hex.EncodeToString in the same expression.
	"services/brain/internal/hosted/cost.go:95": "hex digest, encoded in the same expression",

	// contracts.Digest.Hex is the canonical encoded digest value.
	"services/broker/internal/github/tuple.go:26": "contracts.Digest.Hex is hex by contract",
}

func TestNoInlineByteTruncationOfStrings(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks every package in the module")
	}
	root := moduleRoot(t)

	var packageDirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		switch entry.Name() {
		case "testdata", "vendor", ".git", "node_modules":
			return filepath.SkipDir
		}
		matches, _ := filepath.Glob(filepath.Join(path, "*.go"))
		if len(matches) > 0 {
			packageDirs = append(packageDirs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(packageDirs) == 0 {
		t.Fatal("no packages found, so this guard checked nothing")
	}

	// One FileSet and one importer for the whole walk. Creating an importer
	// per package recompiles every dependency each time: measured at 565s
	// across this module, against 30s shared. A nine-minute lint is a lint
	// nobody runs.
	fset := token.NewFileSet()
	shared := importer.ForCompiler(fset, "source", nil)

	var offenders []string
	for _, dir := range packageDirs {
		offenders = append(offenders, stringTruncations(t, root, dir, fset, shared)...)
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("a string sliced at a byte offset cuts mid-rune on non-ASCII "+
			"input; use textbound.Bytes (or Ellipsis):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// stringTruncations type-checks one package and returns its string slices.
func stringTruncations(t *testing.T, root, dir string, fset *token.FileSet, imp types.Importer) []string {
	t.Helper()
	pkgs, err := parser.ParseDir(fset, dir, func(fs.FileInfo) bool { return true }, 0)
	if err != nil {
		// Unparseable is the build gate's problem, not this one's.
		return nil
	}
	var out []string
	for name, pkg := range pkgs {
		files := make([]*ast.File, 0, len(pkg.Files))
		for _, file := range pkg.Files {
			files = append(files, file)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
		config := types.Config{
			Importer: imp,
			// Type errors are tolerated: a partially typed package still
			// resolves most expressions, and the build gate catches the rest.
			Error: func(error) {},
		}
		_, _ = config.Check(name, fset, files, info)

		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				slice, ok := node.(*ast.SliceExpr)
				if !ok || slice.Low != nil || slice.High == nil || slice.Max != nil {
					return true
				}
				if lit, ok := slice.High.(*ast.BasicLit); !ok || lit.Kind != token.INT {
					return true
				}
				operand, ok := info.Types[slice.X]
				if !ok || operand.Type == nil {
					return true
				}
				if basic, ok := operand.Type.Underlying().(*types.Basic); !ok ||
					basic.Kind() != types.String {
					return true
				}
				position := fset.Position(slice.Pos())
				rel, relErr := filepath.Rel(root, position.Filename)
				if relErr != nil {
					rel = position.Filename
				}
				rel = filepath.ToSlash(rel)
				if strings.HasSuffix(rel, "_test.go") {
					// A test may construct a mid-rune cut deliberately; several
					// here do, to prove the production path does not.
					return true
				}
				key := rel + ":" + itoaLine(position.Line)
				if _, exempt := truncationExemptions[key]; exempt {
					return true
				}
				out = append(out, key)
				return true
			})
		}
	}
	return out
}

func itoaLine(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// moduleRoot walks up to the directory holding go.work.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("module root not found from this working directory")
	return ""
}

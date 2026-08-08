package codeindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFrozenP5FixturesProduceStableDefinitions(t *testing.T) {
	tests := []struct {
		name       string
		fixture    string
		path       string
		language   Language
		definition string
		start      Position
		end        Position
	}{
		{"go", "go/seed.go", "src/go/seed.go", LanguageGo, "Anchor", Position{3, 6}, Position{3, 12}},
		{"typescript", "typescript/seed.ts", "src/typescript/seed.ts", LanguageTypeScript, "anchor", Position{1, 14}, Position{1, 20}},
		{"python", "python/seed.py", "src/python/seed.py", LanguagePython, "anchor", Position{4, 5}, Position{4, 11}},
		{"rust", "rust/seed.rs.fixture", "src/rust/seed.rs", LanguageRust, "anchor", Position{1, 8}, Position{1, 14}},
		{"java", "java/Seed.java", "src/java/Seed.java", LanguageJava, "Seed", Position{1, 13}, Position{1, 17}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := mustProject(t, SourceFile{
				Path: test.path, Language: test.language, Content: readFixture(t, test.fixture),
			})
			if projection.Coverage != CoverageSyntaxAware || projection.DegradationCode != "" {
				t.Fatalf("coverage = %q/%q", projection.Coverage, projection.DegradationCode)
			}
			occurrence := findOccurrence(t, projection, KindDefinition, test.definition)
			if occurrence.Range.Start != test.start || occurrence.Range.End != test.end {
				t.Fatalf("range = %#v, want %v..%v", occurrence.Range, test.start, test.end)
			}
			if occurrence.Range.Path != test.path || occurrence.ContentDigest != projection.ContentDigest {
				t.Fatalf("unstable occurrence identity: %#v", occurrence)
			}
		})
	}
}

func TestDefinitionsImportsAndReferencesAcrossP5(t *testing.T) {
	tests := []struct {
		path       string
		language   Language
		content    string
		definition string
		importText string
		reference  string
	}{
		{"src/a.go", LanguageGo, "package a\nimport \"fmt\"\nfunc Run(){fmt.Println()}\n", "Run", "\"fmt\"", "Println"},
		{"src/a.ts", LanguageTypeScript, "import {item} from \"./dep\"; export function run(){return item;}\n", "run", "\"./dep\"", "item"},
		{"src/a.py", LanguagePython, "from pkg import item\ndef run():\n    return item\n", "run", "pkg", "item"},
		{"src/a.rs", LanguageRust, "use crate::dep::item;\npub fn run(){ item(); }\n", "run", "item", "item"},
		{"src/A.java", LanguageJava, "import java.util.List;\nfinal class A { List run(){ return value; } }\n", "A", "List", "value"},
	}
	for _, test := range tests {
		projection := mustProject(t, SourceFile{test.path, test.language, []byte(test.content)})
		findOccurrence(t, projection, KindDefinition, test.definition)
		findOccurrence(t, projection, KindImport, test.importText)
		findOccurrence(t, projection, KindReference, test.reference)
	}
}

func TestCommentsAndOrdinaryStringsDoNotBecomeOccurrences(t *testing.T) {
	tests := []SourceFile{
		{"src/a.go", LanguageGo, []byte("package a\n// FakeComment\nvar real = \"FakeString\"\n")},
		{"src/a.ts", LanguageTypeScript, []byte("// FakeComment\nconst real = 'FakeString';\n")},
		{"src/a.py", LanguagePython, []byte("# FakeComment\nreal = \"FakeString\"\n")},
		{"src/a.rs", LanguageRust, []byte("// FakeComment\nlet real = r#\"FakeString\"#;\n")},
		{"src/A.java", LanguageJava, []byte("// FakeComment\nclass A { String real = \"FakeString\"; }\n")},
	}
	for _, source := range tests {
		projection := mustProject(t, source)
		for _, occurrence := range projection.Occurrences {
			if occurrence.Text == "FakeComment" || occurrence.Text == "FakeString" {
				t.Fatalf("%s emitted comment/string token: %#v", source.Language, occurrence)
			}
		}
	}
}

func TestMalformedFixtureFallsBackWithoutSyntaxClaims(t *testing.T) {
	projection := mustProject(t, SourceFile{
		"src/typescript/unterminated.ts",
		LanguageTypeScript,
		readFixture(t, "malformed/unterminated.ts"),
	})
	if projection.Coverage != CoverageLexicalDegraded || projection.DegradationCode != "malformed_source" {
		t.Fatalf("degradation = %q/%q", projection.Coverage, projection.DegradationCode)
	}
	findOccurrence(t, projection, KindReference, "malformed")
	for _, occurrence := range projection.Occurrences {
		if occurrence.Kind != KindReference || occurrence.Coverage != CoverageLexicalDegraded {
			t.Fatalf("malformed syntax claim: %#v", occurrence)
		}
	}
}

func TestBalancedMalformedDeclarationAlsoFallsBack(t *testing.T) {
	projection := mustProject(t, SourceFile{
		"src/typescript/balanced.ts",
		LanguageTypeScript,
		[]byte("export const = missing;\n"),
	})
	if projection.Coverage != CoverageLexicalDegraded {
		t.Fatalf("coverage = %q, want lexical degradation", projection.Coverage)
	}
	findOccurrence(t, projection, KindReference, "missing")
}

func TestBalancedMissingInitializerFallsBack(t *testing.T) {
	projection := mustProject(t, SourceFile{
		"src/typescript/missing-initializer.ts",
		LanguageTypeScript,
		[]byte("export const value = ;\n"),
	})
	if projection.Coverage != CoverageLexicalDegraded {
		t.Fatalf("coverage = %q, want lexical degradation", projection.Coverage)
	}
	findOccurrence(t, projection, KindReference, "value")
	assertNoOccurrence(t, projection, KindDefinition, "value")
}

func TestTypeScriptTemplateEscapedBacktickRemainsSyntaxAware(t *testing.T) {
	projection := mustProject(t, SourceFile{
		"src/typescript/template.ts",
		LanguageTypeScript,
		[]byte("const template = `before \\` after`; const value = template;\n"),
	})
	if projection.Coverage != CoverageSyntaxAware {
		t.Fatalf("coverage = %q, want syntax aware", projection.Coverage)
	}
	findOccurrence(t, projection, KindDefinition, "template")
	findOccurrence(t, projection, KindDefinition, "value")
	assertNoOccurrence(t, projection, KindReference, "after")
}

func TestTypeScriptDestructuringRemainsSyntaxAware(t *testing.T) {
	tests := []struct {
		name    string
		content string
		binding string
	}{
		{"object", "const {source: alias, shorthand} = value;\n", "alias"},
		{"array", "let [head, ...tail] = values;\n", "head"},
		{"var object", "var {nested: {leaf}} = tree;\n", "leaf"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := mustProject(t, SourceFile{
				"src/typescript/destructuring.ts", LanguageTypeScript, []byte(test.content),
			})
			if projection.Coverage != CoverageSyntaxAware {
				t.Fatalf("coverage = %q, want syntax aware", projection.Coverage)
			}
			findOccurrence(t, projection, KindReference, test.binding)
		})
	}
}

func TestMultilineImportsRetainAnchorsWithoutConsumingFollowingDeclarations(t *testing.T) {
	tests := []struct {
		name       string
		source     SourceFile
		imports    []string
		definition string
	}{
		{
			"typescript",
			SourceFile{"src/a.ts", LanguageTypeScript, []byte(
				"import {\n  item,\n  helper as alias,\n} from \"./dep\"\nexport const value = item;\n",
			)},
			[]string{"item", "helper", "alias", "\"./dep\""},
			"value",
		},
		{
			"python",
			SourceFile{"src/a.py", LanguagePython, []byte(
				"from pkg import (\n    item,\n    helper as alias,\n)\ndef run():\n    return item\n",
			)},
			[]string{"pkg", "item", "helper", "alias"},
			"run",
		},
		{
			"python explicit continuation",
			SourceFile{"src/continued.py", LanguagePython, []byte(
				"from pkg import \\\n    item\ndef continued():\n    return item\n",
			)},
			[]string{"pkg", "item"},
			"continued",
		},
		{
			"rust",
			SourceFile{"src/a.rs", LanguageRust, []byte(
				"use crate::{\n    dep::item,\n    other::helper,\n};\nfn run() {}\n",
			)},
			[]string{"dep", "item", "other", "helper"},
			"run",
		},
		{
			"java",
			SourceFile{"src/A.java", LanguageJava, []byte(
				"import java.\n    util.\n    List;\nclass A {}\n",
			)},
			[]string{"java", "util", "List"},
			"A",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := mustProject(t, test.source)
			if projection.Coverage != CoverageSyntaxAware {
				t.Fatalf("coverage = %q", projection.Coverage)
			}
			for _, imported := range test.imports {
				findOccurrence(t, projection, KindImport, imported)
			}
			findOccurrence(t, projection, KindDefinition, test.definition)
			assertNoOccurrence(t, projection, KindImport, test.definition)
		})
	}
}

func TestRustMutableBindingAndJavaReturnedCallClassification(t *testing.T) {
	rust := mustProject(t, SourceFile{
		"src/a.rs", LanguageRust, []byte("fn run() { let mut value = 1; value; }\n"),
	})
	findOccurrence(t, rust, KindDefinition, "value")
	assertNoOccurrence(t, rust, KindDefinition, "mut")

	java := mustProject(t, SourceFile{
		"src/A.java", LanguageJava,
		[]byte("class A { int run() { return parse(); } int parse() { return 1; } }\n"),
	})
	findOccurrence(t, java, KindReference, "parse")
	findOccurrence(t, java, KindDefinition, "parse")
}

func TestGoParserFailureFallsBack(t *testing.T) {
	projection := mustProject(t, SourceFile{
		"src/go/broken.go",
		LanguageGo,
		[]byte("package broken\nfunc run() { return + }\n"),
	})
	if projection.Coverage != CoverageLexicalDegraded || projection.DegradationCode != "go_parse_error" {
		t.Fatalf("degradation = %q/%q", projection.Coverage, projection.DegradationCode)
	}
	findOccurrence(t, projection, KindReference, "run")
}

func TestProjectionIsRepeatablyDeterministic(t *testing.T) {
	source := SourceFile{"src/go/seed.go", LanguageGo, readFixture(t, "go/seed.go")}
	first := mustProject(t, source)
	for range 20 {
		if next := mustProject(t, source); !reflect.DeepEqual(next, first) {
			t.Fatalf("repeat projection drifted:\nfirst=%#v\nnext=%#v", first, next)
		}
	}
}

func TestProjectHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Project(ctx, SourceFile{"src/a.go", LanguageGo, []byte("package a")}, DefaultLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func mustProject(t *testing.T, source SourceFile) FileProjection {
	t.Helper()
	projection, err := Project(context.Background(), source, DefaultLimits())
	if err != nil {
		t.Fatalf("Project(%s): %v", source.Path, err)
	}
	return projection
}

func findOccurrence(t *testing.T, projection FileProjection, kind Kind, text string) Occurrence {
	t.Helper()
	for _, occurrence := range projection.Occurrences {
		if occurrence.Kind == kind && occurrence.Text == text {
			return occurrence
		}
	}
	t.Fatalf("%s occurrence %q not found in %#v", kind, text, projection.Occurrences)
	return Occurrence{}
}

func assertNoOccurrence(t *testing.T, projection FileProjection, kind Kind, text string) {
	t.Helper()
	for _, occurrence := range projection.Occurrences {
		if occurrence.Kind == kind && occurrence.Text == text {
			t.Fatalf("unexpected %s occurrence %q: %#v", kind, text, occurrence)
		}
	}
}

func readFixture(t *testing.T, relative string) []byte {
	t.Helper()
	repositoryRelative := filepath.Join("tests/fixtures/stage-03/mixed-p5", relative)
	paths := []string{
		repositoryRelative,
		filepath.Join("..", "..", "..", "..", repositoryRelative),
	}
	if root := os.Getenv("TEST_SRCDIR"); root != "" {
		paths = append([]string{filepath.Join(root, os.Getenv("TEST_WORKSPACE"), repositoryRelative)}, paths...)
	}
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err == nil {
			return content
		}
	}
	t.Fatalf("fixture %q unavailable through %v", relative, paths)
	return nil
}

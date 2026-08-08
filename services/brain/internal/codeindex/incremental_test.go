package codeindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type frozenManifest struct {
	Operations []frozenOperation `json:"operations"`
}

type frozenOperation struct {
	Kind      string `json:"kind"`
	Language  string `json:"language"`
	OldPath   string `json:"oldPath"`
	NewPath   string `json:"newPath"`
	Malformed bool   `json:"malformed"`
}

func TestFrozenDeltaIncrementalEqualsCleanBuild(t *testing.T) {
	var manifest frozenManifest
	if err := json.Unmarshal(readFixture(t, "delta-manifest.json"), &manifest); err != nil {
		t.Fatalf("decode frozen delta: %v", err)
	}
	if len(manifest.Operations) != 100 {
		t.Fatalf("frozen operation count = %d", len(manifest.Operations))
	}

	baseSources := make([]SourceFile, 0, 75)
	finalSources := make([]SourceFile, 0, 75)
	changes := make([]Change, 0, 100)
	for _, operation := range manifest.Operations {
		language := Language(operation.Language)
		baseContent := frozenContent(t, operation, "base")
		switch operation.Kind {
		case "add":
			added := SourceFile{operation.NewPath, language, frozenContent(t, operation, "added")}
			finalSources = append(finalSources, added)
			changes = append(changes, Change{Kind: ChangeUpsert, File: added})
		case "modify":
			baseSources = append(baseSources, SourceFile{operation.OldPath, language, baseContent})
			modified := SourceFile{operation.OldPath, language, frozenContent(t, operation, "modified")}
			finalSources = append(finalSources, modified)
			changes = append(changes, Change{Kind: ChangeUpsert, File: modified})
		case "rename":
			base := SourceFile{operation.OldPath, language, baseContent}
			baseSources = append(baseSources, base)
			renamed := SourceFile{operation.NewPath, language, baseContent}
			finalSources = append(finalSources, renamed)
			changes = append(changes, Change{Kind: ChangeRename, OldPath: operation.OldPath, File: renamed})
		case "delete":
			baseSources = append(baseSources, SourceFile{operation.OldPath, language, baseContent})
			changes = append(changes, Change{Kind: ChangeDelete, OldPath: operation.OldPath})
		default:
			t.Fatalf("unknown frozen operation %q", operation.Kind)
		}
	}

	limits := DefaultLimits()
	base, err := Build(context.Background(), baseSources, limits)
	if err != nil {
		t.Fatalf("build base: %v", err)
	}
	incremental, err := Apply(context.Background(), base, changes, limits)
	if err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	clean, err := Build(context.Background(), finalSources, limits)
	if err != nil {
		t.Fatalf("build final: %v", err)
	}
	if !reflect.DeepEqual(incremental, clean) {
		t.Fatalf("incremental receipt %s != clean receipt %s", incremental.ReceiptDigest, clean.ReceiptDigest)
	}
}

func TestApplyRejectsInvalidBaseAndConflictingChanges(t *testing.T) {
	base, err := Build(context.Background(), []SourceFile{
		{"src/a.py", LanguagePython, []byte("a")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tampered := base
	tampered.ReceiptDigest = "sha256:" + string(make([]byte, 64))
	if _, err := Apply(context.Background(), tampered, nil, DefaultLimits()); err != ErrInvalidSnapshot {
		t.Fatalf("tampered base error = %v", err)
	}
	changes := []Change{
		{Kind: ChangeDelete, OldPath: "src/a.py"},
		{Kind: ChangeUpsert, File: SourceFile{"src/a.py", LanguagePython, []byte("new")}},
	}
	if _, err := Apply(context.Background(), base, changes, DefaultLimits()); err != ErrInvalidChange {
		t.Fatalf("conflicting changes error = %v", err)
	}
}

func TestApplyRejectsDeleteFilePayload(t *testing.T) {
	base, err := Build(context.Background(), []SourceFile{
		{"src/a.py", LanguagePython, []byte("a")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		file SourceFile
	}{
		{"path", SourceFile{Path: "src/ignored.py"}},
		{"language", SourceFile{Language: LanguagePython}},
		{"content", SourceFile{Content: []byte{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changes := []Change{{Kind: ChangeDelete, OldPath: "src/a.py", File: test.file}}
			if _, err := Apply(context.Background(), base, changes, DefaultLimits()); !errors.Is(err, ErrInvalidChange) {
				t.Fatalf("Apply error = %v, want ErrInvalidChange", err)
			}
		})
	}
}

func TestApplyRejectsInvalidUpsertPathAsInvalidChange(t *testing.T) {
	base, err := Build(context.Background(), nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	changes := []Change{{
		Kind: ChangeUpsert,
		File: SourceFile{Path: "../escape.ts", Language: LanguageTypeScript, Content: []byte("value")},
	}}
	_, err = Apply(context.Background(), base, changes, DefaultLimits())
	if !errors.Is(err, ErrInvalidChange) || errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Apply error = %v, want only ErrInvalidChange", err)
	}
}

func TestApplyAcceptsGoDotImportProjection(t *testing.T) {
	base, err := Build(context.Background(), []SourceFile{
		{"src/a.go", LanguageGo, []byte("package a\nimport . \"example.com/dependency\"\n")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	applied, err := Apply(context.Background(), base, nil, DefaultLimits())
	if err != nil {
		t.Fatalf("Apply valid dot import: %v", err)
	}
	if !reflect.DeepEqual(applied, base) {
		t.Fatalf("no-op Apply changed snapshot: %#v", applied)
	}
}

func TestApplyRejectsResignedInvalidProjectionState(t *testing.T) {
	base, err := Build(context.Background(), []SourceFile{
		{"src/a.py", LanguagePython, []byte("from pkg import item\ndef run():\n    return item\n")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"file path", func(snapshot *Snapshot) { snapshot.Files[0].Path = "../a.py" }},
		{"file language", func(snapshot *Snapshot) { snapshot.Files[0].Language = Language("ruby") }},
		{"content digest", func(snapshot *Snapshot) { snapshot.Files[0].ContentDigest = "forged" }},
		{"coverage", func(snapshot *Snapshot) { snapshot.Files[0].Coverage = Coverage("forged") }},
		{"degradation coherence", func(snapshot *Snapshot) {
			snapshot.Files[0].DegradationCode = "malformed_source"
		}},
		{"occurrence language", func(snapshot *Snapshot) {
			snapshot.Files[0].Occurrences[0].Language = LanguageJava
		}},
		{"occurrence kind", func(snapshot *Snapshot) {
			snapshot.Files[0].Occurrences[0].Kind = Kind("forged")
		}},
		{"occurrence text", func(snapshot *Snapshot) { snapshot.Files[0].Occurrences[0].Text = "" }},
		{"occurrence token spelling", func(snapshot *Snapshot) {
			occurrence := &snapshot.Files[0].Occurrences[0]
			occurrence.Text = "!"
			occurrence.Range.End.Column = occurrence.Range.Start.Column + 1
		}},
		{"occurrence path", func(snapshot *Snapshot) {
			snapshot.Files[0].Occurrences[0].Range.Path = "src/other.py"
		}},
		{"occurrence start", func(snapshot *Snapshot) {
			snapshot.Files[0].Occurrences[0].Range.Start.Line = 0
		}},
		{"occurrence order", func(snapshot *Snapshot) {
			snapshot.Files[0].Occurrences[1].Range = snapshot.Files[0].Occurrences[0].Range
		}},
		{"occurrence digest", func(snapshot *Snapshot) {
			snapshot.Files[0].Occurrences[0].ContentDigest = contentDigest([]byte("forged"))
		}},
		{"occurrence coverage", func(snapshot *Snapshot) {
			snapshot.Files[0].Occurrences[0].Coverage = CoverageLexicalDegraded
		}},
		{"unknown degradation", func(snapshot *Snapshot) {
			file := &snapshot.Files[0]
			file.Coverage = CoverageLexicalDegraded
			file.DegradationCode = "forged"
			for index := range file.Occurrences {
				file.Occurrences[index].Coverage = CoverageLexicalDegraded
				file.Occurrences[index].Kind = KindReference
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forged := cloneSnapshotForTest(base)
			test.mutate(&forged)
			resignSnapshotForTest(&forged)
			if _, err := Apply(context.Background(), forged, nil, DefaultLimits()); !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Apply error = %v, want ErrInvalidSnapshot", err)
			}
		})
	}
}

func TestApplyBoundsResignedProjectionState(t *testing.T) {
	base, err := Build(context.Background(), []SourceFile{
		{"src/a.py", LanguagePython, []byte("first second")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		limits Limits
		mutate func(*Snapshot)
	}{
		{
			"occurrence count", limitsWith(func(limits *Limits) { limits.MaxResults = 2 }),
			func(snapshot *Snapshot) {
				snapshot.Files[0].Occurrences = append(
					snapshot.Files[0].Occurrences,
					snapshot.Files[0].Occurrences[1],
				)
			},
		},
		{
			"token lower bound", limitsWith(func(limits *Limits) { limits.MaxTokens = 1 }),
			func(*Snapshot) {},
		},
		{
			"occurrence text bytes", limitsWith(func(limits *Limits) { limits.MaxInputBytes = 8 }),
			func(snapshot *Snapshot) {
				snapshot.Files[0].Occurrences[0].Text = strings.Repeat("x", 9)
			},
		},
		{
			"occurrence coordinates", limitsWith(func(limits *Limits) { limits.MaxColumn = 6 }),
			func(*Snapshot) {},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forged := cloneSnapshotForTest(base)
			test.mutate(&forged)
			resignSnapshotForTest(&forged)
			if _, err := Apply(context.Background(), forged, nil, test.limits); !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Apply error = %v, want ErrInvalidSnapshot", err)
			}
		})
	}
}

func TestApplyRejectsIntermediateCoordinateOverflow(t *testing.T) {
	base, err := Build(context.Background(), []SourceFile{
		{"src/a.ts", LanguageTypeScript, []byte("import \"x\"\n")},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	forged := cloneSnapshotForTest(base)
	occurrence := &forged.Files[0].Occurrences[0]
	occurrence.Text = "\"xxxxxx\n\""
	occurrence.Range.End = Position{Line: 2, Column: 2}
	resignSnapshotForTest(&forged)
	limits := DefaultLimits()
	limits.MaxColumn = 12
	if _, err := Apply(context.Background(), forged, nil, limits); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("Apply error = %v, want ErrInvalidSnapshot", err)
	}
}

func cloneSnapshotForTest(snapshot Snapshot) Snapshot {
	cloned := Snapshot{ReceiptDigest: snapshot.ReceiptDigest, Files: make([]FileProjection, len(snapshot.Files))}
	for index, file := range snapshot.Files {
		cloned.Files[index] = cloneProjection(file)
	}
	return cloned
}

func resignSnapshotForTest(snapshot *Snapshot) {
	for index := range snapshot.Files {
		snapshot.Files[index].ReceiptDigest = fileReceipt(snapshot.Files[index])
	}
	snapshot.ReceiptDigest = snapshotReceipt(snapshot.Files)
}

func frozenContent(t *testing.T, operation frozenOperation, phase string) []byte {
	t.Helper()
	fixture := map[string]string{
		"go": "go/seed.go", "typescript": "typescript/seed.ts", "python": "python/seed.py",
		"rust": "rust/seed.rs.fixture", "java": "java/Seed.java",
	}[operation.Language]
	if operation.Malformed {
		fixture = "malformed/unterminated.ts"
	}
	comment := "//"
	if operation.Language == "python" {
		comment = "#"
	}
	path := operation.OldPath
	if path == "" {
		path = operation.NewPath
	}
	return []byte(fmt.Sprintf("%s\n%s fixture=%s phase=%s\n", readFixture(t, fixture), comment, path, phase))
}

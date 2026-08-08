package codeindex

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type cancelAfterChecksContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *cancelAfterChecksContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestEmptyPresentContentDegradesAndMissingContentRejects(t *testing.T) {
	empty := mustProject(t, SourceFile{"src/empty.py", LanguagePython, []byte{}})
	if empty.Coverage != CoverageLexicalDegraded || len(empty.Occurrences) != 0 {
		t.Fatalf("empty projection = %#v", empty)
	}
	_, err := Project(context.Background(), SourceFile{"src/missing.py", LanguagePython, nil}, DefaultLimits())
	if !errors.Is(err, ErrMissingContent) {
		t.Fatalf("missing content error = %v", err)
	}
}

func TestInvalidPathAndLanguageReject(t *testing.T) {
	for _, path := range []string{"", "/abs.go", "../escape.go", "a/./b.go", "a//b.go", "a\\b.go", "a/"} {
		_, err := Project(context.Background(), SourceFile{path, LanguageGo, []byte("package a")}, DefaultLimits())
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("path %q error = %v", path, err)
		}
	}
	_, err := Project(context.Background(), SourceFile{"src/a.rb", Language("ruby"), []byte("x")}, DefaultLimits())
	if !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("unsupported language error = %v", err)
	}
}

func TestHardAndConfiguredBoundsReject(t *testing.T) {
	tests := []struct {
		name    string
		limits  Limits
		source  SourceFile
		wantErr error
	}{
		{
			"input", limitsWith(func(l *Limits) { l.MaxInputBytes = 3 }),
			SourceFile{"src/a.py", LanguagePython, []byte("four")}, ErrLimitExceeded,
		},
		{
			"tokens", limitsWith(func(l *Limits) { l.MaxTokens = 1 }),
			SourceFile{"src/a.py", LanguagePython, []byte("one two")}, ErrLimitExceeded,
		},
		{
			"results", limitsWith(func(l *Limits) { l.MaxResults = 1 }),
			SourceFile{"src/a.py", LanguagePython, []byte("one two")}, ErrLimitExceeded,
		},
		{
			"lines", limitsWith(func(l *Limits) { l.MaxLines = 1 }),
			SourceFile{"src/a.py", LanguagePython, []byte("one\ntwo")}, ErrLimitExceeded,
		},
		{
			"column", limitsWith(func(l *Limits) { l.MaxColumn = 4 }),
			SourceFile{"src/a.py", LanguagePython, []byte("four")}, ErrLimitExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Project(context.Background(), test.source, test.limits)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}

	invalid := DefaultLimits()
	invalid.MaxFiles = 0
	_, err := Build(context.Background(), nil, invalid)
	if !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("zero limit error = %v", err)
	}

	overHard := DefaultLimits()
	overHard.MaxInputBytes = hardLimits.MaxInputBytes + 1
	_, err = Build(context.Background(), nil, overHard)
	if !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("hard-cap error = %v", err)
	}
}

func TestRangeBoundaryIsOneBasedHalfOpen(t *testing.T) {
	projection := mustProject(t, SourceFile{"src/a.py", LanguagePython, []byte("name")})
	occurrence := findOccurrence(t, projection, KindReference, "name")
	if occurrence.Range.Start != (Position{1, 1}) || occurrence.Range.End != (Position{1, 5}) {
		t.Fatalf("range = %#v", occurrence.Range)
	}
	if projection.ContentDigest != "sha256:82a3537ff0dbce7eec35d69edc3a189ee6f17d82f353a553f9aa96cb0be3ce89" {
		t.Fatalf("content digest = %q", projection.ContentDigest)
	}
	if projection.ReceiptDigest != "sha256:f4f48906b8793c96daed3f3d6a6618183dd18cfab1953dd6a8002cfcb0732b3f" {
		t.Fatalf("receipt digest = %q", projection.ReceiptDigest)
	}
}

func TestConfiguredBoundsAllowExactEquality(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFiles = 1
	limits.MaxInputBytes = 4
	limits.MaxTokens = 1
	limits.MaxResults = 1
	limits.MaxLines = 1
	limits.MaxColumn = 5
	snapshot, err := Build(context.Background(), []SourceFile{
		{"src/a.py", LanguagePython, []byte("name")},
	}, limits)
	if err != nil {
		t.Fatalf("exact limits rejected: %v", err)
	}
	if len(snapshot.Files) != 1 || len(snapshot.Files[0].Occurrences) != 1 {
		t.Fatalf("exact-limit snapshot = %#v", snapshot)
	}
}

func TestBuildRejectsDuplicateAndFileBounds(t *testing.T) {
	source := SourceFile{"src/a.py", LanguagePython, []byte("name")}
	_, err := Build(context.Background(), []SourceFile{source, source}, DefaultLimits())
	if !errors.Is(err, ErrDuplicatePath) {
		t.Fatalf("duplicate error = %v", err)
	}
	limits := DefaultLimits()
	limits.MaxFiles = 1
	_, err = Build(context.Background(), []SourceFile{source, {"src/b.py", LanguagePython, []byte("name")}}, limits)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("file limit error = %v", err)
	}
}

func TestEmptyBuildAndApplyHonorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, nil, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Build error = %v, want context.Canceled", err)
	}
	base, err := Build(context.Background(), nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, base, nil, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
}

func TestLongScannerLoopsHonorCancellation(t *testing.T) {
	tests := []SourceFile{
		{"src/long.ts", LanguageTypeScript, []byte("\"" + strings.Repeat("x", 1024) + "\"")},
		{"src/long.rs", LanguageRust, []byte("'" + strings.Repeat("é", 1024))},
	}
	for _, source := range tests {
		ctx := &cancelAfterChecksContext{Context: context.Background(), cancelAt: 3}
		if _, err := Project(ctx, source, DefaultLimits()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Project(%s) error = %v, want context.Canceled", source.Path, err)
		}
	}
}

func TestGoASTClassificationHonorsCancellation(t *testing.T) {
	ctx := &cancelAfterChecksContext{Context: context.Background(), cancelAt: 3}
	_, err := Project(ctx, SourceFile{
		"src/a.go", LanguageGo, []byte("package a\nfunc run() {}\n"),
	}, DefaultLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Project error = %v, want context.Canceled", err)
	}
}

func limitsWith(change func(*Limits)) Limits {
	limits := DefaultLimits()
	change(&limits)
	return limits
}

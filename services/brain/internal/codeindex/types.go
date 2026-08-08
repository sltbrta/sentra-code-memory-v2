package codeindex

import (
	"errors"
	"fmt"
)

const (
	projectionVersion = "ouroboros.codeindex.p5.v1"
	maxPathBytes      = 4096
)

var (
	// ErrInvalidPath reports a non-canonical or non-relative source path.
	ErrInvalidPath = errors.New("codeindex: invalid repository-relative path")
	// ErrUnsupportedLanguage reports a language outside the frozen P5 set.
	ErrUnsupportedLanguage = errors.New("codeindex: unsupported language")
	// ErrMissingContent reports a nil source-content slice.
	ErrMissingContent = errors.New("codeindex: missing source content")
	// ErrInvalidLimits reports zero, negative, or hard-cap-exceeding limits.
	ErrInvalidLimits = errors.New("codeindex: invalid limits")
	// ErrLimitExceeded reports input, token, coordinate, file, or result overflow.
	ErrLimitExceeded = errors.New("codeindex: bounded limit exceeded")
	// ErrDuplicatePath reports multiple source files with the same path.
	ErrDuplicatePath = errors.New("codeindex: duplicate source path")
	// ErrInvalidChange reports a conflicting or incomplete incremental operation.
	ErrInvalidChange = errors.New("codeindex: invalid incremental change")
	// ErrInvalidSnapshot reports an incremental base not produced by this package.
	ErrInvalidSnapshot = errors.New("codeindex: invalid base snapshot")
)

// Language identifies one member of the frozen P5 language set.
type Language string

const (
	// LanguageGo identifies Go source.
	LanguageGo Language = "go"
	// LanguageTypeScript identifies TypeScript source.
	LanguageTypeScript Language = "typescript"
	// LanguagePython identifies Python source.
	LanguagePython Language = "python"
	// LanguageRust identifies Rust source.
	LanguageRust Language = "rust"
	// LanguageJava identifies Java source.
	LanguageJava Language = "java"
)

// Coverage reports whether syntax classification succeeded or lexical fallback was used.
type Coverage string

const (
	// CoverageSyntaxAware reports successful bounded syntax classification.
	CoverageSyntaxAware Coverage = "COVERAGE_STATE_SYNTAX_AWARE"
	// CoverageLexicalDegraded reports exact identifier tokens without syntax claims.
	CoverageLexicalDegraded Coverage = "COVERAGE_STATE_LEXICAL_DEGRADED"
)

// Kind classifies an indexed occurrence.
type Kind string

const (
	// KindDefinition identifies a bounded syntax-aware declaration.
	KindDefinition Kind = "definition"
	// KindImport identifies a bounded syntax-aware import spelling.
	KindImport Kind = "import"
	// KindReference identifies a source identifier or degraded lexical token.
	KindReference Kind = "reference"
)

// Position is a one-based line and UTF-8 byte column.
type Position struct {
	Line   uint32
	Column uint32
}

// Range is a repository-relative, one-based, half-open forward source range.
type Range struct {
	Path  string
	Start Position
	End   Position
}

// Occurrence is one exact source spelling and its stable classification.
type Occurrence struct {
	Language      Language
	Kind          Kind
	Text          string
	Range         Range
	ContentDigest string
	Coverage      Coverage
}

// SourceFile is one immutable in-memory source input.
//
// Content must be non-nil; an empty but present slice is accepted and degrades
// lexically. Project never retains or mutates Content.
type SourceFile struct {
	Path     string
	Language Language
	Content  []byte
}

// FileProjection is the deterministic projection for one exact source file.
type FileProjection struct {
	Path            string
	Language        Language
	ContentDigest   string
	Coverage        Coverage
	DegradationCode string
	Occurrences     []Occurrence
	ReceiptDigest   string
}

// Snapshot is a path-sorted repository projection with a stable receipt digest.
type Snapshot struct {
	Files         []FileProjection
	ReceiptDigest string
}

// Limits bounds every input and output dimension accepted by this package.
type Limits struct {
	MaxFiles      int
	MaxInputBytes int
	MaxTokens     int
	MaxResults    int
	MaxLines      int
	MaxColumn     int
}

// DefaultLimits returns conservative dependency-free projection limits.
func DefaultLimits() Limits {
	return Limits{
		MaxFiles:      1024,
		MaxInputBytes: 1 << 20,
		MaxTokens:     100_000,
		MaxResults:    50_000,
		MaxLines:      100_000,
		MaxColumn:     16_384,
	}
}

var hardLimits = Limits{
	MaxFiles:      4096,
	MaxInputBytes: 8 << 20,
	MaxTokens:     500_000,
	MaxResults:    250_000,
	MaxLines:      500_000,
	MaxColumn:     1 << 20,
}

// ChangeKind identifies one incremental snapshot operation.
type ChangeKind string

const (
	// ChangeUpsert adds or replaces File.Path.
	ChangeUpsert ChangeKind = "upsert"
	// ChangeDelete removes OldPath.
	ChangeDelete ChangeKind = "delete"
	// ChangeRename removes OldPath and indexes File at its new path.
	ChangeRename ChangeKind = "rename"
)

// Change describes one bounded incremental update.
//
// Upsert uses File only, delete uses OldPath only, and rename uses both. Apply
// rejects duplicate endpoints and changes that reference absent old paths.
type Change struct {
	Kind    ChangeKind
	OldPath string
	File    SourceFile
}

func validateLimits(limits Limits) error {
	values := []struct {
		name string
		got  int
		max  int
	}{
		{"files", limits.MaxFiles, hardLimits.MaxFiles},
		{"input bytes", limits.MaxInputBytes, hardLimits.MaxInputBytes},
		{"tokens", limits.MaxTokens, hardLimits.MaxTokens},
		{"results", limits.MaxResults, hardLimits.MaxResults},
		{"lines", limits.MaxLines, hardLimits.MaxLines},
		{"column", limits.MaxColumn, hardLimits.MaxColumn},
	}
	for _, value := range values {
		if value.got <= 0 || value.got > value.max {
			return fmt.Errorf("%w: %s", ErrInvalidLimits, value.name)
		}
	}
	return nil
}

func validLanguage(language Language) bool {
	switch language {
	case LanguageGo, LanguageTypeScript, LanguagePython, LanguageRust, LanguageJava:
		return true
	default:
		return false
	}
}

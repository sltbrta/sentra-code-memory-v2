package codeindex

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// Project returns a deterministic, side-effect-free projection of one P5 file.
//
// Go completeness and top-level declarations/imports use the go/parser AST;
// other P5 inputs use the bounded token/state scanner. Malformed input returns
// a successful projection with CoverageLexicalDegraded and identifier
// references, never syntax definitions or imports. Returns a sentinel-wrapping
// error for invalid inputs or limits, and returns ctx.Err() on cancellation.
func Project(ctx context.Context, source SourceFile, limits Limits) (FileProjection, error) {
	if err := ctx.Err(); err != nil {
		return FileProjection{}, err
	}
	if err := validateLimits(limits); err != nil {
		return FileProjection{}, err
	}
	if err := validateSource(source, limits); err != nil {
		return FileProjection{}, err
	}

	tokens, structurallyComplete, err := scanSource(ctx, source.Language, source.Content, limits)
	if err != nil {
		return FileProjection{}, err
	}
	degradationCode := ""
	var goFile *ast.File
	var goFileSet *token.FileSet
	if source.Language == LanguageGo && structurallyComplete {
		goFileSet = token.NewFileSet()
		goFile, err = parser.ParseFile(
			goFileSet,
			source.Path,
			source.Content,
			parser.AllErrors|parser.SkipObjectResolution,
		)
		if err != nil {
			structurallyComplete = false
			degradationCode = "go_parse_error"
		}
	}
	if structurallyComplete && !syntaxShapeComplete(source.Language, tokens) {
		structurallyComplete = false
	}

	coverage := CoverageSyntaxAware
	if !structurallyComplete {
		coverage = CoverageLexicalDegraded
		if degradationCode == "" {
			degradationCode = "malformed_source"
		}
	}
	digest := contentDigest(source.Content)
	var classified []classifiedToken
	if source.Language == LanguageGo && coverage == CoverageSyntaxAware {
		classified, err = classifyGoAST(ctx, goFileSet, goFile, tokens)
		if err != nil {
			return FileProjection{}, err
		}
	} else {
		classified = classifyTokens(source.Language, tokens, coverage)
	}
	if len(classified) > limits.MaxResults {
		return FileProjection{}, fmt.Errorf("%w: results", ErrLimitExceeded)
	}
	occurrences := make([]Occurrence, 0, len(classified))
	for index, item := range classified {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return FileProjection{}, err
			}
		}
		occurrences = append(occurrences, Occurrence{
			Language: source.Language,
			Kind:     item.kind,
			Text:     item.token.text,
			Range: Range{
				Path:  source.Path,
				Start: item.token.start,
				End:   item.token.end,
			},
			ContentDigest: digest,
			Coverage:      coverage,
		})
	}
	projection := FileProjection{
		Path:            source.Path,
		Language:        source.Language,
		ContentDigest:   digest,
		Coverage:        coverage,
		DegradationCode: degradationCode,
		Occurrences:     occurrences,
	}
	projection.ReceiptDigest = fileReceipt(projection)
	return projection, nil
}

// Build projects a complete in-memory source set in canonical path order.
//
// Build is side-effect free and sequentially checks cancellation. It rejects
// duplicate paths and applies MaxFiles and MaxResults to the whole snapshot.
func Build(ctx context.Context, sources []SourceFile, limits Limits) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := validateLimits(limits); err != nil {
		return Snapshot{}, err
	}
	if len(sources) > limits.MaxFiles {
		return Snapshot{}, fmt.Errorf("%w: files", ErrLimitExceeded)
	}
	ordered := append([]SourceFile(nil), sources...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Path < ordered[right].Path })
	files := make([]FileProjection, 0, len(ordered))
	totalResults := 0
	for index, source := range ordered {
		if index > 0 && source.Path == ordered[index-1].Path {
			return Snapshot{}, ErrDuplicatePath
		}
		projection, err := Project(ctx, source, limits)
		if err != nil {
			return Snapshot{}, err
		}
		totalResults += len(projection.Occurrences)
		if totalResults > limits.MaxResults {
			return Snapshot{}, fmt.Errorf("%w: snapshot results", ErrLimitExceeded)
		}
		files = append(files, projection)
	}
	return newSnapshot(files), nil
}

// Apply returns a deterministic snapshot after independent incremental changes.
//
// The base must be an unmodified Snapshot returned by Build or Apply. Changes
// may arrive in any order, but their old/new endpoints must not overlap. Apply
// has no side effects and returns ctx.Err() when cancellation is observed.
func Apply(ctx context.Context, base Snapshot, changes []Change, limits Limits) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := validateLimits(limits); err != nil {
		return Snapshot{}, err
	}
	if len(changes) > limits.MaxFiles {
		return Snapshot{}, fmt.Errorf("%w: changes", ErrLimitExceeded)
	}
	files, err := validatedBase(ctx, base, limits)
	if err != nil {
		return Snapshot{}, err
	}
	ordered, err := validatedChanges(changes)
	if err != nil {
		return Snapshot{}, err
	}
	for _, change := range ordered {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		switch change.Kind {
		case ChangeUpsert:
			projection, projectErr := Project(ctx, change.File, limits)
			if projectErr != nil {
				return Snapshot{}, projectErr
			}
			files[change.File.Path] = projection
		case ChangeDelete:
			if _, exists := files[change.OldPath]; !exists {
				return Snapshot{}, ErrInvalidChange
			}
			delete(files, change.OldPath)
		case ChangeRename:
			if _, exists := files[change.OldPath]; !exists {
				return Snapshot{}, ErrInvalidChange
			}
			if _, collision := files[change.File.Path]; collision {
				return Snapshot{}, ErrInvalidChange
			}
			projection, projectErr := Project(ctx, change.File, limits)
			if projectErr != nil {
				return Snapshot{}, projectErr
			}
			delete(files, change.OldPath)
			files[change.File.Path] = projection
		}
	}
	if len(files) > limits.MaxFiles {
		return Snapshot{}, fmt.Errorf("%w: files", ErrLimitExceeded)
	}
	projected := make([]FileProjection, 0, len(files))
	totalResults := 0
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		totalResults += len(file.Occurrences)
		if totalResults > limits.MaxResults {
			return Snapshot{}, fmt.Errorf("%w: snapshot results", ErrLimitExceeded)
		}
		projected = append(projected, cloneProjection(file))
	}
	sort.Slice(projected, func(left, right int) bool { return projected[left].Path < projected[right].Path })
	return newSnapshot(projected), nil
}

func validateSource(source SourceFile, limits Limits) error {
	if !validRepositoryPath(source.Path) {
		return ErrInvalidPath
	}
	if !validLanguage(source.Language) {
		return ErrUnsupportedLanguage
	}
	if source.Content == nil {
		return ErrMissingContent
	}
	if len(source.Content) > limits.MaxInputBytes {
		return fmt.Errorf("%w: input bytes", ErrLimitExceeded)
	}
	return nil
}

func validRepositoryPath(value string) bool {
	if value == "" || len(value) > maxPathBytes || strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func syntaxShapeComplete(language Language, tokens []sourceToken) bool {
	for index, current := range tokens {
		if language != LanguageGo && current.text == "=" && missingAssignmentValue(tokens, index+1) {
			return false
		}
		if current.kind != tokenIdentifier {
			continue
		}
		if requiresFollowingIdentifier(language, current.text) {
			next := nextSignificant(tokens, index+1)
			if next < 0 || !validDefinitionFollower(language, current.text, tokens[next]) {
				return false
			}
		}
		if language == LanguagePython && (current.text == "def" || current.text == "class") {
			end := statementEnd(tokens, index+1, language)
			if !containsToken(tokens[index+1:end], ":") {
				return false
			}
		}
		if isImportStarter(language, current.text) {
			end := statementEnd(tokens, index+1, language)
			if !hasImportCandidate(language, tokens[index+1:end]) {
				return false
			}
		}
	}
	return true
}

func validDefinitionFollower(language Language, starter string, next sourceToken) bool {
	if next.kind == tokenIdentifier && !isKeyword(language, next.text) {
		return true
	}
	return language == LanguageTypeScript && isTypeScriptVariableStarter(starter) &&
		(next.text == "{" || next.text == "[")
}

func missingAssignmentValue(tokens []sourceToken, start int) bool {
	next := nextSignificant(tokens, start)
	if next < 0 {
		return true
	}
	switch tokens[next].text {
	case ";", ",", ")", "]", "}":
		return true
	default:
		return false
	}
}

func requiresFollowingIdentifier(language Language, text string) bool {
	switch language {
	case LanguageTypeScript:
		return text == "function" || text == "class" || text == "interface" || text == "type" ||
			text == "enum" || text == "namespace" || text == "const" || text == "let" || text == "var"
	case LanguagePython:
		return text == "def" || text == "class"
	case LanguageRust:
		return text == "fn" || text == "struct" || text == "enum" || text == "trait" ||
			text == "type" || text == "mod" || text == "const" || text == "static"
	case LanguageJava:
		return text == "class" || text == "interface" || text == "enum" || text == "record"
	default:
		return false
	}
}

func hasImportCandidate(language Language, tokens []sourceToken) bool {
	for _, current := range tokens {
		if current.kind == tokenString ||
			(current.kind == tokenIdentifier && !isKeyword(language, current.text)) {
			return true
		}
	}
	return false
}

func containsToken(tokens []sourceToken, text string) bool {
	for _, current := range tokens {
		if current.text == text {
			return true
		}
	}
	return false
}

func validatedChanges(changes []Change) ([]Change, error) {
	ordered := append([]Change(nil), changes...)
	sort.Slice(ordered, func(left, right int) bool {
		return changeKey(ordered[left]) < changeKey(ordered[right])
	})
	endpoints := make(map[string]bool, len(changes)*2)
	for _, change := range ordered {
		var oldPath, newPath string
		switch change.Kind {
		case ChangeUpsert:
			if change.OldPath != "" || !validRepositoryPath(change.File.Path) {
				return nil, ErrInvalidChange
			}
			newPath = change.File.Path
		case ChangeDelete:
			if !validRepositoryPath(change.OldPath) || change.File.Path != "" ||
				change.File.Language != "" || change.File.Content != nil {
				return nil, ErrInvalidChange
			}
			oldPath = change.OldPath
		case ChangeRename:
			if !validRepositoryPath(change.OldPath) || change.File.Path == "" || change.OldPath == change.File.Path {
				return nil, ErrInvalidChange
			}
			oldPath, newPath = change.OldPath, change.File.Path
		default:
			return nil, ErrInvalidChange
		}
		for _, endpoint := range []string{oldPath, newPath} {
			if endpoint == "" {
				continue
			}
			if endpoints[endpoint] {
				return nil, ErrInvalidChange
			}
			endpoints[endpoint] = true
		}
	}
	return ordered, nil
}

func changeKey(change Change) string {
	return string(change.Kind) + "\x00" + change.OldPath + "\x00" + change.File.Path
}

func cloneProjection(file FileProjection) FileProjection {
	file.Occurrences = append([]Occurrence(nil), file.Occurrences...)
	return file
}

func newSnapshot(files []FileProjection) Snapshot {
	snapshot := Snapshot{Files: files}
	snapshot.ReceiptDigest = snapshotReceipt(files)
	return snapshot
}

package codeindex

// Snapshot validation bounds and checks every mutable projection field before
// Apply trusts a caller-provided base or recomputes its deterministic receipts.

import (
	"context"
	"strings"
	"unicode/utf8"
)

func validatedBase(ctx context.Context, base Snapshot, limits Limits) (map[string]FileProjection, error) {
	if len(base.Files) > limits.MaxFiles || !validDigest(base.ReceiptDigest) {
		return nil, ErrInvalidSnapshot
	}
	files := make(map[string]FileProjection, len(base.Files))
	totalResults := 0
	for index, file := range base.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !validRepositoryPath(file.Path) || !validLanguage(file.Language) ||
			!validDigest(file.ContentDigest) || !validDigest(file.ReceiptDigest) ||
			!validCoverageState(file.Language, file.Coverage, file.DegradationCode) ||
			(index > 0 && base.Files[index-1].Path >= file.Path) ||
			len(file.Occurrences) > limits.MaxResults || len(file.Occurrences) > limits.MaxTokens {
			return nil, ErrInvalidSnapshot
		}
		totalResults += len(file.Occurrences)
		if totalResults > limits.MaxResults {
			return nil, ErrInvalidSnapshot
		}
		if err := validateOccurrences(ctx, file, limits); err != nil {
			return nil, err
		}
		if file.ReceiptDigest != fileReceipt(file) {
			return nil, ErrInvalidSnapshot
		}
		files[file.Path] = cloneProjection(file)
	}
	if base.ReceiptDigest != snapshotReceipt(base.Files) {
		return nil, ErrInvalidSnapshot
	}
	return files, nil
}

func validateOccurrences(ctx context.Context, file FileProjection, limits Limits) error {
	textBytes := 0
	var previousEnd Position
	for index, occurrence := range file.Occurrences {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if len(occurrence.Text) > limits.MaxInputBytes-textBytes {
			return ErrInvalidSnapshot
		}
		if occurrence.Language != file.Language || occurrence.Coverage != file.Coverage ||
			occurrence.ContentDigest != file.ContentDigest || occurrence.Range.Path != file.Path ||
			!validKind(occurrence.Kind) || !validOccurrenceText(file.Language, occurrence) ||
			(file.Coverage == CoverageLexicalDegraded && occurrence.Kind != KindReference) {
			return ErrInvalidSnapshot
		}
		textBytes += len(occurrence.Text)
		if !validPosition(occurrence.Range.Start, limits) ||
			!validPosition(occurrence.Range.End, limits) ||
			positionLess(occurrence.Range.End, occurrence.Range.Start) ||
			occurrence.Range.End == occurrence.Range.Start ||
			(index > 0 && positionLess(occurrence.Range.Start, previousEnd)) {
			return ErrInvalidSnapshot
		}
		computedEnd, err := advancedPosition(ctx, occurrence.Range.Start, occurrence.Text, limits)
		if err != nil {
			return err
		}
		if computedEnd != occurrence.Range.End {
			return ErrInvalidSnapshot
		}
		previousEnd = occurrence.Range.End
	}
	return nil
}

func validCoverageState(language Language, coverage Coverage, degradationCode string) bool {
	switch coverage {
	case CoverageSyntaxAware:
		return degradationCode == ""
	case CoverageLexicalDegraded:
		return degradationCode == "malformed_source" ||
			(language == LanguageGo && degradationCode == "go_parse_error")
	default:
		return false
	}
}

func validKind(kind Kind) bool {
	return kind == KindDefinition || kind == KindImport || kind == KindReference
}

func validOccurrenceText(language Language, occurrence Occurrence) bool {
	if !utf8.ValidString(occurrence.Text) {
		return false
	}
	if validIdentifierText(occurrence.Text) {
		return !isKeyword(language, occurrence.Text)
	}
	return occurrence.Kind == KindImport &&
		(validQuotedText(occurrence.Text) || language == LanguageGo && occurrence.Text == ".")
}

func validIdentifierText(text string) bool {
	if !isIdentifierStart([]byte(text)) {
		return false
	}
	for offset := 0; offset < len(text); {
		if !isIdentifierContinue([]byte(text[offset:])) {
			return false
		}
		_, size := utf8.DecodeRuneInString(text[offset:])
		offset += size
	}
	return true
}

func validQuotedText(text string) bool {
	if len(text) < 2 {
		return false
	}
	quote := text[0]
	return (quote == '\'' || quote == '"' || quote == '`') && text[len(text)-1] == quote
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validPosition(position Position, limits Limits) bool {
	return position.Line > 0 && position.Column > 0 &&
		position.Line <= uint32(limits.MaxLines) && position.Column <= uint32(limits.MaxColumn)
}

func positionLess(left, right Position) bool {
	return left.Line < right.Line || left.Line == right.Line && left.Column < right.Column
}

func advancedPosition(ctx context.Context, start Position, text string, limits Limits) (Position, error) {
	position := start
	for index := range len(text) {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return Position{}, err
			}
		}
		if text[index] == '\n' {
			position.Line++
			position.Column = 1
		} else {
			position.Column++
		}
		if !validPosition(position, limits) {
			return Position{}, ErrInvalidSnapshot
		}
	}
	return position, nil
}

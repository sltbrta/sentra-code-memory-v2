package codeindex

import (
	"context"
	"fmt"
	"unicode"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokenIdentifier tokenKind = iota + 1
	tokenString
	tokenPunctuation
	tokenNewline
)

type sourceToken struct {
	kind  tokenKind
	text  string
	start Position
	end   Position
}

type scanner struct {
	ctx       context.Context
	language  Language
	source    []byte
	limits    Limits
	offset    int
	line      uint32
	column    uint32
	tokens    []sourceToken
	delimiter []byte
	malformed bool
}

const maxDelimiterDepth = 256

func scanSource(
	ctx context.Context,
	language Language,
	source []byte,
	limits Limits,
) ([]sourceToken, bool, error) {
	s := scanner{
		ctx:      ctx,
		language: language,
		source:   source,
		limits:   limits,
		line:     1,
		column:   1,
		tokens:   make([]sourceToken, 0, min(len(source)/4, limits.MaxTokens)),
	}
	for s.offset < len(s.source) {
		if s.offset%256 == 0 {
			if err := s.ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if err := s.scanNext(); err != nil {
			return nil, false, err
		}
	}
	if len(s.delimiter) != 0 {
		s.malformed = true
	}
	return s.tokens, !s.malformed && len(source) != 0, nil
}

func (s *scanner) scanNext() error {
	current := s.source[s.offset]
	if current >= utf8.RuneSelf {
		value, size := utf8.DecodeRune(s.source[s.offset:])
		if value == utf8.RuneError && size == 1 {
			s.malformed = true
		}
	}
	if current == 0 {
		s.malformed = true
		return s.advance(1)
	}
	if current == '\n' {
		start := s.position()
		if err := s.advance(1); err != nil {
			return err
		}
		return s.appendToken(tokenNewline, "\n", start, s.position())
	}
	if isHorizontalWhitespace(current) {
		return s.advance(1)
	}
	if s.startsLineComment() {
		return s.skipLineComment()
	}
	if s.startsBlockComment() {
		return s.skipBlockComment()
	}
	if end, ok := s.rustRawStringEnd(); ok {
		return s.scanRustRawString(end)
	}
	if current == '\'' && s.language == LanguageRust {
		lifetime, err := s.isRustLifetime()
		if err != nil {
			return err
		}
		if lifetime {
			return s.scanPunctuation()
		}
	}
	if current == '\'' || current == '"' || current == '`' {
		return s.scanString(current)
	}
	if isIdentifierStart(s.source[s.offset:]) {
		return s.scanIdentifier()
	}
	return s.scanPunctuation()
}

func (s *scanner) scanIdentifier() error {
	startOffset := s.offset
	start := s.position()
	for s.offset < len(s.source) && isIdentifierContinue(s.source[s.offset:]) {
		_, size := utf8.DecodeRune(s.source[s.offset:])
		if size == 0 {
			break
		}
		if err := s.advance(size); err != nil {
			return err
		}
	}
	return s.appendToken(
		tokenIdentifier,
		string(s.source[startOffset:s.offset]),
		start,
		s.position(),
	)
}

func (s *scanner) scanPunctuation() error {
	startOffset := s.offset
	start := s.position()
	punctuation := s.source[s.offset]
	if err := s.advance(1); err != nil {
		return err
	}
	if err := s.trackDelimiter(punctuation); err != nil {
		return err
	}
	return s.appendToken(
		tokenPunctuation,
		string(s.source[startOffset:s.offset]),
		start,
		s.position(),
	)
}

func (s *scanner) scanString(quote byte) error {
	startOffset := s.offset
	start := s.position()
	width := 1
	if s.language == LanguagePython && s.hasPrefix([]byte{quote, quote, quote}) {
		width = 3
	}
	if err := s.advance(width); err != nil {
		return err
	}
	closed := false
	for s.offset < len(s.source) {
		if s.hasRepeatedByte(quote, width) {
			if err := s.advance(width); err != nil {
				return err
			}
			closed = true
			break
		}
		if s.source[s.offset] == '\\' {
			if err := s.advance(1); err != nil {
				return err
			}
			if s.offset < len(s.source) {
				if err := s.advance(1); err != nil {
					return err
				}
			}
			continue
		}
		if s.source[s.offset] == '\n' && width == 1 && quote != '`' {
			break
		}
		if err := s.advance(1); err != nil {
			return err
		}
	}
	if !closed {
		s.malformed = true
	}
	return s.appendToken(
		tokenString,
		string(s.source[startOffset:s.offset]),
		start,
		s.position(),
	)
}

func (s *scanner) scanRustRawString(end []byte) error {
	startOffset := s.offset
	start := s.position()
	prefixBytes := len(end) + 1
	if err := s.advance(prefixBytes); err != nil {
		return err
	}
	closed := false
	for s.offset < len(s.source) {
		if s.hasPrefix(end) {
			if err := s.advance(len(end)); err != nil {
				return err
			}
			closed = true
			break
		}
		if err := s.advance(1); err != nil {
			return err
		}
	}
	if !closed {
		s.malformed = true
	}
	return s.appendToken(
		tokenString,
		string(s.source[startOffset:s.offset]),
		start,
		s.position(),
	)
}

func (s *scanner) skipLineComment() error {
	for s.offset < len(s.source) && s.source[s.offset] != '\n' {
		if err := s.advance(1); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) skipBlockComment() error {
	depth := 1
	if err := s.advance(2); err != nil {
		return err
	}
	for s.offset < len(s.source) && depth > 0 {
		if s.language == LanguageRust && s.hasPrefix([]byte("/*")) {
			depth++
			if depth > 64 {
				return fmt.Errorf("%w: comment nesting", ErrLimitExceeded)
			}
			if err := s.advance(2); err != nil {
				return err
			}
			continue
		}
		if s.hasPrefix([]byte("*/")) {
			depth--
			if err := s.advance(2); err != nil {
				return err
			}
			continue
		}
		if s.source[s.offset] == '\n' {
			start := s.position()
			if err := s.advance(1); err != nil {
				return err
			}
			if err := s.appendToken(tokenNewline, "\n", start, s.position()); err != nil {
				return err
			}
			continue
		}
		if err := s.advance(1); err != nil {
			return err
		}
	}
	if depth != 0 {
		s.malformed = true
	}
	return nil
}

func (s *scanner) advance(count int) error {
	for range count {
		if s.offset >= len(s.source) {
			return nil
		}
		if s.source[s.offset] == '\n' {
			s.line++
			s.column = 1
		} else {
			s.column++
		}
		s.offset++
		if s.offset%256 == 0 {
			if err := s.ctx.Err(); err != nil {
				return err
			}
		}
		if int(s.line) > s.limits.MaxLines || int(s.column) > s.limits.MaxColumn {
			return fmt.Errorf("%w: source coordinates", ErrLimitExceeded)
		}
	}
	return nil
}

func (s *scanner) appendToken(kind tokenKind, text string, start, end Position) error {
	if len(s.tokens) >= s.limits.MaxTokens {
		return fmt.Errorf("%w: tokens", ErrLimitExceeded)
	}
	s.tokens = append(s.tokens, sourceToken{kind: kind, text: text, start: start, end: end})
	return nil
}

func (s *scanner) position() Position {
	return Position{Line: s.line, Column: s.column}
}

func (s *scanner) trackDelimiter(punctuation byte) error {
	switch punctuation {
	case '(', '[', '{':
		if len(s.delimiter) >= maxDelimiterDepth {
			return fmt.Errorf("%w: delimiter nesting", ErrLimitExceeded)
		}
		s.delimiter = append(s.delimiter, punctuation)
	case ')', ']', '}':
		if len(s.delimiter) == 0 || !matchingDelimiter(s.delimiter[len(s.delimiter)-1], punctuation) {
			s.malformed = true
			return nil
		}
		s.delimiter = s.delimiter[:len(s.delimiter)-1]
	}
	return nil
}

func (s *scanner) startsLineComment() bool {
	if s.language == LanguagePython {
		return s.source[s.offset] == '#'
	}
	return s.hasPrefix([]byte("//"))
}

func (s *scanner) startsBlockComment() bool {
	return s.language != LanguagePython && s.hasPrefix([]byte("/*"))
}

func (s *scanner) rustRawStringEnd() ([]byte, bool) {
	if s.language != LanguageRust || s.source[s.offset] != 'r' {
		return nil, false
	}
	cursor := s.offset + 1
	for cursor < len(s.source) && s.source[cursor] == '#' && cursor-s.offset <= 17 {
		cursor++
	}
	if cursor >= len(s.source) || s.source[cursor] != '"' {
		return nil, false
	}
	hashes := cursor - s.offset - 1
	end := make([]byte, hashes+1)
	end[0] = '"'
	for index := 1; index < len(end); index++ {
		end[index] = '#'
	}
	return end, true
}

func (s *scanner) isRustLifetime() (bool, error) {
	if s.offset+1 >= len(s.source) || !isIdentifierStart(s.source[s.offset+1:]) {
		return false, nil
	}
	cursor := s.offset + 1
	iterations := 0
	for cursor < len(s.source) && isIdentifierContinue(s.source[cursor:]) {
		_, size := utf8.DecodeRune(s.source[cursor:])
		cursor += size
		iterations++
		if iterations%256 == 0 {
			if err := s.ctx.Err(); err != nil {
				return false, err
			}
		}
	}
	return cursor >= len(s.source) || s.source[cursor] != '\'', nil
}

func (s *scanner) hasPrefix(prefix []byte) bool {
	if len(s.source)-s.offset < len(prefix) {
		return false
	}
	for index, value := range prefix {
		if s.source[s.offset+index] != value {
			return false
		}
	}
	return true
}

func (s *scanner) hasRepeatedByte(value byte, count int) bool {
	if len(s.source)-s.offset < count {
		return false
	}
	for index := range count {
		if s.source[s.offset+index] != value {
			return false
		}
	}
	return true
}

func matchingDelimiter(open, close byte) bool {
	return open == '(' && close == ')' || open == '[' && close == ']' || open == '{' && close == '}'
}

func isHorizontalWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\f'
}

func isIdentifierStart(source []byte) bool {
	if len(source) == 0 {
		return false
	}
	value, _ := utf8.DecodeRune(source)
	return value == '_' || value == '$' || unicode.IsLetter(value)
}

func isIdentifierContinue(source []byte) bool {
	if len(source) == 0 {
		return false
	}
	value, _ := utf8.DecodeRune(source)
	return value == '_' || value == '$' || unicode.IsLetter(value) || unicode.IsDigit(value)
}

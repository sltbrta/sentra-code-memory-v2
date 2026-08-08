package query

import (
	"context"
	"fmt"
	"strings"
)

// DeterministicConfidencePerMille is the fixed disclosed confidence of the
// deterministic template lane. It is a declared constant, not a calibrated
// model score: the lane restates cited bytes verbatim and never claims
// semantic understanding.
const DeterministicConfidencePerMille = 900

// DeterministicSynthesizer is the fixture and conformance synthesis adapter:
// a pure function of its request, byte-for-byte reproducible, with no clock,
// randomness, network, or provider. It exists to prove pipeline,
// authorization, citation, and replay behavior; it cannot establish answer
// quality and makes no semantic claim.
//
// Per evidence entry it selects the value line — the first block line holding
// the standalone keyword `return`, else the block's first line — cites that
// line's trimmed span, and renders the first double-quoted literal as the
// returned value. A claim whose statement would exceed the configured bound
// is skipped; an over-bound aggregate prose fails closed.
type DeterministicSynthesizer struct{}

// NewDeterministicSynthesizer returns the stateless deterministic adapter.
func NewDeterministicSynthesizer() DeterministicSynthesizer {
	return DeterministicSynthesizer{}
}

// Synthesize renders claims deterministically from the verified pack.
func (DeterministicSynthesizer) Synthesize(ctx context.Context, request SynthesisRequest) (Synthesis, error) {
	if err := ctx.Err(); err != nil {
		return Synthesis{}, err
	}
	if len(request.Evidence) == 0 {
		return Synthesis{}, nil
	}
	synthesis := Synthesis{}
	evidenceBytes := 0
	for index, entry := range request.Evidence {
		if len(synthesis.Claims) >= request.Limits.MaxClaims {
			break
		}
		evidenceBytes += entryBytes(entry)
		line, selected := selectValueLine(entry)
		statement := renderStatement(entry, selected)
		if len(statement) > request.Limits.MaxStatementBytes {
			continue
		}
		synthesis.Claims = append(synthesis.Claims, ProposedClaim{
			Statement: statement,
			Citations: []ProposedCitation{{
				EvidenceIndex: index,
				StartLine:     line,
				StartColumn:   uint32(len(selected) - len(strings.TrimLeft(selected, " \t")) + 1),
				EndLine:       line,
				EndColumn:     uint32(len(selected) + 1),
			}},
			ConfidencePerMille: DeterministicConfidencePerMille,
		})
	}
	statements := make([]string, 0, len(synthesis.Claims))
	for _, claim := range synthesis.Claims {
		statements = append(statements, claim.Statement)
	}
	synthesis.Prose = strings.Join(statements, " ")
	if len(synthesis.Prose) > request.Limits.MaxProseBytes {
		return Synthesis{}, fmt.Errorf("%w: deterministic prose exceeds the prose bound", ErrSynthesisFailed)
	}
	synthesis.TokenUsage = uint64(len(request.Query)+evidenceBytes+len(synthesis.Prose)) / 4
	return synthesis, nil
}

// selectValueLine returns the one-based line number and raw content of the
// first block line holding the standalone keyword `return`, else the block's
// first line.
func selectValueLine(entry EvidenceEntry) (uint32, string) {
	for index, line := range entry.Lines {
		if containsKeyword(line, "return") {
			return entry.BlockStartLine + uint32(index), line
		}
	}
	return entry.BlockStartLine, entry.Lines[0]
}

// containsKeyword reports whether text holds the keyword as a standalone
// word, bounded by non-identifier characters.
func containsKeyword(text, keyword string) bool {
	for offset := 0; ; {
		index := strings.Index(text[offset:], keyword)
		if index < 0 {
			return false
		}
		index += offset
		before := index == 0 || !isIdentifierByte(text[index-1])
		after := index+len(keyword) == len(text) || !isIdentifierByte(text[index+len(keyword)])
		if before && after {
			return true
		}
		offset = index + len(keyword)
	}
}

func isIdentifierByte(value byte) bool {
	return value == '_' || (value >= '0' && value <= '9') ||
		(value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

// renderStatement renders the deterministic claim statement: the first
// double-quoted literal as the returned value, else the trimmed line.
func renderStatement(entry EvidenceEntry, line string) string {
	trimmed := strings.TrimSpace(line)
	if start := strings.Index(trimmed, "\""); start >= 0 {
		if end := strings.Index(trimmed[start+1:], "\""); end >= 0 {
			return fmt.Sprintf("%s returns %s.", entry.Path, trimmed[start:start+end+2])
		}
	}
	return fmt.Sprintf("%s: %s.", entry.Path, trimmed)
}

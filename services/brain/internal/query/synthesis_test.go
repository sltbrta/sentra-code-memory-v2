package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDeterministicSynthesizerSelectsValueLine pins the deterministic claim
// template: the first block line holding the standalone keyword `return`, the
// trimmed line span, and the first string literal as the returned value.
func TestDeterministicSynthesizerSelectsValueLine(t *testing.T) {
	synthesizer := NewDeterministicSynthesizer()
	request := SynthesisRequest{
		Query: "What does anchor() return in src/python/modify-00.py?",
		Evidence: []EvidenceEntry{{
			Path:           "src/python/modify-00.py",
			Language:       "python",
			RevisionID:     strings.Repeat("a", 64),
			BlobOID:        strings.Repeat("b", 40),
			ContentDigest:  strings.Repeat("c", 64),
			BlockStartLine: 4,
			Lines: []string{
				"def anchor() -> str:",
				"    \"\"\"Return the cross-language fixture marker.\"\"\"",
				"    return \"ouroboros-stage-03\"",
			},
			DefinitionText: "anchor",
		}},
		Limits: DefaultLimits().Synthesis,
	}
	synthesis, err := synthesizer.Synthesize(context.Background(), request)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(synthesis.Claims) != 1 || len(synthesis.Claims[0].Citations) != 1 {
		t.Fatalf("synthesis = %#v", synthesis)
	}
	citation := synthesis.Claims[0].Citations[0]
	want := ProposedCitation{EvidenceIndex: 0, StartLine: 6, StartColumn: 5, EndLine: 6, EndColumn: 32}
	if citation != want {
		t.Fatalf("citation = %#v, want %#v", citation, want)
	}
	if synthesis.Claims[0].Statement != `src/python/modify-00.py returns "ouroboros-stage-03".` {
		t.Fatalf("statement = %q", synthesis.Claims[0].Statement)
	}
	if synthesis.Claims[0].ConfidencePerMille != DeterministicConfidencePerMille {
		t.Fatalf("confidence = %d", synthesis.Claims[0].ConfidencePerMille)
	}
	if synthesis.Prose == "" || synthesis.TokenUsage == 0 {
		t.Fatalf("prose and token usage must be populated: %#v", synthesis)
	}
}

// TestDeterministicSynthesizerFallsBackToDefinitionLine proves a block without
// a return keyword cites its first line.
func TestDeterministicSynthesizerFallsBackToDefinitionLine(t *testing.T) {
	synthesizer := NewDeterministicSynthesizer()
	request := SynthesisRequest{
		Query: "What is the exported anchor constant in src/typescript/add-00.ts?",
		Evidence: []EvidenceEntry{{
			Path:           "src/typescript/add-00.ts",
			Language:       "typescript",
			RevisionID:     strings.Repeat("a", 64),
			BlobOID:        strings.Repeat("b", 40),
			ContentDigest:  strings.Repeat("c", 64),
			BlockStartLine: 1,
			Lines:          []string{`export const anchor = (): string => "ouroboros-stage-03";`},
			DefinitionText: "anchor",
		}},
		Limits: DefaultLimits().Synthesis,
	}
	synthesis, err := synthesizer.Synthesize(context.Background(), request)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	citation := synthesis.Claims[0].Citations[0]
	want := ProposedCitation{EvidenceIndex: 0, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 58}
	if citation != want {
		t.Fatalf("citation = %#v, want %#v", citation, want)
	}
}

// TestDeterministicSynthesizerIsByteReproducible proves identical requests
// produce deeply identical output across independent runs.
func TestDeterministicSynthesizerIsByteReproducible(t *testing.T) {
	request := SynthesisRequest{
		Query: "q",
		Evidence: []EvidenceEntry{{
			Path: "a.go", Language: "go", RevisionID: strings.Repeat("a", 64),
			BlobOID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64),
			BlockStartLine: 1, Lines: []string{"package a", "", `func Anchor() string { return "x" }`},
			DefinitionText: "Anchor",
		}},
		Limits: DefaultLimits().Synthesis,
	}
	first, err := NewDeterministicSynthesizer().Synthesize(context.Background(), request)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	second, err := NewDeterministicSynthesizer().Synthesize(context.Background(), request)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if fmt.Sprintf("%#v", first) != fmt.Sprintf("%#v", second) {
		t.Fatalf("non-deterministic synthesis:\n%#v\n%#v", first, second)
	}
}

// TestDeterministicSynthesizerHandlesEmptyEvidence proves an empty pack is a
// valid silent result, never an error.
func TestDeterministicSynthesizerHandlesEmptyEvidence(t *testing.T) {
	synthesis, err := NewDeterministicSynthesizer().Synthesize(context.Background(), SynthesisRequest{
		Query: "q", Limits: DefaultLimits().Synthesis,
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if synthesis.Prose != "" || len(synthesis.Claims) != 0 {
		t.Fatalf("empty evidence must synthesize silence: %#v", synthesis)
	}
}

// TestProviderSynthesizerFailClosed proves every provider failure mode —
// transport error, timeout, oversized output — becomes one typed synthesis
// failure with no partial prose leak.
func TestProviderSynthesizerFailClosed(t *testing.T) {
	request := SynthesisRequest{Query: "q", Limits: DefaultLimits().Synthesis}
	for name, client := range map[string]ProviderClient{
		"transport error": stubProviderClient{err: errors.New("connection refused")},
		"slow provider":   stubProviderClient{latency: 50 * time.Millisecond},
		"oversized prose": stubProviderClient{response: &ProviderResponse{
			Prose: strings.Repeat("p", 16385),
		}},
		"oversized claim set": stubProviderClient{response: &ProviderResponse{
			Prose:  "ok",
			Claims: make([]ProposedClaim, 65),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			synthesizer, err := NewProviderSynthesizer(ProviderConfig{
				ProviderID: "provider:test", Model: "model:test",
				Client: client, Timeout: 10 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("NewProviderSynthesizer: %v", err)
			}
			synthesis, err := synthesizer.Synthesize(context.Background(), request)
			if !errors.Is(err, ErrSynthesisFailed) {
				t.Fatalf("Synthesize error = %v, want ErrSynthesisFailed", err)
			}
			if synthesis.Prose != "" || len(synthesis.Claims) != 0 {
				t.Fatalf("failed synthesis must leak no partial output: %#v", synthesis)
			}
		})
	}
}

// TestProviderSynthesizerPassesThrough verifies a healthy provider's bounded
// response flows to the engine unchanged.
func TestProviderSynthesizerPassesThrough(t *testing.T) {
	response := &ProviderResponse{
		Prose: "A grounded claim.",
		Claims: []ProposedClaim{{
			Statement: "Claim.", ConfidencePerMille: 800,
			Citations: []ProposedCitation{{EvidenceIndex: 0, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 5}},
		}},
		TokenUsage: 42,
	}
	synthesizer, err := NewProviderSynthesizer(ProviderConfig{
		ProviderID: "provider:test", Model: "model:test",
		Client: stubProviderClient{response: response}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewProviderSynthesizer: %v", err)
	}
	synthesis, err := synthesizer.Synthesize(context.Background(), SynthesisRequest{
		Query: "q", Limits: DefaultLimits().Synthesis,
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if synthesis.Prose != response.Prose || synthesis.TokenUsage != 42 || len(synthesis.Claims) != 1 {
		t.Fatalf("synthesis = %#v", synthesis)
	}
}

// TestProviderSynthesizerValidatesConfig proves misconfigured adapters fail at
// construction, never at request time.
func TestProviderSynthesizerValidatesConfig(t *testing.T) {
	for name, config := range map[string]ProviderConfig{
		"missing client":   {ProviderID: "p", Model: "m", Timeout: time.Second},
		"missing provider": {Model: "m", Client: stubProviderClient{}, Timeout: time.Second},
		"missing model":    {ProviderID: "p", Client: stubProviderClient{}, Timeout: time.Second},
		"missing timeout":  {ProviderID: "p", Model: "m", Client: stubProviderClient{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProviderSynthesizer(config); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("NewProviderSynthesizer error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// stubProviderClient scripts one provider interaction.
type stubProviderClient struct {
	response *ProviderResponse
	err      error
	latency  time.Duration
	seen     *ProviderRequest
}

func (s stubProviderClient) Complete(ctx context.Context, request ProviderRequest) (*ProviderResponse, error) {
	if s.latency > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.latency):
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.response, nil
}

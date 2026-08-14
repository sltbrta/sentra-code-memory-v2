package llmadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/genai"
)

// generateAPI is the subset of the official Google Gemini Go SDK
// (google.golang.org/genai) Models service this adapter uses. *genai.Models
// satisfies it; tests inject a fake so no live key or network is required.
type generateAPI interface {
	GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

// GeminiGenerator implements Generator through the official Gemini SDK. It
// never configures tools or function calling: the model returns structured
// JSON proposals only and cannot write files, execute ChangeSets, or bypass
// local validation.
type GeminiGenerator struct {
	models generateAPI
	cfg    Config
}

// NewGeminiGenerator builds a Generator from the official SDK using
// cfg.APIKey (GEMINI_API_KEY). It returns ErrNoAPIKey when no key is
// configured; callers treat that as "stay deterministic", not as a failure.
func NewGeminiGenerator(ctx context.Context, cfg Config) (*GeminiGenerator, error) {
	if cfg.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("llmadapter: gemini client: %w", err)
	}
	return &GeminiGenerator{models: client.Models, cfg: cfg}, nil
}

// newGeminiGeneratorWithModels injects a models implementation for tests.
func newGeminiGeneratorWithModels(models generateAPI, cfg Config) *GeminiGenerator {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	return &GeminiGenerator{models: models, cfg: cfg}
}

// Describe reports the provider and configured model for diagnostics.
func (g *GeminiGenerator) Describe() (provider, model string) {
	return "gemini", g.cfg.Model
}

// maxResponseBytes bounds accepted model output independent of token limits.
const maxResponseBytes = 1 << 16

// GenerateJSON performs one bounded structured generation with a strict
// response schema, deterministic temperature, and no tools.
func (g *GeminiGenerator) GenerateJSON(ctx context.Context, op Operation, system, prompt string, maxOutputTokens int32) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	schema, err := responseSchema(op)
	if err != nil {
		return nil, err
	}
	zero := float32(0)
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(system, genai.RoleUser),
		Temperature:       &zero,
		MaxOutputTokens:   maxOutputTokens,
		ResponseMIMEType:  "application/json",
		ResponseSchema:    schema,
		// Tools intentionally unset: no function calling, no code execution,
		// no retrieval tools. The model proposes data; local code disposes.
	}
	resp, err := g.models.GenerateContent(ctx, g.cfg.Model,
		[]*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}, cfg)
	if err != nil {
		return nil, fmt.Errorf("llmadapter: gemini generate: %w", err)
	}
	if resp == nil {
		return nil, errors.New("llmadapter: gemini returned no response")
	}
	if calls := resp.FunctionCalls(); len(calls) > 0 {
		// Defensive: the adapter requests no tools, so any function call in
		// the response is a contract violation and falls back deterministically.
		return nil, errors.New("llmadapter: unexpected function call in response")
	}
	text := resp.Text()
	if text == "" {
		return nil, errors.New("llmadapter: gemini returned empty text")
	}
	if len(text) > maxResponseBytes {
		return nil, errors.New("llmadapter: gemini response exceeds byte bound")
	}
	return json.RawMessage(text), nil
}

// responseSchema returns the strict structured-output schema for op.
func responseSchema(op Operation) (*genai.Schema, error) {
	str := func(desc string) *genai.Schema { return &genai.Schema{Type: genai.TypeString, Description: desc} }
	switch op {
	case OpExpandQuery:
		return &genai.Schema{
			Type:        genai.TypeObject,
			Description: "Query expansions.",
			Properties: map[string]*genai.Schema{
				"queries": {
					Type:        genai.TypeArray,
					Description: "Reformulations of the original query.",
					Items:       str("One reformulated query."),
				},
			},
			Required: []string{"queries"},
		}, nil
	case OpScoreCandidates:
		return &genai.Schema{
			Type:        genai.TypeObject,
			Description: "Relevance scores for the provided candidates.",
			Properties: map[string]*genai.Schema{
				"scores": {
					Type: genai.TypeArray,
					Items: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"id":    str("Candidate ID, copied verbatim from the input."),
							"score": {Type: genai.TypeNumber, Description: "Relevance in [0,1].", Minimum: f64(0), Maximum: f64(1)},
						},
						Required: []string{"id", "score"},
					},
				},
			},
			Required: []string{"scores"},
		}, nil
	case OpExtractClaims:
		return &genai.Schema{
			Type:        genai.TypeObject,
			Description: "Extracted subject-predicate-object claim triples.",
			Properties: map[string]*genai.Schema{
				"claims": {
					Type: genai.TypeArray,
					Items: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"subject":   str("Claim subject."),
							"predicate": str("Claim predicate."),
							"object":    str("Claim object."),
							"span":      str("Evidence span from the document."),
						},
						Required: []string{"subject", "predicate", "object"},
					},
				},
			},
			Required: []string{"claims"},
		}, nil
	default:
		return nil, fmt.Errorf("llmadapter: unknown operation %q", string(op))
	}
}

func f64(v float64) *float64 { return &v }

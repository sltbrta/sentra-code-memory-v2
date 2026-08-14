package llmadapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// fakeModels injects an SDK-level fake: no live key, no network.
type fakeModels struct {
	resp      *genai.GenerateContentResponse
	err       error
	gotModel  string
	gotConfig *genai.GenerateContentConfig
	gotPrompt string
}

func (f *fakeModels) GenerateContent(_ context.Context, model string, contents []*genai.Content, cfg *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	f.gotModel = model
	f.gotConfig = cfg
	if len(contents) > 0 {
		f.gotPrompt = contents[0].Parts[0].Text
	}
	return f.resp, f.err
}

func textResponse(text string) *genai.GenerateContentResponse {
	return &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{Content: genai.NewContentFromText(text, genai.RoleModel)},
		},
	}
}

func TestNewGeminiGeneratorRequiresKey(t *testing.T) {
	if _, err := NewGeminiGenerator(context.Background(), DefaultConfig()); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("want ErrNoAPIKey, got %v", err)
	}
}

func TestNewGeminiGeneratorConstructsWithKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "test-only-key"
	g, err := NewGeminiGenerator(context.Background(), cfg)
	if err != nil {
		t.Fatalf("construction must not require network: %v", err)
	}
	provider, model := g.Describe()
	if provider != "gemini" || model != DefaultModel {
		t.Fatalf("got %q %q", provider, model)
	}
}

func TestGeminiGenerateJSONContract(t *testing.T) {
	fm := &fakeModels{resp: textResponse(`{"queries":["a","b"]}`)}
	cfg := DefaultConfig()
	cfg.Model = "gemini-test-model"
	g := newGeminiGeneratorWithModels(fm, cfg)

	raw, err := g.GenerateJSON(context.Background(), OpExpandQuery, "sys", "prompt text", 256)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"queries":["a","b"]}` {
		t.Fatalf("got %s", raw)
	}
	if fm.gotModel != "gemini-test-model" {
		t.Fatalf("model not propagated: %q", fm.gotModel)
	}
	if fm.gotPrompt != "prompt text" {
		t.Fatalf("prompt not propagated: %q", fm.gotPrompt)
	}
	c := fm.gotConfig
	if c == nil {
		t.Fatal("no config sent")
	}
	if c.ResponseMIMEType != "application/json" {
		t.Fatalf("structured output not requested: %q", c.ResponseMIMEType)
	}
	if c.ResponseSchema == nil || c.ResponseSchema.Type != genai.TypeObject {
		t.Fatalf("strict schema missing: %+v", c.ResponseSchema)
	}
	if c.MaxOutputTokens != 256 {
		t.Fatalf("output not bounded: %d", c.MaxOutputTokens)
	}
	if c.Temperature == nil || *c.Temperature != 0 {
		t.Fatalf("temperature must be pinned to 0: %+v", c.Temperature)
	}
	if c.SystemInstruction == nil || c.SystemInstruction.Parts[0].Text != "sys" {
		t.Fatalf("system instruction lost: %+v", c.SystemInstruction)
	}
	if len(c.Tools) != 0 {
		t.Fatalf("no tools may be configured, got %+v", c.Tools)
	}
}

func TestGeminiSchemasPerOperation(t *testing.T) {
	for _, op := range []Operation{OpExpandQuery, OpScoreCandidates, OpExtractClaims} {
		fm := &fakeModels{resp: textResponse(`{}`)}
		g := newGeminiGeneratorWithModels(fm, DefaultConfig())
		if _, err := g.GenerateJSON(context.Background(), op, "s", "p", 64); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if fm.gotConfig.ResponseSchema == nil {
			t.Fatalf("%s: schema missing", op)
		}
	}
	fm := &fakeModels{}
	g := newGeminiGeneratorWithModels(fm, DefaultConfig())
	if _, err := g.GenerateJSON(context.Background(), Operation("bogus"), "s", "p", 64); err == nil {
		t.Fatal("unknown operation must fail before any provider call")
	}
}

func TestGeminiErrorPropagatesForFallback(t *testing.T) {
	fm := &fakeModels{err: errors.New("transport down")}
	g := newGeminiGeneratorWithModels(fm, DefaultConfig())
	if _, err := g.GenerateJSON(context.Background(), OpExpandQuery, "s", "p", 64); err == nil {
		t.Fatal("provider error must propagate so Service falls back")
	}
}

func TestGeminiRejectsFunctionCalls(t *testing.T) {
	resp := textResponse(`{"queries":["a"]}`)
	resp.Candidates[0].Content.Parts = append(resp.Candidates[0].Content.Parts,
		&genai.Part{FunctionCall: &genai.FunctionCall{Name: "write_file"}})
	fm := &fakeModels{resp: resp}
	g := newGeminiGeneratorWithModels(fm, DefaultConfig())
	if _, err := g.GenerateJSON(context.Background(), OpExpandQuery, "s", "p", 64); err == nil ||
		!strings.Contains(err.Error(), "function call") {
		t.Fatalf("function calls must be rejected, got %v", err)
	}
}

func TestGeminiRejectsOversizedResponse(t *testing.T) {
	fm := &fakeModels{resp: textResponse(strings.Repeat("x", maxResponseBytes+1))}
	g := newGeminiGeneratorWithModels(fm, DefaultConfig())
	if _, err := g.GenerateJSON(context.Background(), OpExpandQuery, "s", "p", 64); err == nil {
		t.Fatal("oversized response must be rejected")
	}
}

func TestGeminiRejectsEmptyAndNilResponses(t *testing.T) {
	g := newGeminiGeneratorWithModels(&fakeModels{resp: nil}, DefaultConfig())
	if _, err := g.GenerateJSON(context.Background(), OpExpandQuery, "s", "p", 64); err == nil {
		t.Fatal("nil response must be rejected")
	}
	g = newGeminiGeneratorWithModels(&fakeModels{resp: textResponse("")}, DefaultConfig())
	if _, err := g.GenerateJSON(context.Background(), OpExpandQuery, "s", "p", 64); err == nil {
		t.Fatal("empty text must be rejected")
	}
}

// TestGeminiEndToEndThroughService wires the SDK-level fake through Service to
// prove the full path: schema-valid structured output is used, and SDK-level
// failure degrades to the deterministic fallback with safe diagnostics.
func TestGeminiEndToEndThroughService(t *testing.T) {
	cfg := DefaultConfig()
	cfg.APIKey = "test-only-key"

	ok := newGeminiGeneratorWithModels(&fakeModels{resp: textResponse(`{"queries":["semantic scoring"]}`)}, cfg)
	svc := New(cfg, ok)
	got, diag := svc.ExpandQuery(context.Background(), "query expansion")
	if !diag.LLMUsed || diag.Provider != "gemini" || diag.Model != DefaultModel {
		t.Fatalf("got %+v", diag)
	}
	if len(got) != 2 || got[1] != "semantic scoring" {
		t.Fatalf("got %v", got)
	}

	bad := newGeminiGeneratorWithModels(&fakeModels{err: errors.New("sdk failure")}, cfg)
	svc = New(cfg, bad)
	got, diag = svc.ExpandQuery(context.Background(), "query expansion")
	if diag.LLMUsed || diag.FallbackReason != "provider_error" {
		t.Fatalf("got %+v", diag)
	}
	if len(got) == 0 || got[0] != "query expansion" {
		t.Fatalf("deterministic fallback expected, got %v", got)
	}
}

package llmadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeGenerator is an injected provider used in place of any live SDK client.
type fakeGenerator struct {
	raw        json.RawMessage
	err        error
	calls      int
	lastOp     Operation
	lastSystem string
	lastPrompt string
	lastMaxOut int32
}

func (f *fakeGenerator) GenerateJSON(_ context.Context, op Operation, system, prompt string, maxOut int32) (json.RawMessage, error) {
	f.calls++
	f.lastOp = op
	f.lastSystem = system
	f.lastPrompt = prompt
	f.lastMaxOut = maxOut
	return f.raw, f.err
}

func (f *fakeGenerator) Describe() (string, string) { return "fake", "fake-model-0" }

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.MaxExpansions = 4
	cfg.MaxClaims = 3
	cfg.MaxCandidates = 2
	return cfg
}

func TestExpandQueryDeterministicWhenNoGenerator(t *testing.T) {
	svc := New(testConfig(), nil)
	got, diag := svc.ExpandQuery(context.Background(), "How does Ranked Retrieval work?")
	if diag.LLMUsed {
		t.Fatalf("expected deterministic mode, got %+v", diag)
	}
	if diag.FallbackReason != "llm_not_configured" {
		t.Fatalf("unexpected reason %q", diag.FallbackReason)
	}
	if len(got) == 0 || got[0] != "How does Ranked Retrieval work?" {
		t.Fatalf("original query must lead, got %v", got)
	}
	for _, q := range got[1:] {
		if strings.Contains(q, " ") {
			t.Fatalf("fallback expansions should be tokens, got %q", q)
		}
	}
}

func TestExpandQueryLLM(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`{"queries":["ranked code retrieval","  ","ranked code retrieval","MMR reranking"]}`)}
	svc := New(testConfig(), gen)
	got, diag := svc.ExpandQuery(context.Background(), "ranked retrieval")
	if !diag.LLMUsed || diag.Provider != "fake" || diag.Model != "fake-model-0" {
		t.Fatalf("expected LLM diagnostics, got %+v", diag)
	}
	if gen.lastOp != OpExpandQuery || gen.lastMaxOut <= 0 {
		t.Fatalf("generator saw op=%q maxOut=%d", gen.lastOp, gen.lastMaxOut)
	}
	want := []string{"ranked retrieval", "ranked code retrieval", "MMR reranking"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestExpandQueryInvalidResponseFallsBack(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`{"unexpected":true}`)}
	svc := New(testConfig(), gen)
	got, diag := svc.ExpandQuery(context.Background(), "session recall abstain")
	if diag.LLMUsed || diag.FallbackReason != "invalid_response" {
		t.Fatalf("got %+v", diag)
	}
	if len(got) == 0 || got[0] != "session recall abstain" {
		t.Fatalf("deterministic fallback expected, got %v", got)
	}
}

func TestProviderErrorFallsBack(t *testing.T) {
	gen := &fakeGenerator{err: errors.New("boom: API key sk-123 should not leak")}
	svc := New(testConfig(), gen)
	_, diag := svc.ExpandQuery(context.Background(), "query expansion fallback")
	if diag.LLMUsed || diag.FallbackReason != "provider_error" {
		t.Fatalf("got %+v", diag)
	}
	// Diagnostics must never carry provider error content.
	b, _ := json.Marshal(diag)
	if strings.Contains(string(b), "sk-123") || strings.Contains(string(b), "boom") {
		t.Fatalf("diagnostics leaked provider detail: %s", b)
	}
}

func TestContextDoneSkipsProvider(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`{"queries":["x"]}`)}
	svc := New(testConfig(), gen)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, diag := svc.ExpandQuery(ctx, "dead query")
	if gen.calls != 0 {
		t.Fatalf("provider must not be called on a done context")
	}
	if diag.FallbackReason != "context_done" {
		t.Fatalf("got %+v", diag)
	}
}

func TestDeadlineReserveSkipsProvider(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`{"queries":["x"]}`)}
	svc := New(testConfig(), gen)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, diag := svc.ExpandQuery(ctx, "nearly dead query")
	if gen.calls != 0 {
		t.Fatalf("provider must not start inside the caller's reserve")
	}
	if diag.FallbackReason != "deadline_reserve" {
		t.Fatalf("got %+v", diag)
	}
}

func TestCallerDeadlineClampsBudget(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`{"queries":["x"]}`)}
	cfg := testConfig()
	cfg.Timeout = time.Hour
	svc := New(cfg, gen)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, diag := svc.ExpandQuery(ctx, "bounded query"); !diag.LLMUsed {
		t.Fatalf("expected LLM path within deadline, got %+v", diag)
	}
}

func TestScoreCandidatesLLM(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`{"scores":[
		{"id":"a","score":0.9},
		{"id":"b","score":1.7},
		{"id":"ghost","score":1.0},
		{"id":"a","score":0.1},
		{"id":"c","score":-3}
	]}`)}
	svc := New(testConfig(), gen)
	cands := []Candidate{{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}, {ID: "c", Text: "gamma"}}
	got, diag := svc.ScoreCandidates(context.Background(), "alpha beta", cands)
	if !diag.LLMUsed || diag.Candidates != 2 { // MaxCandidates=2 in testConfig
		t.Fatalf("got %+v", diag)
	}
	// Only the bounded set {a,b} may score; clamped, deduped, sorted.
	if len(got) != 2 || got[0].ID != "b" || got[0].Score != 1 || got[1].ID != "a" || got[1].Score != 0.9 {
		t.Fatalf("got %+v", got)
	}
}

func TestScoreCandidatesFallback(t *testing.T) {
	svc := New(testConfig(), nil)
	cands := []Candidate{
		{ID: "hit", Text: "ranked retrieval pipeline"},
		{ID: "miss", Text: "unrelated content entirely"},
	}
	got, diag := svc.ScoreCandidates(context.Background(), "ranked retrieval", cands)
	if diag.LLMUsed || diag.FallbackReason != "llm_not_configured" {
		t.Fatalf("got %+v", diag)
	}
	if len(got) != 2 || got[0].ID != "hit" || got[0].Score <= got[1].Score {
		t.Fatalf("lexical fallback misranked: %+v", got)
	}
}

func TestRedactionBeforeTransmission(t *testing.T) {
	const key = "gemini-test-key-not-secret"
	cfg := testConfig()
	cfg.APIKey = key
	gen := &fakeGenerator{raw: json.RawMessage(`{"scores":[{"id":"a","score":0.5}]}`)}
	svc := New(cfg, gen)
	cands := []Candidate{{
		ID:   "a",
		Text: "see /Users/sammy/secret/project/main.go and C:\\Users\\sammy\\hidden.go key " + key + " Bearer abc.def-123",
	}}
	if _, diag := svc.ScoreCandidates(context.Background(), "find /opt/home/secret/thing.go", cands); diag.FallbackReason != "" {
		t.Fatalf("got %+v", diag)
	}
	for _, bad := range []string{"/Users/sammy", "C:\\Users", key, "abc.def-123", "/opt/home"} {
		if strings.Contains(gen.lastPrompt, bad) {
			t.Fatalf("prompt leaked %q:\n%s", bad, gen.lastPrompt)
		}
	}
	for _, marker := range []string{"[path]", "[redacted-key]", "[redacted-token]"} {
		if !strings.Contains(gen.lastPrompt, marker) {
			t.Fatalf("prompt missing redaction marker %q:\n%s", marker, gen.lastPrompt)
		}
	}
}

func TestInputBoundsEnforced(t *testing.T) {
	cfg := testConfig()
	cfg.MaxInputBytes = 16
	cfg.MaxCandidateBytes = 8
	gen := &fakeGenerator{raw: json.RawMessage(`{"scores":[{"id":"a","score":0.5}]}`)}
	svc := New(cfg, gen)
	long := strings.Repeat("x", 4096)
	_, _ = svc.ScoreCandidates(context.Background(), long, []Candidate{{ID: "a", Text: long}})
	if len(gen.lastPrompt) > 512 {
		t.Fatalf("prompt not bounded: %d bytes", len(gen.lastPrompt))
	}
}

func TestExtractClaimsLLM(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`{"claims":[
		{"subject":"Retriever","predicate":"uses","object":"MMR","span":"Retriever uses MMR"},
		{"subject":"","predicate":"drops","object":"empty"},
		{"subject":"Retriever","predicate":"uses","object":"MMR","span":"dup"},
		{"subject":"A","predicate":"b","object":"c"},
		{"subject":"D","predicate":"e","object":"f"},
		{"subject":"G","predicate":"h","object":"i"}
	]}`)}
	svc := New(testConfig(), gen) // MaxClaims=3
	got, diag := svc.ExtractClaims(context.Background(), "doc-1", "Retriever uses MMR for ranking.")
	if !diag.LLMUsed {
		t.Fatalf("got %+v", diag)
	}
	if len(got) != 3 {
		t.Fatalf("cap/dedupe failed: %+v", got)
	}
	if got[0].Span != "Retriever uses MMR" {
		t.Fatalf("span lost: %+v", got[0])
	}
	if got[1].Span != "A b c" { // synthesized span when omitted
		t.Fatalf("synthesized span wrong: %+v", got[1])
	}
}

func TestExtractClaimsDeterministicAbstains(t *testing.T) {
	svc := New(testConfig(), nil)
	got, diag := svc.ExtractClaims(context.Background(), "doc-1", "Some document text.")
	if got != nil {
		t.Fatalf("deterministic mode must abstain, got %+v", got)
	}
	if diag.LLMUsed || diag.FallbackReason != "llm_not_configured" {
		t.Fatalf("got %+v", diag)
	}
}

func TestExtractClaimsInvalidJSONFallsBackToAbstain(t *testing.T) {
	gen := &fakeGenerator{raw: json.RawMessage(`not json`)}
	svc := New(testConfig(), gen)
	got, diag := svc.ExtractClaims(context.Background(), "doc-1", "Some document text.")
	if got != nil || diag.LLMUsed || diag.FallbackReason != "invalid_response" {
		t.Fatalf("got %+v %+v", got, diag)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("SENTRA_CODE_MEMORY_GEMINI_MODEL", "")
	cfg := ConfigFromEnv()
	if cfg.APIKey != "" || cfg.Model != DefaultModel {
		t.Fatalf("got %+v", cfg)
	}
	t.Setenv("GEMINI_API_KEY", "  k ")
	t.Setenv("SENTRA_CODE_MEMORY_GEMINI_MODEL", "gemini-custom")
	cfg = ConfigFromEnv()
	if cfg.APIKey != "k" || cfg.Model != "gemini-custom" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestEmptyInputsAreNoops(t *testing.T) {
	gen := &fakeGenerator{}
	svc := New(testConfig(), gen)
	if got, _ := svc.ExpandQuery(context.Background(), "   "); got != nil {
		t.Fatalf("got %v", got)
	}
	if got, _ := svc.ScoreCandidates(context.Background(), "q", nil); got != nil {
		t.Fatalf("got %v", got)
	}
	if got, _ := svc.ExtractClaims(context.Background(), "", "text"); got != nil {
		t.Fatalf("got %v", got)
	}
	if gen.calls != 0 {
		t.Fatalf("no provider call expected for empty inputs")
	}
}

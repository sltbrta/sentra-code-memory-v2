package authorityprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
	broker "github.com/sltbrta/sentra-code-memory-v2/services/broker/localauthority"
	"github.com/sltbrta/sentra-code-memory-v2/services/gateway/internal/queryapi"
)

func TestQuerySynthesizerFromEnvSelectsAdaptersFailClosed(t *testing.T) {
	t.Parallel()
	environment := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	synthesizer, err := querySynthesizerFromEnv(environment(nil))
	if err != nil || synthesizer == nil {
		t.Fatalf("default synthesizer = %#v, %v", synthesizer, err)
	}
	for name, values := range map[string]map[string]string{
		"unknown provider": {queryProviderEnvironment: "anthropic"},
		"missing key":      {queryProviderEnvironment: "openai"},
		"blank key":        {queryProviderEnvironment: "openai", openAIKeyEnvironment: "  "},
		"oversized key": {queryProviderEnvironment: "openai",
			openAIKeyEnvironment: strings.Repeat("k", 513)},
		"key with whitespace": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test key"},
		"non-https base": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIBaseURLEnvironment: "http://169.254.169.254"},
		"base with path": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIBaseURLEnvironment: "https://api.openai.com/v1"},
		"base with query": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIBaseURLEnvironment: "https://api.openai.com/?key=1"},
		"bad model": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIModelEnvironment: "model name"},
		"bad timeout": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAITimeoutEnvironment: "10"},
		"overbound timeout": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAITimeoutEnvironment: "99999"},
		"timeout text": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAITimeoutEnvironment: "soon"},
		"bad max tokens": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIMaxTokensEnvironment: "not-a-number"},
		"non-positive max tokens": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIMaxTokensEnvironment: "0"},
	} {
		t.Run(name, func(t *testing.T) {
			if synthesizer, err := querySynthesizerFromEnv(environment(values)); err == nil || synthesizer != nil {
				t.Fatalf("misconfiguration accepted: %#v, %v", synthesizer, err)
			}
		})
	}
	for name, values := range map[string]map[string]string{
		"default openai": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test"},
		"loopback base": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIBaseURLEnvironment: "http://127.0.0.1:8080", openAITimeoutEnvironment: "1500"},
		"explicit model": {queryProviderEnvironment: "openai", openAIKeyEnvironment: "sk-test",
			openAIModelEnvironment: "gpt-4o-mini-2024-07-18"},
	} {
		t.Run(name, func(t *testing.T) {
			if synthesizer, err := querySynthesizerFromEnv(environment(values)); err != nil || synthesizer == nil {
				t.Fatalf("valid configuration rejected: %v", err)
			}
		})
	}
}

func TestOpenAIClientCompletesStrictProposal(t *testing.T) {
	t.Parallel()
	var seenAuthorization, seenContentType string
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		seenAuthorization = request.Header.Get("Authorization")
		seenContentType = request.Header.Get("Content-Type")
		requestBody, _ = io.ReadAll(io.LimitReader(request.Body, 1<<20))
		writer.Header().Set("Content-Type", "application/json")
		proposal := `{"prose":"main.go returns \"stage-marker\".","claims":[{"statement":"main.go returns \"stage-marker\".","confidence_per_mille":900,"citations":[{"evidence_index":0,"start_line":3,"start_column":1,"end_line":3,"end_column":53}]}]}`
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": proposal}}},
			"usage":   map[string]uint64{"total_tokens": 123},
		})
	}))
	defer server.Close()
	client := &openAIClient{baseURL: server.URL, key: "sk-test-static", http: server.Client()}
	response, err := client.Complete(context.Background(), brain.QueryProviderRequest{
		ProviderID: "openai", Model: "gpt-4o-mini", Query: "What does main.go return?",
		Evidence: []brain.QueryEvidenceEntry{{
			Path: "main.go", BlockStartLine: 1,
			Lines: []string{"package sample", "", "func Anchor() string { return \"stage-marker\" }"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenAuthorization != "Bearer sk-test-static" || seenContentType != "application/json" {
		t.Fatalf("egress headers = %q %q", seenAuthorization, seenContentType)
	}
	if !strings.Contains(string(requestBody), "evidence[0] path=main.go") ||
		!strings.Contains(string(requestBody), `"response_format"`) {
		t.Fatalf("provider request = %s", requestBody)
	}
	// Issue #376: default OpenAI-compatible requests must NOT include an
	// artificial max_tokens cap; the openai/* family uses the model's natural
	// completion window. Callers can opt back in via OUROBOROS_OPENAI_MAX_TOKENS.
	if strings.Contains(string(requestBody), `"max_tokens"`) {
		t.Fatalf("default request must omit max_tokens: %s", requestBody)
	}
	// response_format is the strict-JSON contract — it must remain set.
	if !strings.Contains(string(requestBody), `"response_format":{"type":"json_object"}`) {
		t.Fatalf("default request must keep response_format=json_object: %s", requestBody)
	}
	if response.TokenUsage != 123 || len(response.Claims) != 1 || response.Claims[0].ConfidencePerMille != 900 ||
		len(response.Claims[0].Citations) != 1 || response.Claims[0].Citations[0].EvidenceIndex != 0 ||
		response.Claims[0].Citations[0].EndColumn != 53 {
		t.Fatalf("provider response = %#v", response)
	}
}

func TestOpenAIClientFailsClosedOnProviderFaults(t *testing.T) {
	t.Parallel()
	for name, handler := range map[string]http.HandlerFunc{
		"non-200": func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, `{"error":{"message":"quota"}}`, http.StatusTooManyRequests)
		},
		"malformed completion": func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"choices":[`))
		},
		"zero choices": func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"choices": []any{}})
		},
		"malformed proposal": func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]string{"content": "not json"}}},
			})
		},
		"unknown proposal fields": func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]string{
					"content": `{"prose":"x","claims":[],"unexpected":true}`}}},
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			client := &openAIClient{baseURL: server.URL, key: "sk-test-static", http: server.Client()}
			response, err := client.Complete(context.Background(), brain.QueryProviderRequest{
				ProviderID: "openai", Model: "gpt-4o-mini", Query: "q",
			})
			if err == nil || response != nil {
				t.Fatalf("provider fault accepted: %#v, %v", response, err)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		client := &openAIClient{baseURL: "http://127.0.0.1:1", key: "sk-test-static",
			http: &http.Client{Timeout: time.Second}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if response, err := client.Complete(ctx, brain.QueryProviderRequest{
			ProviderID: "openai", Model: "gpt-4o-mini", Query: "q",
		}); err == nil || response != nil {
			t.Fatalf("unreachable provider accepted: %#v, %v", response, err)
		}
	})
}

func TestOpenAIClientMaxTokensOptIn(t *testing.T) {
	// Not parallel: t.Setenv mutates the process env and cannot run under
	// t.Parallel(). Each subtest sets a distinct max_tokens value.
	proposal := `{"prose":"x","claims":[]}`
	cases := []struct {
		name     string
		env      string
		wantSubs []string
		dontWant []string
	}{
		{
			name:     "unset omits the field",
			wantSubs: []string{`"response_format":{"type":"json_object"}`},
			dontWant: []string{`"max_tokens"`},
		},
		{
			name:     "explicit positive includes the field",
			env:      "1500",
			wantSubs: []string{`"max_tokens":1500`},
		},
		{
			name:     "explicit cap clamps to the safe ceiling",
			env:      "99999",
			wantSubs: []string{fmt.Sprintf(`"max_tokens":%d`, openAIMaxTokensCap)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv(openAIMaxTokensEnvironment, "")
			} else {
				t.Setenv(openAIMaxTokensEnvironment, tc.env)
			}
			maxTokens, err := openAIMaxTokensFromEnv(func(string) string { return tc.env })
			if err != nil {
				t.Fatal(err)
			}
			var requestBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requestBody, _ = io.ReadAll(io.LimitReader(request.Body, 1<<20))
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": proposal}}},
					"usage":   map[string]uint64{"total_tokens": 5},
				})
			}))
			defer server.Close()
			client := &openAIClient{baseURL: server.URL, key: "sk-test-static", http: server.Client(), maxTokens: maxTokens}
			if _, err := client.Complete(context.Background(), brain.QueryProviderRequest{
				ProviderID: "openai", Model: "gpt-4o-mini", Query: "q",
			}); err != nil {
				t.Fatal(err)
			}
			body := string(requestBody)
			for _, want := range tc.wantSubs {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q in request: %s", want, body)
				}
			}
			for _, dont := range tc.dontWant {
				if strings.Contains(body, dont) {
					t.Fatalf("unexpected %q in request: %s", dont, body)
				}
			}
		})
	}
}

func TestOpenAIMaxTokensEnvFailsClosed(t *testing.T) {
	// Not parallel: t.Setenv mutates the process env.
	for _, raw := range []string{"not-a-number", "-1", "0"} {
		t.Run(raw, func(t *testing.T) {
			_, err := openAIMaxTokensFromEnv(func(string) string { return raw })
			if err == nil {
				t.Fatalf("invalid max_tokens accepted: %q", raw)
			}
		})
	}
}

func TestQueryAuthorizerAdapterEvaluatesCurrentRelationship(t *testing.T) {
	t.Parallel()
	policy, err := broker.New(broker.Config{
		UID: 501, Principal: broker.Identifier{Namespace: "principal", Value: "p"},
		Tenant:  broker.Identifier{Namespace: "tenant", Value: "t"},
		Session: broker.Identifier{Namespace: "session", Value: "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, relationship := range []string{
		"brain:b#tenant@tenant:t", "brain:b#owner@user:p", "brain:b#viewer@user:v",
	} {
		if err := policy.AddRelationship(relationship); err != nil {
			t.Fatal(err)
		}
	}
	adapter := queryAuthorizerAdapter{
		broker: policy, brain: brain.Identifier{Namespace: "brain", Value: "b"}, source: "source-1",
	}
	owner := queryapi.Principal{Tenant: "t", PrincipalID: "p", Session: "s"}
	for _, action := range []queryapi.Action{queryapi.ActionQuery, queryapi.ActionHydrate, queryapi.ActionEmit} {
		decision, err := adapter.Authorize(context.Background(), owner, action, "source-1")
		if err != nil || !decision.Allowed {
			t.Fatalf("owner %s = %#v, %v", action, decision, err)
		}
	}
	decision, err := adapter.Authorize(context.Background(), owner, queryapi.ActionHydrate, "conversation")
	if err != nil || !decision.Allowed {
		t.Fatalf("history hydration = %#v, %v", decision, err)
	}
	viewer := queryapi.Principal{Tenant: "t", PrincipalID: "v", Session: "s"}
	decision, err = adapter.Authorize(context.Background(), viewer, queryapi.ActionQuery, "source-1")
	if err != nil || !decision.Allowed {
		t.Fatalf("viewer query = %#v, %v", decision, err)
	}
	for name, args := range map[string]struct {
		action   queryapi.Action
		resource string
	}{
		"unknown resource":       {queryapi.ActionQuery, "source-2"},
		"emit on conversation":   {queryapi.ActionEmit, "conversation"},
		"query on conversation":  {queryapi.ActionQuery, "conversation"},
		"hydrate on empty value": {queryapi.ActionHydrate, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if decision, err := adapter.Authorize(context.Background(), owner, args.action, args.resource); err == nil || decision.Allowed {
				t.Fatalf("out-of-scope resource allowed: %#v, %v", decision, err)
			}
		})
	}
	stranger := queryapi.Principal{Tenant: "t", PrincipalID: "stranger", Session: "s"}
	if decision, err := adapter.Authorize(context.Background(), stranger, queryapi.ActionQuery, "source-1"); err == nil || decision.Allowed {
		t.Fatalf("relationship-less principal allowed: %#v, %v", decision, err)
	}
}

func TestMapConversationErrorCoversSentinels(t *testing.T) {
	t.Parallel()
	for sentinel, want := range map[error]error{
		brain.ErrConversationIdempotencyConflict: queryapi.ErrIdempotencyConflict,
		brain.ErrConversationUnknownAdmission:    queryapi.ErrUnknownAdmission,
		brain.ErrConversationCompletionConflict:  queryapi.ErrCompletionConflict,
		brain.ErrConversationUnknownSession:      queryapi.ErrRequestDenied,
		context.Canceled:                         context.Canceled,
		errors.New("backend down"):               queryapi.ErrRequestDenied,
	} {
		if got := mapConversationError(sentinel); !errors.Is(got, want) {
			t.Fatalf("mapConversationError(%v) = %v, want %v", sentinel, got, want)
		}
	}
}

func TestEngineResultRoundTripPreservesFields(t *testing.T) {
	t.Parallel()
	stored := &queryapi.EngineResult{
		Answer: queryapi.EngineAnswer{
			QueryID: "q-1", Status: "answered", Prose: "main.go returns \"stage-marker\".", TokenUsage: 42,
			FactualConsistency: queryapi.EngineFactualConsistency{
				Status: "scored", ScorePerMille: 850,
				Provenance: &queryapi.EngineFactualConsistencyProvenance{
					ScorerID: "fixture-scorer", ScorerVersion: "v1", CalibrationID: "fixture-calibration-v1",
					CalibrationDigest: strings.Repeat("d", 64),
				},
				EvaluatedClaimCount: 1, TotalClaimCount: 1,
			},
			Claims: []queryapi.EngineClaim{{
				ClaimID: "claim-0001", Statement: "main.go returns \"stage-marker\".", ConfidencePerMille: 900,
				Citations: []queryapi.EngineCitation{{
					EvidenceID: "e-1", SourceRevisionID: "r-1", GitOID: strings.Repeat("a", 40),
					Path: "main.go", StartLine: 3, StartColumn: 1, EndLine: 3, EndColumn: 53,
					SupportingTextDigest: strings.Repeat("b", 64),
				}},
			}},
		},
		Freshness: queryapi.EngineFreshness{
			GenerationID: "g-1", Sequence: 1, CommitOID: strings.Repeat("a", 40),
			TreeOID: strings.Repeat("c", 40), GenerationState: "ready", State: "current",
			ACLEpoch: 3, ObservedAt: time.Unix(1_000, 0).UTC(),
		},
		Coverage:   queryapi.EngineCoverage{CanonicalRevisionCount: 3, IndexedRevisionCount: 1},
		Projection: "ready",
	}
	roundTripped := engineResultFromBrain(*engineResultToBrain(stored))
	if roundTripped.Answer.QueryID != stored.Answer.QueryID ||
		roundTripped.Answer.Claims[0].Citations[0] != stored.Answer.Claims[0].Citations[0] ||
		!reflect.DeepEqual(roundTripped.Answer.FactualConsistency, stored.Answer.FactualConsistency) ||
		roundTripped.Freshness.ObservedAt != stored.Freshness.ObservedAt ||
		roundTripped.Projection != stored.Projection {
		t.Fatalf("round trip = %#v", roundTripped)
	}
}

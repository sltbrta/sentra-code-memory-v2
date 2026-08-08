// Package authorityprocess wires the query model adapter: the deterministic synthesizer is
// the default, and the policy-approved OpenAI provider adapter activates only
// when explicitly configured through the environment. Configuration is
// fail-closed: a partial or malformed provider configuration rejects startup
// and never falls back to another provider or billing identity.
package authorityprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	brain "github.com/sltbrta/sentra-code-memory-v2/services/brain/localauthority"
)

const (
	queryProviderEnvironment = "OUROBOROS_QUERY_PROVIDER"
	openAIKeyEnvironment     = "OPENAI_API_KEY"
	openAIBaseURLEnvironment = "OUROBOROS_OPENAI_BASE_URL"
	openAIModelEnvironment   = "OUROBOROS_OPENAI_MODEL"
	openAITimeoutEnvironment = "OUROBOROS_OPENAI_TIMEOUT_MS"
	// openAIMaxTokensEnvironment opts into a bounded max_tokens on the
	// OpenAI-compatible request. The default is to omit the field entirely
	// (issue #376): the openai/* family uses the model's natural completion
	// window, the engine re-verifies every claim, and the response body stays
	// bounded by openAIMaxResponseBytes plus the prompt's prose/claim caps.
	openAIMaxTokensEnvironment = "OUROBOROS_OPENAI_MAX_TOKENS"
	openAIDefaultBaseURL       = "https://api.openai.com"
	openAIDefaultModel         = "gpt-4o-mini"
	openAIDefaultTimeoutMillis = 3000
	openAIMaxTimeoutMillis     = 4000
	openAIMaxKeyBytes          = 512
	openAIMaxModelBytes        = 128
	openAIMaxResponseBytes     = 1 << 20
	openAIMaxTokensCap         = 8192
	openAIProviderID           = "openai"
)

// querySynthesizerFromEnv selects the query model adapter. With no provider
// configured the deterministic synthesizer answers; a configured provider must
// be complete and valid or startup rejects.
func querySynthesizerFromEnv(getenv func(string) string) (brain.QuerySynthesizer, error) {
	if getenv == nil {
		return nil, errInvalidConfig
	}
	provider := strings.TrimSpace(getenv(queryProviderEnvironment))
	if provider == "" {
		return brain.NewDeterministicQuerySynthesizer(), nil
	}
	if provider != openAIProviderID {
		return nil, errInvalidConfig
	}
	key := getenv(openAIKeyEnvironment)
	if len(key) == 0 || len(key) > openAIMaxKeyBytes || strings.ContainsAny(key, " \t\r\n") {
		return nil, errInvalidConfig
	}
	base := strings.TrimSpace(getenv(openAIBaseURLEnvironment))
	if base == "" {
		base = openAIDefaultBaseURL
	}
	if !validProviderBaseURL(base) {
		return nil, errInvalidConfig
	}
	model := strings.TrimSpace(getenv(openAIModelEnvironment))
	if model == "" {
		model = openAIDefaultModel
	}
	if !validProviderModel(model) {
		return nil, errInvalidConfig
	}
	timeout, err := providerTimeout(getenv(openAITimeoutEnvironment))
	if err != nil {
		return nil, err
	}
	maxTokens, err := openAIMaxTokensFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	synthesizer, err := brain.NewProviderQuerySynthesizer(openAIProviderID, model, &openAIClient{
		baseURL: base, key: key, http: &http.Client{}, maxTokens: maxTokens,
	}, timeout)
	if err != nil {
		return nil, errInvalidConfig
	}
	return synthesizer, nil
}

// validProviderBaseURL accepts only HTTPS endpoints, plus loopback HTTP for
// hermetic tests. Userinfo, queries, and fragments reject.
func validProviderBaseURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return false
	}
	if parsed.Scheme == "https" {
		return parsed.Host != ""
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func validProviderModel(model string) bool {
	if len(model) == 0 || len(model) > openAIMaxModelBytes {
		return false
	}
	for _, character := range model {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func providerTimeout(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return openAIDefaultTimeoutMillis * time.Millisecond, nil
	}
	milliseconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || milliseconds < 500 || milliseconds > openAIMaxTimeoutMillis {
		return 0, errInvalidConfig
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

// openAIClient is the policy-approved live provider egress. One bounded chat
// completion maps the verified evidence pack onto the strict JSON proposal
// shape the engine re-verifies; every failure returns an error with no partial
// output and no provider or billing fallback. The client never logs prompts,
// evidence, responses, or credentials.
type openAIClient struct {
	baseURL   string
	key       string
	http      *http.Client
	maxTokens int
}

// openAIRequest intentionally omits max_tokens unless the caller opted in via
// OUROBOROS_OPENAI_MAX_TOKENS. The default OpenAI-compatible request uses the
// model's natural completion window; the engine still re-verifies the parsed
// proposal and rejects oversized bodies via openAIMaxResponseBytes.
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	// Keep strict JSON mode enabled; only max_tokens is optional.
	ResponseFormat openAIResponseFormat `json:"response_format"`
}

type openAIResponseFormat struct {
	Type string `json:"type"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAICompletion struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens uint64 `json:"total_tokens"`
	} `json:"usage"`
}

// openAIProposal is the strict JSON shape the provider must render inside the
// message content. The engine re-verifies every citation against canonical
// bytes, so a fabricated anchor can only discard the proposal.
type openAIProposal struct {
	Prose  string `json:"prose"`
	Claims []struct {
		Statement          string `json:"statement"`
		ConfidencePerMille uint32 `json:"confidence_per_mille"`
		Citations          []struct {
			EvidenceIndex int    `json:"evidence_index"`
			StartLine     uint32 `json:"start_line"`
			StartColumn   uint32 `json:"start_column"`
			EndLine       uint32 `json:"end_line"`
			EndColumn     uint32 `json:"end_column"`
		} `json:"citations"`
	} `json:"claims"`
}

func (client *openAIClient) Complete(ctx context.Context, request brain.QueryProviderRequest) (*brain.QueryProviderResponse, error) {
	if client == nil || client.http == nil || request.ProviderID != openAIProviderID || request.Model == "" {
		return nil, fmt.Errorf("openai client: invalid request")
	}
	payload, err := json.Marshal(openAIRequest{
		Model: request.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: openAISystemPrompt},
			{Role: "user", Content: renderProviderPrompt(request)},
		},
		Temperature:    0,
		MaxTokens:      client.maxTokens,
		ResponseFormat: openAIResponseFormat{Type: "json_object"},
	})
	if err != nil {
		return nil, fmt.Errorf("openai client: encode request")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		client.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai client: build request")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+client.key)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("openai client: provider call failed")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, openAIMaxResponseBytes+1))
	if err != nil || len(body) > openAIMaxResponseBytes {
		return nil, fmt.Errorf("openai client: read response")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai client: provider rejected the call")
	}
	var completion openAICompletion
	if err := json.Unmarshal(body, &completion); err != nil || len(completion.Choices) != 1 {
		return nil, fmt.Errorf("openai client: malformed completion")
	}
	var proposal openAIProposal
	decoder := json.NewDecoder(strings.NewReader(completion.Choices[0].Message.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return nil, fmt.Errorf("openai client: malformed proposal")
	}
	if decoder.More() {
		return nil, fmt.Errorf("openai client: trailing proposal content")
	}
	result := &brain.QueryProviderResponse{Prose: proposal.Prose, TokenUsage: completion.Usage.TotalTokens}
	for _, claim := range proposal.Claims {
		mapped := brain.QueryProposedClaim{
			Statement:          claim.Statement,
			ConfidencePerMille: claim.ConfidencePerMille,
		}
		for _, citation := range claim.Citations {
			mapped.Citations = append(mapped.Citations, brain.QueryProposedCitation{
				EvidenceIndex: citation.EvidenceIndex,
				StartLine:     citation.StartLine,
				StartColumn:   citation.StartColumn,
				EndLine:       citation.EndLine,
				EndColumn:     citation.EndColumn,
			})
		}
		result.Claims = append(result.Claims, mapped)
	}
	return result, nil
}

// openAIMaxTokensFromEnv returns the optional bounded max_tokens override for
// the OpenAI-compatible chat completion. The zero default omits the field
// entirely so the provider uses its natural completion window; the response
// body remains bounded by openAIMaxResponseBytes and the system prompt's
// prose/claim caps. A positive value is clamped to openAIMaxTokensCap.
func openAIMaxTokensFromEnv(getenv func(string) string) (int, error) {
	if getenv == nil {
		return 0, fmt.Errorf("%s getter is nil", openAIMaxTokensEnvironment)
	}
	raw := strings.TrimSpace(getenv(openAIMaxTokensEnvironment))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", openAIMaxTokensEnvironment, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("invalid %s: must be positive", openAIMaxTokensEnvironment)
	}
	if value > openAIMaxTokensCap {
		value = openAIMaxTokensCap
	}
	return value, nil
}

// openAISystemPrompt fixes the bounded proposal contract: grounded claims
// only, exact in-block citation coordinates, strict JSON, and abstention by
// empty claim set rather than unsupported prose.
const openAISystemPrompt = `You answer questions strictly from the supplied code evidence blocks.
Respond with one JSON object of the form {"prose": string, "claims": [...]} and nothing else.
Each claim is {"statement": string, "confidence_per_mille": 0-1000, "citations": [...]}.
Each citation is {"evidence_index": integer, "start_line": integer, "start_column": integer,
"end_line": integer, "end_column": integer} naming one supplied evidence block and the exact
one-based half-open line/column range inside that block that supports the statement.
Cite only ranges you can see in the supplied blocks; one to three citations per claim.
Keep prose under 4000 characters and at most four claims. When the evidence cannot support
any material claim, return an empty claims array and empty prose.`

// renderProviderPrompt renders the admitted question and the verified evidence
// pack with stable evidence indices and absolute line numbers, bounded by the
// engine's own pack limits.
func renderProviderPrompt(request brain.QueryProviderRequest) string {
	var builder strings.Builder
	builder.WriteString("Question:\n")
	builder.WriteString(request.Query)
	builder.WriteString("\n\nEvidence blocks:\n")
	for index, entry := range request.Evidence {
		fmt.Fprintf(&builder, "\nevidence[%d] path=%s lines=%d-%d\n", index, entry.Path,
			entry.BlockStartLine, entry.BlockStartLine+uint32(len(entry.Lines))-1)
		for offset, line := range entry.Lines {
			fmt.Fprintf(&builder, "%d: %s\n", entry.BlockStartLine+uint32(offset), line)
		}
	}
	return builder.String()
}

var _ brain.QueryProviderClient = (*openAIClient)(nil)

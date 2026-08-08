package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LLM multi-query expands paraphrase-hard ERB questions into short BM25/ANN bags.
// Prefer Cerebras (fast) → Groq → OpenAI → OpenRouter. Always falls back to static
// multiQueryVariants on any failure / empty key / timeout.
//
// Gate: OUROBOROS_ERB_LLM_MULTIQUERY (default on under QUALITY, off under prod).
// Budget: OUROBOROS_ERB_LLM_MULTIQUERY_MS (default 1200ms).

type llmMQCand struct {
	name  string
	key   string
	model string
	url   string
}

// llmMultiQueryEnabled is true when LLM expand may run.
// QUALITY default-on; semantic recovery also wants it when keys present.
// Explicit env always wins.
func llmMultiQueryEnabled() bool {
	if v := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_LLM_MULTIQUERY")); v != "" {
		return envTruthy("OUROBOROS_ERB_LLM_MULTIQUERY", false)
	}
	// Default: on for QUALITY / bench; off for pure prod light (latency).
	return envTruthy("OUROBOROS_ERB_QUALITY", false) ||
		benchmaxEnabled() ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "bench") ||
		strings.EqualFold(envOr("OUROBOROS_ERB_MODE", ""), "research")
}

// llmMultiQueryWanted gates by question type so agentic "basic" expand
// retrieves (and lean single-doc) do not stack extra LLM RTT.
func llmMultiQueryWanted(questionType string) bool {
	if !llmMultiQueryEnabled() {
		return false
	}
	qt := strings.ToLower(strings.TrimSpace(questionType))
	switch qt {
	case "basic", "lean", "extractive":
		return false
	case "semantic", "completeness", "project_related", "conflicting_info",
		"constrained", "high_level", "intra_document_reasoning":
		return true
	case "":
		// Untyped QUALITY retrieve: allow (static still always present).
		return true
	default:
		return isMultiDocType(qt)
	}
}

func llmMultiQueryBudget() time.Duration {
	ms := envInt("OUROBOROS_ERB_LLM_MULTIQUERY_MS", 1200)
	if ms < 200 {
		ms = 200
	}
	if ms > 4000 {
		ms = 4000
	}
	return time.Duration(ms) * time.Millisecond
}

func llmMultiQueryCandidates() []llmMQCand {
	var out []llmMQCand
	if k := geminiAPIKey(); k != "" {
		out = append(out, llmMQCand{
			name:  "gemini",
			key:   k,
			model: envOr("OUROBOROS_ERB_GEMINI_MQ_MODEL", envOr("OUROBOROS_ERB_GEMINI_MODEL", "gemini-3.6-flash")),
			url:   "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		})
	}
	if k := strings.TrimSpace(os.Getenv("CEREBRAS_API_KEY")); k != "" {
		out = append(out, llmMQCand{
			name:  "cerebras",
			key:   k,
			model: envOr("OUROBOROS_ERB_CEREBRAS_MODEL", "llama3.1-8b"),
			url:   "https://api.cerebras.ai/v1/chat/completions",
		})
	}
	if k := strings.TrimSpace(os.Getenv("GROQ_API_KEY")); k != "" {
		out = append(out, llmMQCand{
			name:  "groq",
			key:   k,
			model: envOr("OUROBOROS_ERB_GROQ_MQ_MODEL", envOr("OUROBOROS_ERB_GROQ_MODEL", "llama-3.1-8b-instant")),
			url:   "https://api.groq.com/openai/v1/chat/completions",
		})
	}
	if k := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); k != "" {
		out = append(out, llmMQCand{
			name:  "openai",
			key:   k,
			model: envOr("OUROBOROS_ERB_OPENAI_MQ_MODEL", "gpt-4.1-mini"),
			url:   "https://api.openai.com/v1/chat/completions",
		})
	}
	if k := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")); k != "" {
		// gpt-oss on OpenRouter when available; override via env.
		out = append(out, llmMQCand{
			name:  "openrouter",
			key:   k,
			model: envOr("OUROBOROS_ERB_OPENROUTER_MQ_MODEL", "openai/gpt-oss-20b"),
			url:   "https://openrouter.ai/api/v1/chat/completions",
		})
	}
	return out
}

// llmExpandQueries returns up to maxN short search bags from a fast chat model.
// Empty slice on disable / no keys / timeout / parse failure (caller uses static).
func llmExpandQueries(ctx context.Context, question, questionType string, maxN int) (queries []string, meta map[string]any) {
	meta = map[string]any{"llm_multiquery": false}
	if maxN <= 0 {
		maxN = 3
	}
	if !llmMultiQueryWanted(questionType) {
		if !llmMultiQueryEnabled() {
			meta["skip"] = "disabled"
		} else {
			meta["skip"] = "type_" + strings.ToLower(strings.TrimSpace(questionType))
		}
		return nil, meta
	}
	q := strings.TrimSpace(question)
	if q == "" {
		meta["skip"] = "empty_question"
		return nil, meta
	}
	cands := llmMultiQueryCandidates()
	if len(cands) == 0 {
		meta["skip"] = "no_keys"
		return nil, meta
	}

	budget := llmMultiQueryBudget()
	cctx, cancel := withTimeout(ctx, budget)
	defer cancel()

	sys := "You expand enterprise RAG search queries. Return ONLY JSON: " +
		`{"queries":["..."]}. ` +
		"Emit 2-4 short bags (3-10 words). Prefer product names, ticket IDs, " +
		"region codes, technical jargon, and paraphrases that appear in ops docs. " +
		"No full sentences. No explanation."
	user := fmt.Sprintf("question_type=%s\n<<<USER_QUESTION>>>\n%s\n<<<END_USER_QUESTION>>>",
		strings.TrimSpace(questionType), sanitizeUntrustedPromptText(q, 1500))

	var lastErr string
	for _, cand := range cands {
		got, err := llmMQOnce(cctx, cand, sys, user, maxN)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if len(got) == 0 {
			lastErr = cand.name + ":empty"
			continue
		}
		meta["llm_multiquery"] = true
		meta["provider"] = cand.name
		meta["model"] = cand.model
		meta["n"] = len(got)
		meta["ms_budget"] = budget.Milliseconds()
		return got, meta
	}
	if lastErr != "" {
		meta["error"] = lastErr
	}
	meta["skip"] = "all_failed"
	return nil, meta
}

func llmMQOnce(ctx context.Context, cand llmMQCand, sys, user string, maxN int) ([]string, error) {
	temp := synthTemperature(0)
	seed := synthSeed()
	bodyMap := map[string]any{
		"model": cand.model,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": user},
		},
		"temperature": temp,
		"max_tokens":  180,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}
	if seed != nil {
		if !providerSupportsSeed(cand.name) {
			return nil, fmt.Errorf("%s does not support seed; unset OUROBOROS_ERB_SEED or use a supported provider", cand.name)
		}
		bodyMap["seed"] = *seed
	}
	body, _ := json.Marshal(bodyMap)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cand.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cand.key)
	req.Header.Set("Content-Type", "application/json")
	if cand.name == "openrouter" {
		req.Header.Set("HTTP-Referer", "https://github.com/sltbrta/sentra-code-memory-v2")
		req.Header.Set("X-Title", "ouroboros-llm-multiquery")
	}
	// Client timeout slightly above ctx budget so ctx cancels first.
	client := providerHTTPClient(llmMultiQueryBudget() + 200*time.Millisecond)
	// Issue #302: planning-stage usage accounting (no effect on #278 caps).
	ledgerFrom(ctx).attempt("llm_multiquery", cand.name, cand.model)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s HTTP %d: %s", cand.name, resp.StatusCode, truncate(string(raw), 160))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("%s empty choices", cand.name)
	}
	ledgerFrom(ctx).recordUsage("llm_multiquery", cand.name, cand.model,
		parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, parsed.Usage.TotalTokens)
	return parseLLMQueryBags(parsed.Choices[0].Message.Content, maxN), nil
}

// parseLLMQueryBags extracts short bags from model JSON (tolerant of minor shape drift).
func parseLLMQueryBags(content string, maxN int) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	// Strip markdown fences if present.
	if strings.HasPrefix(content, "```") {
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```JSON")
		content = strings.TrimPrefix(content, "```")
		content = strings.TrimSuffix(content, "```")
		content = strings.TrimSpace(content)
	}
	var obj struct {
		Queries []string `json:"queries"`
		// Some models use "query" or nested.
		Query []string `json:"query"`
	}
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		// Try raw string array.
		var arr []string
		if err2 := json.Unmarshal([]byte(content), &arr); err2 != nil {
			return nil
		}
		return sanitizeQueryBags(arr, maxN)
	}
	bags := obj.Queries
	if len(bags) == 0 {
		bags = obj.Query
	}
	return sanitizeQueryBags(bags, maxN)
}

func sanitizeQueryBags(bags []string, maxN int) []string {
	if maxN <= 0 {
		maxN = 3
	}
	var out []string
	seen := map[string]struct{}{}
	for _, b := range bags {
		b = strings.TrimSpace(b)
		if b == "" || len(b) > 120 {
			continue
		}
		// Drop full-sentence dumps (static path handles long Q).
		if len(strings.Fields(b)) > 14 {
			continue
		}
		k := strings.ToLower(b)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, b)
		if len(out) >= maxN {
			break
		}
	}
	return out
}

// multiQueryVariantsWithLLM merges fast LLM bags (if any) ahead of static variants.
// meta is safe to stamp into retrieval diagnostics.
func multiQueryVariantsWithLLM(ctx context.Context, question, questionType string) (variants []string, meta map[string]any) {
	static := multiQueryVariants(question, questionType)
	llm, meta := llmExpandQueries(ctx, question, questionType, 4)
	if len(llm) == 0 {
		return static, meta
	}
	// LLM first so HotLex/FTS prefer paraphrase bags; original+static still present.
	return dedupeQueries(append(append([]string{}, llm...), static...)), meta
}

// missingContentTokens returns question content tokens absent from the passage bag.
// Used for one-bound recursive gap expand (not unbounded recursion).
func missingContentTokens(question string, passages []Passage, max int) []string {
	if max <= 0 {
		max = 8
	}
	toks := contentTokens(question)
	if len(toks) == 0 {
		return nil
	}
	bag := map[string]struct{}{}
	for _, p := range passages {
		for _, t := range wordRE.FindAllString(strings.ToLower(p.Text), -1) {
			bag[t] = struct{}{}
		}
	}
	var miss []string
	seen := map[string]struct{}{}
	for _, t := range toks {
		if _, ok := bag[t]; ok {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		// Drop ultra-generic gap fillers (full500 gap_query was pure noise:
		// "made math safer machine").
		if gapStopword(t) {
			continue
		}
		if len(t) < 5 {
			continue
		}
		seen[t] = struct{}{}
		miss = append(miss, t)
		if len(miss) >= max {
			break
		}
	}
	return miss
}

func gapStopword(t string) bool {
	switch t {
	case "system", "using", "based", "make", "made", "need", "also", "like",
		"with", "from", "that", "this", "when", "what", "where", "which",
		"during", "after", "before", "about", "their", "there", "would",
		"could", "should", "being", "having", "doing", "safer", "major",
		"specifically", "recent", "change", "default", "allowed", "decide",
		"used", "name", "prevents", "getting", "until", "full", "user":
		return true
	default:
		return false
	}
}

// gapQueryFromPassages builds one short BM25 bag for one-bound recursive expand.
// Prefer uncovered paraphrase bags, then rare identifiers, then filtered miss tokens.
func gapQueryFromPassages(question string, passages []Passage) string {
	lowBag := strings.ToLower(passagesText(passages))
	// 1) Paraphrase bags not present in the pack (highest value for semantic).
	for _, bag := range pickHotLexPhrases(question, 4) {
		b := strings.ToLower(strings.TrimSpace(bag))
		if len(b) < 6 {
			continue
		}
		btoks := contentTokens(bag)
		hit := 0
		for _, t := range btoks {
			if len(t) >= 4 && strings.Contains(lowBag, t) {
				hit++
			}
		}
		if len(btoks) == 0 || hit*2 < len(btoks) {
			return bag
		}
	}
	// 2) Identifiers never seen.
	ids := extractIdentifiers(question)
	var idMiss []string
	for _, id := range ids {
		if !strings.Contains(lowBag, strings.ToLower(id)) {
			idMiss = append(idMiss, id)
		}
		if len(idMiss) >= 4 {
			break
		}
	}
	if len(idMiss) >= 1 {
		return strings.Join(idMiss, " ")
	}
	// 3) Filtered missing content tokens (prefer 2–5 specific terms).
	miss := missingContentTokens(question, passages, 6)
	if len(miss) >= 2 {
		if len(miss) > 5 {
			miss = miss[:5]
		}
		return strings.Join(miss, " ")
	}
	if len(miss) == 1 {
		return miss[0]
	}
	return ""
}

func passagesText(ps []Passage) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.Text)
		b.WriteByte(' ')
	}
	return b.String()
}

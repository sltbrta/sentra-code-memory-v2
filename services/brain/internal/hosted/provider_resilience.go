package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// synthCandidate is one already-authorized, BYOK provider in the existing
// synthesis fallback chain. Resilience never adds providers to that chain.
type synthCandidate struct {
	name  string
	key   string
	model string
	url   string
}

type synthRequest struct {
	system string
	user   string
	temp   float64
	seed   *int
}

type synthAttemptResult struct {
	raw      synthRaw
	provider string
	model    string
	err      error
}

// providerCooldowns is deliberately small and process-local. A rate-limited
// provider is skipped briefly by later requests instead of making every
// request rediscover the same 429. It is not a circuit breaker or retry loop.
type providerCooldowns struct {
	mu    sync.Mutex
	until map[string]time.Time
}

var synthProviderCooldowns = providerCooldowns{until: map[string]time.Time{}}

// synthHTTPDo is a narrow test seam; production always delegates to Client.Do.
var synthHTTPDo = func(client *http.Client, req *http.Request) (*http.Response, error) {
	return client.Do(req)
}

func (p *providerCooldowns) remaining(provider string, now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	until := p.until[provider]
	if !until.After(now) {
		delete(p.until, provider)
		return 0
	}
	return until.Sub(now)
}

func (p *providerCooldowns) rateLimit(provider string, delay time.Duration, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until := now.Add(delay)
	if until.After(p.until[provider]) {
		p.until[provider] = until
	}
}

// synthHedgeDelay enables at most one delayed hedge to the next configured
// provider. Default-off preserves production spend and provider ordering.
func synthHedgeDelay() time.Duration {
	raw := strings.TrimSpace(os.Getenv("OUROBOROS_ERB_HEDGE_DELAY_MS"))
	if raw == "" {
		return 0
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 0
	}
	if ms > 30000 {
		ms = 30000
	}
	return time.Duration(ms) * time.Millisecond
}

func defaultRateLimitCooldown() time.Duration {
	ms := envInt("OUROBOROS_ERB_RATE_LIMIT_COOLDOWN_MS", 30000)
	if ms < 1000 {
		ms = 1000
	}
	if ms > 300000 {
		ms = 300000
	}
	return time.Duration(ms) * time.Millisecond
}

// retryAfterDelay accepts both RFC Retry-After forms and clamps provider input
// to a finite local cooldown. It never sleeps or extends a request deadline.
func retryAfterDelay(raw string, now time.Time) time.Duration {
	delay := defaultRateLimitCooldown()
	if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if when, err := http.ParseTime(strings.TrimSpace(raw)); err == nil {
		delay = when.Sub(now)
	}
	if delay < time.Second {
		delay = time.Second
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

func synthAttemptTimeout(ctx context.Context, provider string) (time.Duration, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	to := synthCallTimeout()
	if dl, ok := ctx.Deadline(); ok {
		rem := time.Until(dl)
		if rem <= deadlineMargin() {
			return 0, fmt.Errorf("%s skipped: %v remaining before request deadline", provider, rem.Round(time.Millisecond))
		}
		if rem < to {
			to = rem
		}
	}
	return to, nil
}

func callSynthCandidate(ctx context.Context, cand synthCandidate, in synthRequest) synthAttemptResult {
	if ctx == nil {
		ctx = context.Background()
	}
	ledger := ledgerFrom(ctx)
	if wait := synthProviderCooldowns.remaining(cand.name, time.Now()); wait > 0 {
		ledger.skip("synth_provider:"+cand.name, "provider_cooldown")
		ledger.providerEvent(cand.name, "cooldown_skip", wait)
		return synthAttemptResult{err: fmt.Errorf("%s rate-limit cooldown active", cand.name)}
	}
	to, err := synthAttemptTimeout(ctx, cand.name)
	if err != nil {
		reason := "deadline_margin"
		if ctx != nil && ctx.Err() != nil {
			reason = "context_canceled"
		}
		ledger.skip("synth_provider:"+cand.name, reason)
		return synthAttemptResult{err: err}
	}
	if in.seed != nil && !providerSupportsSeed(cand.name) {
		ledger.skip("synth_provider:"+cand.name, "seed_unsupported")
		return synthAttemptResult{err: fmt.Errorf("%s does not support seed; unset OUROBOROS_ERB_SEED or use a supported provider", cand.name)}
	}
	// Issue #376: ERB OpenAI-compatible provider payloads intentionally omit
	// max_tokens so each vendor uses its natural completion window. The
	// bounded response-size limit (1 MiB) and the prompt's claim/prose caps
	// keep the result finite; claim/citation fail-closed bounds are preserved
	// by the grounded-claim verifier downstream. Anthropic-specific paths
	// (none in the current chain) would still require max_tokens.
	bodyMap := map[string]any{
		"model": cand.model,
		"messages": []map[string]string{
			{"role": "system", "content": in.system},
			{"role": "user", "content": in.user},
		},
		"temperature": in.temp,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}
	if in.seed != nil {
		bodyMap["seed"] = *in.seed
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		ledger.providerEvent(cand.name, "request_encode_error", 0)
		return synthAttemptResult{err: fmt.Errorf("%s request encoding: %w", cand.name, err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cand.url, bytes.NewReader(body))
	if err != nil {
		return synthAttemptResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+cand.key)
	req.Header.Set("Content-Type", "application/json")
	if cand.name == "openrouter" {
		req.Header.Set("HTTP-Referer", "https://github.com/sltbrta/sentra-code-memory-v2")
		req.Header.Set("X-Title", "ouroboros-product-brain")
	}
	stage := llmStageFrom(ctx)
	ledger.attempt(stage, cand.name, cand.model)
	resp, err := synthHTTPDo(providerHTTPClient(to), req)
	if err != nil {
		ledger.providerEvent(cand.name, "transport_error", 0)
		return synthAttemptResult{err: err}
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())
			synthProviderCooldowns.rateLimit(cand.name, wait, time.Now())
			ledger.providerEvent(cand.name, "rate_limited", wait)
		} else if resp.StatusCode >= 500 {
			ledger.providerEvent(cand.name, "transient_error", 0)
		} else {
			ledger.providerEvent(cand.name, "permanent_error", 0)
		}
		return synthAttemptResult{err: fmt.Errorf("%s HTTP %d: %s", cand.name, resp.StatusCode, truncate(string(raw), 200))}
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
		ledger.providerEvent(cand.name, "malformed_response", 0)
		return synthAttemptResult{err: err}
	}
	if len(parsed.Choices) == 0 {
		ledger.providerEvent(cand.name, "malformed_response", 0)
		return synthAttemptResult{err: fmt.Errorf("%s empty choices", cand.name)}
	}
	ledger.recordUsage(stage, cand.name, cand.model, parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, parsed.Usage.TotalTokens)
	content := parsed.Choices[0].Message.Content
	var obj struct {
		Answer           string   `json:"answer"`
		CitedDocumentIDs []string `json:"cited_document_ids"`
		Claims           []Claim  `json:"claims"`
	}
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		ledger.providerEvent(cand.name, "healthy", 0)
		return synthAttemptResult{raw: synthRaw{Answer: content}, provider: cand.name, model: cand.model}
	}
	if obj.Answer == "" {
		obj.Answer = content
	}
	ledger.providerEvent(cand.name, "healthy", 0)
	return synthAttemptResult{
		raw:      synthRaw{Answer: obj.Answer, Cited: obj.CitedDocumentIDs, Claims: obj.Claims},
		provider: cand.name,
		model:    cand.model,
	}
}

func hedgeFitsDeadline(ctx context.Context, delay time.Duration) bool {
	if ctx == nil || delay <= 0 || ctx.Err() != nil {
		return false
	}
	dl, ok := ctx.Deadline()
	return ok && time.Until(dl) > delay+deadlineMargin()
}

// callSynthHedge starts the preferred provider immediately and, only if it is
// still pending after delay, starts the next configured provider. First success
// wins and cancels the loser. A fast primary failure returns to serial fallback.
func callSynthHedge(ctx context.Context, first, second synthCandidate, in synthRequest, delay time.Duration) (synthAttemptResult, int) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan synthAttemptResult, 2)
	go func() { results <- callSynthCandidate(ctx, first, in) }()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case result := <-results:
		return result, 1
	case <-timer.C:
		if wait := synthProviderCooldowns.remaining(second.name, time.Now()); wait > 0 {
			ledgerFrom(ctx).providerEvent(second.name, "hedge_skipped_cooldown", wait)
			return <-results, 1
		}
		ledgerFrom(ctx).providerEvent(second.name, "hedge_launched", delay)
		go func() { results <- callSynthCandidate(ctx, second, in) }()
	case <-ctx.Done():
		return synthAttemptResult{err: ctx.Err()}, 1
	}

	firstResult := <-results
	if firstResult.err == nil {
		ledgerFrom(ctx).providerEvent(firstResult.provider, "hedge_won", 0)
		return firstResult, 2
	}
	select {
	case secondResult := <-results:
		if secondResult.err == nil {
			ledgerFrom(ctx).providerEvent(secondResult.provider, "hedge_won", 0)
			return secondResult, 2
		}
		return secondResult, 2
	case <-ctx.Done():
		return synthAttemptResult{err: ctx.Err()}, 2
	}
}

func callSynthCandidates(ctx context.Context, cands []synthCandidate, in synthRequest) synthAttemptResult {
	var last synthAttemptResult
	for i := 0; i < len(cands); {
		delay := synthHedgeDelay()
		if i == 0 && i+1 < len(cands) && delay > 0 {
			if hedgeFitsDeadline(ctx, delay) {
				result, consumed := callSynthHedge(ctx, cands[i], cands[i+1], in, delay)
				if result.err == nil {
					return result
				}
				last = result
				i += consumed
				if ctx != nil && (ctx.Err() != nil || !deadlineMarginOK(ctx)) {
					return last
				}
				continue
			}
			ledgerFrom(ctx).providerEvent(cands[i+1].name, "hedge_skipped_deadline", 0)
		}
		result := callSynthCandidate(ctx, cands[i], in)
		if result.err == nil {
			return result
		}
		last = result
		if ctx != nil && (ctx.Err() != nil || !deadlineMarginOK(ctx)) {
			return last
		}
		i++
	}
	return last
}

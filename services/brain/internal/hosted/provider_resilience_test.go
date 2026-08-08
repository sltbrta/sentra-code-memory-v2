package hosted

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func resetProviderCooldowns(t *testing.T) {
	t.Helper()
	synthProviderCooldowns.mu.Lock()
	synthProviderCooldowns.until = map[string]time.Time{}
	synthProviderCooldowns.mu.Unlock()
	t.Cleanup(func() {
		synthProviderCooldowns.mu.Lock()
		synthProviderCooldowns.until = map[string]time.Time{}
		synthProviderCooldowns.mu.Unlock()
	})
}

func stubSynthHTTP(t *testing.T, fn func(*http.Client, *http.Request) (*http.Response, error)) {
	t.Helper()
	previous := synthHTTPDo
	synthHTTPDo = fn
	t.Cleanup(func() { synthHTTPDo = previous })
}

func TestRetryAfterDelayBounded(t *testing.T) {
	unsetBudgetEnv(t)
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	if got := retryAfterDelay("12", now); got != 12*time.Second {
		t.Fatalf("numeric Retry-After=%v want 12s", got)
	}
	date := now.Add(45 * time.Second).Format(http.TimeFormat)
	if got := retryAfterDelay(date, now); got != 45*time.Second {
		t.Fatalf("date Retry-After=%v want 45s", got)
	}
	if got := retryAfterDelay("999999", now); got != 5*time.Minute {
		t.Fatalf("Retry-After cap=%v want 5m", got)
	}
	if got := retryAfterDelay("invalid", now); got != 30*time.Second {
		t.Fatalf("invalid Retry-After=%v want default 30s", got)
	}
}

func TestRateLimitCooldownSkipsRepeatedProvider(t *testing.T) {
	unsetBudgetEnv(t)
	resetProviderCooldowns(t)
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "10")
	var hits atomic.Int32
	stubSynthHTTP(t, func(_ *http.Client, _ *http.Request) (*http.Response, error) {
		hits.Add(1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"2"}},
			Body:       io.NopCloser(strings.NewReader("busy")),
		}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ledger := newLLMLedger(4)
	ctx = withLLMLedger(ctx, ledger)
	cand := synthCandidate{name: "openai", key: "test", model: "test", url: "https://openai.test/chat"}
	in := synthRequest{system: "system", user: "question", temp: 0}
	if got := callSynthCandidate(ctx, cand, in); got.err == nil {
		t.Fatal("429 must fail the attempt")
	}
	if got := callSynthCandidate(ctx, cand, in); got.err == nil {
		t.Fatal("cooled provider must fail closed")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("provider hits=%d want 1 after cooldown skip", got)
	}
	diag := map[string]any{}
	ledger.stampInto(diag)
	budget := diag["llm_budget"].(map[string]any)
	events := budget["provider_health"].([]map[string]any)
	if len(events) != 2 || events[0]["event"] != "rate_limited" || events[1]["event"] != "cooldown_skip" {
		t.Fatalf("unexpected provider health events: %#v", events)
	}
	if budget["provider_attempts"] != 1 {
		t.Fatalf("cooldown skip must not count as an HTTP attempt: %#v", budget)
	}
}

func TestDeadlineBoundedHedgeUsesConfiguredNextProvider(t *testing.T) {
	unsetBudgetEnv(t)
	resetProviderCooldowns(t)
	t.Setenv("OUROBOROS_ERB_HEDGE_DELAY_MS", "20")
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "10")
	var slowHits, fastHits atomic.Int32
	slowDone := make(chan struct{})
	stubSynthHTTP(t, func(_ *http.Client, req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "slow.test":
			slowHits.Add(1)
			<-req.Context().Done()
			close(slowDone)
			return nil, req.Context().Err()
		case "fast.test":
			fastHits.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"choices":[{"message":{"content":"{\"answer\":\"hedged\",\"cited_document_ids\":[\"d1\"]}"}}]}`,
				)),
			}, nil
		default:
			t.Fatalf("unexpected provider URL: %s", req.URL)
			return nil, nil
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ledger := newLLMLedger(4)
	ctx = withLLMLedger(ctx, ledger)
	started := time.Now()
	result := callSynthCandidates(ctx, []synthCandidate{
		{name: "openai", key: "test", model: "slow", url: "https://slow.test/chat"},
		{name: "gemini", key: "test", model: "fast", url: "https://fast.test/chat"},
	}, synthRequest{system: "system", user: "question", temp: 0})
	<-slowDone
	if result.err != nil || result.provider != "gemini" || result.raw.Answer != "hedged" {
		t.Fatalf("unexpected hedge result: %#v", result)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("hedge took %v; expected fast provider before deadline", elapsed)
	}
	if slowHits.Load() != 1 || fastHits.Load() != 1 {
		t.Fatalf("hedge hits slow=%d fast=%d want 1 each", slowHits.Load(), fastHits.Load())
	}
	diag := map[string]any{}
	ledger.stampInto(diag)
	budget := diag["llm_budget"].(map[string]any)
	if budget["provider_attempts"] != 2 {
		t.Fatalf("hedge attempts not bounded/audited: %#v", budget)
	}
	events := budget["provider_health"].([]map[string]any)
	var launched, won bool
	for _, event := range events {
		launched = launched || event["event"] == "hedge_launched"
		won = won || event["event"] == "hedge_won"
	}
	if !launched || !won {
		t.Fatalf("missing hedge diagnostics: %#v", events)
	}
}

func TestHedgeSkippedWithoutDeadlineMargin(t *testing.T) {
	unsetBudgetEnv(t)
	resetProviderCooldowns(t)
	t.Setenv("OUROBOROS_ERB_HEDGE_DELAY_MS", "200")
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "150")
	var secondHits atomic.Int32
	stubSynthHTTP(t, func(_ *http.Client, req *http.Request) (*http.Response, error) {
		if req.URL.Host == "secondary.test" {
			secondHits.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"choices":[{"message":{"content":"{\"answer\":\"primary\"}"}}]}`,
			)),
		}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ledger := newLLMLedger(4)
	ctx = withLLMLedger(ctx, ledger)
	result := callSynthCandidates(ctx, []synthCandidate{
		{name: "openai", key: "test", model: "primary", url: "https://primary.test/chat"},
		{name: "gemini", key: "test", model: "secondary", url: "https://secondary.test/chat"},
	}, synthRequest{system: "system", user: "question", temp: 0})
	if result.err != nil || result.provider != "openai" {
		t.Fatalf("primary result changed when hedge skipped: %#v", result)
	}
	if secondHits.Load() != 0 {
		t.Fatalf("near-deadline hedge called secondary %d times", secondHits.Load())
	}
	diag := map[string]any{}
	ledger.stampInto(diag)
	events := diag["llm_budget"].(map[string]any)["provider_health"].([]map[string]any)
	if len(events) < 1 || events[0]["event"] != "hedge_skipped_deadline" {
		t.Fatalf("missing deadline hedge skip diagnostic: %#v", events)
	}
}

// Issue #376: the ERB OpenAI-compatible synthesis body must not carry an
// artificial max_tokens cap. The bounded response-size guard and the
// prompt's claim/prose caps keep the result finite; claim/citation
// fail-closed bounds are preserved by downstream grounding. Anthropic-only
// paths (none in the current chain) would still need max_tokens.
func TestSynthRequestBodyOmitsMaxTokensForOpenAICompatible(t *testing.T) {
	unsetBudgetEnv(t)
	resetProviderCooldowns(t)
	// Tight deadline margin mirrors the other synth-candidate tests.
	t.Setenv("OUROBOROS_ERB_DEADLINE_MARGIN_MS", "10")
	var requestBody []byte
	stubSynthHTTP(t, func(_ *http.Client, req *http.Request) (*http.Response, error) {
		requestBody, _ = io.ReadAll(io.LimitReader(req.Body, 1<<20))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"choices":[{"message":{"content":"{\"answer\":\"ok\"}"}}]}`,
			)),
		}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ledger := newLLMLedger(2)
	ctx = withLLMLedger(ctx, ledger)
	result := callSynthCandidates(ctx, []synthCandidate{
		{name: "openai", key: "test", model: "gpt", url: "https://openai.test/chat"},
	}, synthRequest{system: "system", user: "question", temp: 0})
	if result.err != nil {
		t.Fatalf("synth error: %v", result.err)
	}
	if strings.Contains(string(requestBody), `"max_tokens"`) {
		t.Fatalf("synthesis body must omit max_tokens: %s", requestBody)
	}
	if !strings.Contains(string(requestBody), `"response_format"`) {
		t.Fatalf("synthesis body must keep response_format contract: %s", requestBody)
	}
}

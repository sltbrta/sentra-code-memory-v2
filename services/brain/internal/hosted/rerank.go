package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// crossEncodeRerank reorders passages with ZeroEntropy zerank-2, else lexical CE.
func crossEncodeRerank(ctx context.Context, question string, passages []Passage, topN int) ([]Passage, map[string]any) {
	return crossEncodeRerankModeCached(ctx, question, passages, topN, false, rerankScoreScope{}, nil)
}

// crossEncodeRerankMode forces lexical CE when forceLexical (interactive G15 path
// avoids multi-second ZeroEntropy RTT on the critical path).
func crossEncodeRerankMode(ctx context.Context, question string, passages []Passage, topN int, forceLexical bool) (out []Passage, diag map[string]any) {
	return crossEncodeRerankModeCached(ctx, question, passages, topN, forceLexical, rerankScoreScope{}, nil)
}

// crossEncodeRerankClient is the production issue #301 path. Keeping the
// cache/scope on Client avoids process-global score sharing while the digested
// identity still makes WithBrainID and generation/ACL changes safe.
func (c *Client) crossEncodeRerankClient(ctx context.Context, question string, passages []Passage, topN int, forceLexical bool) ([]Passage, map[string]any) {
	if c == nil {
		return crossEncodeRerankMode(ctx, question, passages, topN, forceLexical)
	}
	return crossEncodeRerankModeCached(ctx, question, passages, topN, forceLexical, c.rerankScope(), c.rerankScores)
}

func crossEncodeRerankModeCached(ctx context.Context, question string, passages []Passage, topN int, forceLexical bool, scope rerankScoreScope, cache *rerankScoreCache) (out []Passage, diag map[string]any) {
	ctx, qualitySpan := startRerankQualitySpan(ctx, len(passages))
	diag = map[string]any{"rerank": "skipped"}
	defer func() { finishRerankQualitySpan(qualitySpan, out, diag) }()
	if len(passages) == 0 {
		stampRerankScoreRun(diag, rerankScoreRun{})
		return passages, diag
	}
	if topN <= 0 || topN > len(passages) {
		topN = len(passages)
	}
	if !envTruthy("OUROBOROS_ERB_RERANK", true) {
		diag["rerank"] = "disabled"
		stampRerankScoreRun(diag, rerankScoreRun{input: len(passages)})
		return passages, diag
	}
	var attempted, failed, failureReasons []string
	scoreRun := rerankScoreRun{input: len(passages)}
	fallbackLatency := time.Duration(0)
	mergeScoreRun := func(run rerankScoreRun) {
		scoreRun.input = run.input
		scoreRun.selected = run.selected
		// Cache status describes the latest/serving backend, matching the
		// pre-#301 diagnostic contract. Provider work below is cumulative so a
		// failed first backend cannot disappear from cost exposure.
		scoreRun.cacheHits = run.cacheHits
		scoreRun.misses = run.misses
		scoreRun.stales = run.stales
		scoreRun.providerScored += run.providerScored
		scoreRun.providerSubmitted = scoreRun.providerSubmitted || run.providerSubmitted
		scoreRun.ceCharacters += run.ceCharacters
		scoreRun.ceCharactersCapped = scoreRun.ceCharactersCapped || run.ceCharactersCapped || scoreRun.ceCharacters > maxRerankCECharactersDiag
		if scoreRun.ceCharacters > maxRerankCECharactersDiag {
			scoreRun.ceCharacters = maxRerankCECharactersDiag
		}
		scoreRun.providerLatency += run.providerLatency
		scoreRun.providerTimeout = run.providerTimeout
		scoreRun.cacheEnabled = run.cacheEnabled
		scoreRun.cacheable = run.cacheable
	}
	recordFailure := func(backend string, run rerankScoreRun, reason string) {
		failed = append(failed, backend)
		if reason == "" {
			reason = run.failureReason
		}
		if reason == "" {
			reason = "unknown"
		}
		failureReasons = append(failureReasons, reason)
		fallbackLatency += run.providerLatency
	}
	stampAttempts := func() {
		stampRerankScoreRun(diag, scoreRun)
		if len(attempted) > 0 {
			diag["rerank_attempted"] = append([]string(nil), attempted...)
		}
		diag["rerank_fallback"] = len(failed) > 0
		if len(failed) > 0 {
			diag["rerank_fallback_from"] = append([]string(nil), failed...)
			diag["rerank_fallback_reasons"] = append([]string(nil), failureReasons...)
			diag["rerank_fallback_latency_ms"] = boundedRerankDurationMS(fallbackLatency)
		}
	}
	ranker := strings.ToLower(strings.TrimSpace(os.Getenv("OUROBOROS_BRAIN_RANKER")))
	if !forceLexical {
		// Local MLX / BYOC ranker first when selected (air-gapped residual).
		if ranker == SubstrateAPIMLX || ranker == "local" {
			attempted = append(attempted, "mlx")
			model := mlxRankModel()
			scoreCall := func(ctx context.Context, question string, selected []Passage, topN int) ([]remoteRerankResult, error) {
				return mlxRerankResults(ctx, model, question, selected, topN)
			}
			reordered, cacheRun, err := rerankRemoteBounded(ctx, question, passages, topN, "mlx", model, scope, cache, scoreCall)
			mergeScoreRun(cacheRun)
			if err == nil && len(reordered) > 0 && samePassageCandidates(passages, reordered) {
				diag["rerank"] = "ok"
				diag["rerank_backend"] = "mlx"
				diag["rerank_model"] = model
				diag["rerank_top_n"] = topN
				stampAttempts()
				return reordered, diag
			}
			if err == nil {
				recordFailure("mlx", cacheRun, "candidate_set_changed")
			} else {
				recordFailure("mlx", cacheRun, "")
			}
		}
		// Hosted: Cohere Rerank first (v5 + arXiv 2026: CE is largest single gain),
		// then ZeroEntropy, then lexical.
		if ranker == "" || ranker == SubstrateAPIHosted || ranker == "remote" || ranker == "cohere" {
			if key := cohereKey(); key != "" {
				attempted = append(attempted, "cohere")
				model := envOr("OUROBOROS_ERB_COHERE_RERANK_MODEL", "rerank-v3.5")
				scoreCall := func(ctx context.Context, question string, selected []Passage, topN int) ([]remoteRerankResult, error) {
					return cohereRerankResults(ctx, key, model, question, selected, topN)
				}
				reordered, cacheRun, err := rerankRemoteBounded(ctx, question, passages, topN, "cohere", model, scope, cache, scoreCall)
				mergeScoreRun(cacheRun)
				if err == nil && len(reordered) > 0 {
					diag["rerank"] = "ok"
					diag["rerank_backend"] = "cohere"
					diag["rerank_model"] = model
					diag["rerank_top_n"] = topN
					stampAttempts()
					return reordered, diag
				}
				if err == nil {
					recordFailure("cohere", cacheRun, "empty_response")
				} else {
					recordFailure("cohere", cacheRun, "")
				}
			}
			if key := zeKey(); key != "" {
				attempted = append(attempted, "zeroentropy")
				model := envOr("OUROBOROS_ERB_ZE_RERANK_MODEL", "zerank-2")
				scoreCall := func(ctx context.Context, question string, selected []Passage, topN int) ([]remoteRerankResult, error) {
					return zeRerankResults(ctx, key, model, question, selected, topN)
				}
				reordered, cacheRun, err := rerankRemoteBounded(ctx, question, passages, topN, "zeroentropy", model, scope, cache, scoreCall)
				mergeScoreRun(cacheRun)
				if err == nil && len(reordered) > 0 {
					diag["rerank"] = "ok"
					diag["rerank_backend"] = "zeroentropy"
					diag["rerank_model"] = model
					diag["rerank_top_n"] = topN
					stampAttempts()
					return reordered, diag
				}
				if err == nil {
					recordFailure("zeroentropy", cacheRun, "empty_response")
				} else {
					recordFailure("zeroentropy", cacheRun, "")
				}
			}
		}
	} else {
		diag["rerank_force_lexical"] = true
	}
	// Lexical CE: content-token overlap score (in-process, sub-ms–ms). It only
	// reorders the already ACL-filtered pool and retains every raw candidate.
	scored := make([]Passage, len(passages))
	copy(scored, passages)
	qtoks := contentTokens(question)
	for i := range scored {
		// denser: count unique content token hits
		low := strings.ToLower(scored[i].Text)
		n := 0
		for _, t := range qtoks {
			if strings.Contains(low, t) {
				n++
			}
		}
		scored[i].Score = float64(n)
		scored[i].Channel = scored[i].Channel + "+lex_ce"
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].DocumentID < scored[j].DocumentID
	})
	diag["rerank"] = "ok"
	diag["rerank_backend"] = "lexical"
	stampAttempts()
	return scored, diag
}

func zeKey() string {
	for _, name := range []string{"ZEROENTROPY_API_KEY", "SENTRA_ZEROENTROPY_API_KEY", "ZE_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func cohereKey() string {
	for _, name := range []string{"COHERE_API_KEY", "CO_API_KEY", "SENTRA_COHERE_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

type remoteRerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

// assembleRemoteRerank treats provider indices as untrusted. A malformed or
// duplicate index fails the entire response so callers fall back over the same
// ACL-filtered candidates. Provider top_n only bounds scored heads; unreturned
// lexical/dense tails are appended unchanged.
func assembleRemoteRerank(passages []Passage, results []remoteRerankResult, backend string) ([]Passage, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("%s rerank empty results", backend)
	}
	out := make([]Passage, 0, len(passages))
	seen := make(map[int]struct{}, len(results))
	for _, r := range results {
		if r.Index < 0 || r.Index >= len(passages) {
			return nil, fmt.Errorf("%s rerank invalid index %d", backend, r.Index)
		}
		if _, ok := seen[r.Index]; ok {
			return nil, fmt.Errorf("%s rerank duplicate index %d", backend, r.Index)
		}
		seen[r.Index] = struct{}{}
		p := passages[r.Index]
		p.Score = r.RelevanceScore
		switch backend {
		case "cohere":
			if p.Channel == "" {
				p.Channel = "cohere_rerank"
			} else if !strings.Contains(p.Channel, "cohere") {
				p.Channel += "+cohere"
			}
		case "mlx":
			p.Channel += "+mlx_ce"
		default:
			p.Channel += "+ce"
		}
		out = append(out, p)
	}
	for i, p := range passages {
		if _, ok := seen[i]; !ok {
			out = append(out, p)
		}
	}
	if !samePassageCandidates(passages, out) {
		return nil, fmt.Errorf("%s rerank candidate set changed", backend)
	}
	return out, nil
}

// validateCompleteRerankScores turns an untrusted provider response into one
// score per submitted candidate. The bounded cache never stores partial,
// duplicate, non-finite, or out-of-range responses. Negative finite scores are
// valid and intentionally remain distinguishable from cache misses.
func validateCompleteRerankScores(results []remoteRerankResult, want int, backend string) ([]float64, error) {
	if want <= 0 || len(results) != want {
		return nil, fmt.Errorf("%s rerank incomplete results: got %d want %d", backend, len(results), want)
	}
	scores := make([]float64, want)
	seen := make([]bool, want)
	for _, result := range results {
		if result.Index < 0 || result.Index >= want {
			return nil, fmt.Errorf("%s rerank invalid index %d", backend, result.Index)
		}
		if seen[result.Index] {
			return nil, fmt.Errorf("%s rerank duplicate index %d", backend, result.Index)
		}
		if math.IsNaN(result.RelevanceScore) || math.IsInf(result.RelevanceScore, 0) {
			return nil, fmt.Errorf("%s rerank non-finite score", backend)
		}
		seen[result.Index] = true
		scores[result.Index] = result.RelevanceScore
	}
	return scores, nil
}

// cohereRerank calls Cohere v2 /rerank (model default rerank-v3.5 = v5 parity).
func cohereRerank(ctx context.Context, apiKey, question string, passages []Passage, topN int) ([]Passage, error) {
	model := envOr("OUROBOROS_ERB_COHERE_RERANK_MODEL", "rerank-v3.5")
	results, err := cohereRerankResults(ctx, apiKey, model, question, passages, topN)
	if err != nil {
		return nil, err
	}
	return assembleRemoteRerank(passages, results, "cohere")
}

func cohereRerankResults(ctx context.Context, apiKey, model, question string, passages []Passage, topN int) ([]remoteRerankResult, error) {
	docs := make([]string, len(passages))
	for i, p := range passages {
		docs[i] = clippedRerankText(p.Text, "cohere")
	}
	payload := map[string]any{
		"model":     model,
		"query":     question,
		"documents": docs,
		"top_n":     topN,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.cohere.com/v2/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ouroboros-product-brain-hosted/0.3")
	client := providerHTTPClient(45 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cohere rerank HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Results []remoteRerankResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Results, nil
}

func zeRerank(ctx context.Context, apiKey, question string, passages []Passage, topN int) ([]Passage, error) {
	model := envOr("OUROBOROS_ERB_ZE_RERANK_MODEL", "zerank-2")
	results, err := zeRerankResults(ctx, apiKey, model, question, passages, topN)
	if err != nil {
		return nil, err
	}
	return assembleRemoteRerank(passages, results, "zeroentropy")
}

func zeRerankResults(ctx context.Context, apiKey, model, question string, passages []Passage, topN int) ([]remoteRerankResult, error) {
	base := envOr("OUROBOROS_ERB_ZE_BASE", "https://api.zeroentropy.dev/v1")
	docs := make([]string, len(passages))
	for i, p := range passages {
		docs[i] = clippedRerankText(p.Text, "zeroentropy")
	}
	payload := map[string]any{
		"model":     model,
		"query":     question,
		"documents": docs,
		"top_n":     topN,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/models/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ouroboros-product-brain-hosted/0.2")
	client := providerHTTPClient(60 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ze rerank HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Results []remoteRerankResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Results, nil
}

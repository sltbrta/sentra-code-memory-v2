package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// planWithLedger resolves the query plan for one request and returns it with a
// context carrying the request-scoped LLM ledger.
//
// Ordering matters (issue #302): the ledger must be attached *before* the
// optional plan-refine call, otherwise refineQueryPlanLLM's skip / attempt /
// usage records land on a nil ledger and the query_plan stage is silently
// missing from the consolidated budget and usage diagnostics. Planning records
// counters only — it never calls beginCall, so it consumes no generation call
// slot — which is why the ledger is created here with a provisional cap and the
// real per-mode cap (issue #278) is set by the caller via setMaxCalls once the
// plan and question type have settled.
//
// The returned context is never nil, so downstream deadline-bounded calls stay
// safe even when a caller passes a nil context.
func planWithLedger(ctx context.Context, question, labeledType, mode string) (context.Context, *llmLedger, string, QueryPlan) {
	if ctx == nil {
		ctx = context.Background()
	}
	questionType, plan := PlanFromOpts(question, labeledType, mode)
	ledger := newLLMLedger(1)
	ctx = withLLMLedger(ctx, ledger)
	// Thin plans: optional cheap LLM refine (QUALITY default-on; fail-open).
	plan = refineQueryPlanLLM(ctx, question, plan)
	if labeledType == "" && plan.EffectiveType != "" {
		questionType = plan.EffectiveType
	}
	return ctx, ledger, questionType, plan
}

// refineQueryPlanLLM optionally tightens a thin plan via a tiny JSON classifier.
// Deterministic InferQueryPlan always runs first; this only fires when:
//   - plan.IsThin(question)
//   - OUROBOROS_ERB_QUERY_PLAN_LLM=1 (default on for QUALITY, off for prod light)
//   - a fast key is present (Cerebras → Groq → OpenAI)
//
// Fail-open on call errors: the original plan is returned. Latency is
// fail-closed (issue #282): the call budget is clamped to the request's
// remaining deadline minus deadlineMargin(), and when that bounded budget
// cannot fit, the refine is skipped outright (plan.LLMSkipped stamped) so an
// optional planning call never consumes the retrieval/synthesis reserve.
func refineQueryPlanLLM(ctx context.Context, question string, plan QueryPlan) QueryPlan {
	if ctx == nil {
		// context.WithTimeout panics on a nil parent; keep the optional refine
		// safe for callers that never established a request context.
		ctx = context.Background()
	}
	if !plan.IsThin(question) {
		return plan
	}
	if !queryPlanLLMWanted() {
		return plan
	}
	budget, skip := queryPlanLLMBudget(ctx)
	if skip != "" {
		// Fail closed before any network attempt: deterministic plan stands.
		plan.LLMSkipped = skip
		ledgerFrom(ctx).skip("query_plan", skip)
		return plan
	}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	sys := `Classify the user question for enterprise document Q&A. Return ONLY JSON:
{"multi_doc":bool,"semantic_expand":bool,"conflict":bool,"completeness":bool,"checklist":bool,"deep_hydrate":bool,"agentic":bool,"effective_type":"basic|semantic|completeness|conflicting_info|project_related|constrained|info_not_found"}
Use conflict for A-vs-B / correction. completeness for list-all entities. semantic_expand for paraphrase. Prefer multi_doc+agentic when unsure.`
	user := "question=" + strings.TrimSpace(question)

	raw, ok := queryPlanLLMCall(cctx, sys, user)
	if !ok || raw == "" {
		return plan
	}
	// Extract JSON object.
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var obj struct {
		MultiDoc       bool   `json:"multi_doc"`
		SemanticExpand bool   `json:"semantic_expand"`
		Conflict       bool   `json:"conflict"`
		Completeness   bool   `json:"completeness"`
		Checklist      bool   `json:"checklist"`
		DeepHydrate    bool   `json:"deep_hydrate"`
		Agentic        bool   `json:"agentic"`
		EffectiveType  string `json:"effective_type"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return plan
	}
	// Union flags (never strip deterministic surface cues already true).
	if obj.MultiDoc {
		plan.MultiDoc = true
	}
	if obj.SemanticExpand {
		plan.SemanticExpand = true
	}
	if obj.Conflict {
		plan.Conflict = true
		plan.MultiDoc = true
	}
	if obj.Completeness {
		plan.Completeness = true
		plan.MultiDoc = true
	}
	if obj.Checklist {
		plan.Checklist = true
	}
	if obj.DeepHydrate {
		plan.DeepHydrate = true
	}
	if obj.Agentic {
		plan.Agentic = true
	}
	et := strings.ToLower(strings.TrimSpace(obj.EffectiveType))
	switch et {
	case "basic", "semantic", "completeness", "conflicting_info", "project_related",
		"constrained", "info_not_found", "intra_document_reasoning", "high_level":
		// Only replace effective type when deterministic was weak default.
		if plan.Source == "inferred" && (plan.IsThin(question) || plan.EffectiveType == "semantic" || plan.EffectiveType == "basic") {
			plan.EffectiveType = et
		}
	}
	plan.LLMRefined = true
	if plan.Source == "inferred" {
		plan.Source = "inferred+llm"
	} else if plan.Source == "labeled" {
		plan.Source = "labeled+inferred"
	}
	// Re-apply mode after LLM so light still demotes agentic.
	plan = ApplyServeMode(plan, plan.Mode)
	return plan
}

// queryPlanLLMFloor is the minimum useful budget for one plan-refine attempt.
// Anything shorter is a doomed network round-trip — skip instead of rushing.
const queryPlanLLMFloor = 100 * time.Millisecond

// queryPlanLLMBudget bounds the optional plan-refine call (issue #282). The
// env budget (OUROBOROS_ERB_QUERY_PLAN_LLM_MS, default 400) is clamped to
// 100–1500ms and then to the request's remaining deadline minus
// deadlineMargin() — the same reserve issue #278 requires before starting a
// generation call — so plan refinement can never eat the time retrieval and
// synthesis need. Returns a non-empty skip reason (and zero budget) when the
// bounded budget falls under the floor or the context is already done.
func queryPlanLLMBudget(ctx context.Context) (time.Duration, string) {
	ms := envInt("OUROBOROS_ERB_QUERY_PLAN_LLM_MS", 400)
	if ms < 100 {
		ms = 100
	}
	if ms > 1500 {
		ms = 1500
	}
	d := time.Duration(ms) * time.Millisecond
	if ctx == nil {
		return d, ""
	}
	if ctx.Err() != nil {
		return 0, "context_done"
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return d, ""
	}
	avail := time.Until(dl) - deadlineMargin()
	if avail < queryPlanLLMFloor {
		return 0, "deadline_reserve"
	}
	if d > avail {
		d = avail
	}
	return d, ""
}

func queryPlanLLMWanted() bool {
	if envTruthy("OUROBOROS_ERB_QUERY_PLAN_LLM", false) {
		return true
	}
	// Default-on for QUALITY residual; off for prod interactive unless set.
	if envTruthy("OUROBOROS_ERB_QUALITY", false) {
		return !envTruthy("OUROBOROS_ERB_QUERY_PLAN_LLM_OFF", false)
	}
	return false
}

func queryPlanLLMCall(ctx context.Context, sys, user string) (string, bool) {
	type cand struct {
		name, key, url, model string
	}
	var cands []cand
	if k := geminiAPIKey(); k != "" {
		cands = append(cands, cand{
			"gemini", k,
			"https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
			envOr("OUROBOROS_ERB_GEMINI_MODEL", "gemini-3.6-flash"),
		})
	}
	if k := strings.TrimSpace(os.Getenv("CEREBRAS_API_KEY")); k != "" {
		cands = append(cands, cand{"cerebras", k, "https://api.cerebras.ai/v1/chat/completions", "llama-3.3-70b"})
	}
	if k := strings.TrimSpace(os.Getenv("GROQ_API_KEY")); k != "" {
		cands = append(cands, cand{"groq", k, "https://api.groq.com/openai/v1/chat/completions", "llama-3.3-70b-versatile"})
	}
	if k := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); k != "" {
		cands = append(cands, cand{"openai", k, "https://api.openai.com/v1/chat/completions", "gpt-4.1-mini"})
	}
	if len(cands) == 0 {
		return "", false
	}
	temp := synthTemperature(0)
	seed := synthSeed()
	body := map[string]any{
		"model": cands[0].model,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": user},
		},
		"temperature": temp,
		"max_tokens":  120,
	}
	if seed != nil {
		body["seed"] = *seed
	}
	for _, c := range cands {
		if seed != nil && !providerSupportsSeed(c.name) {
			continue
		}
		body["model"] = c.model
		raw, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(raw))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.key)
		// Issue #302: planning-stage usage accounting (no effect on #278 caps).
		ledgerFrom(ctx).attempt("query_plan", c.name, c.model)
		resp, err := providerHTTPClient(0).Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			continue
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
		if err := json.Unmarshal(b, &parsed); err != nil || len(parsed.Choices) == 0 {
			continue
		}
		ledgerFrom(ctx).recordUsage("query_plan", c.name, c.model,
			parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, parsed.Usage.TotalTokens)
		return parsed.Choices[0].Message.Content, true
	}
	return "", false
}

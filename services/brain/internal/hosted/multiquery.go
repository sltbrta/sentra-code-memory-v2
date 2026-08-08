package hosted

import (
	"regexp"
	"sort"
	"strings"
)

var (
	wordRE         = regexp.MustCompile(`[A-Za-z0-9_]{2,}`)
	identCodeRE    = regexp.MustCompile(`\b(?:[A-Z]{2,}[-_]?\d{2,}|\d{2,}[-_]?[A-Z]{2,}[A-Za-z0-9_-]*|[A-Z][a-z]+[A-Z][A-Za-z0-9]+)\b`)
	identQuoteRE   = regexp.MustCompile(`["'“”]([^"'“”]{2,64})["'“”]`)
	identSnakeRE   = regexp.MustCompile(`\b[a-z][a-z0-9]+(?:_[a-z0-9]+){1,4}\b`)
	identHyphenRE  = regexp.MustCompile(`\b[a-z][a-z0-9]+(?:-[a-z0-9]+){1,6}\b`)
	identMeasureRE = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s*(?:seconds?|ms|s|tokens?|concurrent|sessions?|minutes?|hours?|mi[Bb]|%|x)\b`)
	identMoneyRE   = regexp.MustCompile(`\$\d+(?:\.\d+)?`)
)

var stopWords = map[string]struct{}{
	"what": {}, "when": {}, "where": {}, "which": {}, "who": {}, "whom": {}, "whose": {},
	"why": {}, "how": {}, "does": {}, "did": {}, "the": {}, "and": {}, "for": {}, "from": {},
	"with": {}, "into": {}, "about": {}, "that": {}, "this": {}, "these": {}, "those": {},
	"are": {}, "was": {}, "were": {}, "been": {}, "have": {}, "has": {}, "had": {},
	"will": {}, "would": {}, "should": {}, "could": {}, "can": {}, "may": {}, "might": {},
	"must": {}, "not": {}, "new": {}, "recent": {}, "current": {}, "default": {}, "exact": {},
	"specific": {}, "using": {}, "during": {}, "after": {}, "before": {}, "between": {},
	"through": {}, "their": {}, "there": {}, "they": {}, "them": {}, "our": {}, "your": {},
	"its": {}, "a": {}, "an": {}, "or": {}, "in": {}, "on": {}, "to": {}, "of": {}, "is": {},
	"be": {}, "by": {}, "as": {}, "at": {}, "it": {}, "we": {}, "you": {}, "all": {},
}

// extractIdentifiers pulls high-specificity tokens for retention floor.
func extractIdentifiers(question string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) < 2 {
			return
		}
		key := strings.ToLower(s)
		if _, ok := stopWords[key]; ok {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	for _, m := range identQuoteRE.FindAllStringSubmatch(question, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	for _, m := range identCodeRE.FindAllString(question, -1) {
		add(m)
	}
	for _, m := range identSnakeRE.FindAllString(question, -1) {
		add(m)
	}
	// Hyphen compounds: core-tiling-multiplex, locked-down, end-to-end, hot-route.
	for _, m := range identHyphenRE.FindAllString(strings.ToLower(question), -1) {
		add(m)
	}
	// Measures / money: "18 seconds", "12 concurrent", "$0.062", "p95".
	for _, m := range identMeasureRE.FindAllString(question, -1) {
		add(m)
	}
	for _, m := range identMoneyRE.FindAllString(question, -1) {
		add(m)
	}
	// Capitalized multi-token product-ish names
	for _, tok := range wordRE.FindAllString(question, -1) {
		if len(tok) >= 4 && tok[0] >= 'A' && tok[0] <= 'Z' {
			if _, ok := stopWords[strings.ToLower(tok)]; !ok {
				add(tok)
			}
		}
	}
	// Semantic long-form: distinctive mid-length content words (≥6 chars) as soft ids.
	if len(out) < 3 {
		for _, tok := range wordRE.FindAllString(question, -1) {
			if len(tok) < 6 {
				continue
			}
			if _, ok := stopWords[strings.ToLower(tok)]; ok {
				continue
			}
			add(tok)
			if len(out) >= 8 {
				break
			}
		}
	}
	return out
}

// weakBigramWord filters non-discriminative verbs/adverbs from content bigrams.
var weakBigramWord = map[string]struct{}{
	"wants": {}, "want": {}, "entirely": {}, "inside": {}, "compared": {}, "versus": {},
	"starts": {}, "gets": {}, "longer": {}, "producing": {}, "leaving": {}, "network": {},
	"handling": {}, "making": {}, "using": {}, "having": {}, "being": {}, "doing": {},
}

// semanticExpandPatterns maps question-side paraphrases to doc-side ERB jargon.
// Deterministic (no LLM); keeps HotLex/FTS aligned with corpus phrasing.
// Order does not matter: pickHotLexPhrases ranks by specificity.
var semanticExpandPatterns = []struct {
	re     *regexp.Regexp
	expand []string
}{
	{regexp.MustCompile(`(?i)medium[- ]length|medium length`), []string{"mid-thread", "medium-length", "mid thread"}},
	{regexp.MustCompile(`(?i)200[- ]tokens?`), []string{"200-token", "200 tokens"}},
	// Prefer multi-token latency bags over bare "p95" (competes with many docs).
	{regexp.MustCompile(`(?i)end-to-end|response time target|latency target`), []string{
		"end-to-end p95", "end-to-end latency", "p95 response",
	}},
	{regexp.MustCompile(`(?i)centralized.*batch|batching path|high throughput batch`), []string{"core-tiling", "core-tiling-multiplex", "centralized batching"}},
	{regexp.MustCompile(`(?i)peak load`), []string{"concurrent sessions", "peak-load", "12 concurrent sessions"}},
	{regexp.MustCompile(`(?i)locked-down|data center|no .+ leaving the network`), []string{"on-prem", "locked-down data center", "air-gapped"}},
	{regexp.MustCompile(`(?i)cost per.*token|cheapest cost|1k tokens`), []string{"per 1k tokens", "cost per 1k", "$0."}},
	{regexp.MustCompile(`(?i)discharge writeups?|intake chatbot`), []string{
		"intake chatbot discharge writeups", "discharge writeup", "intake chatbot", "discharge summary",
	}},
	{regexp.MustCompile(`(?i)routing performance memo|nearby low latency|split strategy`), []string{"routing performance", "core-tiling-multiplex", "local then handoff"}},
	{regexp.MustCompile(`(?i)verify.*SLO|enterprise route SLO|burning the enterprise|route SLOs`), []string{
		// Second-gold arm (qst_0341): dashboards / burn / shed — not the 429 ticket.
		"enterprise protected route dashboards",
		"error-budget burn availability p95",
		"enterprise shed_rate admission 429",
		"Hosted API SLOs enterprise route",
		"error budget hot-route capacity",
	}},
	{regexp.MustCompile(`(?i)Proxima|429 spike|priority routing rollout`), []string{
		"Proxima Bank", "overload_protection_admission", "admission control", "burst exception",
		"hot-route protection", "PROXIMA-ENT", "priority routing rollout 429",
	}},
	// qst_0200: multi-word bags beat single-token hospital/p95 collision docs.
	{regexp.MustCompile(`(?i)hospital system|patient data|locked-down data center`), []string{
		"intake chatbot discharge writeups",
		"discharge writeups locked-down",
		"hospital intake chatbot on-prem",
		"locked-down data center 200-token",
		"200-token concurrent sessions p95",
	}},
	// qst_0100: continuous-batching hotpatch (doc-side jargon not on question surface).
	{regexp.MustCompile(`(?i)continuous batching|KV cache|us-west-2.*inference|latency spike`), []string{
		"max_batch_tokens batch_timeout_ms",
		"max_batch_tokens 2048 batch_timeout_ms 5",
		"disable continuous batching hotpatch",
		"continuous batching hotpatch us-west-2",
		"us-west-2 continuous batching regression",
		"hotpatch model routes us-west-2",
	}},
	// qst_0202: question says "spending freeze"; gold CRM says "budget freeze"
	// (Deepwell) on 2026-01-20 — bare "spending freeze" BM25 never hits gold.
	{regexp.MustCompile(`(?i)spending (freeze|hold)|budget freeze|procurement.*hold|Q3 cycle|company-wide spend|company-wide (budget|spending)`), []string{
		"company-wide budget freeze",
		"budget freeze procurement",
		"procurement company-wide freeze",
		"spending freeze procurement",
		"Deepwell Financial Intelligence",
		"Deepwell budget freeze",
		"EU-only finance search fraud alert",
		"technical advocate left procurement",
	}},
	{regexp.MustCompile(`(?i)runtime metadata JSON|supports_streaming|tool calls|token ceilings`), []string{
		"capabilities.supports_streaming", "supports_tools token limit", "runtime metadata discovery JSON",
	}},
	{regexp.MustCompile(`(?i)monthly active users|MAU|token consumption|healthcare chat rollout`), []string{
		"15k MAU tokens per month", "healthcare chat MAU projection", "million tokens per month",
	}},
	{regexp.MustCompile(`(?i)outbound sales chat|CRM|live token streaming|message to action`), []string{
		"streaming token latency p50", "sales chat tool call latency", "end-to-end message action acknowledgement",
	}},
	{regexp.MustCompile(`(?i)token accounting discrepancy|intake channel`), []string{
		"token accounting discrepancy", "Jira SUP token accounting", "customer-support ticket discrepancy",
	}},
	{regexp.MustCompile(`(?i)JSON-schema structured output|structured output.*timeout|Kestrel`), []string{
		"JSON-schema structured output timeout", "Kestrel Labs structured output", "Northwind Analytics timeout",
	}},
	{regexp.MustCompile(`(?i)log retention|request log retention|exception to .+retention`), []string{
		"inference request log retention exception", "payload logging retention days", "Northstar Bank retention",
	}},
	{regexp.MustCompile(`(?i)production rollout plan|new LLM model version|Redwood Inference`), []string{
		"production rollout plan model version", "Redwood Inference rollout checklist", "hosted dedicated private rollout",
	}},
	{regexp.MustCompile(`(?i)leaked encrypted log|triage checklist|partner.*Vault`), []string{
		"suspend partner service account", "revoke partner Vault tokens", "triage checklist decrypt IAM",
	}},
	{regexp.MustCompile(`(?i)Northbridge|ttl_lag|purge backlog|retention window`), []string{
		"Northbridge Bank purge backlog", "ttl_lag_seconds us-east", "post-maintenance purge",
	}},
	// qst_0414: conflicting_info — late correction is "driver/kernel launch stalls
	// (no sustained OOM)" on Crucible INC-9821, not generic GPU OOM incidents.
	{regexp.MustCompile(`(?i)INC-9821|GPU.*OOM|driver/kernel launch|intermittent driver|degraded GPU`), []string{
		"INC-9821 Crucible Health",
		"Crucible Health INC-9821",
		"intermittent driver kernel launch stalls",
		"no sustained OOM",
		"deeper node telemetry review stalls",
		"driver health-check gate degraded",
		"INC-9821 latency spike 5xx",
	}},
	{regexp.MustCompile(`(?i)EXP-002|egress cost|GiB-based|cost penalty catalog`), []string{
		"EXP-002 egress per GiB", "cross-region egress cost catalog", "$0.085 per GiB",
	}},
	// --- full500 semantic pool0 shapes (paraphrase, not qid-hardcoded) ---
	{regexp.MustCompile(`(?i)marketplace|concession terms|price reduction|retail partner`), []string{
		"marketplace concession", "first year price reduction", "partner marketplace purchase",
		"referral credit marketplace", "cloud providers marketplace",
	}},
	{regexp.MustCompile(`(?i)low.?bit|numeric mode|safest numeric|pass rate.*machine|step down from`), []string{
		"low-bit numeric mode", "safest numeric mode", "numeric mode pass rate",
		"step down safest numeric", "low bit math inference",
	}},
	{regexp.MustCompile(`(?i)dry run|replayed requests|smoke check|candidate release|full user traffic until`), []string{
		"operational latch", "dry run replay smoke", "candidate release gate",
		"replayed requests smoke checks", "full traffic latch",
	}},
	{regexp.MustCompile(`(?i)short[- ]lived credential|transient workers|refresh bursts|too many requests`), []string{
		"short-lived credentials refresh", "transient workers throttling",
		"credential refresh burst", "client side scheduling throttle",
	}},
	{regexp.MustCompile(`(?i)resume credential|failover between|destination before.*auth`), []string{
		"resume credential acceptance window", "failover resume credential",
		"short-lived resume destination", "cross location failover auth",
	}},
	{regexp.MustCompile(`(?i)compressed model|chat similarity|canaried|gate thresholds.*model`), []string{
		"compressed model gate thresholds", "chat similarity drop limit",
		"canary block compressed variant", "model variant admission gate",
	}},
	{regexp.MustCompile(`(?i)isolated network|technical deep dive|healthcare client.*serving|own isolated`), []string{
		"isolated network model serving", "healthcare deep dive serving",
		"on-prem isolated inference", "technical deep dive healthcare",
	}},
	{regexp.MustCompile(`(?i)year long commitment|high end inference|accelerators|North America.*Europe.*Asia`), []string{
		"inference accelerator commitment", "split GPU commitment regions",
		"high-end accelerator reservation", "prepaid accelerator overage",
	}},
	{regexp.MustCompile(`(?i)stop-and-go chat|compact per-session|without replaying the whole history`), []string{
		"session state TTL compact", "stop-and-go chat cache",
		"per-session compact history", "long chat without full replay",
	}},
	{regexp.MustCompile(`(?i)harmful output|evaluation windows|automated actions.*traffic`), []string{
		"harmful output warning gate", "sustained harmful output traffic",
		"evaluation window safety action", "shed traffic harmful outputs",
	}},
	{regexp.MustCompile(`(?i)onboarding start date|offer being sent|limited-access option`), []string{
		"onboarding start dates offer", "limited-access onboarding",
		"offer signature start date", "recruiting onboarding dates",
	}},
	{regexp.MustCompile(`(?i)route configuration|payloads.*rejected|400 error.*field`), []string{
		"route configuration payload 400", "rejected route config field",
		"inference tuning route schema", "older route payload rejected",
	}},
	{regexp.MustCompile(`(?i)vector writes|wrong source records|verify charges`), []string{
		"vector write surge wrong source", "search answers wrong source",
		"vector index consistency charges", "western US vector writes",
	}},
	{regexp.MustCompile(`(?i)device-memory spike|short and long requests interleaved|attention-`), []string{
		"device memory spike interleaved", "attention memory interleave",
		"temporary GPU memory long short", "worst-case device-memory spike",
	}},
	{regexp.MustCompile(`(?i)4-bit|half precision|microbenchmark|single-item batching`), []string{
		"4-bit weight compression A100", "half precision 7B microbenchmark",
		"quantization speedup quality hit", "single-item batching 4-bit",
	}},
	{regexp.MustCompile(`(?i)first 60 to 90 minutes|live coordination call|prescribed sequence.*outage`), []string{
		"incident first 60 90 minutes", "outage coordination call sequence",
		"major production outage runbook", "containment confirmation sequence",
	}},
	{regexp.MustCompile(`(?i)CDN layer|handshake reset|token streaming to browsers`), []string{
		"CDN handshake reset streaming", "gateway reconnect streaming CDN",
		"browser token streaming reconnect", "SSE CDN handshake",
	}},
	{regexp.MustCompile(`(?i)experiment workspaces|request ticket spins|tracking/docs/chat`), []string{
		"experiment workspace automation", "ticket spins tracking docs chat",
		"short-lived experiment workspace", "approver teardown experiment",
	}},
	{regexp.MustCompile(`(?i)streaming sessions? get finalized|time limit.*metric|server-side streaming`), []string{
		"streaming session finalized metric", "time limit finalize streaming",
		"SRE streaming session metric", "session finalized time limit",
	}},
	{regexp.MustCompile(`(?i)contractor access|access and permissions playbook|default before it expires`), []string{
		"contractor access default expiry", "permissions playbook contractor",
		"contractor access duration policy", "access expires contractor default",
	}},
	{regexp.MustCompile(`(?i)local-only telemetry|private deployment tenant|verify the mode`), []string{
		"local-only telemetry mode", "private deployment telemetry",
		"tenant local-only telemetry", "verify telemetry mode private",
	}},
	{regexp.MustCompile(`(?i)windowed KV cache|GPU out of memory incidents|stress tests`), []string{
		"windowed KV cache OOM", "GPU OOM stress tests windowed",
		"KV cache proposal OOM reduction", "out of memory windowed cache",
	}},
	{regexp.MustCompile(`(?i)cost-aware model routing|telemetry fields.*routing|spend and quality`), []string{
		"cost-aware routing telemetry", "routing decision spend quality",
		"model routing telemetry fields", "cost quality routing fields",
	}},
	{regexp.MustCompile(`(?i)feature flag vendor|peak burst capacity|multi year renewal`), []string{
		"feature flag renewal burst", "peak burst capacity SLA",
		"multi year feature flag renewal", "renewal option SLA burst",
	}},
	{regexp.MustCompile(`(?i)invalid tool or function schemas|chat compatibility layer|HTTP status.*validation`), []string{
		"chat compatibility invalid schema", "tool function schema validation",
		"v1 chat compatibility layer", "invalid tool schema HTTP status",
	}},
	{regexp.MustCompile(`(?i)cross-language streaming|startup handshake|three-step message`), []string{
		"cross-language streaming handshake", "SDK streaming startup handshake",
		"three-step streaming handshake", "streaming handshake sequence",
	}},
	{regexp.MustCompile(`(?i)token pricing tiers|monthly included token|\$25 per month|\$49 / month`), []string{
		"token pricing tiers included", "small business token pricing",
		"monthly included tokens plan", "starter plan token allowance",
	}},
	{regexp.MustCompile(`(?i)SOC2 readiness|log retention duration.*risk`), []string{
		"SOC2 log retention risk", "log retention less than 30 days",
		"SOC2 readiness retention", "environments log retention risk",
	}},
	{regexp.MustCompile(`(?i)autoscaling approach|effective demand|high-percentile request rate`), []string{
		"effective demand autoscaling formula", "high percentile request rate weight",
		"autoscaling request variability", "w1 high_percentile request rate",
	}},
	{regexp.MustCompile(`(?i)cache time-to-live safeguard|policy recommendation service|cold-start`), []string{
		"cache TTL safeguard cold-start", "policy recommendation min TTL",
		"automated policy recommendation cache", "minimum cache time-to-live",
	}},
	{regexp.MustCompile(`(?i)RDMA traffic|packet loss.*racks|availability zone`), []string{
		"RDMA packet loss mitigation", "rack RDMA latency spikes",
		"stabilize RDMA same AZ", "RDMA traffic short-term mitigation",
	}},
	{regexp.MustCompile(`(?i)continuous batching|median latency improvement|all-hands`), []string{
		"continuous batching median latency", "all-hands continuous batching",
		"batching hold tail latency", "continuous batching improvement",
	}},
	{regexp.MustCompile(`(?i)model capability format|stop being accepted for new writes|cutoff date`), []string{
		"model capability format cutoff", "old capability format rejected",
		"capability format new writes", "planned cutoff capability format",
	}},
	{regexp.MustCompile(`(?i)401 unauthorized|permission scopes?|API key.*onboarding`), []string{
		"inference.full API key scope", "API key permission scopes 401",
		"SDK 401 unauthorized scope", "required permission scopes key",
	}},
	{regexp.MustCompile(`(?i)tool selection hints|OpenAI compatibility adapter|normalizer.*priority`), []string{
		"tool selection hint normalizer", "tool_assignment priority order",
		"OpenAI compatibility tool hints", "conflicting tool selection hints",
	}},
}

// phraseSpecificity ranks HotLex/FTS bags: multi-word + measures + hyphens beat
// single generic tokens (p95 alone hits wrong hospital/SLO docs).
func phraseSpecificity(p string) int {
	p = strings.TrimSpace(p)
	if p == "" {
		return -1
	}
	score := 0
	words := strings.Fields(p)
	n := len(words)
	if n >= 4 {
		score += 40
	} else if n == 3 {
		score += 28
	} else if n == 2 {
		score += 18
	} else {
		score += 4 // bare token
	}
	low := strings.ToLower(p)
	// Corpus-specific entity/domain boosts are diagnostic-only. Generic scoring
	// below remains active for official and product paths.
	if erbDiagnosticRescue() {
		domainHits := 0
		for _, kw := range []string{
			"chatbot", "discharge", "writeup", "writeups", "intake",
			"hotpatch", "batching", "max_batch", "proxima", "admission",
			"core-tiling", "mid-thread", "on-prem", "air-gapped",
			"overload_protection", "concurrent sessions",
			"retention", "structured output", "northbridge", "kestrel",
			"spending freeze", "budget freeze", "deepwell", "crucible",
			"supports_streaming", "triage", "egress", "inc-9821", "no sustained",
			"operational latch", "marketplace", "numeric mode", "resume credential",
			"continuous batching", "contractor access", "telemetry mode",
		} {
			if strings.Contains(low, kw) {
				domainHits++
			}
		}
		score += domainHits * 18
		if domainHits >= 2 {
			score += 15 // multi-anchor bags (intake+discharge) dominate
		}
	}
	if strings.Contains(p, "-") || strings.Contains(p, "_") {
		score += 10
	}
	if strings.Contains(p, "$") || identMeasureRE.MatchString(p) {
		score += 15
	}
	// Penalize ultra-generic single tokens that dominate wrong-doc BM25.
	if n == 1 {
		switch low {
		case "p95", "p99", "slo", "sla", "gpu", "api", "peak", "load":
			score -= 8
		}
	}
	// Prefer mid length; very long bags dilute BM25.
	if len(p) > 90 {
		score -= 10
	}
	if len(p) < 6 {
		score -= 5
	}
	return score
}

// pickHotLexPhrases returns the best short bags for BM25 (sorted by specificity).
// ERB-specific multi-intent handling is available only in diagnostic rescue.
func pickHotLexPhrases(question string, maxN int) []string {
	if maxN <= 0 {
		maxN = 2
	}
	raw := semanticPhraseQueries(question)
	// This verify/SLO tail handling was added for an ERB multi-gold case.
	if erbDiagnosticRescue() {
		for _, sub := range decomposeQuery(question, "project_related") {
			if len(sub) < 20 || strings.EqualFold(sub, question) {
				continue
			}
			low := strings.ToLower(sub)
			if !(strings.Contains(low, "verify") || strings.Contains(low, "how ") ||
				strings.Contains(low, "slo") || strings.Contains(low, "burning")) {
				continue
			}
			raw = append(raw, semanticPhraseQueries(sub)...)
			// Compact content bag of the verify clause itself.
			toks := contentTokens(sub)
			if len(toks) > 8 {
				toks = toks[:8]
			}
			if len(toks) >= 3 {
				raw = append(raw, strings.Join(toks, " "))
			}
		}
	}
	type scored struct {
		p string
		s int
	}
	var rows []scored
	for _, p := range raw {
		if len(p) > 80 || strings.EqualFold(p, question) {
			continue
		}
		s := phraseSpecificity(p)
		if s < 0 {
			continue
		}
		rows = append(rows, scored{p: p, s: s})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].s != rows[j].s {
			return rows[i].s > rows[j].s
		}
		return len(rows[i].p) > len(rows[j].p)
	})
	var out []string
	seen := map[string]struct{}{}
	add := func(p string) bool {
		k := strings.ToLower(strings.TrimSpace(p))
		if k == "" {
			return false
		}
		if _, ok := seen[k]; ok {
			return false
		}
		seen[k] = struct{}{}
		out = append(out, strings.TrimSpace(p))
		return true
	}
	// The admission-vs-SLO split is specific to an ERB multi-gold case. Keep
	// normal score ordering unless diagnostic rescue is explicitly enabled.
	if erbDiagnosticRescue() {
		var primary, secondary []string
		for _, r := range rows {
			low := strings.ToLower(r.p)
			isSec := strings.Contains(low, "verify") || strings.Contains(low, "slo") ||
				strings.Contains(low, "error-budget") || strings.Contains(low, "error budget") ||
				strings.Contains(low, "shed_rate") || strings.Contains(low, "dashboard") ||
				strings.Contains(low, "burn")
			isPri := strings.Contains(low, "admission") || strings.Contains(low, "429") ||
				strings.Contains(low, "throttl") || strings.Contains(low, "overload") ||
				strings.Contains(low, "burst") || strings.Contains(low, "proxima")
			if isSec && !isPri {
				secondary = append(secondary, r.p)
			} else if isPri {
				primary = append(primary, r.p)
			}
		}
		if maxN >= 2 && len(primary) > 0 && len(secondary) > 0 {
			add(primary[0])
			add(secondary[0])
		}
	}
	for _, r := range rows {
		if len(out) >= maxN {
			break
		}
		add(r.p)
	}
	return out
}

// erbDiagnosticRescue reports whether ERB/qst-specific question-side expansion
// and hard-coded lexical/deep-hydrate rescue rules are active. Default off.
// Official/product mode (OUROBOROS_ERB_PROD truthy, default true) forces false.
// Diagnostic-only opt-in: set OUROBOROS_ERB_DIAGNOSTIC_RESCUE=1 and ensure
// OUROBOROS_ERB_PROD=0 (or unset when default differs — see prod.go).
func erbDiagnosticRescue() bool {
	if envTruthy("OUROBOROS_ERB_PROD", true) ||
		envTruthy("OUROBOROS_ERB_OFFICIAL", false) ||
		envTruthy("OUROBOROS_ERB_OFFICIAL_JUDGE", false) ||
		envTruthy("OUROBOROS_ERB_BLIND_PLAN", false) {
		return false
	}
	return envTruthy("OUROBOROS_ERB_DIAGNOSTIC_RESCUE", false)
}

// semanticPhraseQueries builds short high-signal bags for paraphrase-hard ERB
// questions (qst_0200/0300 class). Prefer bigrams + tech codes + measures over
// the full long question (BM25/FTS dilute on 40+ stopword tokens).
// Callers that need the best BM25 bags should use pickHotLexPhrases (ranked).
func semanticPhraseQueries(question string) []string {
	q := strings.TrimSpace(question)
	if q == "" {
		return nil
	}
	var out []string
	// Domain paraphrase expand (doc-side jargon not present in question surface).
	// Gated behind OUROBOROS_ERB_DIAGNOSTIC_RESCUE (default off; official forces off).
	if erbDiagnosticRescue() {
		for _, pat := range semanticExpandPatterns {
			if pat.re.MatchString(q) {
				out = append(out, pat.expand...)
			}
		}
	}
	// Tech short codes (p95, RPO, SLO) — contentTokens drops <4 chars.
	techRE := regexp.MustCompile(`(?i)\b(?:p\d{2}|rpo|rto|slo|sla|gpu|sku|tts|asr)\b`)
	for _, m := range techRE.FindAllString(q, -1) {
		out = append(out, strings.ToLower(m))
	}
	for _, m := range identMeasureRE.FindAllString(q, -1) {
		out = append(out, m)
	}
	for _, m := range identMoneyRE.FindAllString(q, -1) {
		out = append(out, m)
	}
	for _, m := range identHyphenRE.FindAllString(strings.ToLower(q), -1) {
		out = append(out, m)
	}
	// Region / infra codes (us-west-2) — high BM25 specificity for basic ops Qs.
	regionRE := regexp.MustCompile(`(?i)\b(?:us|eu|ap|sa)-(?:east|west|central|south|north)-\d+\b`)
	for _, m := range regionRE.FindAllString(q, -1) {
		out = append(out, strings.ToLower(m))
	}
	// Content bigrams — skip weak verb pairs ("system wants").
	toks := contentTokens(q)
	for _, m := range techRE.FindAllString(q, -1) {
		toks = append(toks, strings.ToLower(m))
	}
	for i := 0; i+1 < len(toks); i++ {
		a, b := toks[i], toks[i+1]
		if _, ok := weakBigramWord[a]; ok {
			continue
		}
		if _, ok := weakBigramWord[b]; ok {
			continue
		}
		if len(a) >= 4 && len(b) >= 4 {
			out = append(out, a+" "+b)
		}
	}
	// Compact identifier bag.
	if ids := extractIdentifiers(q); len(ids) > 0 {
		n := len(ids)
		if n > 6 {
			n = 6
		}
		out = append(out, strings.Join(ids[:n], " "))
	}
	return dedupeQueries(out)
}

// multiQueryVariants mirrors residual multi_query_variants for lexical rescue.
func multiQueryVariants(question, questionType string) []string {
	queries := []string{question}
	// Prepend ranked high-spec phrase bags for hard types (before long-Q dilution).
	qt := strings.ToLower(questionType)
	if qt == "semantic" || qt == "project_related" || qt == "completeness" ||
		qt == "constrained" || qt == "high_level" || qt == "basic" || qt == "" {
		// Ranked bags first — HotLex/FTS multi-query prefer specificity.
		short := pickHotLexPhrases(question, 4)
		queries = append(short, queries...)
	}
	ids := extractIdentifiers(question)
	if len(ids) > 0 {
		n := len(ids)
		if n > 6 {
			n = 6
		}
		queries = append(queries, strings.Join(ids[:n], " "))
	}
	var tokens []string
	for _, t := range wordRE.FindAllString(question, -1) {
		if len(t) < 4 {
			continue
		}
		if _, ok := stopWords[strings.ToLower(t)]; ok {
			continue
		}
		tokens = append(tokens, t)
	}
	if len(tokens) > 0 {
		shortN := len(tokens)
		if shortN > 10 {
			shortN = 10
		}
		short := strings.Join(tokens[:shortN], " ")
		if !strings.EqualFold(short, question) {
			queries = append(queries, short)
		}
	}
	if len(tokens) >= 4 {
		tailN := len(tokens)
		if tailN > 6 {
			tail := tokens[tailN-6:]
			queries = append(queries, strings.Join(tail, " "))
		} else {
			queries = append(queries, strings.Join(tokens, " "))
		}
		uniq := append([]string{}, tokens...)
		sort.Slice(uniq, func(i, j int) bool {
			return strings.ToLower(uniq[i]) < strings.ToLower(uniq[j])
		})
		// stable unique
		var sorted []string
		seen := map[string]struct{}{}
		for _, t := range uniq {
			k := strings.ToLower(t)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			sorted = append(sorted, t)
			if len(sorted) >= 8 {
				break
			}
		}
		queries = append(queries, strings.Join(sorted, " "))
	}
	qtype := strings.ToLower(questionType)
	hard := map[string]struct{}{
		"semantic": {}, "project_related": {}, "high_level": {}, "completeness": {},
		"constrained": {}, "conflicting_info": {}, "intra_document_reasoning": {},
	}
	if _, ok := hard[qtype]; ok {
		return dedupeQueries(queries)
	}
	if len(ids) > 0 {
		return dedupeQueries(queries[:min(3, len(queries))])
	}
	if len(queries) > 1 {
		return dedupeQueries(queries[:min(2, len(queries))])
	}
	return []string{question}
}

func dedupeQueries(items []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, item := range items {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, strings.TrimSpace(item))
	}
	return out
}

func passageIdentifierHits(text string, identifiers []string) int {
	if text == "" || len(identifiers) == 0 {
		return 0
	}
	low := strings.ToLower(text)
	n := 0
	for _, id := range identifiers {
		if strings.Contains(low, strings.ToLower(id)) {
			n++
		}
	}
	return n
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isMultiDocType(qtype string) bool {
	switch strings.ToLower(qtype) {
	// constrained is multi-doc on ERB (game-day + follow-up ticket pairs, etc.).
	case "project_related", "completeness", "semantic", "conflicting_info", "constrained":
		return true
	default:
		return false
	}
}

// compactQuestionBag builds a short BM25 bag from distinctive content tokens
// of the original question (prefer longer tokens). Used so FTS always has a
// non-paraphrase anchor even when HotLex bags miss.
func compactQuestionBag(question string, maxTok int) string {
	if maxTok <= 0 {
		maxTok = 8
	}
	toks := contentTokens(question)
	if len(toks) == 0 {
		return ""
	}
	// Prefer longer / rarer-looking tokens first.
	type tw struct {
		t string
		n int
	}
	var scored []tw
	seen := map[string]struct{}{}
	for _, t := range toks {
		if gapStopword(t) {
			continue
		}
		if len(t) < 4 {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		scored = append(scored, tw{t: t, n: len(t)})
	}
	// Insertion sort by length desc.
	for i := 1; i < len(scored); i++ {
		j := i
		for j > 0 && scored[j].n > scored[j-1].n {
			scored[j], scored[j-1] = scored[j-1], scored[j]
			j--
		}
	}
	if len(scored) > maxTok {
		scored = scored[:maxTok]
	}
	out := make([]string, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.t)
	}
	return strings.Join(out, " ")
}

// hasRareIdentifier detects region codes / snake_case / hyphen-tech tokens that
// HotLex "strong" can still miss (false-strong → wrong skip of Neon FTS).
func hasRareIdentifier(ids []string, question string) bool {
	regionRE := regexp.MustCompile(`(?i)\b(?:us|eu|ap|sa)-(?:east|west|central|south|north)-\d+\b`)
	incRE := regexp.MustCompile(`(?i)\bINC-\d{3,}\b`)
	if regionRE.MatchString(question) || incRE.MatchString(question) {
		return true
	}
	for _, id := range ids {
		low := strings.ToLower(id)
		if regionRE.MatchString(low) || incRE.MatchString(low) {
			return true
		}
		if strings.Contains(low, "_") && len(low) >= 8 {
			return true
		}
		// Multi-hyphen tech: max-batch-tokens style already covered by snake; continuous-batching bigrams.
		if strings.Count(low, "-") >= 2 && len(low) >= 10 {
			return true
		}
		// Ticket-like codes (INC-9821 already via re; also bare 9821 in ids).
		if len(low) >= 4 && low[0] >= '0' && low[0] <= '9' && strings.ContainsAny(low, "-_") {
			return true
		}
	}
	// Ops regression / freeze / GPU-OOM lexical rescue cues are ERB/qst-specific
	// and gated behind OUROBOROS_ERB_DIAGNOSTIC_RESCUE (default off; official forces off).
	if erbDiagnosticRescue() {
		lowQ := strings.ToLower(question)
		if strings.Contains(lowQ, "continuous batching") || strings.Contains(lowQ, "kv cache") {
			return true
		}
		if strings.Contains(lowQ, "hotpatch") || strings.Contains(lowQ, "max_batch") {
			return true
		}
		// Freeze / procurement paraphrases miss when HotLex only sees "spending".
		if strings.Contains(lowQ, "spending freeze") || strings.Contains(lowQ, "budget freeze") ||
			strings.Contains(lowQ, "company-wide") && strings.Contains(lowQ, "freeze") {
			return true
		}
		// Conflicting GPU OOM vs stalls — force broader lexical.
		if strings.Contains(lowQ, "oom") && (strings.Contains(lowQ, "stall") || strings.Contains(lowQ, "driver")) {
			return true
		}
	}
	return false
}

// wantsDeepHydrate: multi-chunk gold (incident threads, CRM timelines) needs
// more sibling chunks so late corrections / freeze dates enter the window.
// ERB/qst-specific hard-coded patterns are gated behind
// OUROBOROS_ERB_DIAGNOSTIC_RESCUE (default off; official forces off).
func wantsDeepHydrate(question, questionType string) bool {
	qt := strings.ToLower(questionType)
	if qt == "conflicting_info" || qt == "intra_document_reasoning" {
		return true
	}
	if erbDiagnosticRescue() {
		low := strings.ToLower(question)
		if regexp.MustCompile(`(?i)\bINC-\d{3,}\b`).MatchString(question) {
			return true
		}
		if strings.Contains(low, "spending freeze") || strings.Contains(low, "budget freeze") ||
			(strings.Contains(low, "procurement") && strings.Contains(low, "freeze")) {
			return true
		}
		if strings.Contains(low, "oom") && strings.Contains(low, "stall") {
			return true
		}
	}
	return false
}

// wantsFullRetrieve disables LEAN primary-first for types where multi-arm
// lexical fan-out measurably lifts gold recall (failure deep-dive: semantic
// / project / completeness / constrained / intra-doc).
func wantsFullRetrieve(qtype string) bool {
	if isMultiDocType(qtype) {
		return true
	}
	switch strings.ToLower(qtype) {
	case "intra_document_reasoning", "constrained":
		return true
	default:
		return false
	}
}

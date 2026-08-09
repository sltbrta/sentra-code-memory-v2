# Issue #376 — Remove answer-token truncation and improve rendered answer formatting

## Problem

The hosted answer stack was shipping an artificial `max_tokens` cap on every
OpenAI-compatible provider payload (synthesis: none after the fix; gateway
query provider: hard-coded `2048`), forcing long, multi-fact answers and the
direct-answer lead-in to truncate before the model finished. The Modal web
UI was also rendering the raw answer string as `textContent`, so the
synthesizer's Markdown (bold, code, lists) showed up as literal markers in
the page, while the same answer rendered correctly elsewhere. A gold-tier
MedThink RPO smoke case (`What is the MedThink gold-tier RPO?`)
previously recorded a model-prior answer when retrieval was empty, while the
older smoke assertion did not require the supported value and a citation.

## Contract

### 1. Provider payloads (no artificial cap by default)

- The ERB OpenAI-compatible synthesis body (`callSynthCandidate` in
  `services/brain/internal/hosted/provider_resilience.go`) MUST NOT include
  `max_tokens` unless an explicit caller override opts in. The bounded
  response-size guard (1 MiB), the system prompt's claim/prose caps, and the
  grounded-claim verifier remain in force. Anthropic-specific paths still
  require `max_tokens`; the current chain has none.
- The gateway OpenAI-compatible request (`openAIClient.Complete` in
  `services/gateway/authorityprocess/query_provider.go`) MUST omit
  `max_tokens` by default. The new `OUROBOROS_OPENAI_MAX_TOKENS` env var
  opts a positive value in (clamped to a safe ceiling of 8192) when an
  operator needs the legacy bound. `response_format: json_object` is
  preserved as the strict-JSON contract.
- Internal `max_tokens` on ancillary calls (`llm_multiquery.go: 180`,
  `query_plan_llm.go: 120`) stay as explicit caller overrides — they cap
  the bounded ancillary responses, not the primary synthesis or query
  provider.

### 2. Prompt formatting guidance (issue #376 system prompt contract)

- `buildSystemPromptOpts` always includes a new `answerFormatGuidance`
  block that asks the model to:
  - Lead with the direct answer in the first sentence (no "Based on the
    documents…" preamble).
  - Use clean Markdown paragraphs and a numbered or bulleted list for
    multi-fact answers; one fact per list item.
  - Allow inline code with backticks for short identifiers/numbers and
    `**…**` / `*…*` for bold/italic.
  - NEVER include raw citation markers (`[doc-id]`, `(doc-id)`, `[1]`,
    `[^1]`), fenced code blocks, headings starting with `#`, raw HTML,
    links, or horizontal rules.
  - NEVER introduce facts, numbers, names, or dates that the supplied
    documents do not support.
- The guidance is layered between `packDiscipline` and `jsonSchema` so the
  JSON contract, fail-closed citation rules, and the false-abstain rule
  remain authoritative.

### 3. Modal web UI Markdown rendering (safe subset)

- `site/app.js` adds a `parseMarkdownToTokens` + `renderMarkdownSubset`
  pair used by both `renderAnswer` (single ask) and `renderBatchResults`
  (batch results). The supported subset is headings (`#`/`##`/`###` →
  `h2`..`h4`), bulleted lists (`-`/`*`/`+`), numbered lists (`1.`, `2.`),
  paragraphs (blank-line split, `<br>` for soft line breaks), and inline
  `**bold**` / `*italic*` / `` `code` `` spans.
- The renderer strips fenced code blocks, known dangerous tags, and
  letter-prefixed generic HTML tags while preserving ordinary comparison
  prose such as `latency < 200 ms`; it also removes bracketed link syntax
  and bare autolinks. It emits the remaining
  text only through `textContent` / `createTextNode`; no path calls
  `innerHTML` with model output. It does not pre-escape text before sending
  it to these sinks, avoiding literal `&amp;`/`&#39;` output.
- `initTabs`, `initAsk`, `initUpload`, `initHistory`, and `initFooter`
  only run when `document` is defined so the file is safely `require`-able
  from a Node test harness.

### 4. MedThink gold-tier RPO evidence / diagnostic

- The gold-tier RPO for the MedThink failover procedure is **15 minutes**.
- The prior regression presented as a silent green: an empty retrieval pack
  allowed a provider prior to produce a plausible RPO answer, while the
  earlier smoke assertion only required a non-empty answer. The current smoke
  harness requires the supported value, at least one citation, and no failure;
  grounded-claim verification remains fail-closed for the product path.
- This spec MUST NOT hardcode a corpus answer inside the brain: the answer text
  remains corpus-driven, and only the smoke fixture/assertion records the
  expected value and citation requirement. Gold-free behavior is preserved.
- The MedThink gold-tier RPO smoke is implemented in
  `deploy/modal/company-brain-web/smoke_web_all.py` (upload fixture and
  `/api/query` assertion). The ERB fixture independently records the same
  recovery target for the benchmark corpus.

## Diagnostics

- `provider_max_tokens` is not stamped: the field is simply absent. Receipt
  audit readers can detect the legacy `2048` literal and flag any future
  reintroduction.
- The gateway's `OUROBOROS_OPENAI_MAX_TOKENS` override, when set, is
  included only in the outbound request; the bounded response guard still
  applies.
- The system prompt's `answerFormatGuidance` block is part of the prompt
  digest in any future prompt-receipt capture.

## Authorization

The default omission of `max_tokens` is a fail-closed change: a model that
emits more than `openAIMaxResponseBytes` bytes or produces an unparsable
proposal is still rejected, the prompt's prose cap (4000 chars) still
fires, and the grounded-claim verifier still rejects empty citation sets.
A caller that needs the legacy bound can opt in with
`OUROBOROS_OPENAI_MAX_TOKENS` without weakening any fail-closed check.

## Tests

- `services/brain/internal/hosted/provider_resilience_test.go` —
  `TestSynthRequestBodyOmitsMaxTokensForOpenAICompatible`: asserts the
  ERB synthesis body omits `max_tokens` and keeps `response_format`.
- `services/brain/internal/hosted/prompt_test.go` —
  `TestBuildSystemPromptIncludesAnswerFormatGuidance` and
  `TestBuildSystemPromptAppliesFormatAndModePreamble`: assert the
  formatting guidance is present, forbids raw markers, and coexists with
  the source-mode preamble.
- `services/gateway/authorityprocess/query_adapter_test.go` —
  `TestOpenAIClientCompletesStrictProposal` (extended): asserts the
  default request body omits `max_tokens` and keeps the strict-JSON
  `response_format`.
- `services/gateway/authorityprocess/query_adapter_test.go` —
  `TestOpenAIClientMaxTokensOptIn`: asserts that
  `OUROBOROS_OPENAI_MAX_TOKENS` opts the field in and clamps to the safe
  ceiling.
- `services/gateway/authorityprocess/query_adapter_test.go` —
  `TestQuerySynthesizerFromEnvSelectsAdaptersFailClosed` and
  `TestOpenAIMaxTokensEnvFailsClosed`: assert malformed
  `OUROBOROS_OPENAI_MAX_TOKENS` values fail configuration closed.
- `deploy/modal/company-brain-web/app/test_markdown_render.py` — Node
  subprocess tests with a minimal DOM stub for `renderMarkdownSubset`,
  `parseMarkdownToTokens`, and `sanitizeForMarkdown`. Asserts rendered
  structure/text, headings, lists, inline spans, paragraph splitting,
  comparison operators, and that raw HTML, scripts, fenced code, links, and
  autolinks never reach the rendered output.

# llmadapter — optional provider-neutral LLM seam

Bounded, structured LLM operations for query expansion, semantic candidate
scoring, and memory claim extraction (issues #55/#58/#60). This package is not
wired into `product-brain`, codeserve, or another product path. Setting
`GEMINI_API_KEY` alone therefore changes no product behavior; a caller must
explicitly construct and invoke the adapter. When invoked without a configured
generator, every operation returns a deterministic local fallback (token-based
expansion, lexical scoring, claim abstention).

## Configuration

| Setting | Default | Notes |
| --- | --- | --- |
| `GEMINI_API_KEY` | unset | Read only when a caller constructs the adapter. Never logged or persisted. |
| `SENTRA_CODE_MEMORY_GEMINI_MODEL` | `gemini-3.6-flash` | Override for tests/operations. |

## Usage

```go
cfg := llmadapter.ConfigFromEnv()
gen, err := llmadapter.NewGeminiGenerator(ctx, cfg) // llmadapter.ErrNoAPIKey when unset
if err != nil && !errors.Is(err, llmadapter.ErrNoAPIKey) {
  // treat as "stay deterministic"
  gen = nil
}
svc := llmadapter.New(cfg, gen) // gen may be nil

queries, diag := svc.ExpandQuery(ctx, "ranked retrieval")
scores, diag := svc.ScoreCandidates(ctx, query, candidates)
claims, diag := svc.ExtractClaims(ctx, docID, text)
```

Every call returns `Diagnostics` (`llm_used`, provider, model, bounded
candidate count, fallback reason) with no prompt or source content. Any
transport, parse, policy, or deadline failure returns the deterministic
fallback with `fallback_reason` set; a failing provider never fails a local
code operation.

## Safety contract

- Caller deadlines are enforced; the per-call budget is clamped to the
  caller's remaining deadline minus a reserve, and there are no automatic
  retries.
- Inputs are redacted (credentials, bearer tokens, absolute paths) and
  byte-bounded before transmission; only the bounded candidate set is sent.
- Outputs are token/byte bounded and strictly schema-validated (`unknown
  fields rejected`, scores clamped to `[0,1]`, counts capped).
- The Gemini implementation uses the official Google Go SDK
  (`google.golang.org/genai`) with structured response schemas and **no
  tools**: the model proposes queries, scores, and claims; deterministic local
  code validates and applies anything that mutates state. Function calls in a
  response are rejected.

## Tests

All tests use injected fakes (`Generator` and SDK-level `generateAPI` seams).
No live API key or network access is required.

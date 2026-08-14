// Package llmadapter provides a narrow, provider-neutral, optional LLM seam
// for bounded structured operations: query expansion, semantic candidate
// scoring, and memory claim extraction (issues #55/#58/#60).
//
// The adapter is strictly opt-in and local-first: with no configured
// Generator (no GEMINI_API_KEY), every operation returns a deterministic
// local fallback. Provider, transport, parse, and policy failures also return
// the deterministic fallback with a safe diagnostic reason — a failing LLM
// never fails a local code operation.
//
// Safety contract:
//   - caller deadlines are enforced and never exceeded by retries (there are
//     no automatic retries);
//   - inputs and outputs are byte/token bounded;
//   - credentials, absolute paths, and bearer tokens are redacted before
//     transmission; only the bounded candidate set is ever sent;
//   - responses are strictly schema-validated before use;
//   - diagnostics carry counts and reason codes only, never prompt or source
//     content;
//   - the model is given no tools: it may propose queries, scores, and claim
//     data, while deterministic local code validates and applies anything
//     that mutates state.
package llmadapter

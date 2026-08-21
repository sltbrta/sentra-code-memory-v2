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
//     that mutates state;
//   - content the caller did not author -- a document, a query, a retrieved
//     passage -- is fenced in a per-call randomised block and the instruction
//     says that region is data. Every prompt here used to concatenate that
//     content straight into the instruction, which made a document containing
//     "ignore the above" structurally indistinguishable from the operator.
//     Framing is not immunity; it removes the part that was the caller's
//     fault. See untrusted.go.
//
// # Where this is used
//
// One consumer, off by default: query expansion in the retrieval path, behind
// OUROBOROS_BRAIN_LLMADAPTER_EXPAND=1 (hosted/llmadapter_expansion.go).
// Candidate scoring and claim extraction have no consumer yet.
//
// The package had no non-test importer at all until 2026-08-21, while this
// doc comment read as though it were in use. That is worth stating plainly:
// a package described in the present tense and called by nothing is a
// capability a reader believes the product has.
package llmadapter

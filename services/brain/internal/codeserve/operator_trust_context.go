package codeserve

import "context"

// Operator trust travels on the context, never in the request.
//
// The gate previously lived only in the HTTP and MCP adapters. Handle, the one
// dispatch entry point every surface funnels through, did not inspect it — so
// the JSONL `serve` loop, which calls Handle directly, had no gate at all. That
// is the surface the README tells coding agents to keep warm, and the
// code_apply_changeset verb reaches an external command through it.
//
// Putting the flag on the context rather than in the Request map is the whole
// point. A Request is authored by the caller; on a model-facing surface that
// caller is the model. A permission a caller can write into its own request is
// not a boundary. The context is set by the surface at start-up from something
// the model cannot reach: a CLI flag, an environment variable, or an HTTP
// header supplied by the operator's own client.

type operatorTrustKey struct{}

// WithOperatorTrust marks ctx as carrying an explicit operator opt-in for
// mutating verbs. Surfaces call it only from an out-of-band signal.
func WithOperatorTrust(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operatorTrustKey{}, true)
}

// HasOperatorTrust reports whether ctx carries the operator opt-in.
func HasOperatorTrust(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	granted, _ := ctx.Value(operatorTrustKey{}).(bool)
	return granted
}

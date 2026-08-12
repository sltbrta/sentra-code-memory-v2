// Package adapters exposes the canonical codeserve contract over local HTTP
// and MCP stdio (Phase 5, issue #35). Both transports reuse codeserve.Handle
// for identical behavior to the JSONL/CLI path: the same request map produces
// the same response map regardless of surface.
//
// The package adds no dependencies (Go stdlib + codeserve only) and never
// changes the existing JSONL or direct CLI behavior. Requests are bounded and
// errors are structured (codeserve ErrorResponse) so the three surfaces remain
// equivalent and machine-readable.
package adapters

import (
	"context"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// MaxRequestBytes bounds a single request body/line across HTTP and MCP so a
// runaway client cannot exhaust the process. It matches the CLI serve limit.
const MaxRequestBytes = 8 << 20

// dispatch invokes codeserve.Handle. Centralized so both transports share one
// behavior and one structured-error path; callers pass a request-scoped ctx.
func dispatch(ctx context.Context, req codeserve.Request) codeserve.Response {
	return codeserve.Handle(ctx, req)
}

// ServerName identifies the local adapter process for health/MCP metadata.
const ServerName = "sentra-code-memory"

// ServerVersion mirrors the codeserve contract id for surface metadata.
const ServerVersion = codeserve.ContractID

package adapters

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// HTTPConfig configures the local HTTP adapter.
type HTTPConfig struct {
	// Addr is the listen address (e.g. "127.0.0.1:0" for any free loopback port).
	Addr string
	// Timeout bounds a single dispatch (0 means caller lifetime).
	Timeout time.Duration
	// Token, when set, requires "Authorization: Bearer <token>" on every
	// endpoint (issue #41 explicit local trust).
	Token string
	// OperatorTrust grants every dispatch on this listener the operator opt-in,
	// so mutating verbs are admitted without a per-request header. It is set
	// from a process-level flag or environment variable, never from a request.
	OperatorTrust bool
	// RootPin confines every request to this subtree. Empty serves any root,
	// which is what the CLI does only when the operator passes --root=/.
	RootPin string
}

// Operator opt-in surface constants. When a verb carries
// VerbSpec.RequiresOperatorTrust (issue #63), its mutating actions
// (hooks install/uninstall/run, changeset apply) are refused at the
// /dispatch boundary unless
// the request carries the opt-in via header or query parameter. This
// keeps host-state-mutating verbs inaccessible to model-controlled
// callers by default while preserving status-like read-only actions
// and the direct CLI path.
const (
	operatorTrustHeaderName = "X-Sentra-Operator-Trust"
	operatorTrustQueryKey   = "operator_trust"
	operatorTrustOptInValue = "1"
)

// NewHTTP returns an http.Handler that exposes health and dispatch endpoints
// over codeserve.Handle. It is the canonical local HTTP surface (issue #35):
// requests are bounded, errors are structured, and behavior matches JSONL/CLI.
// The trust boundary is explicit (issue #41): callers are expected to bind
// loopback (validated by ValidateListenAddr in the CLI) and may require a
// bearer token via HTTPConfig.Token. Mutating actions on
// VerbSpec.RequiresOperatorTrust verbs are gated by
// codeserve.IsOperatorTrustAction and require an explicit operator opt-in
// (issue #63); see operatorTrustOptInValue.
func NewHTTP(cfg HTTPConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/dispatch", dispatchHandler(cfg.Timeout, cfg.OperatorTrust, cfg.RootPin))
	return bounded(TrustPolicy{Token: cfg.Token}.wrap(mux))
}

// requestHasOperatorTrustOptIn reports whether the HTTP request carries
// the explicit operator opt-in via header (X-Sentra-Operator-Trust: 1)
// or query parameter (?operator_trust=1). It returns true the moment it
// finds either signal; both forms exist because some caller shells and
// some MCP-to-HTTP bridges can only set one of them. The value must be
// exactly "1" to keep the signal explicit and grep-friendly.
func requestHasOperatorTrustOptIn(r *http.Request) bool {
	if got := strings.TrimSpace(r.Header.Get(operatorTrustHeaderName)); got == operatorTrustOptInValue {
		return true
	}
	if got := strings.TrimSpace(r.URL.Query().Get(operatorTrustQueryKey)); got == operatorTrustOptInValue {
		return true
	}
	return false
}

// healthHandler reports liveness and the active contract id.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{
		"ok":       true,
		"service":  ServerName,
		"contract": ServerVersion,
		"verbs":    codeserve.Catalog(),
	})
}

// dispatchHandler dispatches one codeserve request map.
func dispatchHandler(timeout time.Duration, processTrust bool, rootPin string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONStatus(w, http.StatusMethodNotAllowed, structuredErr(
				"dispatch", "POST required", http.StatusMethodNotAllowed))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			writeJSONStatus(w, http.StatusForbidden, structuredErr("dispatch", "cross-origin requests are forbidden", http.StatusForbidden))
			return
		}
		contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
		if contentType != "" && contentType != "application/json" {
			writeJSONStatus(w, http.StatusUnsupportedMediaType, structuredErr("dispatch", "application/json required", http.StatusUnsupportedMediaType))
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, structuredErr(
				"dispatch", "request exceeds bound", http.StatusRequestEntityTooLarge))
			return
		}
		var req codeserve.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, codeserve.Response{
				"ok": false, "verb": "dispatch", "error": "invalid JSON: " + err.Error(),
				"error_code": string(codeserve.ErrInvalidRequest), "product_owned": true,
			})
			return
		}
		if req == nil {
			writeJSONStatus(w, http.StatusBadRequest, codeserve.Response{
				"ok": false, "verb": "dispatch", "error": "request must be a JSON object",
				"error_code": string(codeserve.ErrInvalidRequest), "product_owned": true,
			})
			return
		}
		// Operator-trust gate (issue #63): mutating actions on verbs marked
		// VerbSpec.RequiresOperatorTrust are refused on /dispatch unless the
		// caller explicitly opts in. Status and other read-only actions are
		// passed through unchanged. codeserve owns the verb/action decision
		// so the surface here stays one structured-error response.
		optedIn := processTrust || requestHasOperatorTrustOptIn(r)
		if codeserve.IsOperatorTrustAction(stringField(req, "verb"), stringField(req, "action")) {
			if !optedIn {
				// Refused here as well as in Handle so the HTTP surface can
				// answer 403 rather than a 200 carrying ok:false. Handle's gate
				// is the boundary; this one is the status code.
				writeJSONStatus(w, http.StatusForbidden, codeserve.OperatorTrustError(
					stringField(req, "verb"), stringField(req, "action"), "http"))
				return
			}
		}
		ctx := r.Context()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		if optedIn {
			// The opt-in arrives out of band -- a header or query parameter set
			// by the operator's own client, never a field inside the JSON body
			// the model authored.
			ctx = codeserve.WithOperatorTrust(ctx)
		}
		ctx = codeserve.WithRootPin(ctx, rootPin)
		resp := dispatch(ctx, req)
		// 200 even on codeserve-level errors: the response carries ok:false and
		// a structured error_code, matching JSONL semantics.
		writeJSONStatus(w, http.StatusOK, resp)
	}
}

// bounded wraps h with a hard request size limit on the raw body so even
// malformed/missing Content-Length cannot exceed MaxRequestBytes.
func bounded(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		h.ServeHTTP(w, r)
	})
}

func structuredErr(verb, msg string, status int) codeserve.Response {
	return codeserve.Response{
		"ok": false, "verb": verb, "error": msg,
		"error_code": string(codeserve.ErrInvalidRequest), "product_owned": true,
		"http_status": status,
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// stringField reads a string field from a codeserve request map with the
// same trimming rules as codeserve.str, so adapter-level and
// handler-level reads agree on what an "empty" verb or action is. The
// trust gate runs before Handle so the same normalization matters.
func stringField(req codeserve.Request, key string) string {
	v, _ := req[key].(string)
	return strings.TrimSpace(v)
}

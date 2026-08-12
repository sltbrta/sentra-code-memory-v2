package adapters

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// HTTPConfig configures the local HTTP adapter.
type HTTPConfig struct {
	// Addr is the listen address (e.g. "127.0.0.1:0" for any free loopback port).
	Addr string
	// Timeout bounds a single dispatch (0 means caller lifetime).
	Timeout time.Duration
}

// NewHTTP returns an http.Handler that exposes health and dispatch endpoints
// over codeserve.Handle. It is the canonical local HTTP surface (issue #35):
// requests are bounded, errors are structured, and behavior matches JSONL/CLI.
func NewHTTP(cfg HTTPConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/dispatch", dispatchHandler(cfg.Timeout))
	return bounded(mux)
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
func dispatchHandler(timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONStatus(w, http.StatusMethodNotAllowed, structuredErr(
				"dispatch", "POST required", http.StatusMethodNotAllowed))
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
		ctx := r.Context()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
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

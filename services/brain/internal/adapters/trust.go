package adapters

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/codeserve"
)

// TrustPolicy makes the local HTTP trust boundary explicit (issue #41).
//
// The safe default is loopback-only with no token: only same-host processes
// can reach the listener. Binding a non-loopback address without a bearer
// token is refused at startup (fail closed) unless AllowInsecure is set as an
// explicit opt-out. When Token is set, every endpoint — including /health —
// requires "Authorization: Bearer <token>" and failures return a structured
// 401 (codeserve "unauthorized" error code).
//
// The MCP stdio adapter has no network surface: its trust boundary is the
// spawning parent process, which owns the child's stdin/stdout. It therefore
// carries no token machinery; this policy is HTTP-only.
type TrustPolicy struct {
	// Token, when non-empty, is the required bearer credential.
	Token string
	// AllowInsecure permits a non-loopback bind without a token. It exists
	// only as an explicit escape hatch and is never the default.
	AllowInsecure bool
}

// authorized reports whether r carries the configured bearer credential.
// With no token configured the policy is loopback-only and every request is
// authorized at this layer (the bind check is the gate).
func (p TrustPolicy) authorized(r *http.Request) bool {
	if p.Token == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.Token)) == 1
}

// wrap enforces the bearer requirement in front of every endpoint when a
// token is configured. Unauthorized requests get a structured 401 that keeps
// the codeserve error envelope so clients can branch on error_code.
func (p TrustPolicy) wrap(next http.Handler) http.Handler {
	if p.Token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sentra-code-memory"`)
			writeJSONStatus(w, http.StatusUnauthorized, codeserve.Response{
				"ok":            false,
				"verb":          strings.TrimPrefix(r.URL.Path, "/"),
				"error":         "unauthorized: bearer token required",
				"error_code":    string(codeserve.ErrUnauthorized),
				"product_owned": true,
				"http_status":   http.StatusUnauthorized,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ValidateListenAddr fails closed on a non-loopback bind without a bearer
// token. Loopback hosts (127.0.0.0/8, ::1, localhost) are always allowed.
// An empty host is a wildcard bind (all interfaces) and is not loopback.
func ValidateListenAddr(addr string, p TrustPolicy) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	if p.Token == "" && !p.AllowInsecure {
		return fmt.Errorf(
			"refusing non-loopback listen address %q without a bearer token "+
				"(set --token / SENTRA_CODE_MEMORY_HTTP_TOKEN, or --allow-insecure to opt out)",
			addr)
	}
	return nil
}

// isLoopbackHost reports whether host names only the local loopback
// interface. Empty (wildcard) and unresolvable names are not loopback.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
